// Command bench puts the same decisions to a model and scores its answers, so
// that models can be compared on equal terms. The decisions come from a shadow
// log (real prompts recorded on the robot or the simulator) and from a file of
// hand-written cases with an expected tool.
//
// One run scores one model: the one behind the configured endpoint.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/config"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/fuser"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/llm"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/shadow"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/tools"
)

// caseTime stands in for "now" in the prompts built from cases, so that every
// model reads the same text.
var caseTime = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

// item is one decision to put to the model.
type item struct {
	Name   string
	Prompt string
	State  tools.State
	Expect []string // the tools a good answer may call; empty = no expectation
}

// benchCase is one entry of the cases file. State must agree with what the
// observations say: the model reads the text, the guard reads the state.
type benchCase struct {
	Name         string      `json:"name"`
	Observations []string    `json:"observations"`
	State        tools.State `json:"state"`
	Expect       []string    `json:"expect"`
}

// result is how the model did on one item.
type result struct {
	Item   item
	Record shadow.Record
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config/dog.json", "path to the configuration file (the model's endpoint and the prompts)")
	logPath := flag.String("log", "", "shadow log (JSONL) whose prompts are replayed")
	casesPath := flag.String("cases", "", "cases file (JSON) with expected tools")
	repeat := flag.Int("repeat", 1, "times each item is asked")
	verbose := flag.Bool("v", false, "print every answer, not just the misses")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if cfg.LLM == nil {
		return fmt.Errorf("%s configures no llm", *configPath)
	}

	var items []item
	if *logPath != "" {
		f, err := os.Open(*logPath)
		if err != nil {
			return err
		}
		defer f.Close()
		got, err := itemsFromLog(f)
		if err != nil {
			return fmt.Errorf("%s: %w", *logPath, err)
		}
		items = append(items, got...)
	}
	if *casesPath != "" {
		raw, err := os.ReadFile(*casesPath)
		if err != nil {
			return err
		}
		got, err := itemsFromCases(raw, cfg)
		if err != nil {
			return fmt.Errorf("%s: %w", *casesPath, err)
		}
		items = append(items, got...)
	}
	if len(items) == 0 {
		return fmt.Errorf("nothing to ask: give -log, -cases or both")
	}

	client := llm.New(llm.Config{
		BaseURL:     cfg.LLM.BaseURL,
		Model:       cfg.LLM.Model,
		Timeout:     time.Duration(cfg.LLM.TimeoutS * float64(time.Second)),
		MaxTokens:   cfg.LLM.MaxTokens,
		Temperature: cfg.LLM.Temperature,
		Thinking:    cfg.LLM.Thinking,
	})

	var results []result
	for range *repeat {
		for _, it := range items {
			rec := shadow.Record{Model: cfg.LLM.Model, Prompt: it.Prompt, State: it.State}
			resp, err := client.Complete(context.Background(), shadow.Request(it.Prompt, it.State.Locations))
			if err != nil {
				rec.Error = err.Error()
			} else {
				rec.Fill(resp, it.State)
			}
			r := result{Item: it, Record: rec}
			results = append(results, r)
			if *verbose || !r.good() {
				fmt.Println(r.line())
			}
		}
	}

	fmt.Printf("\nmodel %s at %s\n%s", cfg.LLM.Model, cfg.LLM.BaseURL, summarize(results))
	return nil
}

// itemsFromLog reads the decisions of a shadow log. A record whose completion
// failed still has its prompt and state, so it is replayed too.
func itemsFromLog(r io.Reader) ([]item, error) {
	var items []item
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var rec shadow.Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		name := fmt.Sprintf("tick %d", rec.Tick)
		if len(rec.Reasons) > 0 {
			name += " [" + strings.Join(rec.Reasons, ", ") + "]"
		}
		items = append(items, item{Name: name, Prompt: rec.Prompt, State: rec.State})
	}
	return items, sc.Err()
}

// itemsFromCases builds each case's prompt the way the agent would.
func itemsFromCases(raw []byte, cfg config.Config) ([]item, error) {
	var cases []benchCase
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cases); err != nil {
		return nil, err
	}
	f := fuser.New(fuser.PromptConfig{
		SystemPromptBase:     cfg.SystemPromptBase,
		SystemGovernance:     cfg.SystemGovernance,
		SystemPromptExamples: cfg.SystemPromptExamples,
		Actions:              tools.Actions(tools.Definitions(nil)),
	})
	items := make([]item, len(cases))
	for i, c := range cases {
		if c.Name == "" || len(c.Observations) == 0 {
			return nil, fmt.Errorf("case %d needs a name and observations", i+1)
		}
		items[i] = item{Name: c.Name, Prompt: f.Fuse(caseTime, c.Observations), State: c.State, Expect: c.Expect}
	}
	return items, nil
}

// wellFormed: the model answered with at least one call, each a real tool with
// arguments that fit its schema.
func (r result) wellFormed() bool {
	return r.Record.Error == "" && len(r.Record.Calls) > 0 &&
		!slices.ContainsFunc(r.Record.Calls, func(c shadow.Call) bool { return c.Malformed })
}

// passed: well-formed, and the guard allowed every call.
func (r result) passed() bool {
	return r.wellFormed() && !slices.ContainsFunc(r.Record.Calls, func(c shadow.Call) bool { return !c.Allowed })
}

// expected: an allowed call is one of the tools the case expects.
func (r result) expected() bool {
	return slices.ContainsFunc(r.Record.Calls, func(c shadow.Call) bool {
		return c.Allowed && slices.Contains(r.Item.Expect, c.Name)
	})
}

func (r result) good() bool {
	return r.passed() && (len(r.Item.Expect) == 0 || r.expected())
}

func (r result) line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-4s %s (%d ms)", map[bool]string{true: "ok", false: "MISS"}[r.good()], r.Item.Name, r.Record.LatencyMS)
	if len(r.Item.Expect) > 0 {
		fmt.Fprintf(&b, ", expected %s", strings.Join(r.Item.Expect, " or "))
	}
	switch {
	case r.Record.Error != "":
		fmt.Fprintf(&b, "\n       error: %s", r.Record.Error)
	case len(r.Record.Calls) == 0:
		fmt.Fprintf(&b, "\n       no tool call; said: %q", r.Record.Content)
	}
	if r.Record.Truncated {
		fmt.Fprintf(&b, "\n       cut off at max_tokens (%d completion tokens)", r.Record.CompletionTokens)
	}
	for _, c := range r.Record.Calls {
		fmt.Fprintf(&b, "\n       %s %s", c.Name, c.Arguments)
		if !c.Allowed {
			fmt.Fprintf(&b, "  -> refused: %s", c.Refusal)
		}
	}
	return b.String()
}

// summarize renders the scores of one model.
func summarize(results []result) string {
	var (
		errs, formed, passed, withExpect, expected, truncated int
		latencies                                             []int64
		tps                                                   float64
	)
	for _, r := range results {
		if r.Record.Error != "" {
			errs++
			continue
		}
		latencies = append(latencies, r.Record.LatencyMS)
		tps += r.Record.TokensPerSecond
		if r.Record.Truncated {
			truncated++
		}
		if r.wellFormed() {
			formed++
		}
		if r.passed() {
			passed++
		}
		if len(r.Item.Expect) > 0 {
			withExpect++
			if r.passed() && r.expected() {
				expected++
			}
		}
	}

	n := len(results)
	var b strings.Builder
	fmt.Fprintf(&b, "  asked              %d (%d failed to complete)\n", n, errs)
	fmt.Fprintf(&b, "  well-formed calls  %s\n", ratio(formed, n))
	fmt.Fprintf(&b, "  passed the guard   %s\n", ratio(passed, n))
	if truncated > 0 {
		fmt.Fprintf(&b, "  cut off            %s hit max_tokens\n", ratio(truncated, n))
	}
	if withExpect > 0 {
		fmt.Fprintf(&b, "  expected tool      %s\n", ratio(expected, withExpect))
	}
	if len(latencies) > 0 {
		slices.Sort(latencies)
		fmt.Fprintf(&b, "  latency            p50 %d ms, p95 %d ms\n", percentile(latencies, 50), percentile(latencies, 95))
		fmt.Fprintf(&b, "  decode rate        %.1f tok/s (mean)\n", tps/float64(len(latencies)))
	}
	return b.String()
}

func ratio(a, n int) string {
	if n == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", a, n, 100*float64(a)/float64(n))
}

// percentile reads the nearest-rank percentile of sorted values.
func percentile(sorted []int64, p int) int64 {
	rank := (len(sorted)*p + 99) / 100
	return sorted[max(rank, 1)-1]
}

package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/backend"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/config"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/fuser"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/inputs"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/llm"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/runtime"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/shadow"
	"github.com/chungweeeei/SyncAI-Robot-Agent/internal/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config/dog.json", "path to the configuration file")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	printPrompt := flag.Bool("print-prompt", false, "print each tick's prompt to stdout")
	noLLM := flag.Bool("no-llm", false, "do not call the model; only fuse (and print) prompts")
	shadowLog := flag.String("shadow-log", "shadow.jsonl", `file the model's decisions are appended to, one JSON object per line; "" to disable`)
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("-log-level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The timeout stays under the sensors' poll intervals. The client only has
	// GET methods: nothing in this process can start, change or stop a task.
	client := backend.NewClient(cfg.BackendURL, time.Second)
	robot := inputs.NewRobotState(client, inputs.DefaultRobotStateConfig())
	tasks := inputs.NewTaskStatus(client, inputs.DefaultTaskStatusConfig())

	opts := runtime.DefaultOptions()
	opts.Hertz = cfg.Hertz
	prompt := fuser.PromptConfig{
		SystemPromptBase:     cfg.SystemPromptBase,
		SystemGovernance:     cfg.SystemGovernance,
		SystemPromptExamples: cfg.SystemPromptExamples,
	}

	useLLM := cfg.LLM != nil && !*noLLM
	if useLLM {
		sh := &shadow.Shadow{
			LLM: llm.New(llm.Config{
				BaseURL:     cfg.LLM.BaseURL,
				Model:       cfg.LLM.Model,
				Timeout:     time.Duration(cfg.LLM.TimeoutS * float64(time.Second)),
				MaxTokens:   cfg.LLM.MaxTokens,
				Temperature: cfg.LLM.Temperature,
			}),
			Model:    cfg.LLM.Model,
			Robot:    robot.Current,
			Tasks:    tasks.Current,
			Vertices: client,
			Log:      log,
		}
		if *shadowLog != "" {
			f, err := os.OpenFile(*shadowLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			sh.SetOutput(f)
		}
		prompt.Actions = tools.Actions(tools.Definitions(nil))
		opts.Decide = sh.Decide
		opts.DecideEvery = cfg.DecideEvery
	}

	if *printPrompt || !useLLM {
		opts.OnTick = func(t runtime.Tick) {
			fmt.Printf("===== tick %d [%s] [%s] =====\n%s\n\n", t.N, t.Trigger, strings.Join(t.Reasons, ", "), t.Prompt)
		}
	}

	log.Info("agent starting", "name", cfg.Name, "backend", cfg.BackendURL, "hertz", cfg.Hertz,
		"shadow", useLLM, "shadow_log", *shadowLog)
	rt := runtime.New([]inputs.Sensor{robot, tasks}, fuser.New(prompt), log, opts)
	return rt.Run(ctx)
}

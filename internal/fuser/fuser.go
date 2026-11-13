// Package fuser assembles the prompt from the sensors' texts.
package fuser

import (
	"strings"
	"time"
)

// PromptConfig holds the fixed parts of the prompt.
type PromptConfig struct {
	SystemPromptBase     string
	SystemGovernance     string
	SystemPromptExamples string

	// Actions are the lines of the "Available actions" section, one per tool.
	// Empty leaves the section out (no LLM wired).
	Actions []string
}

// Fuser builds one prompt per tick. It knows nothing of sensors: it is handed
// lines that are already rendered, which keeps it a pure function.
type Fuser struct {
	cfg PromptConfig
}

// New creates a Fuser.
func New(cfg PromptConfig) *Fuser {
	cfg.SystemPromptBase = strings.TrimSpace(cfg.SystemPromptBase)
	cfg.SystemGovernance = strings.TrimSpace(cfg.SystemGovernance)
	cfg.SystemPromptExamples = strings.TrimSpace(cfg.SystemPromptExamples)
	return &Fuser{cfg: cfg}
}

// Fuse lays the prompt out the way OM1 does. A section with nothing in it is
// left out whole, and an empty observation is skipped.
func (f *Fuser) Fuse(now time.Time, observations []string) string {
	var sections []string
	add := func(s string) {
		if s != "" {
			sections = append(sections, s)
		}
	}

	add(f.cfg.SystemPromptBase)
	if f.cfg.SystemGovernance != "" {
		add("Governance rules:\n" + f.cfg.SystemGovernance)
	}
	add("Current time is " + now.Format("Monday, January 2, 2006 at 3:04 PM") + ".")
	add(list("Current observations:", observations))
	add(list("Available actions:", f.cfg.Actions))
	add(f.cfg.SystemPromptExamples)
	add("What will you do next?")

	return strings.Join(sections, "\n\n")
}

// list renders a titled bullet list, or "" when no item has text.
func list(title string, items []string) string {
	var b strings.Builder
	for _, it := range items {
		if it = strings.TrimSpace(it); it != "" {
			b.WriteString("\n- " + it)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return title + b.String()
}

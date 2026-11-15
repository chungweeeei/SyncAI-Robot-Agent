// Package config loads the agent's JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Config is the agent's configuration file.
type Config struct {
	Name string `json:"name"`
	// Hertz is the base tick rate. Kept low: the sensors' wakes cover what needs
	// a prompt answer.
	Hertz      float64 `json:"hertz"`
	BackendURL string  `json:"backend_url"`

	SystemPromptBase     string `json:"system_prompt_base"`
	SystemGovernance     string `json:"system_governance"`
	SystemPromptExamples string `json:"system_prompt_examples"`

	// LLM is the local model. Left out, the agent only prints its prompts.
	LLM *LLM `json:"llm"`
	// DecideEvery asks the model on every n-th idle tick besides every wake;
	// 0 asks on wakes only.
	DecideEvery int `json:"decide_every"`
}

// LLM is an OpenAI-compatible endpoint (llama-server, Ollama).
type LLM struct {
	BaseURL     string  `json:"base_url"`
	Model       string  `json:"model"`
	TimeoutS    float64 `json:"timeout_s"`
	MaxTokens   int     `json:"max_tokens"`
	Temperature float64 `json:"temperature"`
	// Thinking lets a reasoning model think first. It multiplies the latency and
	// the tokens of a decision, so max_tokens must be raised with it.
	Thinking bool `json:"thinking"`
}

// Load reads, expands and validates a configuration file.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader([]byte(expandEnv(string(raw)))))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports the first thing wrong with the configuration.
func (c Config) Validate() error {
	switch {
	case strings.TrimSpace(c.Name) == "":
		return errors.New("name is empty")
	case c.Hertz <= 0 || c.Hertz > 10:
		return fmt.Errorf("hertz %v is outside (0, 10]", c.Hertz)
	case strings.TrimSpace(c.SystemPromptBase) == "":
		return errors.New("system_prompt_base is empty")
	case c.DecideEvery < 0:
		return errors.New("decide_every is negative")
	}
	if err := checkURL(c.BackendURL); err != nil {
		return fmt.Errorf("backend_url: %w", err)
	}
	if l := c.LLM; l != nil {
		if err := checkURL(l.BaseURL); err != nil {
			return fmt.Errorf("llm.base_url: %w", err)
		}
		switch {
		case l.TimeoutS <= 0:
			return errors.New("llm.timeout_s must be positive")
		case l.MaxTokens <= 0:
			return errors.New("llm.max_tokens must be positive")
		case l.Temperature < 0 || l.Temperature > 2:
			return fmt.Errorf("llm.temperature %v is outside [0, 2]", l.Temperature)
		}
	}
	return nil
}

func checkURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http(s) URL with a host", s)
	}
	return nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// expandEnv replaces ${VAR} and ${VAR:-default} with the environment's value,
// the default when the variable is unset or empty. Not os.Expand: that would
// also rewrite a "$5" or "$x" inside a prompt.
func expandEnv(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(ref string) string {
		m := envRef.FindStringSubmatch(ref)
		if v := os.Getenv(m[1]); v != "" {
			return v
		}
		return m[2]
	})
}

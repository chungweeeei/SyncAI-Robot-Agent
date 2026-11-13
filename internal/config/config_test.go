package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const valid = `{
  "name": "dog",
  "hertz": 0.2,
  "backend_url": "${SYNCAI_BACKEND_URL:-http://localhost:3000}",
  "system_prompt_base": "You cost $5 and $x.",
  "system_governance": "",
  "system_prompt_examples": ""
}`

func load(t *testing.T, content string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoad(t *testing.T) {
	t.Setenv("SYNCAI_BACKEND_URL", "")
	cfg, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	// An empty variable falls back to the default, and a "$" in a prompt is left alone.
	if cfg.BackendURL != "http://localhost:3000" || cfg.SystemPromptBase != "You cost $5 and $x." || cfg.LLM != nil {
		t.Errorf("cfg = %+v", cfg)
	}

	t.Setenv("SYNCAI_BACKEND_URL", "http://dog:3000")
	if cfg, err = load(t, valid); err != nil || cfg.BackendURL != "http://dog:3000" {
		t.Errorf("with the variable set: %+v, %v", cfg, err)
	}
}

func TestLoadLLM(t *testing.T) {
	withLLM := strings.Replace(valid, `"name": "dog",`,
		`"name": "dog", "decide_every": 12,
		 "llm": {"base_url": "${LLM_URL:-http://localhost:8080}", "model": "qwen", "timeout_s": 30, "max_tokens": 512, "temperature": 0.2},`, 1)
	cfg, err := load(t, withLLM)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM == nil || cfg.LLM.BaseURL != "http://localhost:8080" || cfg.LLM.MaxTokens != 512 || cfg.DecideEvery != 12 {
		t.Errorf("cfg = %+v, llm = %+v", cfg, cfg.LLM)
	}
}

func TestLoadRejects(t *testing.T) {
	llm := func(body string) string {
		return strings.Replace(valid, `"name": "dog",`, `"name": "dog", "llm": {`+body+`},`, 1)
	}
	for name, tt := range map[string]struct{ content, want string }{
		"unknown key":    {strings.Replace(valid, `"name"`, `"nmae": "x", "name"`, 1), "unknown field"},
		"no name":        {strings.Replace(valid, `"dog"`, `" "`, 1), "name is empty"},
		"hertz zero":     {strings.Replace(valid, `0.2`, `0`, 1), "hertz"},
		"hertz too high": {strings.Replace(valid, `0.2`, `11`, 1), "hertz"},
		"bad url":        {strings.Replace(valid, `${SYNCAI_BACKEND_URL:-http://localhost:3000}`, `localhost:3000`, 1), "backend_url"},
		"no prompt":      {strings.Replace(valid, `You cost $5 and $x.`, ``, 1), "system_prompt_base"},
		"llm url":        {llm(`"base_url": "", "timeout_s": 30, "max_tokens": 512`), "llm.base_url"},
		"llm timeout":    {llm(`"base_url": "http://localhost:8080", "max_tokens": 512`), "llm.timeout_s"},
		"llm max tokens": {llm(`"base_url": "http://localhost:8080", "timeout_s": 30`), "llm.max_tokens"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SYNCAI_BACKEND_URL", "")
			if _, err := load(t, tt.content); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

// The shipped configuration must load.
func TestShippedConfig(t *testing.T) {
	cfg, err := Load("../../config/dog.json")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM == nil {
		t.Error("config/dog.json should configure the local model")
	}
}

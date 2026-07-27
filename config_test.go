package main

import (
	"net/http"
	"testing"
)

const sampleConfig = `
enabled: true
max_retries: 10
retry_delay_ms: 200
protected_models:
  - model: "my-glm-5.2"
    expect_model: "glm-5.2"
  - model: "my-glm-5.1"
    expect_model: "glm-5.1"
    max_retries: 3
`

func TestDecodeConfig(t *testing.T) {
	cfg, errDecode := decodeConfig([]byte(sampleConfig))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	if !cfg.Enabled || cfg.MaxRetries != 10 || cfg.RetryDelayMs != 200 {
		t.Fatalf("unexpected globals: %+v", cfg)
	}
	if len(cfg.ProtectedModels) != 2 {
		t.Fatalf("want 2 protected models, got %d", len(cfg.ProtectedModels))
	}

	entry, ok := cfg.findProtectedModel("MY-GLM-5.2")
	if !ok {
		t.Fatal("lookup should be case insensitive")
	}
	if got := cfg.retryBudget(entry); got != 10 {
		t.Fatalf("global budget = %d, want 10", got)
	}

	override, ok := cfg.findProtectedModel("my-glm-5.1")
	if !ok {
		t.Fatal("my-glm-5.1 should be protected")
	}
	if got := cfg.retryBudget(override); got != 3 {
		t.Fatalf("per-model budget = %d, want 3", got)
	}

	if _, ok := cfg.findProtectedModel("gpt-5.6-sol"); ok {
		t.Fatal("unprotected model must not match")
	}
}

func TestDecodeConfigDefaults(t *testing.T) {
	cfg, errDecode := decodeConfig([]byte("protected_models:\n  - model: glm-5.2\n"))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	if cfg.MaxRetries != defaultMaxRetries || cfg.RetryDelayMs != defaultRetryDelayMs {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestDecodeConfigDropsBlankEntries(t *testing.T) {
	cfg, errDecode := decodeConfig([]byte("protected_models:\n  - model: \"  \"\n  - model: glm-5.2\n"))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	if len(cfg.ProtectedModels) != 1 {
		t.Fatalf("blank entry should be dropped, got %+v", cfg.ProtectedModels)
	}
}

func TestCarriesBypass(t *testing.T) {
	if carriesBypass(nil) {
		t.Fatal("nil headers must not carry a bypass marker")
	}
	if carriesBypass(http.Header{}) {
		t.Fatal("empty headers must not carry a bypass marker")
	}
	headers := http.Header{}
	headers.Set(bypassHeader, newBypassToken())
	if !carriesBypass(headers) {
		t.Fatal("plugin-issued replay must be detected")
	}
}

func TestResolveProtection(t *testing.T) {
	cfg, errDecode := decodeConfig([]byte(sampleConfig))
	if errDecode != nil {
		t.Fatalf("decodeConfig: %v", errDecode)
	}
	_, expected, protected := resolveProtection(cfg, "my-glm-5.2", "my-glm-5.2")
	if !protected {
		t.Fatal("my-glm-5.2 should resolve as protected")
	}
	if expected != "glm-5.2" {
		t.Fatalf("expected model = %q, want glm-5.2", expected)
	}

	if _, _, protected := resolveProtection(cfg, "gpt-5.6-sol", "gpt-5.6-sol"); protected {
		t.Fatal("gpt-5.6-sol must not be protected")
	}
}

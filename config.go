package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const (
	defaultMaxRetries   = 10
	defaultRetryDelayMs = 200
)

var currentConfig atomic.Value

// protectedModel declares one model that must never be served by a different model.
type protectedModel struct {
	// Model is the client-facing model name, matched against the inbound request.
	Model string `yaml:"model"`
	// ExpectModel is the processing model name the upstream is expected to report.
	// It is only needed when the alias and the upstream name differ in a way the
	// built-in matcher cannot bridge.
	ExpectModel string `yaml:"expect_model"`
	// MaxRetries overrides the global retry budget for this model when non-nil.
	MaxRetries *int `yaml:"max_retries"`
}

type pluginConfig struct {
	Enabled         bool             `yaml:"enabled"`
	MaxRetries      int              `yaml:"max_retries"`
	RetryDelayMs    int              `yaml:"retry_delay_ms"`
	ProtectedModels []protectedModel `yaml:"protected_models"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:      true,
		MaxRetries:   defaultMaxRetries,
		RetryDelayMs: defaultRetryDelayMs,
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	currentConfig.Store(cfg)
	return nil
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	}
	if cfg.RetryDelayMs < 0 {
		cfg.RetryDelayMs = 0
	}
	cleaned := make([]protectedModel, 0, len(cfg.ProtectedModels))
	for _, entry := range cfg.ProtectedModels {
		entry.Model = strings.TrimSpace(entry.Model)
		entry.ExpectModel = strings.TrimSpace(entry.ExpectModel)
		if entry.Model == "" {
			continue
		}
		if entry.MaxRetries != nil && *entry.MaxRetries < 0 {
			zero := 0
			entry.MaxRetries = &zero
		}
		cleaned = append(cleaned, entry)
	}
	cfg.ProtectedModels = cleaned
	return cfg, nil
}

func loadedConfig() pluginConfig {
	if cfg, ok := currentConfig.Load().(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

// findProtectedModel returns the protection rule covering model, if any.
// Candidates are compared case-insensitively so config stays forgiving.
func (c pluginConfig) findProtectedModel(model string) (protectedModel, bool) {
	needle := strings.ToLower(strings.TrimSpace(model))
	if needle == "" {
		return protectedModel{}, false
	}
	for _, entry := range c.ProtectedModels {
		if strings.ToLower(entry.Model) == needle {
			return entry, true
		}
	}
	return protectedModel{}, false
}

// retryBudget resolves the effective retry count for a protection rule.
func (c pluginConfig) retryBudget(entry protectedModel) int {
	if entry.MaxRetries != nil {
		return *entry.MaxRetries
	}
	return c.MaxRetries
}

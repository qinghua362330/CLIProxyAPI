package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigOptional_CodexHeaderDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := []byte(`
codex-header-defaults:
  user-agent: "  my-codex-client/1.0  "
  beta-features: "  feature-a,feature-b  "
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if got := cfg.CodexHeaderDefaults.UserAgent; got != "my-codex-client/1.0" {
		t.Fatalf("UserAgent = %q, want %q", got, "my-codex-client/1.0")
	}
	if got := cfg.CodexHeaderDefaults.BetaFeatures; got != "feature-a,feature-b" {
		t.Fatalf("BetaFeatures = %q, want %q", got, "feature-a,feature-b")
	}
}

func TestLoadConfigOptional_CodexIdentityConfuse(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	configYAML := []byte(`
codex:
  identity-confuse: true
`)
	if err := os.WriteFile(configPath, configYAML, 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadConfigOptional(configPath, false)
	if err != nil {
		t.Fatalf("LoadConfigOptional() error = %v", err)
	}

	if !cfg.Codex.IdentityConfuse {
		t.Fatalf("IdentityConfuse = false, want true")
	}
}

func TestLoadConfigOptional_CodexRequestCompression(t *testing.T) {
	tests := []struct {
		name       string
		configYAML string
		wantNil    bool
		want       bool
	}{
		{name: "omitted", configYAML: "codex:\n  identity-confuse: false\n", wantNil: true},
		{name: "true", configYAML: "codex:\n  request-compression: true\n", want: true},
		{name: "false", configYAML: "codex:\n  request-compression: false\n", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.configYAML), 0o600); err != nil {
				t.Fatalf("failed to write config: %v", err)
			}

			cfg, err := LoadConfigOptional(configPath, false)
			if err != nil {
				t.Fatalf("LoadConfigOptional() error = %v", err)
			}
			if tt.wantNil {
				if cfg.Codex.RequestCompression != nil {
					t.Fatalf("RequestCompression = %v, want nil", *cfg.Codex.RequestCompression)
				}
				return
			}
			if cfg.Codex.RequestCompression == nil {
				t.Fatal("RequestCompression = nil, want non-nil")
			}
			if got := *cfg.Codex.RequestCompression; got != tt.want {
				t.Fatalf("RequestCompression = %v, want %v", got, tt.want)
			}
		})
	}
}

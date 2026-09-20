package config

import "testing"

func TestNormalizeCodexClientVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "0.155.1", want: "0.155.1"},
		{input: "  1.2  ", want: "1.2"},
		{input: "1.2.3.4", want: "1.2.3.4"},
		{input: "", want: ""},
		{input: "v0.155.1", want: ""},
		{input: "0.155.1-beta", want: ""},
		{input: "rust-v0.155.1", want: ""},
	}
	for _, tt := range tests {
		if got := NormalizeCodexClientVersion(tt.input); got != tt.want {
			t.Fatalf("NormalizeCodexClientVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCompareCodexClientVersions(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"0.155.1", "0.155.0", 1},
		{"0.155.0", "0.155.1", -1},
		{"0.155.1", "0.155.1", 0},
		{"0.9.0", "0.10.0", -1},
	}
	for _, tt := range tests {
		if got := CompareCodexClientVersions(tt.left, tt.right); got != tt.want {
			t.Fatalf("CompareCodexClientVersions(%q, %q) = %d, want %d", tt.left, tt.right, got, tt.want)
		}
	}
}

func TestEffectiveCodexClientVersionPrefersOverrideThenSynced(t *testing.T) {
	cfg := Default()
	if got := cfg.EffectiveCodexClientVersion(); got != CodexClientVersionFallback {
		t.Fatalf("fallback version = %q", got)
	}

	cfg.CodexClientVersionSynced = "0.160.0"
	cfg.Normalize()
	if got := cfg.EffectiveCodexClientVersion(); got != "0.160.0" {
		t.Fatalf("synchronized version = %q", got)
	}

	cfg.CodexClientVersion = "0.170.0"
	cfg.Normalize()
	if got := cfg.EffectiveCodexClientVersion(); got != "0.170.0" {
		t.Fatalf("override version = %q", got)
	}
}

func TestCodexClientVersionIgnoredWhenInvalid(t *testing.T) {
	cfg := Default()
	cfg.CodexClientVersion = "not-a-version"
	cfg.CodexClientVersionSynced = "rust-v0.160.0"
	cfg.Normalize()
	if got := cfg.EffectiveCodexClientVersion(); got != CodexClientVersionFallback {
		t.Fatalf("invalid versions must fall back, got %q", got)
	}
}

func TestCodexVersionAutoSyncDefaultsToEnabled(t *testing.T) {
	cfg := Default()
	if !cfg.CodexVersionAutoSyncEnabled() {
		t.Fatal("auto sync must default to enabled")
	}
	disabled := false
	cfg.CodexVersionAutoSync = &disabled
	cfg.Normalize()
	if cfg.CodexVersionAutoSyncEnabled() {
		t.Fatal("auto sync must stay disabled once cleared")
	}
}

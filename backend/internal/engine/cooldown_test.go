package engine

import (
	"net/http"
	"testing"

	"github.com/minkicc/mkrouter/backend/internal/config"
)

func TestCooldownWithReasonSurfacesCountdownAndReason(t *testing.T) {
	eng, err := New(testConfig(config.Channel{
		ID:      "#1",
		Name:    "primary",
		BaseURL: "https://example.com",
		Models:  []string{"gpt-5.6-sol"},
	}))
	if err != nil {
		t.Fatal(err)
	}

	eng.CooldownWithReason("#1", "upstream returned status 502")

	state := eng.State()[0]
	if state.CooldownReason != "upstream returned status 502" {
		t.Fatalf("cooldown reason = %q", state.CooldownReason)
	}
	if state.CooldownUntil == 0 || state.CooldownDuration == 0 {
		t.Fatalf("cooldown window = %+v", state)
	}
	if state.Status != StatusUnhealthy {
		t.Fatalf("status = %q", state.Status)
	}
}

func TestCooldownWithoutReasonUsesLastError(t *testing.T) {
	eng, err := New(testConfig(config.Channel{
		ID:      "#1",
		BaseURL: "https://example.com",
		Models:  []string{"gpt-5.6-sol"},
	}))
	if err != nil {
		t.Fatal(err)
	}

	eng.recordFailure("#1", http.StatusBadGateway, 0, errString("dial tcp: connection refused"))
	eng.Cooldown("#1")

	if got := eng.State()[0].CooldownReason; got != "dial tcp: connection refused" {
		t.Fatalf("cooldown reason = %q", got)
	}
}

func TestSuccessfulRequestClearsCooldownReason(t *testing.T) {
	eng, err := New(testConfig(config.Channel{
		ID:      "#1",
		BaseURL: "https://example.com",
		Models:  []string{"gpt-5.6-sol"},
	}))
	if err != nil {
		t.Fatal(err)
	}

	eng.CooldownWithReason("#1", "upstream returned status 502")
	eng.recordSuccess("#1", http.StatusOK, 0, true)

	state := eng.State()[0]
	if state.CooldownReason != "" {
		t.Fatalf("cooldown reason = %q, want cleared", state.CooldownReason)
	}
	if state.CooldownCount != 0 {
		t.Fatalf("cooldown count = %d, want reset", state.CooldownCount)
	}
}

func TestCodexChannelUsesConfiguredVersionOverride(t *testing.T) {
	cfg := testConfig(config.Channel{
		ID:       "#1",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken: "access",
			AccountID:   "account-1",
		},
		Models: []string{"*"},
	})
	cfg.CodexClientVersion = "0.170.0"
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if got := eng.CodexClientVersion(); got != "0.170.0" {
		t.Fatalf("resolved version = %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

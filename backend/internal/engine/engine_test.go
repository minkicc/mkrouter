package engine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/minkicc/mkrouter/backend/internal/config"
	"github.com/minkicc/mkrouter/backend/internal/routing"
)

type engineRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn engineRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func boolPtr(v bool) *bool { return &v }

func testConfig(channels ...config.Channel) *config.Config {
	cfg := config.Default()
	cfg.Channels = channels
	cfg.Normalize()
	return cfg
}

func TestSelectPicksHighestPriority(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "low", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}, Priority: 1},
		config.Channel{ID: "high", BaseURL: "https://b.example.com", Models: []string{"gpt-4"}, Priority: 10},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := eng.Select("gpt-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.ID != "high" {
		t.Fatalf("picked %q, want high", got.Channel.ID)
	}
}

func TestSelectPrefersHealthyOverUnknown(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "unknown", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}, Priority: 10},
		config.Channel{ID: "healthy", BaseURL: "https://b.example.com", Models: []string{"gpt-4"}, Priority: 0},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RecordAttempt("healthy", 200, nil, 10*time.Millisecond)
	got, err := eng.Select("gpt-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.ID != "healthy" {
		t.Fatalf("picked %q, want healthy", got.Channel.ID)
	}
}

func TestSelectExcludesFailedChannel(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "a", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Select("gpt-4", map[string]bool{"a": true}); err == nil {
		t.Fatal("expected error when all channels are excluded")
	}
}

func TestSelectSkipsChannelInCooldown(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "a", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}, Priority: 10},
		config.Channel{ID: "b", BaseURL: "https://b.example.com", Models: []string{"gpt-4"}, Priority: 1},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	eng.Cooldown("a")
	got, err := eng.Select("gpt-4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.ID != "b" {
		t.Fatalf("picked %q, want b (a should be in cooldown)", got.Channel.ID)
	}

	reported := false
	for _, st := range eng.State() {
		if st.ID == "a" && st.CooldownUntil != 0 {
			reported = true
		}
	}
	if !reported {
		t.Fatal("expected channel a to report a cooldown deadline")
	}
}

func TestRecordAttemptDoesNotCooldown(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "a", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.RecordAttempt("a", 500, nil, 10*time.Millisecond)
	for _, st := range eng.State() {
		if st.ID == "a" && st.CooldownUntil != 0 {
			t.Fatal("RecordAttempt alone should not trigger cooldown")
		}
	}
}

func TestCooldownDurationEscalates(t *testing.T) {
	cases := []struct {
		n    int
		want time.Duration
	}{
		{1, 60 * time.Second},
		{2, 5 * time.Minute},
		{3, 15 * time.Minute},
		{4, 15 * time.Minute},
		{10, 15 * time.Minute},
	}
	for _, c := range cases {
		if got := cooldownDuration(c.n); got != c.want {
			t.Errorf("cooldownDuration(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

func TestCooldownEscalatesAndResetsOnSuccess(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "a", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	eng.Cooldown("a")
	eng.Cooldown("a")
	eng.Cooldown("a")
	eng.Cooldown("a")

	cooldownCount := func() int {
		for _, st := range eng.State() {
			if st.ID == "a" {
				return st.CooldownCount
			}
		}
		return -1
	}
	if got := cooldownCount(); got != 4 {
		t.Fatalf("cooldown count = %d, want 4", got)
	}

	// A successful attempt resets the escalation ladder.
	eng.RecordAttempt("a", 200, nil, 10*time.Millisecond)
	eng.Cooldown("a")
	if got := cooldownCount(); got != 1 {
		t.Fatalf("cooldown count after success = %d, want 1", got)
	}
}

func TestCooldownMarksChannelUnhealthy(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "a", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	eng.RecordAttempt("a", 200, nil, 10*time.Millisecond)
	eng.Cooldown("a")

	for _, st := range eng.State() {
		if st.ID == "a" {
			if st.Status != StatusUnhealthy {
				t.Fatalf("status after cooldown = %q, want unhealthy", st.Status)
			}
			return
		}
	}
	t.Fatal("channel a not found")
}

func TestHealthCheckSuccessDoesNotResetCooldown(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "a", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	eng.Cooldown("a")
	eng.Cooldown("a")

	// A successful /v1/models probe must not reset the escalation ladder.
	eng.recordSuccess("a", 200, 10*time.Millisecond, false)

	for _, st := range eng.State() {
		if st.ID == "a" {
			if st.CooldownCount != 2 {
				t.Fatalf("cooldown count after health success = %d, want 2", st.CooldownCount)
			}
			if st.Status != StatusUnhealthy {
				t.Fatalf("status after health success during cooldown = %q, want unhealthy", st.Status)
			}
			return
		}
	}
	t.Fatal("channel a not found")
}

func TestHealthCheckSkippedDuringCooldown(t *testing.T) {
	cfg := testConfig(
		config.Channel{ID: "a", BaseURL: "https://a.example.com", Models: []string{"gpt-4"}},
	)
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	eng.Cooldown("a")

	probed := false
	eng.healthClient.Transport = engineRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		probed = true
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})

	eng.CheckNow("a")
	if probed {
		t.Fatal("health probe must be skipped while the channel is cooling down")
	}
}

func TestResolveChannelModelMapping(t *testing.T) {
	ch := config.Channel{
		ID:            "deepseek",
		BaseURL:       "https://api.deepseek.com",
		Models:        []string{"deepseek-chat"},
		ModelMappings: map[string]string{"codex-default": "deepseek-chat"},
	}
	route := routing.Route{
		ID:            ch.ID,
		Models:        ch.Models,
		ModelMappings: ch.ModelMappings,
	}
	got, ok := routing.ResolveUpstreamModel(&route, "codex-default", nil)
	if !ok || got != "deepseek-chat" {
		t.Fatalf("resolve = %q, %v; want deepseek-chat, true", got, ok)
	}
}

func TestResolveGlobalMappingOnlyForSupportingChannel(t *testing.T) {
	ch := config.Channel{
		ID:     "ds",
		Models: []string{"deepseek-chat"},
	}
	mappings := []routing.ModelMapping{
		{
			PlatformModel: "codex-default",
			UpstreamModel: "deepseek-chat",
			Enabled:       boolPtr(true),
		},
	}
	route := routing.Route{ID: ch.ID, Models: ch.Models}
	got, ok := routing.ResolveUpstreamModel(&route, "codex-default", mappings)
	if !ok || got != "deepseek-chat" {
		t.Fatalf("resolve = %q, %v; want deepseek-chat, true", got, ok)
	}
	unsupported := routing.Route{ID: "other", Models: []string{"gpt-5.6-sol"}}
	if got, ok := routing.ResolveUpstreamModel(&unsupported, "codex-default", mappings); ok {
		t.Fatalf("unsupported channel resolved mapping as %q", got)
	}
}

func TestBuildURL(t *testing.T) {
	cases := []struct {
		base string
		path string
		want string
	}{
		{"https://api.deepseek.com", "/v1/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1", "/v1/chat/completions", "https://api.deepseek.com/v1/chat/completions"},
		{"https://api.deepseek.com/v1/", "/v1/models", "https://api.deepseek.com/v1/models"},
	}
	for _, c := range cases {
		got := BuildURL(c.base, c.path)
		if got != c.want {
			t.Errorf("BuildURL(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

func TestCodexHealthProbeUsesSharedAuthorizationResolver(t *testing.T) {
	ch := config.Channel{
		ID:       "#1",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken: "stale-access",
			AccountID:   "account-1",
		},
		Models: []string{"*"},
	}
	eng, err := New(testConfig(ch))
	if err != nil {
		t.Fatal(err)
	}
	resolverCalls := 0
	eng.SetCodexAuthResolver(func(ctx context.Context, channelID string) (*config.CodexAuth, error) {
		resolverCalls++
		if channelID != "#1" {
			t.Fatalf("channel id = %q", channelID)
		}
		return &config.CodexAuth{
			AccessToken: "fresh-access",
			AccountID:   "account-1",
		}, nil
	})
	eng.healthClient.Transport = engineRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://chatgpt.com/backend-api/codex/models?client_version="+config.CodexClientVersionFallback {
			t.Fatalf("target = %s", req.URL)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer fresh-access" {
			t.Fatalf("authorization = %q", got)
		}
		if got := req.Header.Get("ChatGPT-Account-ID"); got != "account-1" {
			t.Fatalf("account id = %q", got)
		}
		if req.Header.Get("User-Agent") != "codex_cli_rs/"+config.CodexClientVersionFallback ||
			req.Header.Get("Originator") != "codex_cli_rs" ||
			req.Header.Get("Version") != config.CodexClientVersionFallback {
			t.Fatalf("Codex model headers = %v", req.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})

	status, _, err := eng.probe(ch, "/v1/models", 2)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || resolverCalls != 1 {
		t.Fatalf("status = %d, resolver calls = %d", status, resolverCalls)
	}
}

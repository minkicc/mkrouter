package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/minkicc/mkrouter/backend/internal/config"
	"github.com/minkicc/mkrouter/backend/internal/engine"
)

func withCodexReleaseFeed(t *testing.T, handler roundTripFunc) {
	t.Helper()
	original := codexVersionClient
	codexVersionClient = &http.Client{Transport: handler}
	t.Cleanup(func() { codexVersionClient = original })
}

func releasesBody(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func TestStableCodexReleaseVersionSkipsPrereleaseAndUnrelatedTags(t *testing.T) {
	version := stableCodexReleaseVersion([]codexRelease{
		{TagName: "rust-v0.156.0", Prerelease: true},
		{TagName: "v0.157.0"},
		{TagName: "rust-v0.154.0"},
		{TagName: "rust-v0.155.1"},
		{TagName: "rust-v0.155.0"},
		{TagName: "rust-v0.155.2", Draft: true},
		{TagName: "rust-vnot-a-version"},
	})
	if version != "0.155.1" {
		t.Fatalf("stable version = %q, want 0.155.1", version)
	}
}

func TestSyncCodexClientVersionPersistsLatestRelease(t *testing.T) {
	cfg := config.Default()
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	withCodexReleaseFeed(t, func(req *http.Request) (*http.Response, error) {
		requests++
		if got := req.Header.Get("Accept"); got != "application/vnd.github+json" {
			t.Fatalf("accept = %q", got)
		}
		return releasesBody(`{"tag_name":"rust-v0.155.1","draft":false,"prerelease":false}`), nil
	})
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	srv := &Server{engine: eng, cfgPath: cfgPath, logger: log.New(io.Discard, "", 0)}

	srv.syncCodexClientVersion(context.Background())

	if requests == 0 {
		t.Fatal("release feed was not queried")
	}
	if got := eng.Config().CodexClientVersionSynced; got != "0.155.1" {
		t.Fatalf("synchronized version = %q", got)
	}
	if got := eng.CodexClientVersion(); got != "0.155.1" {
		t.Fatalf("resolved version = %q", got)
	}
	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CodexClientVersionSynced != "0.155.1" {
		t.Fatalf("persisted version = %q", saved.CodexClientVersionSynced)
	}
}

func TestSyncCodexClientVersionSkipsWhenAutoSyncDisabled(t *testing.T) {
	cfg := config.Default()
	disabled := false
	cfg.CodexVersionAutoSync = &disabled
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	withCodexReleaseFeed(t, func(req *http.Request) (*http.Response, error) {
		requests++
		return releasesBody(`{"tag_name":"rust-v0.155.1"}`), nil
	})
	srv := &Server{engine: eng, logger: log.New(io.Discard, "", 0)}

	srv.syncCodexClientVersion(context.Background())

	if requests != 0 {
		t.Fatalf("release feed requests = %d, want 0", requests)
	}
}

func TestSyncCodexClientVersionKeepsVersionWhenFeedFails(t *testing.T) {
	cfg := config.Default()
	cfg.CodexClientVersionSynced = "0.150.0"
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	withCodexReleaseFeed(t, func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewBufferString(`{"message":"rate limited"}`)),
		}, nil
	})
	srv := &Server{engine: eng, logger: log.New(io.Discard, "", 0)}

	srv.syncCodexClientVersion(context.Background())

	if got := eng.CodexClientVersion(); got != "0.150.0" {
		t.Fatalf("version after feed failure = %q, want the previous 0.150.0", got)
	}
}

func TestCodexVersionFeedFallsBackToReleaseList(t *testing.T) {
	withCodexReleaseFeed(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/repos/openai/codex/releases/latest" {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewBufferString(`{}`)),
			}, nil
		}
		return releasesBody(`[{"tag_name":"rust-v0.155.0"},{"tag_name":"rust-v0.155.1"}]`), nil
	})

	version, err := fetchLatestStableCodexClientVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != "0.155.1" {
		t.Fatalf("version = %q", version)
	}
}

func TestCodexUserAgentWithResolvedVersion(t *testing.T) {
	fallback := "codex-tui/0.155.1 (Windows 11; x86_64) WindowsTerminal"
	tests := []struct {
		name      string
		userAgent string
		want      string
	}{
		{
			name:      "rewrites official client version and keeps the platform",
			userAgent: "codex_cli_rs/0.200.0 (Windows 11; x86_64)",
			want:      "codex_cli_rs/0.155.1 (Windows 11; x86_64)",
		},
		{
			name:      "rewrites bare official client",
			userAgent: "codex-tui/0.199.0",
			want:      "codex-tui/0.155.1",
		},
		{
			name:      "replaces non Codex clients",
			userAgent: "curl/8.4.0",
			want:      fallback,
		},
		{
			name:      "replaces unparsable agents",
			userAgent: "codex_cli_rs",
			want:      fallback,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexUserAgentWithResolvedVersion(tt.userAgent, "0.155.1", fallback); got != tt.want {
				t.Fatalf("user agent = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigSavePreservesSynchronizedCodexVersion(t *testing.T) {
	cfg := config.Default()
	cfg.CodexClientVersionSynced = "0.155.1"
	disabled := false
	cfg.CodexVersionAutoSync = &disabled
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{engine: eng, cfgPath: filepath.Join(t.TempDir(), "config.json")}
	// The desktop UI sends the config it knows about, which does not include
	// the server-managed version fields.
	body := bytes.NewBufferString(`{"listen_addr":"127.0.0.1:8787","channels":[],"model_mappings":[]}`)
	rec := httptest.NewRecorder()

	srv.handlePutConfig(rec, httptest.NewRequest(http.MethodPut, "/api/config", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	saved := eng.Config()
	if saved.CodexClientVersionSynced != "0.155.1" {
		t.Fatalf("synchronized version = %q, want preserved", saved.CodexClientVersionSynced)
	}
	if saved.CodexVersionAutoSyncEnabled() {
		t.Fatal("auto sync setting must survive a config save")
	}
}

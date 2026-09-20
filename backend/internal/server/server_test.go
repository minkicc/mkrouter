package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/minkicc/mkrouter/backend/internal/config"
	"github.com/minkicc/mkrouter/backend/internal/engine"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return fn(req) }

type cancelingBody struct {
	data   []byte
	read   bool
	cancel context.CancelFunc
}

type completingCancelBody struct {
	data   []byte
	read   bool
	cancel context.CancelFunc
}

func (b *completingCancelBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, b.data), nil
	}
	b.cancel()
	return 0, context.Canceled
}

func (b *completingCancelBody) Close() error { return nil }

func (b *cancelingBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, b.data), nil
	}
	b.cancel()
	return 0, context.Canceled
}

func (b *cancelingBody) Close() error { return nil }

func TestForwardStreamsResponseImmediately(t *testing.T) {
	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(bytes.NewBufferString(
				"event: response.completed\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n",
			)),
		}, nil
	})
	srv := &Server{engine: eng}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	selection := &engine.Selection{Channel: config.Channel{ID: "#1", BaseURL: "https://example.com"}, UpstreamModel: "gpt-test"}

	retry, usage, err := srv.forward(recorder, req, selection, []byte(`{"model":"gpt-test","stream":true}`))
	if err != nil || retry {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
	if usage != (usageInfo{PromptTokens: 3, CompletionTokens: 2}) {
		t.Fatalf("usage = %+v", usage)
	}
	if recorder.Body.Len() == 0 {
		t.Fatal("stream body was not forwarded")
	}
}

func TestForwardCodexJSONChannelUsesChatGPTResponsesHeaders(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{
		ID:       "#1",
		Name:     "Codex",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken: "codex-access",
			AccountID:   "account-1",
			ExpiresAt:   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var captured *http.Request
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		captured = req.Clone(req.Context())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"id":"response-1","usage":{"input_tokens":1,"output_tokens":2}}`)),
		}, nil
	})
	srv := &Server{engine: eng}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("User-Agent", "codex_cli_rs/0.200.0 (Windows 11; x86_64)")
	req.Header.Set("Originator", "codex_cli_rs")
	selection := &engine.Selection{Channel: cfg.Channels[0], UpstreamModel: "gpt-5.6-sol"}

	retry, _, err := srv.forward(httptest.NewRecorder(), req, selection, []byte(`{"model":"gpt-5.6-sol"}`))
	if retry || err != nil {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
	if captured == nil {
		t.Fatal("upstream request was not captured")
	}
	if captured.URL.String() != "https://chatgpt.com/backend-api/codex/responses" {
		t.Fatalf("target = %s", captured.URL)
	}
	if got := captured.Header.Get("Authorization"); got != "Bearer codex-access" {
		t.Fatalf("authorization = %q", got)
	}
	if got := captured.Header.Get("ChatGPT-Account-ID"); got != "account-1" {
		t.Fatalf("account header = %q", got)
	}
	if got := captured.Header.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("originator = %q", got)
	}
	// The advertised Codex version must come from the shared resolver, not from
	// whatever version the client happened to send.
	if got := captured.Header.Get("Version"); got != config.CodexClientVersionFallback {
		t.Fatalf("version = %q", got)
	}
	if got := captured.Header.Get("User-Agent"); got != "codex_cli_rs/"+config.CodexClientVersionFallback+" (Windows 11; x86_64)" {
		t.Fatalf("user agent = %q", got)
	}
}

func TestEnsureCodexAuthRefreshesAndPersistsRotatedToken(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{
		ID:       "#1",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "expired-access",
			RefreshToken: "old-refresh",
			AccountID:    "account-1",
			ExpiresAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != codexOAuthTokenURL {
			t.Fatalf("refresh target = %s", req.URL)
		}
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if req.Form.Get("refresh_token") != "old-refresh" || req.Form.Get("client_id") != config.CodexClientID {
			t.Fatalf("refresh form = %v", req.Form)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(
				`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`,
			)),
		}, nil
	})
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	srv := &Server{engine: eng, cfgPath: cfgPath}

	auth, err := srv.ensureCodexAuth(context.Background(), "#1")
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccessToken != "new-access" || auth.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed auth = %+v", auth)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved config.Config
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved.Channels[0].CodexAuth.AccessToken; got != "new-access" {
		t.Fatalf("saved access token = %q", got)
	}
}

func TestHandleProxyRefreshesCodexAuthAfter401AndRetries(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{{
		ID:         "#1",
		BaseURL:    config.CodexBaseURL,
		AuthType:   config.ChannelAuthCodex,
		MaxRetries: 1,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "stale-access",
			RefreshToken: "refresh-1",
			AccountID:    "account-1",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	responseCalls := 0
	refreshCalls := 0
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == codexOAuthTokenURL {
			refreshCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"access_token":"fresh-access","expires_in":3600}`)),
			}, nil
		}
		responseCalls++
		if responseCalls == 1 {
			if got := req.Header.Get("Authorization"); got != "Bearer stale-access" {
				t.Fatalf("first authorization = %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"expired"}}`)),
			}, nil
		}
		if got := req.Header.Get("Authorization"); got != "Bearer fresh-access" {
			t.Fatalf("retry authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"id":"ok","usage":{"input_tokens":1,"output_tokens":1}}`)),
		}, nil
	})
	srv := &Server{engine: eng, cfgPath: filepath.Join(t.TempDir(), "config.json")}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.6-sol"}`))

	srv.handleProxy(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if responseCalls != 2 || refreshCalls != 1 {
		t.Fatalf("response calls = %d, refresh calls = %d", responseCalls, refreshCalls)
	}
}

func TestConcurrentCodex401RecoveryRefreshesOnlyOnce(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{
		ID:       "#1",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "stale-access",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			UpdatedAt:    1,
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var once sync.Once
	var callMu sync.Mutex
	refreshCalls := 0
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		callMu.Lock()
		refreshCalls++
		callMu.Unlock()
		once.Do(func() { close(refreshStarted) })
		<-releaseRefresh
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"access_token":"fresh-access","refresh_token":"refresh-2","expires_in":3600}`)),
		}, nil
	})
	srv := &Server{engine: eng}

	type result struct {
		auth *config.CodexAuth
		err  error
	}
	results := make(chan result, 2)
	go func() {
		auth, err := srv.ensureCodexAuthAfterUnauthorized(context.Background(), "#1", "stale-access")
		results <- result{auth: auth, err: err}
	}()
	<-refreshStarted
	go func() {
		auth, err := srv.ensureCodexAuthAfterUnauthorized(context.Background(), "#1", "stale-access")
		results <- result{auth: auth, err: err}
	}()
	close(releaseRefresh)

	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.auth.AccessToken != "fresh-access" {
			t.Fatalf("access token = %q", got.auth.AccessToken)
		}
	}
	callMu.Lock()
	defer callMu.Unlock()
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}
}

func TestCodexRefreshLockWaitHonorsContextCancellation(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{
		ID:       "#1",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "stale-access",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var once sync.Once
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		once.Do(func() { close(refreshStarted) })
		<-releaseRefresh
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"access_token":"fresh-access","expires_in":3600}`)),
		}, nil
	})
	srv := &Server{engine: eng}
	firstDone := make(chan error, 1)
	go func() {
		_, err := srv.ensureCodexAuthAfterUnauthorized(context.Background(), "#1", "stale-access")
		firstDone <- err
	}()
	<-refreshStarted

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, waitErr := srv.ensureCodexAuthAfterUnauthorized(ctx, "#1", "stale-access")
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want deadline exceeded", waitErr)
	}
	close(releaseRefresh)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestCodexInvalidGrantRaceUsesNewerCredentials(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{
		ID:       "#1",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "old-access",
			RefreshToken: "old-refresh",
			ExpiresAt:    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			UpdatedAt:    1,
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		latest := eng.Config()
		latest.Channels[0].CodexAuth = &config.CodexAuth{
			AccessToken:  "winner-access",
			RefreshToken: "winner-refresh",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			UpdatedAt:    2,
		}
		if err := eng.ReplaceConfig(latest); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"code":"invalid_grant","message":"refresh token already used"}}`)),
		}, nil
	})
	srv := &Server{engine: eng}

	auth, err := srv.ensureCodexAuth(context.Background(), "#1")
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccessToken != "winner-access" || auth.RefreshToken != "winner-refresh" {
		t.Fatalf("race recovery auth = %+v", auth)
	}
}

func TestPreserveNewerCodexAuthorizationFromStaleConfigSave(t *testing.T) {
	current := config.Default()
	current.Channels = []config.Channel{{
		ID:       "#1",
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "fresh-access",
			RefreshToken: "fresh-refresh",
			UpdatedAt:    200,
		},
	}}
	incoming := current.Clone()
	incoming.Channels[0].CodexAuth = &config.CodexAuth{
		AccessToken:  "stale-access",
		RefreshToken: "stale-refresh",
		UpdatedAt:    100,
	}

	preserveNewerCodexAuthorizations(incoming, current)

	if got := incoming.Channels[0].CodexAuth.AccessToken; got != "fresh-access" {
		t.Fatalf("stale config overwrote refreshed token: %q", got)
	}
}

func TestHandleProxyDoesNotRefreshPermanentlyRevokedCodexToken(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{{
		ID:       "#1",
		BaseURL:  config.CodexBaseURL,
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "revoked-access",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	refreshCalls := 0
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == codexOAuthTokenURL {
			refreshCalls++
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"code":"token_revoked","message":"revoked"}}`)),
		}, nil
	})
	srv := &Server{engine: eng}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.6-sol"}`))

	srv.handleProxy(recorder, req)

	if refreshCalls != 0 {
		t.Fatalf("refresh calls = %d, want 0", refreshCalls)
	}
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestHandleProxyAttemptsOnlyOneCodexRefreshForRepeated401(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{{
		ID:         "#1",
		BaseURL:    config.CodexBaseURL,
		AuthType:   config.ChannelAuthCodex,
		MaxRetries: 4,
		CodexAuth: &config.CodexAuth{
			AccessToken:  "stale-access",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
		Models: []string{"*"},
	}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	refreshCalls := 0
	responseCalls := 0
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == codexOAuthTokenURL {
			refreshCalls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"access_token":"fresh-access","expires_in":3600}`)),
			}, nil
		}
		responseCalls++
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"still unauthorized"}}`)),
		}, nil
	})
	srv := &Server{engine: eng, cfgPath: filepath.Join(t.TempDir(), "config.json")}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.6-sol"}`))

	srv.handleProxy(recorder, req)

	if refreshCalls != 1 || responseCalls != 2 {
		t.Fatalf("refresh calls = %d, response calls = %d", refreshCalls, responseCalls)
	}
}

func TestForwardStreamDoesNotMarkChannelFailedOnClientCancel(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &cancelingBody{data: []byte("data: {\"type\":\"response.created\"}\n\n"), cancel: cancel},
		}, nil
	})
	srv := &Server{engine: eng}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	selection := &engine.Selection{Channel: config.Channel{ID: "#1", BaseURL: "https://example.com"}, UpstreamModel: "gpt-test"}

	retry, _, err := srv.forward(httptest.NewRecorder(), req, selection, []byte(`{"model":"gpt-test","stream":true}`))
	if retry {
		t.Fatal("client cancel must not trigger failover")
	}
	if err == nil {
		t.Fatal("client cancel must be reported as a cancellation, not success")
	}
	for _, st := range eng.State() {
		if st.ID == "#1" && st.ConsecutiveFailures != 0 {
			t.Fatalf("client cancel must not mark the channel failed, got %d failures", st.ConsecutiveFailures)
		}
	}
}

func TestForwardStreamCountsCompletedResponseWhenClientCloses(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &completingCancelBody{
				data:   []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n"),
				cancel: cancel,
			},
		}, nil
	})
	srv := &Server{engine: eng}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	selection := &engine.Selection{Channel: config.Channel{ID: "#1", BaseURL: "https://example.com"}, UpstreamModel: "gpt-test"}

	retry, usage, err := srv.forward(httptest.NewRecorder(), req, selection, []byte(`{"model":"gpt-test","stream":true}`))
	if retry || err != nil {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
	if usage != (usageInfo{PromptTokens: 3, CompletionTokens: 2}) {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestForwardStreamTruncatedBeforeCompletionIsFailure(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(bytes.NewBufferString(
				"event: response.created\n" +
					"data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\n",
			)),
		}, nil
	})
	srv := &Server{engine: eng}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	selection := &engine.Selection{Channel: config.Channel{ID: "#1", BaseURL: "https://example.com"}, UpstreamModel: "gpt-test"}

	retry, _, err := srv.forward(httptest.NewRecorder(), req, selection, []byte(`{"model":"gpt-test","stream":true}`))
	if retry {
		t.Fatal("truncated stream must not retry after headers were written")
	}
	if err == nil {
		t.Fatal("truncated stream must be reported as a failure, not success")
	}
}

func TestForwardStreamEmptyResponseIsRetryable(t *testing.T) {
	cfg := config.Default()
	cfg.Channels = []config.Channel{{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(bytes.NewBufferString("")),
		}, nil
	})
	srv := &Server{engine: eng}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	selection := &engine.Selection{Channel: config.Channel{ID: "#1", BaseURL: "https://example.com"}, UpstreamModel: "gpt-test"}

	retry, _, err := srv.forward(httptest.NewRecorder(), req, selection, []byte(`{"model":"gpt-test","stream":true}`))
	if !retry || err == nil {
		t.Fatalf("empty stream should be retryable: retry=%v err=%v", retry, err)
	}
}

func TestForwardDoesNotRetryCanceledRequest(t *testing.T) {
	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	srv := &Server{engine: eng}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	selection := &engine.Selection{Channel: config.Channel{ID: "#1", BaseURL: "https://example.com"}, UpstreamModel: "gpt-test"}

	retry, _, err := srv.forward(httptest.NewRecorder(), req, selection, []byte(`{"model":"gpt-test"}`))
	if !retry || retryOnSameChannel(err) {
		t.Fatalf("retry=%v same-channel=%v err=%v", retry, retryOnSameChannel(err), err)
	}
}

func TestHandleProxyCoolsDownAfterRetriesExhausted(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{
		{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}, MaxRetries: 2},
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"down"}}`)),
		}, nil
	})
	srv := &Server{engine: eng}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-4"}`))
	srv.handleProxy(httptest.NewRecorder(), req)

	states := eng.State()
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	if states[0].CooldownUntil == 0 {
		t.Fatal("expected channel to be in cooldown after retries exhausted")
	}
}

func TestHandleProxyFailsOverToAnotherChannel(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{
		{ID: "#1", BaseURL: "https://first.example.com", Models: []string{"gpt-4"}, Priority: 10},
		{ID: "#2", BaseURL: "https://second.example.com", Models: []string{"gpt-4"}, Priority: 1},
	}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstAttempts := 0
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "first.example.com" {
			firstAttempts++
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"error":{"message":"down"}}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewBufferString(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)),
		}, nil
	})
	srv := &Server{engine: eng}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4"}`))
	srv.handleProxy(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"id":"ok"`)) {
		t.Fatalf("response = %s", recorder.Body.String())
	}
	if firstAttempts != 2 {
		t.Fatalf("first channel attempts = %d, want 2 with default max_retries=1", firstAttempts)
	}
	states := eng.State()
	if len(states) != 2 || states[0].CooldownUntil == 0 {
		t.Fatalf("expected first channel cooldown, states = %+v", states)
	}

	secondRecorder := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4"}`))
	srv.handleProxy(secondRecorder, secondReq)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200: %s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if firstAttempts != 2 {
		t.Fatalf("cooled channel was retried on the next request: attempts = %d", firstAttempts)
	}
}

func TestHandleProxyDoesNotRecordClientCancellation(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := newUsageStore(filepath.Join(t.TempDir(), "config.json"), 10)
	srv := &Server{engine: eng, usage: store}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4"}`)).WithContext(ctx)

	srv.handleProxy(httptest.NewRecorder(), req)
	if records := store.list(); len(records) != 0 {
		t.Fatalf("client cancellation should not create a usage failure: %+v", records)
	}
}

func TestHandleProxyDoesNotRecordTransportCancellation(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	store := newUsageStore(filepath.Join(t.TempDir(), "config.json"), 10)
	srv := &Server{engine: eng, usage: store}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4"}`))

	srv.handleProxy(httptest.NewRecorder(), req)
	if records := store.list(); len(records) != 0 {
		t.Fatalf("transport cancellation should not create a usage failure: %+v", records)
	}
}

func TestHandleProxyDoesNotRecordWrappedTransportCancellation(t *testing.T) {
	cfg := config.Default()
	cfg.NoAuth = true
	cfg.Channels = []config.Channel{{ID: "#1", BaseURL: "https://example.com", Models: []string{"gpt-4"}}}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New(`Post "https://example.com/v1/chat/completions": context canceled`)
	})
	store := newUsageStore(filepath.Join(t.TempDir(), "config.json"), 10)
	srv := &Server{engine: eng, usage: store}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"gpt-4"}`))

	srv.handleProxy(httptest.NewRecorder(), req)
	if records := store.list(); len(records) != 0 {
		t.Fatalf("wrapped transport cancellation should not create a usage failure: %+v", records)
	}
}

func TestRetryOnSameChannel(t *testing.T) {
	if retryOnSameChannel(errors.New("network failure")) {
		t.Fatal("plain errors must not be treated as retry decisions")
	}
	if !retryOnSameChannel(&retryDecisionError{err: errors.New("timeout"), same: true}) {
		t.Fatal("same-channel retry decision was not recognized")
	}
	if retryOnSameChannel(&retryDecisionError{err: errors.New("unauthorized"), same: false}) {
		t.Fatal("route-switch decision must not retry the same channel")
	}
}

func TestBuildFallbackChain(t *testing.T) {
	fallbacks := map[string]string{
		"gpt-5.6":     "deepseek-v4",
		"deepseek-v4": "deepseek-chat",
	}
	got := buildFallbackChain("gpt-5.6", fallbacks)
	want := []string{"gpt-5.6", "deepseek-v4", "deepseek-chat"}
	if len(got) != len(want) {
		t.Fatalf("chain = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain = %v, want %v", got, want)
		}
	}
}

func TestParseOAuthCallbackInput(t *testing.T) {
	tests := []struct {
		input     string
		wantCode  string
		wantState string
	}{
		{
			input:     "http://localhost:1455/auth/callback?code=code-1&state=state-1",
			wantCode:  "code-1",
			wantState: "state-1",
		},
		{
			input:     "code=code-2&state=state-2",
			wantCode:  "code-2",
			wantState: "state-2",
		},
		{
			input:    "plain-code",
			wantCode: "plain-code",
		},
	}
	for _, tt := range tests {
		code, state := parseOAuthCallbackInput(tt.input)
		if code != tt.wantCode || state != tt.wantState {
			t.Fatalf("parseOAuthCallbackInput(%q) = %q, %q", tt.input, code, state)
		}
	}
}

func TestCodexOAuthStartReturnsPKCESession(t *testing.T) {
	srv := &Server{}
	req := httptest.NewRequest(http.MethodPost, "/api/codex/oauth/start", nil)
	rec := httptest.NewRecorder()
	srv.handleCodexOAuthStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		AuthURL   string `json:"auth_url"`
		SessionID string `json:"session_id"`
		State     string `json:"state"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AuthURL == "" || payload.SessionID == "" || payload.State == "" {
		t.Fatalf("oauth start payload = %+v", payload)
	}
	value, ok := srv.codexOAuthSessions.Load(payload.SessionID)
	session, sessionOK := value.(*codexOAuthSession)
	if !ok || !sessionOK || session.State != payload.State || session.CodeVerifier == "" {
		t.Fatalf("stored oauth session = %#v", value)
	}
}

func TestFetchUpstreamModelsUsesCodexAccountManifest(t *testing.T) {
	accessToken := testServerCodexJWT(t, map[string]any{
		"exp":       time.Now().Add(time.Hour).Unix(),
		"client_id": config.CodexClientID,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-from-token",
		},
	})
	eng, err := engine.New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	eng.ProxyClient().Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != config.CodexModelsURL+"?client_version="+config.CodexClientVersionFallback {
			t.Fatalf("target = %s", req.URL)
		}
		if req.Header.Get("Authorization") != "Bearer "+accessToken {
			t.Fatalf("authorization = %q", req.Header.Get("Authorization"))
		}
		if req.Header.Get("ChatGPT-Account-ID") != "account-from-token" {
			t.Fatalf("account id = %q", req.Header.Get("ChatGPT-Account-ID"))
		}
		if req.Header.Get("User-Agent") != "codex_cli_rs/"+config.CodexClientVersionFallback ||
			req.Header.Get("Originator") != "codex_cli_rs" ||
			req.Header.Get("Version") != config.CodexClientVersionFallback {
			t.Fatalf("Codex model headers = %v", req.Header)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(bytes.NewBufferString(
				`{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.5"},{"slug":"gpt-5.6-sol"}]}`,
			)),
		}, nil
	})
	srv := &Server{engine: eng}
	models, err := srv.fetchUpstreamModels(config.Channel{
		AuthType: config.ChannelAuthCodex,
		CodexAuth: &config.CodexAuth{
			AccessToken: accessToken,
			AccountID:   "wrong@example.com",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "gpt-5.5" || models[1] != "gpt-5.6-sol" {
		t.Fatalf("models = %v", models)
	}
}

func testServerCodexJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
}

func TestRewriteRequestBody(t *testing.T) {
	out, stream, err := rewriteRequestBody([]byte(`{"model":"codex-default","stream":true}`), "deepseek-chat")
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Fatal("stream should be true")
	}
	if string(out) != `{"model":"deepseek-chat","stream":true}` {
		t.Fatalf("body = %s", out)
	}
}

func TestCopyResponseHeadersStripsContentLength(t *testing.T) {
	src := http.Header{
		"Content-Type":   []string{"text/event-stream"},
		"Content-Length": []string{"10"},
		"Connection":     []string{"keep-alive"},
	}
	dst := http.Header{}
	copyResponseHeaders(dst, src)
	if got := dst.Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length should be stripped, got %q", got)
	}
	if got := dst.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type was not copied: %q", got)
	}
	if got := dst.Get("Connection"); got != "" {
		t.Fatalf("hop-by-hop Connection should be stripped, got %q", got)
	}
}

func TestParseUsage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want usageInfo
	}{
		{
			name: "chat completions",
			body: `{"usage":{"prompt_tokens":11,"completion_tokens":7}}`,
			want: usageInfo{PromptTokens: 11, CompletionTokens: 7},
		},
		{
			name: "responses",
			body: `{"type":"response.completed","response":{"usage":{"input_tokens":13,"output_tokens":5,"total_tokens":18}}}`,
			want: usageInfo{PromptTokens: 13, CompletionTokens: 5},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := parseUsage([]byte(test.body)); got != test.want {
				t.Fatalf("usage = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestParseSSEUsageBody(t *testing.T) {
	body := []byte("event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":21,\"output_tokens\":8}}}\n\n")
	want := usageInfo{PromptTokens: 21, CompletionTokens: 8}
	if got := parseSSEUsageBody(body); got != want {
		t.Fatalf("usage = %+v, want %+v", got, want)
	}
}

func TestUsageStoreNotifiesSubscribers(t *testing.T) {
	store := newUsageStore(filepath.Join(t.TempDir(), "config.json"), 10)
	events, cancel := store.subscribe()
	defer cancel()

	store.add(usageRecord{Model: "gpt-5.6-sol", Success: true})
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("usage subscriber was not notified")
	}
}

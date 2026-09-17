package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/minkicc/codex-sync"
	"github.com/minkicc/mkrouter/backend/internal/config"
	"github.com/minkicc/mkrouter/backend/internal/engine"
	"github.com/minkicc/mkrouter/backend/internal/routing"
)

const maxRequestBody = 64 << 20

const (
	codexOAuthAuthorizeURL = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL     = "https://auth.openai.com/oauth/token"
	codexPATWhoAmIURL      = "https://auth.openai.com/api/accounts/v1/user-auth-credential/whoami"
	codexOAuthRedirectURI  = "http://localhost:1455/auth/callback"
	codexOAuthSessionTTL   = 30 * time.Minute
	codexRefreshSkew       = 3 * time.Minute
	codexDefaultVersion    = "0.146.0"
	codexDefaultUA         = "codex-tui/" + codexDefaultVersion + " (Windows 11; x86_64) WindowsTerminal"
)

type Server struct {
	engine             *engine.Engine
	cfgPath            string
	ui                 http.Handler
	logger             *log.Logger
	saveMu             sync.Mutex
	usage              *usageStore
	codex              *codex.Service
	codexRefreshLocks  sync.Map
	codexOAuthSessions sync.Map
}

type usageInfo struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type codexContextLock struct {
	token chan struct{}
}

func newCodexContextLock() *codexContextLock {
	return &codexContextLock{token: make(chan struct{}, 1)}
}

func (m *codexContextLock) Lock(ctx context.Context) error {
	select {
	case m.token <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *codexContextLock) Unlock() {
	<-m.token
}

type codex401RefreshStateKey struct{}

type codex401RefreshState struct {
	attempted map[string]bool
}

type codexOAuthSession struct {
	State        string
	CodeVerifier string
	RedirectURI  string
	CreatedAt    time.Time
}

func beginCodex401Refresh(ctx context.Context, channelID string) bool {
	state, _ := ctx.Value(codex401RefreshStateKey{}).(*codex401RefreshState)
	if state == nil {
		return true
	}
	if state.attempted[channelID] {
		return false
	}
	state.attempted[channelID] = true
	return true
}

func New(eng *engine.Engine, cfgPath string, uiFS fs.FS, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	usageLimit := 500
	if cfg := eng.Config(); cfg != nil && cfg.UsageMaxRecords > 0 {
		usageLimit = cfg.UsageMaxRecords
	}
	s := &Server{
		engine:  eng,
		cfgPath: cfgPath,
		ui:      http.FileServerFS(uiFS),
		logger:  logger,
		usage:   newUsageStore(cfgPath, usageLimit),
		codex:   &codex.Service{},
	}
	eng.SetCodexAuthResolver(s.ensureCodexAuth)
	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleProxy)
	mux.HandleFunc("POST /v1/responses", s.handleProxy)
	mux.HandleFunc("POST /v1/images/generations", s.handleProxy)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("GET /api/usage", s.handleUsage)
	mux.HandleFunc("GET /api/usage/events", s.handleUsageEvents)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("POST /api/probe_models", s.handleProbeModels)
	mux.HandleFunc("POST /api/parse_codex_auth", s.handleParseCodexAuth)
	mux.HandleFunc("POST /api/codex/oauth/start", s.handleCodexOAuthStart)
	mux.HandleFunc("POST /api/codex/oauth/exchange", s.handleCodexOAuthExchange)
	mux.HandleFunc("POST /api/codex/refresh-token", s.handleCodexRefreshToken)
	mux.HandleFunc("POST /api/codex/pat/validate", s.handleCodexPATValidate)
	mux.HandleFunc("POST /api/codex/refresh/{id}", s.handleCodexRefreshChannel)
	mux.HandleFunc("GET /api/tokens", s.handleListTokens)
	mux.HandleFunc("POST /api/tokens", s.handleCreateToken)
	mux.HandleFunc("PUT /api/tokens/{id}", s.handleUpdateToken)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.handleDeleteToken)
	mux.HandleFunc("POST /api/reload", s.handleReload)
	mux.HandleFunc("POST /api/check", s.handleCheckAll)
	mux.HandleFunc("POST /api/check/{id}", s.handleCheckOne)
	mux.HandleFunc("GET /api/codex/status", s.handleCodexStatus)
	mux.HandleFunc("POST /api/codex/sync", s.handleCodexSync)
	mux.HandleFunc("POST /api/codex/switch", s.handleCodexSwitch)
	mux.HandleFunc("POST /api/codex/restore", s.handleCodexRestore)
	mux.HandleFunc("POST /api/codex/prune", s.handleCodexPrune)
	mux.HandleFunc("GET /api/codex/backups", s.handleCodexBackups)
	mux.HandleFunc("GET /api/codex/sync/events", s.handleCodexSyncEvents)
	mux.Handle("/", s.ui)
	return corsMiddleware(mux)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	states := s.engine.State()
	unhealthy := 0
	for _, st := range states {
		if st.Status == engine.StatusUnhealthy {
			unhealthy++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"build":              "1.1.5",
		"channels":           len(states),
		"unhealthy_channels": unhealthy,
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(r); !ok {
		writeAuthError(w)
		return
	}
	models := s.engine.AvailableModels()
	data := make([]map[string]any, 0, len(models))
	now := time.Now().Unix()
	for _, model := range models {
		data = append(data, map[string]any{
			"id":       model,
			"object":   "model",
			"created":  now,
			"owned_by": "mkrouter",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) authenticate(r *http.Request) (string, bool) {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	auth = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	t, ok := s.engine.Authorize(auth)
	if !ok {
		return "", false
	}
	return t.ID, true
}

func writeAuthError(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error": map[string]any{
			"message": "invalid api key",
			"type":    "invalid_request_error",
			"code":    "invalid_api_key",
		},
	})
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := s.authenticate(r)
	if !ok {
		writeAuthError(w)
		return
	}
	s.recordRequest(tokenID)
	r = r.WithContext(context.WithValue(r.Context(), codex401RefreshStateKey{}, &codex401RefreshState{
		attempted: map[string]bool{},
	}))

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error", "")
		return
	}
	requestedModel, err := requestModel(body)
	if err != nil || strings.TrimSpace(requestedModel) == "" {
		writeProxyError(w, http.StatusBadRequest, "model is required", "invalid_request_error", "")
		return
	}

	start := time.Now()
	excluded := map[string]bool{}
	var lastErr error
	var lastForwardErr error
	var lastSelection *engine.Selection
	for {
		if err := r.Context().Err(); err != nil {
			lastErr = err
			break
		}
		selection, selectErr := s.engine.Select(requestedModel, excluded)
		if selectErr != nil {
			lastErr = selectErr
			break
		}
		lastSelection = selection
		maxRetries := selection.Channel.MaxRetries
		if maxRetries <= 0 {
			// Keep one same-channel retry when the channel does not
			// configure an explicit value: one request plus one retry.
			maxRetries = 1
		}
		attempts := 0
		for {
			if err := r.Context().Err(); err != nil {
				lastErr = err
				break
			}
			retry, usage, forwardErr := s.forward(w, r, selection, body)
			if forwardErr != nil {
				lastErr = forwardErr
				lastForwardErr = forwardErr
			}
			if isCancellationError(forwardErr) {
				return
			}
			if r.Context().Err() != nil {
				if forwardErr == nil {
					s.recordTokens(tokenID, usage)
					s.recordUsage(r, tokenID, requestedModel, selection, usage, start, true, "")
				}
				// A client cancellation is not an upstream attempt failure.
				// Do not pollute usage statistics when a caller races or
				// abandons a parallel request after another one completed.
				return
			}
			if !retry {
				if forwardErr != nil {
					s.recordUsage(r, tokenID, requestedModel, selection, usageInfo{}, start, false, errorText(forwardErr))
					return
				}
				s.recordTokens(tokenID, usage)
				s.recordUsage(r, tokenID, requestedModel, selection, usage, start, true, "")
				return
			}
			attempts++
			if !retryOnSameChannel(forwardErr) || attempts > maxRetries {
				excluded[selection.Channel.ID] = true
				s.engine.Cooldown(selection.Channel.ID)
				break
			}
		}
	}
	if r.Context().Err() != nil {
		return
	}
	errMsg := errorText(lastForwardErr)
	if errMsg == "" {
		errMsg = errorText(lastErr)
	}
	s.recordUsage(r, tokenID, requestedModel, lastSelection, usageInfo{}, start, false, errMsg)
	writeProxyError(w, http.StatusBadGateway, "all channels failed", "upstream_unavailable", errorText(lastErr))
}

type retryDecisionError struct {
	err  error
	same bool
}

func (e *retryDecisionError) Error() string { return e.err.Error() }
func (e *retryDecisionError) Unwrap() error { return e.err }

func retryOnSameChannel(err error) bool {
	var decision *retryDecisionError
	return errors.As(err, &decision) && decision.same
}

func (s *Server) recordUsage(r *http.Request, tokenID, model string, selection *engine.Selection, usage usageInfo, start time.Time, success bool, errText string) {
	if s.usage == nil {
		return
	}
	if !success && isCancellationError(errors.New(errText)) {
		return
	}
	rec := usageRecord{
		Time:      time.Now().Unix(),
		TokenID:   tokenID,
		TokenName: s.engine.TokenName(tokenID),
		Model:     model,
		ElapsedMS: time.Since(start).Milliseconds(),
		Success:   success,
		Endpoint:  r.URL.Path,
		Error:     errText,
	}
	if selection != nil {
		rec.UpstreamModel = selection.UpstreamModel
		rec.ChannelID = selection.Channel.ID
		rec.ChannelName = selection.Channel.Name
	}
	if success {
		rec.PromptTokens = usage.PromptTokens
		rec.CompletionTokens = usage.CompletionTokens
	}
	s.usage.add(rec)
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, selection *engine.Selection, body []byte) (bool, usageInfo, error) {
	ch := selection.Channel
	if ch.IsCodexAuth() && r.URL.Path != "/v1/responses" {
		return true, usageInfo{}, &retryDecisionError{
			err:  fmt.Errorf("Codex JSON channels only support /v1/responses"),
			same: false,
		}
	}
	rewritten, stream, err := rewriteRequestBody(body, selection.UpstreamModel)
	if err != nil {
		return true, usageInfo{}, &retryDecisionError{err: err, same: false}
	}
	target := upstreamTarget(ch, r.URL.Path)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(rewritten))
	if err != nil {
		return true, usageInfo{}, &retryDecisionError{err: err, same: false}
	}
	copyRequestHeaders(req.Header, r.Header)
	ch.ApplyAPIKeyAuthorization(req)
	for k, v := range ch.Headers {
		req.Header.Set(k, v)
	}
	var requestCodexAuth *config.CodexAuth
	if ch.IsCodexAuth() {
		auth, authErr := s.ensureCodexAuth(r.Context(), ch.ID)
		if authErr != nil {
			return true, usageInfo{}, &retryDecisionError{err: authErr, same: false}
		}
		requestCodexAuth = auth
		applyCodexRequestHeaders(req.Header, r.Header, auth, stream)
	}
	start := time.Now()
	resp, err := s.engine.ProxyClient().Do(req)
	if err != nil {
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			return true, usageInfo{}, &retryDecisionError{err: err, same: false}
		}
		s.engine.RecordAttempt(ch.ID, 0, err, time.Since(start))
		return true, usageInfo{}, &retryDecisionError{err: err, same: true}
	}
	defer resp.Body.Close()
	latency := time.Since(start)

	if routing.IsRetryableError(resp.StatusCode, nil) {
		if ch.IsCodexAuth() && resp.StatusCode == http.StatusUnauthorized {
			responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if code, permanent := codexPermanentAuthFailure(responseBody); permanent {
				s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, latency)
				return true, usageInfo{}, &retryDecisionError{
					err:  fmt.Errorf("Codex authorization was permanently rejected: %s", code),
					same: false,
				}
			}
			if requestCodexAuth != nil && beginCodex401Refresh(r.Context(), ch.ID) {
				if _, refreshErr := s.ensureCodexAuthAfterUnauthorized(r.Context(), ch.ID, requestCodexAuth.AccessToken); refreshErr == nil {
					return true, usageInfo{}, &retryDecisionError{
						err:  errors.New("Codex authorization refreshed after upstream 401"),
						same: true,
					}
				} else {
					s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, latency)
					return true, usageInfo{}, &retryDecisionError{
						err:  fmt.Errorf("Codex authorization refresh after 401 failed: %w", refreshErr),
						same: false,
					}
				}
			}
			s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, latency)
			return true, usageInfo{}, &retryDecisionError{
				err:  errors.New("Codex authorization remained unauthorized after one refresh attempt"),
				same: false,
			}
		}
		retryErr := &retryDecisionError{
			err:  fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode)),
			same: routing.IsRetryableOnSameRoute(resp.StatusCode),
		}
		// Do not consume a potentially large error response before switching.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, latency)
		return true, usageInfo{}, retryErr
	}

	if stream {
		copyResponseHeaders(w.Header(), resp.Header)
		flusher, _ := w.(http.Flusher)
		var captured bytes.Buffer
		tee := io.TeeReader(resp.Body, &captured)
		buf := make([]byte, 32<<10)
		wrote := false
		for {
			n, readErr := tee.Read(buf)
			if n > 0 {
				if !wrote {
					if routing.ResponseFailed(captured.Bytes()) {
						s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, time.Since(start))
						return true, usageInfo{}, &retryDecisionError{err: errors.New("upstream response failed"), same: true}
					}
					w.WriteHeader(resp.StatusCode)
				}
				if _, writeErr := w.Write(buf[:n]); writeErr != nil {
					return false, parseSSEUsageBody(captured.Bytes()), writeErr
				}
				wrote = true
				if flusher != nil {
					flusher.Flush()
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					if !sseResponseCompleted(captured.Bytes()) {
						// The upstream closed the SSE stream before it reached a
						// terminal event. Forwarding this to the client as success
						// would hide a truncated response, so treat it as a failure.
						truncErr := errors.New("upstream stream ended before completion")
						s.engine.RecordAttempt(ch.ID, resp.StatusCode, truncErr, time.Since(start))
						if !wrote {
							return true, usageInfo{}, &retryDecisionError{err: truncErr, same: true}
						}
						return false, parseSSEUsageBody(captured.Bytes()), truncErr
					}
					if !wrote {
						w.WriteHeader(resp.StatusCode)
					}
					s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, time.Since(start))
					return false, parseSSEUsageBody(captured.Bytes()), nil
				}
				if r.Context().Err() != nil || isCancellationError(readErr) {
					usage := parseSSEUsageBody(captured.Bytes())
					// Clients commonly close a stream immediately after receiving the
					// terminal event. If the upstream already completed successfully,
					// keep the request as a success even though draining the body was
					// interrupted by the client disconnect.
					if sseResponseCompleted(captured.Bytes()) {
						s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, time.Since(start))
						return false, usage, nil
					}
					return false, usage, readErr
				}
				s.engine.RecordAttempt(ch.ID, resp.StatusCode, readErr, time.Since(start))
				if !wrote {
					return true, usageInfo{}, &retryDecisionError{err: readErr, same: true}
				}
				return false, parseSSEUsageBody(captured.Bytes()), readErr
			}
		}
	}

	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		if r.Context().Err() != nil || isCancellationError(readErr) {
			return true, usageInfo{}, &retryDecisionError{err: readErr, same: false}
		}
		s.engine.RecordAttempt(ch.ID, resp.StatusCode, readErr, latency)
		return true, usageInfo{}, &retryDecisionError{err: readErr, same: true}
	}

	if routing.IsRetryableError(resp.StatusCode, data) {
		s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, latency)
		return true, usageInfo{}, &retryDecisionError{
			err:  errors.New(http.StatusText(resp.StatusCode)),
			same: routing.IsRetryableOnSameRoute(resp.StatusCode),
		}
	}

	s.engine.RecordAttempt(ch.ID, resp.StatusCode, nil, latency)
	usage := parseUsage(data)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if _, writeErr := w.Write(data); writeErr != nil {
		return false, usage, writeErr
	}
	return false, usage, nil
}

func parseUsage(data []byte) usageInfo {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return usageInfo{}
	}
	usage, _ := payload["usage"].(map[string]any)
	if usage == nil {
		if response, ok := payload["response"].(map[string]any); ok {
			usage, _ = response["usage"].(map[string]any)
		}
	}
	if usage == nil {
		return usageInfo{}
	}
	return usageInfo{
		PromptTokens:     usageTokenCount(usage["prompt_tokens"], usage["input_tokens"]),
		CompletionTokens: usageTokenCount(usage["completion_tokens"], usage["output_tokens"]),
	}
}

func usageTokenCount(values ...any) int64 {
	for _, value := range values {
		switch number := value.(type) {
		case float64:
			return int64(number)
		case int64:
			return number
		case int:
			return int64(number)
		case json.Number:
			parsed, _ := number.Int64()
			return parsed
		}
	}
	return 0
}

func parseSSEUsage(line []byte) (usageInfo, bool) {
	lineText := strings.TrimSpace(string(line))
	if !strings.HasPrefix(lineText, "data:") {
		return usageInfo{}, false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(lineText, "data:"))
	if payload == "" || payload == "[DONE]" {
		return usageInfo{}, false
	}
	usage := parseUsage([]byte(payload))
	return usage, usage != (usageInfo{})
}

func parseSSEUsageBody(data []byte) usageInfo {
	var usage usageInfo
	for _, line := range strings.Split(string(data), "\n") {
		if u, ok := parseSSEUsage([]byte(line)); ok {
			usage = u
		}
	}
	return usage
}

func isCancellationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "context canceled")
}

func sseResponseCompleted(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return true
		}
		if payload == "" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		typ, _ := event["type"].(string)
		if typ == "response.completed" || typ == "response.done" {
			return true
		}
		if response, ok := event["response"].(map[string]any); ok {
			status, _ := response["status"].(string)
			if status == "completed" {
				return true
			}
		}
		if choices, ok := event["choices"].([]any); ok {
			for _, item := range choices {
				choice, _ := item.(map[string]any)
				finish, _ := choice["finish_reason"].(string)
				if strings.TrimSpace(finish) != "" {
					return true
				}
			}
		}
	}
	return false
}

func (s *Server) recordRequest(tokenID string) {
	if tokenID == "" {
		return
	}
	if s.engine.RecordTokenRequest(tokenID) {
		s.persistConfig()
	}
}

func (s *Server) recordTokens(tokenID string, usage usageInfo) {
	if tokenID == "" {
		return
	}
	if s.engine.RecordTokenTokens(tokenID, usage.PromptTokens, usage.CompletionTokens) {
		s.persistConfig()
	}
}

func (s *Server) persistConfig() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if err := s.engine.Config().Save(s.cfgPath); err != nil {
		s.logger.Printf("persist config: %v", err)
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	cfg := s.engine.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"listen_addr":     cfg.ListenAddr,
		"allow_lan":       cfg.AllowLAN,
		"lan_addrs":       lanAddrs(),
		"channels":        s.engine.State(),
		"model_mappings":  cfg.ModelMappings,
		"fallback_models": cfg.FallbackModels,
	})
}

func lanAddrs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return []string{}
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil && ip4.IsPrivate() {
			out = append(out, ip4.String())
		}
	}
	return out
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Config())
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	records := []usageRecord{}
	summary := map[string]any{}
	if s.usage != nil {
		records = s.usage.list()
		summary = s.usage.summary()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": records,
		"summary": summary,
	})
}

func (s *Server) handleUsageEvents(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		http.Error(w, "usage store unavailable", http.StatusServiceUnavailable)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	events, cancel := s.usage.subscribe()
	defer cancel()
	_, _ = fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	keepAlive := time.NewTicker(20 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-events:
			if !ok {
				return
			}
			if _, err := fmt.Fprint(w, "event: usage\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var cfg config.Config
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody)).Decode(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	// Tokens are managed through dedicated endpoints; keep the live set on save.
	current := s.engine.Config()
	cfg.Tokens = current.Tokens
	cfg.AuthToken = current.AuthToken
	preserveNewerCodexAuthorizations(&cfg, current)
	if err := s.engine.ReplaceConfig(&cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.engine.Config().Save(s.cfgPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func preserveNewerCodexAuthorizations(incoming, current *config.Config) {
	if incoming == nil || current == nil {
		return
	}
	currentByID := make(map[string]*config.CodexAuth, len(current.Channels))
	for i := range current.Channels {
		ch := &current.Channels[i]
		if ch.IsCodexAuth() && ch.CodexAuth != nil {
			currentByID[ch.ID] = ch.CodexAuth
		}
	}
	for i := range incoming.Channels {
		ch := &incoming.Channels[i]
		if !ch.IsCodexAuth() || ch.CodexAuth == nil {
			continue
		}
		live := currentByID[ch.ID]
		if live == nil {
			continue
		}
		if live.UpdatedAt > ch.CodexAuth.UpdatedAt {
			latest := *live
			ch.CodexAuth = &latest
		}
	}
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.cfgPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if err := s.engine.ReplaceConfig(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleCheckAll(w http.ResponseWriter, r *http.Request) {
	s.engine.CheckAll()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleCheckOne(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.engine.FindChannel(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "channel not found"})
		return
	}
	s.engine.CheckNow(id)
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func codexHomeFromRequest(r *http.Request) string {
	if value := strings.TrimSpace(r.URL.Query().Get("codex_home")); value != "" {
		return value
	}
	return ""
}

func (s *Server) handleCodexStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.codex.Status(codexHomeFromRequest(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleCodexSync(w http.ResponseWriter, r *http.Request) {
	var req codex.SyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	result, err := s.codex.Sync(req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCodexSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider              string `json:"provider"`
		CodexHome             string `json:"codex_home"`
		KeepCount             int    `json:"keep_count"`
		RestorePinnedProjects bool   `json:"restore_pinned_projects"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	result, err := s.codex.Switch(req.Provider, req.CodexHome, req.KeepCount, req.RestorePinnedProjects)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCodexRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BackupDir string `json:"backup_dir"`
		CodexHome string `json:"codex_home"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	result, err := s.codex.Restore(req.BackupDir, req.CodexHome)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCodexPrune(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CodexHome string `json:"codex_home"`
		KeepCount int    `json:"keep_count"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	result, err := s.codex.Prune(req.CodexHome, req.KeepCount)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleCodexBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := s.codex.ListBackups(codexHomeFromRequest(r))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": backups})
}

func (s *Server) handleCodexSyncEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	writeEvent := func(event, data string) {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return
		}
		flusher.Flush()
	}

	query := r.URL.Query()
	provider := strings.TrimSpace(query.Get("provider"))
	codexHome := strings.TrimSpace(query.Get("codex_home"))
	doSwitch := query.Get("switch") == "1" || query.Get("switch") == "true"
	restorePinned := query.Get("restore_pinned_projects") == "1" || query.Get("restore_pinned_projects") == "true"

	keep := 0
	var keepPtr *int
	if raw := strings.TrimSpace(query.Get("keep_count")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			payload, _ := json.Marshal(map[string]string{"error": "invalid keep_count"})
			writeEvent("failed", string(payload))
			return
		}
		keep = parsed
		keepPtr = &parsed
	}

	progress := func(line string) {
		payload, _ := json.Marshal(map[string]string{"line": line})
		writeEvent("progress", string(payload))
	}

	var result codex.SyncResult
	var err error
	if doSwitch {
		result, err = s.codex.SwitchWithProgress(provider, codexHome, keep, restorePinned, progress)
	} else {
		result, err = s.codex.SyncWithProgress(codex.SyncRequest{
			Provider:              provider,
			KeepCount:             keepPtr,
			RestorePinnedProjects: restorePinned,
			CodexHome:             codexHome,
		}, progress)
	}
	if err != nil {
		payload, _ := json.Marshal(map[string]string{"error": err.Error()})
		writeEvent("failed", string(payload))
		return
	}

	label := "Synchronized"
	if doSwitch {
		label = "Switched to"
	}
	for _, line := range codex.SummaryLines(result, label) {
		progress(line)
	}
	progress("SUCCESS")

	payload, _ := json.Marshal(result)
	writeEvent("result", string(payload))
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	tokens := s.engine.Tokens()
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, map[string]any{
			"id":                t.ID,
			"name":              t.Name,
			"token":             t.Token,
			"enabled":           t.IsEnabled(),
			"created_at":        t.CreatedAt,
			"last_used_at":      t.LastUsedAt,
			"request_count":     t.RequestCount,
			"prompt_tokens":     t.PromptTokens,
			"completion_tokens": t.CompletionTokens,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req)
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	t, err := s.engine.AddToken(req.Name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if err := s.engine.Config().Save(s.cfgPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": t})
}

func (s *Server) handleUpdateToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	t, ok := s.engine.UpdateToken(id, req.Name, req.Enabled)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "token not found"})
		return
	}
	if err := s.engine.Config().Save(s.cfgPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": t})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	if !s.engine.RemoveToken(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "token not found"})
		return
	}
	if err := s.engine.Config().Save(s.cfgPath); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleProbeModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL         string `json:"base_url"`
		AuthType        string `json:"auth_type"`
		APIKey          string `json:"api_key"`
		APIKeyPlacement string `json:"api_key_placement"`
		APIKeyHeader    string `json:"api_key_header"`
		APIKeyPrefix    string `json:"api_key_prefix"`
		APIKeyQuery     string `json:"api_key_query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.BaseURL = strings.TrimSpace(req.BaseURL)
	if req.BaseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "base_url is required"})
		return
	}
	channel := config.Channel{
		BaseURL:         req.BaseURL,
		AuthType:        req.AuthType,
		APIKey:          strings.TrimSpace(req.APIKey),
		APIKeyPlacement: req.APIKeyPlacement,
		APIKeyHeader:    req.APIKeyHeader,
		APIKeyPrefix:    req.APIKeyPrefix,
		APIKeyQuery:     req.APIKeyQuery,
	}
	if channel.AuthType == "" {
		channel.AuthType = config.ChannelAuthAPIKey
	}
	models, err := s.fetchUpstreamModels(channel)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

func (s *Server) handleParseCodexAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	auth, err := config.ParseCodexAuthInput(req.Content)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth": auth})
}

func (s *Server) handleCodexOAuthStart(w http.ResponseWriter, r *http.Request) {
	state, err := randomHex(32)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	sessionID, err := randomHex(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	codeVerifier, err := randomHex(64)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	session := &codexOAuthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  codexOAuthRedirectURI,
		CreatedAt:    time.Now(),
	}
	s.codexOAuthSessions.Store(sessionID, session)
	s.deleteExpiredCodexOAuthSessions()

	challengeHash := sha256.Sum256([]byte(codeVerifier))
	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", config.CodexClientID)
	params.Set("redirect_uri", session.RedirectURI)
	params.Set("scope", "openid profile email offline_access")
	params.Set("state", state)
	params.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challengeHash[:]))
	params.Set("code_challenge_method", "S256")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url":   codexOAuthAuthorizeURL + "?" + params.Encode(),
		"session_id": sessionID,
		"state":      state,
		"expires_at": session.CreatedAt.Add(codexOAuthSessionTTL).Unix(),
	})
}

func (s *Server) handleCodexOAuthExchange(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	value, ok := s.codexOAuthSessions.Load(strings.TrimSpace(req.SessionID))
	session, sessionOK := value.(*codexOAuthSession)
	if !ok || !sessionOK || time.Since(session.CreatedAt) > codexOAuthSessionTTL {
		if req.SessionID != "" {
			s.codexOAuthSessions.Delete(strings.TrimSpace(req.SessionID))
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Codex OAuth session not found or expired"})
		return
	}
	code, callbackState := parseOAuthCallbackInput(req.Code)
	state := strings.TrimSpace(req.State)
	if callbackState != "" {
		state = callbackState
	}
	if code == "" || state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "authorization code and state are required"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(session.State)) != 1 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Codex OAuth state does not match the current authorization session"})
		return
	}
	auth, err := s.exchangeCodexAuthorizationCode(r.Context(), code, session)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	s.codexOAuthSessions.Delete(strings.TrimSpace(req.SessionID))
	writeJSON(w, http.StatusOK, map[string]any{"auth": auth})
}

func (s *Server) handleCodexRefreshToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
		ClientID     string `json:"client_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	refreshToken := strings.TrimSpace(req.RefreshToken)
	if refreshToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "refresh_token is required"})
		return
	}
	auth := &config.CodexAuth{
		AuthMode:     config.CodexAuthModeRefreshToken,
		RefreshToken: refreshToken,
		ClientID:     strings.TrimSpace(req.ClientID),
		UpdatedAt:    time.Now().UnixMilli(),
	}
	auth.Normalize()
	refreshed, err := s.refreshCodexAuth(r.Context(), auth)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	refreshed.AuthMode = config.CodexAuthModeRefreshToken
	writeJSON(w, http.StatusOK, map[string]any{"auth": refreshed})
}

func (s *Server) handleCodexPATValidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	auth, err := s.validateCodexPAT(r.Context(), req.AccessToken)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth": auth})
}

func (s *Server) handleCodexRefreshChannel(w http.ResponseWriter, r *http.Request) {
	channelID := strings.TrimSpace(r.PathValue("id"))
	_, current, err := codexAuthFromConfig(s.engine.Config(), channelID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	if strings.TrimSpace(current.RefreshToken) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "this Codex authorization has no refresh_token"})
		return
	}
	auth, err := s.ensureCodexAuthCurrent(r.Context(), channelID, "", true)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth": auth})
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate secure random value: %w", err)
	}
	return hex.EncodeToString(data), nil
}

func parseOAuthCallbackInput(input string) (code, state string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}
	if parsed, err := url.Parse(input); err == nil && parsed.RawQuery != "" {
		if value := strings.TrimSpace(parsed.Query().Get("code")); value != "" {
			return value, strings.TrimSpace(parsed.Query().Get("state"))
		}
	}
	if strings.Contains(input, "code=") {
		raw := strings.TrimPrefix(input, "?")
		if values, err := url.ParseQuery(raw); err == nil {
			if value := strings.TrimSpace(values.Get("code")); value != "" {
				return value, strings.TrimSpace(values.Get("state"))
			}
		}
	}
	return input, ""
}

func (s *Server) deleteExpiredCodexOAuthSessions() {
	now := time.Now()
	s.codexOAuthSessions.Range(func(key, value any) bool {
		session, ok := value.(*codexOAuthSession)
		if !ok || now.Sub(session.CreatedAt) > codexOAuthSessionTTL {
			s.codexOAuthSessions.Delete(key)
		}
		return true
	})
}

func (s *Server) exchangeCodexAuthorizationCode(ctx context.Context, code string, session *codexOAuthSession) (*config.CodexAuth, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", config.CodexClientID)
	form.Set("code", strings.TrimSpace(code))
	form.Set("redirect_uri", session.RedirectURI)
	form.Set("code_verifier", session.CodeVerifier)
	auth, err := s.requestCodexToken(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("exchange Codex OAuth authorization code: %w", err)
	}
	auth.AuthMode = config.CodexAuthModeOAuth
	auth.Normalize()
	return auth, nil
}

func (s *Server) validateCodexPAT(ctx context.Context, accessToken string) (*config.CodexAuth, error) {
	accessToken = strings.TrimSpace(accessToken)
	if !strings.HasPrefix(accessToken, "at-") {
		return nil, errors.New("Codex Personal Access Token must start with at-")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexPATWhoAmIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", codexDefaultUA)
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("Version", codexDefaultVersion)
	resp, err := s.engine.ProxyClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("validate Codex Personal Access Token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read Codex Personal Access Token validation response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, message, detail := codexErrorDetails(body)
		if message == "" {
			message = detail
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("Codex Personal Access Token validation returned status %d: %s", resp.StatusCode, message)
	}
	var whoami struct {
		Email            string `json:"email"`
		ChatGPTUserID    string `json:"chatgpt_user_id"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(body, &whoami); err != nil {
		return nil, fmt.Errorf("parse Codex Personal Access Token validation response: %w", err)
	}
	if strings.TrimSpace(whoami.ChatGPTAccountID) == "" {
		return nil, errors.New("Codex Personal Access Token validation response is missing chatgpt_account_id")
	}
	auth := &config.CodexAuth{
		AuthMode:    config.CodexAuthModePersonalAccessToken,
		AccessToken: accessToken,
		AccountID:   whoami.ChatGPTAccountID,
		UserID:      whoami.ChatGPTUserID,
		Email:       whoami.Email,
		ClientID:    config.CodexClientID,
		UpdatedAt:   time.Now().UnixMilli(),
	}
	auth.Normalize()
	return auth, nil
}

func upstreamTarget(ch config.Channel, path string) string {
	if !ch.IsCodexAuth() {
		return engine.BuildURL(ch.BaseURL, path)
	}
	base := strings.TrimRight(strings.TrimSpace(ch.BaseURL), "/")
	if strings.HasSuffix(strings.ToLower(base), "/responses") {
		return base
	}
	return base + "/responses"
}

func applyCodexRequestHeaders(dst, src http.Header, auth *config.CodexAuth, stream bool) {
	if dst == nil || auth == nil {
		return
	}
	for _, key := range []string{
		"Accept-Language",
		"Conversation-Id",
		"Session-Id",
		"X-Codex-Beta-Features",
		"X-Codex-Installation-Id",
		"X-Codex-Turn-State",
		"X-Codex-Turn-Metadata",
		"X-Codex-Window-Id",
	} {
		if value := strings.TrimSpace(src.Get(key)); value != "" {
			dst.Set(key, value)
		}
	}
	userAgent := strings.TrimSpace(src.Get("User-Agent"))
	if userAgent == "" {
		userAgent = codexDefaultUA
	}
	originator := strings.TrimSpace(src.Get("Originator"))
	if originator == "" {
		originator = codexOriginatorFromUA(userAgent)
	}
	version := strings.TrimSpace(src.Get("Version"))
	if version == "" {
		version = codexVersionFromUA(userAgent)
	}
	dst.Set("Authorization", "Bearer "+auth.AccessToken)
	dst.Set("User-Agent", userAgent)
	dst.Set("Originator", originator)
	dst.Set("Version", version)
	dst.Del("OpenAI-Beta")
	if auth.AccountID != "" {
		dst.Set("ChatGPT-Account-ID", auth.AccountID)
	}
	if stream {
		dst.Set("Accept", "text/event-stream")
	} else {
		dst.Set("Accept", "application/json")
	}
}

func codexOriginatorFromUA(userAgent string) string {
	client := strings.TrimSpace(strings.SplitN(userAgent, "/", 2)[0])
	if strings.HasPrefix(strings.ToLower(client), "codex") {
		return client
	}
	return "codex-tui"
}

func codexVersionFromUA(userAgent string) string {
	parts := strings.SplitN(strings.TrimSpace(userAgent), "/", 2)
	if len(parts) != 2 {
		return codexDefaultVersion
	}
	version := strings.Fields(parts[1])
	if len(version) == 0 || strings.TrimSpace(version[0]) == "" {
		return codexDefaultVersion
	}
	return version[0]
}

func (s *Server) ensureCodexAuth(ctx context.Context, channelID string) (*config.CodexAuth, error) {
	return s.ensureCodexAuthCurrent(ctx, channelID, "", false)
}

func (s *Server) ensureCodexAuthAfterUnauthorized(ctx context.Context, channelID, rejectedAccessToken string) (*config.CodexAuth, error) {
	return s.ensureCodexAuthCurrent(ctx, channelID, rejectedAccessToken, true)
}

func (s *Server) codexRefreshLock(channelID string) *codexContextLock {
	actual, _ := s.codexRefreshLocks.LoadOrStore(channelID, newCodexContextLock())
	lock, ok := actual.(*codexContextLock)
	if !ok {
		lock = newCodexContextLock()
		s.codexRefreshLocks.Store(channelID, lock)
	}
	return lock
}

func (s *Server) ensureCodexAuthCurrent(ctx context.Context, channelID, rejectedAccessToken string, forceRefresh bool) (*config.CodexAuth, error) {
	refreshLock := s.codexRefreshLock(channelID)
	if err := refreshLock.Lock(ctx); err != nil {
		return nil, fmt.Errorf("wait for Codex authorization refresh: %w", err)
	}
	defer refreshLock.Unlock()

	cfg := s.engine.Config()
	_, auth, err := codexAuthFromConfig(cfg, channelID)
	if err != nil {
		return nil, err
	}
	if forceRefresh && rejectedAccessToken != "" && auth.AccessToken != "" && auth.AccessToken != rejectedAccessToken {
		// Another request already refreshed this channel while the current
		// request was waiting for the per-channel lock.
		return auth, nil
	}
	expiresAt, hasExpiry := auth.ExpiresAtTime()
	if !forceRefresh && auth.AccessToken != "" && (!hasExpiry || time.Until(expiresAt) > codexRefreshSkew) {
		return auth, nil
	}
	if auth.RefreshToken == "" {
		if auth.AccessToken != "" && (!hasExpiry || time.Now().Before(expiresAt)) {
			return auth, nil
		}
		return nil, errors.New("Codex access_token expired and refresh_token is missing")
	}

	attempted := *auth
	refreshed, err := s.refreshCodexAuth(ctx, auth)
	if err != nil {
		if isCodexRefreshTokenRejected(err) {
			latestCfg := s.engine.Config()
			_, latest, latestErr := codexAuthFromConfig(latestCfg, channelID)
			if latestErr == nil && codexCredentialsChanged(latest, &attempted) {
				return latest, nil
			}
		}
		if !forceRefresh && auth.AccessToken != "" && (!hasExpiry || time.Now().Before(expiresAt)) {
			// Match sub2api's OpenAI policy: a proactive refresh failure may
			// temporarily fall back only while the existing access token is
			// still valid. Forced 401 recovery never falls back.
			return auth, nil
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		// Do not persist provider credentials returned after the request
		// boundary has been canceled.
		return nil, err
	}

	s.saveMu.Lock()
	latestCfg := s.engine.Config()
	index, latest, latestErr := codexAuthFromConfig(latestCfg, channelID)
	if latestErr != nil {
		s.saveMu.Unlock()
		return nil, latestErr
	}
	if codexCredentialsChanged(latest, &attempted) {
		// A manual re-import or another credential writer won while the
		// provider request was in flight. Never overwrite the newer token set.
		s.saveMu.Unlock()
		return latest, nil
	}
	latestCfg.Channels[index].CodexAuth = refreshed
	if err := s.engine.ReplaceConfig(latestCfg); err != nil {
		s.saveMu.Unlock()
		return nil, err
	}
	saveErr := s.saveCodexRefreshLocked(ctx)
	s.saveMu.Unlock()
	if saveErr != nil {
		// The provider may already have rotated the refresh token. Keep the
		// refreshed in-memory credentials usable and report the durability
		// problem in logs instead of rolling back to a consumed token.
		if s.logger != nil {
			s.logger.Printf("persist refreshed Codex authorization for channel %s: %v", channelID, saveErr)
		}
	}
	return refreshed, nil
}

func codexAuthFromConfig(cfg *config.Config, channelID string) (int, *config.CodexAuth, error) {
	if cfg == nil {
		return -1, nil, errors.New("Codex configuration is unavailable")
	}
	for i := range cfg.Channels {
		ch := &cfg.Channels[i]
		if ch.ID != channelID {
			continue
		}
		if !ch.IsCodexAuth() || ch.CodexAuth == nil {
			return -1, nil, errors.New("Codex channel authorization is missing")
		}
		auth := *ch.CodexAuth
		auth.Normalize()
		return i, &auth, nil
	}
	return -1, nil, errors.New("Codex channel not found")
}

func codexCredentialsChanged(current, attempted *config.CodexAuth) bool {
	if current == nil || attempted == nil {
		return current != attempted
	}
	return current.AccessToken != attempted.AccessToken ||
		current.RefreshToken != attempted.RefreshToken ||
		current.IDToken != attempted.IDToken ||
		current.UpdatedAt != attempted.UpdatedAt
}

func (s *Server) saveCodexRefreshLocked(ctx context.Context) error {
	if strings.TrimSpace(s.cfgPath) == "" {
		return nil
	}
	var lastErr error
	for attempt, delay := range []time.Duration{0, 25 * time.Millisecond, 100 * time.Millisecond} {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := s.engine.Config().Save(s.cfgPath); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

type codexOAuthRefreshError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *codexOAuthRefreshError) Error() string {
	if e == nil {
		return "Codex OAuth refresh failed"
	}
	detail := strings.TrimSpace(e.Code)
	if detail == "" {
		detail = strings.TrimSpace(e.Message)
	}
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("Codex OAuth refresh returned status %d: %s", e.StatusCode, detail)
}

func isCodexRefreshTokenRejected(err error) bool {
	var refreshErr *codexOAuthRefreshError
	if errors.As(err, &refreshErr) {
		switch strings.ToLower(strings.TrimSpace(refreshErr.Code)) {
		case "invalid_grant", "invalid_refresh_token", "token_expired",
			"refresh_token_reused", "refresh_token_invalidated",
			"app_session_terminated", "invalid_client":
			return true
		}
	}
	text := strings.ToLower(errorText(err))
	for _, marker := range []string{
		"invalid_grant",
		"invalid_refresh_token",
		"token_expired",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"app_session_terminated",
		"invalid_client",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func codexPermanentAuthFailure(body []byte) (string, bool) {
	code, _, detail := codexErrorDetails(body)
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "token_invalidated", "token_revoked":
		return code, true
	}
	if strings.EqualFold(strings.TrimSpace(detail), "Unauthorized") {
		return "unauthorized", true
	}
	return "", false
}

func codexErrorDetails(body []byte) (code, message, detail string) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return "", "", ""
	}
	if rawError, ok := payload["error"].(map[string]any); ok {
		code = jsonString(rawError["code"])
		message = jsonString(rawError["message"])
	} else if rawError, ok := payload["error"].(string); ok {
		code = strings.TrimSpace(rawError)
	}
	switch rawDetail := payload["detail"].(type) {
	case string:
		detail = strings.TrimSpace(rawDetail)
	case map[string]any:
		if code == "" {
			code = jsonString(rawDetail["code"])
		}
		if message == "" {
			message = jsonString(rawDetail["message"])
		}
	}
	if code == "" {
		code = jsonString(payload["code"])
	}
	if message == "" {
		message = jsonString(payload["message"])
	}
	if message == "" {
		message = jsonString(payload["error_description"])
	}
	return code, message, detail
}

func jsonString(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func (s *Server) refreshCodexAuth(ctx context.Context, current *config.CodexAuth) (*config.CodexAuth, error) {
	clientID := strings.TrimSpace(current.ClientID)
	if clientID == "" {
		clientID = config.CodexClientID
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", current.RefreshToken)
	form.Set("client_id", clientID)
	form.Set("scope", "openid profile email")
	token, err := s.requestCodexToken(ctx, form)
	if err != nil {
		return nil, fmt.Errorf("refresh Codex authorization: %w", err)
	}

	next := *current
	next.AccessToken = token.AccessToken
	if token.RefreshToken != "" {
		next.RefreshToken = token.RefreshToken
	}
	if token.IDToken != "" {
		next.IDToken = token.IDToken
	}
	next.ClientID = clientID
	if token.AccountID != "" {
		next.AccountID = token.AccountID
	}
	if token.UserID != "" {
		next.UserID = token.UserID
	}
	if token.Email != "" {
		next.Email = token.Email
	}
	next.ExpiresAt = token.ExpiresAt
	next.UpdatedAt = time.Now().UnixMilli()
	next.Normalize()
	return &next, nil
}

func (s *Server) requestCodexToken(ctx context.Context, form url.Values) (*config.CodexAuth, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", codexDefaultUA)
	req.Header.Set("Originator", "codex-tui")

	resp, err := s.engine.ProxyClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read token response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code, message, detail := codexErrorDetails(body)
		if message == "" {
			message = detail
		}
		return nil, &codexOAuthRefreshError{
			StatusCode: resp.StatusCode,
			Code:       code,
			Message:    message,
		}
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("Codex token response is missing access_token")
	}
	auth := &config.CodexAuth{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		IDToken:      token.IDToken,
		ClientID:     strings.TrimSpace(form.Get("client_id")),
		UpdatedAt:    time.Now().UnixMilli(),
	}
	if token.ExpiresIn > 0 {
		auth.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	auth.Normalize()
	return auth, nil
}

func (s *Server) fetchUpstreamModels(channel config.Channel) ([]string, error) {
	paths := []string{"/v1/models", "/models"}
	var lastErr error
	for _, path := range paths {
		target := engine.BuildURL(channel.BaseURL, path)
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			return nil, err
		}
		channel.ApplyAPIKeyAuthorization(req)
		resp, err := s.engine.ProxyClient().Do(req)
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		cancel()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			lastErr = fmt.Errorf("upstream returned 404 for %s", path)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("upstream returned status %d", resp.StatusCode)
			continue
		}
		models := parseModelList(body)
		if len(models) > 0 {
			return models, nil
		}
		lastErr = errors.New("upstream returned an empty model list")
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("unable to fetch models")
}

func parseModelList(body []byte) []string {
	var raw struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw.Data))
	for _, m := range raw.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func requestModel(body []byte) (string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return "", nil
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", err
	}
	model, _ := raw["model"].(string)
	return strings.TrimSpace(model), nil
}

func rewriteRequestBody(body []byte, upstreamModel string) ([]byte, bool, error) {
	raw := map[string]any{}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, false, err
		}
	}
	raw["model"] = upstreamModel
	out, err := json.Marshal(raw)
	if err != nil {
		return nil, false, err
	}
	stream, _ := raw["stream"].(bool)
	return out, stream, nil
}

func buildFallbackChain(requested string, fallbacks map[string]string) []string {
	chain := []string{requested}
	seen := map[string]bool{requested: true}
	current := requested
	for {
		next := strings.TrimSpace(fallbacks[current])
		if next == "" || seen[next] {
			break
		}
		seen[next] = true
		chain = append(chain, next)
		current = next
	}
	return chain
}

func copyRequestHeaders(dst, src http.Header) {
	for _, key := range []string{"Accept", "Content-Type", "User-Agent", "X-Request-Id"} {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		// Content-Length must not be forwarded verbatim. The router may stream
		// the body (or the upstream length may be wrong), so Go recomputes it
		// from what is actually written, or switches to chunked encoding.
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func writeProxyError(w http.ResponseWriter, status int, message, typ, code string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    typ,
			"code":    code,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

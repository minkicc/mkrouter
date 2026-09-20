package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minkicc/mkrouter/backend/internal/config"
	"github.com/minkicc/mkrouter/backend/internal/routing"
)

type HealthStatus string

const (
	StatusUnknown   HealthStatus = "unknown"
	StatusHealthy   HealthStatus = "healthy"
	StatusUnhealthy HealthStatus = "unhealthy"
	StatusDisabled  HealthStatus = "disabled"
)

type ChannelState struct {
	ID                   string            `json:"id"`
	Name                 string            `json:"name"`
	BaseURL              string            `json:"base_url"`
	AuthType             string            `json:"auth_type,omitempty"`
	Models               []string          `json:"models"`
	ModelMappings        map[string]string `json:"model_mappings,omitempty"`
	Priority             int               `json:"priority"`
	Group                string            `json:"group"`
	Weight               int               `json:"weight"`
	Price                float64           `json:"price"`
	Enabled              bool              `json:"enabled"`
	APIKeySet            bool              `json:"api_key_set"`
	AuthStatus           string            `json:"auth_status,omitempty"`
	AuthEmail            string            `json:"auth_email,omitempty"`
	AuthExpiresAt        string            `json:"auth_expires_at,omitempty"`
	AuthRefreshable      bool              `json:"auth_refreshable,omitempty"`
	AuthMode             string            `json:"auth_mode,omitempty"`
	Status               HealthStatus      `json:"status"`
	ResponseTimeMS       int64             `json:"response_time_ms"`
	ConsecutiveFailures  int               `json:"consecutive_failures"`
	ConsecutiveSuccesses int               `json:"consecutive_successes"`
	LastStatusCode       int               `json:"last_status_code"`
	LastChecked          int64             `json:"last_checked"`
	LastError            string            `json:"last_error,omitempty"`
	CooldownUntil        int64             `json:"cooldown_until,omitempty"`
	CooldownCount        int               `json:"cooldown_count,omitempty"`
	CooldownDuration     int64             `json:"cooldown_duration_seconds,omitempty"`
	CooldownReason       string            `json:"cooldown_reason,omitempty"`
}

type Selection struct {
	Channel       config.Channel `json:"channel"`
	UpstreamModel string         `json:"upstream_model"`
}

type healthState struct {
	status               HealthStatus
	responseTime         time.Duration
	consecutiveFailures  int
	consecutiveSuccesses int
	lastStatusCode       int
	lastChecked          time.Time
	lastError            string
	nextCheck            time.Time
	cooldownUntil        time.Time
	cooldownReason       string
	consecutiveCooldowns int
}

// cooldownReasonLimit bounds the stored explanation so a chatty upstream error
// cannot bloat the state payload.
const cooldownReasonLimit = 512

type channelRuntime struct {
	channel config.Channel
	health  healthState
}

type Engine struct {
	mu                sync.RWMutex
	cfg               config.Config
	runtimes          map[string]*channelRuntime
	checkMu           sync.Mutex
	checking          map[string]bool
	proxyClient       *http.Client
	healthClient      *http.Client
	codexAuthResolver func(context.Context, string) (*config.CodexAuth, error)
}

const (
	proxyTimeout = 10 * time.Minute
	// Allow slow reasoning/model cold starts while still failing a dead
	// upstream before the full streaming request timeout elapses.
	proxyHeaderTimeout = 5 * time.Minute
	// Cooldown escalates with consecutive failures: the first failed request
	// cools a channel down briefly, and repeated failures keep it out of
	// routing for progressively longer windows. A success resets the ladder.
	cooldownFirst  = 60 * time.Second
	cooldownSecond = 5 * time.Minute
	cooldownLater  = 15 * time.Minute
)

func (e *Engine) ProxyClient() *http.Client {
	return e.proxyClient
}

func (e *Engine) SetCodexAuthResolver(resolver func(context.Context, string) (*config.CodexAuth, error)) {
	e.mu.Lock()
	e.codexAuthResolver = resolver
	e.mu.Unlock()
}

// CodexClientVersion is the version advertised to the Codex upstream. Model
// discovery, health probes, and inference all resolve it here so they can never
// disagree during a release rollover.
func (e *Engine) CodexClientVersion() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg.EffectiveCodexClientVersion()
}

func New(cfg *config.Config) (*Engine, error) {
	if cfg == nil {
		cfg = config.Default()
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	proxyTransport := http.DefaultTransport.(*http.Transport).Clone()
	proxyTransport.ResponseHeaderTimeout = proxyHeaderTimeout
	e := &Engine{
		runtimes:     map[string]*channelRuntime{},
		checking:     map[string]bool{},
		proxyClient:  &http.Client{Transport: proxyTransport, Timeout: proxyTimeout},
		healthClient: &http.Client{},
	}
	e.ReplaceConfig(cfg)
	return e, nil
}

func (e *Engine) ReplaceConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	cfg.Normalize()
	if err := cfg.Validate(); err != nil {
		return err
	}
	next := map[string]*channelRuntime{}
	e.mu.Lock()
	old := e.runtimes
	for i := range cfg.Channels {
		ch := cfg.Channels[i]
		rt := &channelRuntime{channel: ch}
		if old != nil {
			if prev, ok := old[ch.ID]; ok {
				rt.health = prev.health
				rt.health.nextCheck = time.Now()
			}
		}
		if !ch.IsEnabled() {
			rt.health.status = StatusDisabled
		} else if rt.health.status == "" {
			rt.health.status = StatusUnknown
		}
		if rt.health.nextCheck.IsZero() {
			rt.health.nextCheck = time.Now()
		}
		next[ch.ID] = rt
	}
	e.runtimes = next
	e.cfg = *cfg.Clone()
	e.mu.Unlock()
	return nil
}

func (e *Engine) Config() *config.Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg.Clone()
}

func (e *Engine) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.checkDue()
			}
		}
	}()
}

func (e *Engine) checkDue() {
	now := time.Now()
	e.mu.RLock()
	due := make([]string, 0)
	for id, rt := range e.runtimes {
		if !rt.channel.IsEnabled() || rt.health.status == StatusDisabled {
			continue
		}
		if !rt.health.nextCheck.After(now) {
			due = append(due, id)
		}
	}
	e.mu.RUnlock()
	for _, id := range due {
		go e.CheckNow(id)
	}
}

func (e *Engine) CheckAll() {
	e.mu.RLock()
	ids := make([]string, 0, len(e.runtimes))
	for id := range e.runtimes {
		ids = append(ids, id)
	}
	e.mu.RUnlock()
	for _, id := range ids {
		e.CheckNow(id)
	}
}

func (e *Engine) CheckNow(id string) {
	now := time.Now()
	e.mu.Lock()
	rt, ok := e.runtimes[id]
	if !ok || !rt.channel.IsEnabled() {
		e.mu.Unlock()
		return
	}
	if !rt.health.cooldownUntil.IsZero() && now.Before(rt.health.cooldownUntil) {
		// A cooling channel must stay out of the health-check loop: probing
		// /v1/models while it is cooling would clear the failure signal that
		// triggered the cooldown in the first place.
		interval := rt.channel.EffectiveHealth(e.cfg.HealthCheck).IntervalSeconds
		if interval <= 0 {
			interval = 30
		}
		rt.health.nextCheck = now.Add(time.Duration(interval) * time.Second)
		e.mu.Unlock()
		return
	}
	ch := rt.channel
	e.mu.Unlock()

	if !e.beginCheck(id) {
		return
	}
	defer e.endCheck(id)

	health := ch.EffectiveHealth(e.currentGlobalHealth())
	path := strings.TrimSpace(health.Path)
	if path == "" {
		path = "/v1/models"
	}
	status, latency, err := e.probe(ch, path, health.TimeoutSeconds)
	if err != nil {
		e.recordFailure(id, status, latency, err)
		return
	}
	if status >= 200 && status < 300 {
		e.recordSuccess(id, status, latency, false)
		return
	}
	if status == http.StatusNotFound && (path == "/v1/models" || path == "/models") {
		alt := "/models"
		if path == "/models" {
			alt = "/v1/models"
		}
		altStatus, altLatency, altErr := e.probe(ch, alt, health.TimeoutSeconds)
		if altErr == nil && altStatus >= 200 && altStatus < 300 {
			e.recordSuccess(id, altStatus, altLatency, false)
			return
		}
	}
	e.recordFailure(id, status, latency, nil)
}

func (e *Engine) currentGlobalHealth() config.HealthCheck {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg.HealthCheck
}

func (e *Engine) beginCheck(id string) bool {
	e.checkMu.Lock()
	defer e.checkMu.Unlock()
	if e.checking[id] {
		return false
	}
	e.checking[id] = true
	return true
}

func (e *Engine) endCheck(id string) {
	e.checkMu.Lock()
	defer e.checkMu.Unlock()
	delete(e.checking, id)
}

func (e *Engine) probe(ch config.Channel, path string, timeoutSeconds int) (int, time.Duration, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
	defer cancel()
	if ch.IsCodexAuth() {
		e.mu.RLock()
		resolver := e.codexAuthResolver
		e.mu.RUnlock()
		if resolver != nil {
			auth, err := resolver(ctx, ch.ID)
			if err != nil {
				return 0, 0, err
			}
			ch.CodexAuth = auth
		}
	}
	target := BuildURL(ch.BaseURL, path)
	if ch.IsCodexAuth() {
		target = config.CodexModelsURL + "?client_version=" + e.CodexClientVersion()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, 0, err
	}
	ch.ApplyAPIKeyAuthorization(req)
	if ch.IsCodexAuth() && ch.CodexAuth != nil {
		auth := *ch.CodexAuth
		auth.Normalize()
		if auth.AccessToken != "" {
			req.Header.Set("Authorization", "Bearer "+auth.AccessToken)
		}
		if auth.AccountID != "" {
			req.Header.Set("ChatGPT-Account-ID", auth.AccountID)
		}
		req.Header.Set("Accept", "application/json")
		version := e.CodexClientVersion()
		req.Header.Set("User-Agent", "codex_cli_rs/"+version)
		req.Header.Set("Originator", "codex_cli_rs")
		req.Header.Set("Version", version)
	}
	for k, v := range ch.Headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := e.healthClient.Do(req)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	_ = drainBody(resp)
	return status, time.Since(start), nil
}

func drainBody(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	_, err := io.Copy(io.Discard, resp.Body)
	return err
}

func (e *Engine) recordSuccess(id string, status int, latency time.Duration, fromRequest bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt, ok := e.runtimes[id]
	if !ok {
		return
	}
	cfg := e.cfg
	global := cfg.HealthCheck
	health := rt.channel.EffectiveHealth(global)
	rt.health.consecutiveSuccesses++
	rt.health.consecutiveFailures = 0
	rt.health.lastStatusCode = status
	rt.health.lastChecked = time.Now()
	rt.health.lastError = ""
	rt.health.responseTime = latency
	// Only a real request success resets the cooldown escalation ladder. A
	// successful /v1/models probe must not hide repeated request failures.
	if fromRequest {
		rt.health.consecutiveCooldowns = 0
		rt.health.cooldownReason = ""
	}
	threshold := health.SuccessThreshold
	if threshold <= 0 {
		threshold = 1
	}
	if rt.health.consecutiveSuccesses >= threshold {
		// A channel in request-failure cooldown must remain unhealthy until
		// the cooldown expires and a subsequent health check succeeds.
		if rt.health.cooldownUntil.IsZero() || !time.Now().Before(rt.health.cooldownUntil) {
			rt.health.status = StatusHealthy
		}
	}
	rt.health.nextCheck = time.Now().Add(time.Duration(health.IntervalSeconds) * time.Second)
}

func (e *Engine) recordFailure(id string, status int, latency time.Duration, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt, ok := e.runtimes[id]
	if !ok {
		return
	}
	cfg := e.cfg
	global := cfg.HealthCheck
	health := rt.channel.EffectiveHealth(global)
	rt.health.consecutiveFailures++
	rt.health.consecutiveSuccesses = 0
	rt.health.lastStatusCode = status
	rt.health.lastChecked = time.Now()
	rt.health.responseTime = latency
	if err != nil {
		rt.health.lastError = err.Error()
	} else {
		rt.health.lastError = fmt.Sprintf("status %d", status)
	}
	threshold := health.FailureThreshold
	if threshold <= 0 {
		threshold = 1
	}
	if rt.health.consecutiveFailures >= threshold {
		rt.health.status = StatusUnhealthy
	}
	rt.health.nextCheck = time.Now().Add(time.Duration(health.IntervalSeconds) * time.Second)
}

func (e *Engine) RecordAttempt(id string, status int, err error, latency time.Duration) {
	if err != nil {
		e.recordFailure(id, status, latency, err)
		return
	}
	if status >= 200 && status < 300 {
		e.recordSuccess(id, status, latency, true)
		return
	}
	if routing.IsRetryableStatus(status) {
		e.recordFailure(id, status, latency, nil)
	}
}

// Cooldown removes a channel from routing for a short window and explains it
// with the recorded failure.
func (e *Engine) Cooldown(id string) {
	e.CooldownWithReason(id, "")
}

// CooldownWithReason removes a channel from routing for a short window and
// keeps the trigger explanation so the channel list can show why it is cooling
// down and when it will be retried. It must be called after the channel's
// retries have been exhausted (or the failure is not worth retrying on the same
// channel), not on the first failed attempt.
func (e *Engine) CooldownWithReason(id, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if rt, ok := e.runtimes[id]; ok {
		rt.health.consecutiveCooldowns++
		rt.health.cooldownUntil = time.Now().Add(cooldownDuration(rt.health.consecutiveCooldowns))
		// Cooldown is caused by an exhausted request retry cycle, so the
		// channel must not continue to advertise a stale healthy status.
		rt.health.status = StatusUnhealthy
		rt.health.cooldownReason = normalizeCooldownReason(reason, rt.health.lastError)
	}
}

func normalizeCooldownReason(reason, lastError string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = strings.TrimSpace(lastError)
	}
	if reason == "" {
		return "upstream request failed"
	}
	if len([]rune(reason)) > cooldownReasonLimit {
		return string([]rune(reason)[:cooldownReasonLimit])
	}
	return reason
}

// cooldownDuration returns the cooldown window for the nth consecutive
// failure: 60s for the first, 5m for the second, and 15m from the third on.
func cooldownDuration(consecutive int) time.Duration {
	switch {
	case consecutive <= 1:
		return cooldownFirst
	case consecutive == 2:
		return cooldownSecond
	default:
		return cooldownLater
	}
}

func IsRetryableStatus(status int) bool {
	return routing.IsRetryableStatus(status)
}

func (e *Engine) State() []ChannelState {
	e.mu.RLock()
	defer e.mu.RUnlock()
	ids := make([]string, 0, len(e.runtimes))
	for id := range e.runtimes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ChannelState, 0, len(ids))
	for _, id := range ids {
		rt := e.runtimes[id]
		ch := rt.channel
		st := ChannelState{
			ID:                   ch.ID,
			Name:                 ch.Name,
			BaseURL:              ch.BaseURL,
			AuthType:             ch.AuthType,
			Models:               append([]string(nil), ch.Models...),
			ModelMappings:        cloneStringMap(ch.ModelMappings),
			Priority:             ch.Priority,
			Group:                ch.Group,
			Weight:               ch.Weight,
			Price:                ch.Price,
			Enabled:              ch.IsEnabled(),
			APIKeySet:            strings.TrimSpace(ch.APIKey) != "",
			Status:               rt.health.status,
			ResponseTimeMS:       rt.health.responseTime.Milliseconds(),
			ConsecutiveFailures:  rt.health.consecutiveFailures,
			ConsecutiveSuccesses: rt.health.consecutiveSuccesses,
			LastStatusCode:       rt.health.lastStatusCode,
			LastChecked:          rt.health.lastChecked.Unix(),
			LastError:            rt.health.lastError,
			CooldownUntil:        cooldownUntilUnix(rt.health.cooldownUntil),
			CooldownCount:        rt.health.consecutiveCooldowns,
			CooldownDuration:     int64(cooldownDuration(rt.health.consecutiveCooldowns) / time.Second),
			CooldownReason:       activeCooldownReason(rt.health),
		}
		st.AuthStatus, st.AuthEmail, st.AuthExpiresAt, st.AuthRefreshable, st.AuthMode = channelAuthorizationState(ch)
		out = append(out, st)
	}
	return out
}

func channelAuthorizationState(ch config.Channel) (status, email, expiresAt string, refreshable bool, mode string) {
	switch {
	case ch.IsNoAuth():
		return "not_required", "", "", false, config.ChannelAuthNone
	case ch.IsCodexAuth():
		if ch.CodexAuth == nil {
			return "missing", "", "", false, config.ChannelAuthCodex
		}
		auth := *ch.CodexAuth
		auth.Normalize()
		status = "configured"
		if expiry, ok := auth.ExpiresAtTime(); ok {
			expiresAt = expiry.Format(time.RFC3339)
			switch {
			case !time.Now().Before(expiry):
				status = "expired"
			case time.Until(expiry) <= 10*time.Minute:
				status = "expiring"
			default:
				status = "active"
			}
		} else if auth.AccessToken == "" {
			status = "refresh_required"
		} else {
			status = "active"
		}
		return status, auth.Email, expiresAt, auth.RefreshToken != "", auth.AuthMode
	default:
		if strings.TrimSpace(ch.APIKey) == "" {
			return "missing", "", "", false, config.ChannelAuthAPIKey
		}
		return "configured", "", "", false, ch.APIKeyPlacement
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (e *Engine) Select(requestedModel string, excluded map[string]bool) (*Selection, error) {
	requestedModel = strings.TrimSpace(requestedModel)
	e.mu.RLock()
	cfg := e.cfg.Clone()
	routes := make([]routing.Route, 0, len(e.runtimes))
	now := time.Now()
	for _, rt := range e.runtimes {
		routes = append(routes, routeFromChannel(rt.channel, rt.health.status, rt.health.responseTime, rt.coolingDown(now)))
	}
	e.mu.RUnlock()

	mappings := make([]routing.ModelMapping, 0, len(cfg.ModelMappings))
	for _, m := range cfg.ModelMappings {
		mappings = append(mappings, routing.ModelMapping{
			PlatformModel: m.PlatformModel,
			UpstreamModel: m.UpstreamModel,
			ChannelID:     m.ChannelID,
			Enabled:       m.Enabled,
		})
	}
	groups := make([]routing.Group, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		groups = append(groups, routing.Group{Name: g.Name, Priority: g.Priority})
	}

	sel, err := routing.Select(requestedModel, routes, mappings, groups, excluded)
	if err != nil {
		return nil, err
	}
	channel, ok := e.FindChannel(sel.Route.ID)
	if !ok {
		return nil, errors.New("selected channel not found: " + sel.Route.ID)
	}
	return &Selection{Channel: channel, UpstreamModel: sel.UpstreamModel}, nil
}

func routeFromChannel(ch config.Channel, status HealthStatus, latency time.Duration, coolingDown bool) routing.Route {
	return routing.Route{
		ID:            ch.ID,
		Name:          ch.Name,
		Models:        append([]string(nil), ch.Models...),
		ModelMappings: cloneStringMap(ch.ModelMappings),
		Priority:      ch.Priority,
		Group:         ch.Group,
		Weight:        ch.Weight,
		Price:         ch.Price,
		Enabled:       ch.IsEnabled(),
		MaxRetries:    ch.MaxRetries,
		Status:        routingStatus(status),
		ResponseTime:  latency,
		Cooldown:      coolingDown,
	}
}

func (rt *channelRuntime) coolingDown(now time.Time) bool {
	return !rt.health.cooldownUntil.IsZero() && now.Before(rt.health.cooldownUntil)
}

func cooldownUntilUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// activeCooldownReason only surfaces the explanation while the channel is
// actually cooling down, so a stale reason cannot outlive the window.
func activeCooldownReason(health healthState) string {
	if health.cooldownUntil.IsZero() || !time.Now().Before(health.cooldownUntil) {
		return ""
	}
	return health.cooldownReason
}

func routingStatus(status HealthStatus) routing.Status {
	switch status {
	case StatusHealthy:
		return routing.StatusHealthy
	case StatusUnhealthy:
		return routing.StatusUnhealthy
	case StatusDisabled:
		return routing.StatusDisabled
	default:
		return routing.StatusUnknown
	}
}

func mappingEnabled(m config.ModelMapping) bool {
	return m.Enabled == nil || *m.Enabled
}

func (e *Engine) AvailableModels() []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	seen := map[string]bool{}
	for _, rt := range e.runtimes {
		if !rt.channel.IsEnabled() || rt.health.status == StatusDisabled || rt.health.status == StatusUnhealthy {
			continue
		}
		for _, model := range rt.channel.Models {
			model = strings.TrimSpace(model)
			if model == "" || model == "*" {
				continue
			}
			seen[model] = true
		}
		for platform := range rt.channel.ModelMappings {
			if platform = strings.TrimSpace(platform); platform != "" {
				seen[platform] = true
			}
		}
	}
	for _, m := range e.cfg.ModelMappings {
		if mappingEnabled(m) {
			seen[strings.TrimSpace(m.PlatformModel)] = true
		}
	}
	for platform := range e.cfg.FallbackModels {
		if platform = strings.TrimSpace(platform); platform != "" {
			seen[platform] = true
		}
	}
	out := make([]string, 0, len(seen))
	for model := range seen {
		if model != "" {
			out = append(out, model)
		}
	}
	sort.Strings(out)
	return out
}

func (e *Engine) FindChannel(id string) (config.Channel, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	rt, ok := e.runtimes[id]
	if !ok {
		return config.Channel{}, false
	}
	return rt.channel, true
}

func (e *Engine) Tokens() []config.Token {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]config.Token, len(e.cfg.Tokens))
	copy(out, e.cfg.Tokens)
	return out
}

func (e *Engine) TokenName(id string) string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, t := range e.cfg.Tokens {
		if t.ID == id {
			return strings.TrimSpace(t.Name)
		}
	}
	return ""
}

func (e *Engine) Authorize(bearer string) (config.Token, bool) {
	e.mu.RLock()
	cfg := e.cfg
	e.mu.RUnlock()

	if cfg.NoAuth {
		return config.Token{}, true
	}

	bearer = strings.TrimSpace(bearer)
	legacy := strings.TrimSpace(cfg.AuthToken)
	if bearer == "" {
		return config.Token{}, false
	}
	if legacy != "" && bearer == legacy {
		return config.Token{ID: "legacy", Name: "legacy"}, true
	}
	for _, t := range cfg.Tokens {
		if t.IsEnabled() && strings.TrimSpace(t.Token) != "" && t.Token == bearer {
			return t, true
		}
	}
	return config.Token{}, false
}

func (e *Engine) AddToken(name string) (config.Token, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "token"
	}
	id, err := randomHex(8)
	if err != nil {
		return config.Token{}, err
	}
	secret, err := randomHex(24)
	if err != nil {
		return config.Token{}, err
	}
	enabled := true
	t := config.Token{
		ID:        "tok_" + id,
		Name:      name,
		Token:     "lr-" + secret,
		Enabled:   &enabled,
		CreatedAt: time.Now().Unix(),
	}
	e.mu.Lock()
	e.cfg.Tokens = append(e.cfg.Tokens, t)
	e.mu.Unlock()
	return t, nil
}

func (e *Engine) RemoveToken(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.cfg.Tokens {
		if e.cfg.Tokens[i].ID == id {
			e.cfg.Tokens = append(e.cfg.Tokens[:i], e.cfg.Tokens[i+1:]...)
			return true
		}
	}
	return false
}

func (e *Engine) UpdateToken(id string, name *string, enabled *bool) (config.Token, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.cfg.Tokens {
		if e.cfg.Tokens[i].ID == id {
			if name != nil {
				n := strings.TrimSpace(*name)
				if n != "" {
					e.cfg.Tokens[i].Name = n
				}
			}
			if enabled != nil {
				v := *enabled
				e.cfg.Tokens[i].Enabled = &v
			}
			return e.cfg.Tokens[i], true
		}
	}
	return config.Token{}, false
}

func (e *Engine) RecordTokenRequest(id string) bool {
	if id == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.cfg.Tokens {
		if e.cfg.Tokens[i].ID == id {
			e.cfg.Tokens[i].RequestCount++
			e.cfg.Tokens[i].LastUsedAt = time.Now().Unix()
			return true
		}
	}
	return false
}

func (e *Engine) RecordTokenTokens(id string, prompt, completion int64) bool {
	if id == "" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range e.cfg.Tokens {
		if e.cfg.Tokens[i].ID == id {
			e.cfg.Tokens[i].PromptTokens += prompt
			e.cfg.Tokens[i].CompletionTokens += completion
			return true
		}
	}
	return false
}

func hasEnabledToken(tokens []config.Token) bool {
	for _, t := range tokens {
		if t.IsEnabled() && strings.TrimSpace(t.Token) != "" {
			return true
		}
	}
	return false
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (e *Engine) MarshalConfig() ([]byte, error) {
	return json.MarshalIndent(e.Config(), "", "  ")
}

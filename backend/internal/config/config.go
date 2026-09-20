package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type HealthCheck struct {
	IntervalSeconds  int    `json:"interval_seconds"`
	TimeoutSeconds   int    `json:"timeout_seconds"`
	Path             string `json:"path"`
	FailureThreshold int    `json:"failure_threshold"`
	SuccessThreshold int    `json:"success_threshold"`
}

type Channel struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	BaseURL         string            `json:"base_url"`
	AuthType        string            `json:"auth_type,omitempty"`
	APIKey          string            `json:"api_key,omitempty"`
	APIKeyPlacement string            `json:"api_key_placement,omitempty"`
	APIKeyHeader    string            `json:"api_key_header,omitempty"`
	APIKeyPrefix    string            `json:"api_key_prefix,omitempty"`
	APIKeyQuery     string            `json:"api_key_query,omitempty"`
	CodexAuth       *CodexAuth        `json:"codex_auth,omitempty"`
	Models          []string          `json:"models,omitempty"`
	ModelMappings   map[string]string `json:"model_mappings,omitempty"`
	Priority        int               `json:"priority,omitempty"`
	Group           string            `json:"group,omitempty"`
	MaxRetries      int               `json:"max_retries,omitempty"`
	Weight          int               `json:"weight,omitempty"`
	Price           float64           `json:"price,omitempty"`
	Enabled         *bool             `json:"enabled,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	HealthCheck     *HealthCheck      `json:"health_check,omitempty"`
}

type ModelMapping struct {
	PlatformModel string `json:"platform_model"`
	UpstreamModel string `json:"upstream_model"`
	ChannelID     string `json:"channel_id,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

type Token struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Token            string `json:"token,omitempty"`
	Enabled          *bool  `json:"enabled,omitempty"`
	CreatedAt        int64  `json:"created_at"`
	LastUsedAt       int64  `json:"last_used_at,omitempty"`
	RequestCount     int64  `json:"request_count,omitempty"`
	PromptTokens     int64  `json:"prompt_tokens,omitempty"`
	CompletionTokens int64  `json:"completion_tokens,omitempty"`
}

type Group struct {
	Name     string `json:"name"`
	Priority int    `json:"priority,omitempty"`
}

func (g *Group) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		g.Name = name
		g.Priority = 0
		return nil
	}
	type groupAlias Group
	var alias groupAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*g = Group(alias)
	return nil
}

type Config struct {
	ListenAddr      string            `json:"listen_addr"`
	AuthToken       string            `json:"auth_token,omitempty"`
	NoAuth          bool              `json:"no_auth,omitempty"`
	HealthCheck     HealthCheck       `json:"health_check"`
	Channels        []Channel         `json:"channels"`
	ModelMappings   []ModelMapping    `json:"model_mappings,omitempty"`
	FallbackModels  map[string]string `json:"fallback_models,omitempty"`
	Tokens          []Token           `json:"tokens,omitempty"`
	Groups          []Group           `json:"groups,omitempty"`
	AllowLAN        bool              `json:"allow_lan,omitempty"`
	UsageMaxRecords int               `json:"usage_max_records,omitempty"`
	// CodexClientVersion overrides the Codex client version advertised to the
	// upstream. When empty the synchronized release, then the compiled
	// fallback, is used instead.
	CodexClientVersion       string `json:"codex_client_version,omitempty"`
	CodexClientVersionSynced string `json:"codex_client_version_synced,omitempty"`
	CodexVersionAutoSync     *bool  `json:"codex_version_auto_sync,omitempty"`
}

func (c *Config) Clone() *Config {
	data, err := json.Marshal(c)
	if err != nil {
		out := Default()
		return out
	}
	var out Config
	if err := json.Unmarshal(data, &out); err != nil {
		return Default()
	}
	return &out
}

func Default() *Config {
	return &Config{
		ListenAddr: "127.0.0.1:8787",
		HealthCheck: HealthCheck{
			IntervalSeconds:  30,
			TimeoutSeconds:   5,
			Path:             "/v1/models",
			FailureThreshold: 2,
			SuccessThreshold: 1,
		},
		Channels:       []Channel{},
		ModelMappings:  []ModelMapping{},
		FallbackModels: map[string]string{},
		Tokens:         []Token{},
		Groups:         []Group{{Name: "default"}},
	}
}

func DefaultPath() string {
	if dir, err := os.UserConfigDir(); err == nil && strings.TrimSpace(dir) != "" {
		return filepath.Join(dir, "mkrouter", "config.json")
	}
	return "mkrouter.json"
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.Normalize()
	return &cfg, nil
}

func (c *Config) Save(path string) error {
	c.Normalize()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0600)
}

func (c *Config) Normalize() {
	if strings.TrimSpace(c.ListenAddr) == "" {
		c.ListenAddr = "127.0.0.1:8787"
	}
	if c.HealthCheck.IntervalSeconds <= 0 {
		c.HealthCheck.IntervalSeconds = 30
	}
	if c.HealthCheck.TimeoutSeconds <= 0 {
		c.HealthCheck.TimeoutSeconds = 5
	}
	if strings.TrimSpace(c.HealthCheck.Path) == "" {
		c.HealthCheck.Path = "/v1/models"
	}
	if c.HealthCheck.FailureThreshold <= 0 {
		c.HealthCheck.FailureThreshold = 2
	}
	if c.HealthCheck.SuccessThreshold <= 0 {
		c.HealthCheck.SuccessThreshold = 1
	}
	used := map[string]bool{}
	for i := range c.Channels {
		if id := strings.TrimSpace(c.Channels[i].ID); id != "" && !isLegacyAutoID(id) {
			used[id] = true
		}
	}
	nextID := nextChannelNumber(used)
	for i := range c.Channels {
		ch := &c.Channels[i]
		ch.AuthType = strings.ToLower(strings.TrimSpace(ch.AuthType))
		if ch.AuthType == "" {
			ch.AuthType = ChannelAuthAPIKey
		}
		ch.APIKeyPlacement = strings.ToLower(strings.TrimSpace(ch.APIKeyPlacement))
		ch.APIKeyHeader = strings.TrimSpace(ch.APIKeyHeader)
		ch.APIKeyPrefix = strings.TrimSpace(ch.APIKeyPrefix)
		ch.APIKeyQuery = strings.TrimSpace(ch.APIKeyQuery)
		if ch.AuthType == ChannelAuthAPIKey {
			if ch.APIKeyPlacement == "" {
				ch.APIKeyPlacement = APIKeyPlacementBearer
			}
			switch ch.APIKeyPlacement {
			case APIKeyPlacementBearer:
				if ch.APIKeyPrefix == "" {
					ch.APIKeyPrefix = "Bearer"
				}
			case APIKeyPlacementHeader:
				if ch.APIKeyHeader == "" {
					ch.APIKeyHeader = "X-API-Key"
				}
			case APIKeyPlacementQuery:
				if ch.APIKeyQuery == "" {
					ch.APIKeyQuery = "api_key"
				}
			}
		}
		if ch.AuthType == ChannelAuthCodex {
			if strings.TrimSpace(ch.BaseURL) == "" {
				ch.BaseURL = CodexBaseURL
			}
			if ch.CodexAuth != nil {
				ch.CodexAuth.Normalize()
			}
			if len(ch.Models) == 0 {
				ch.Models = []string{"*"}
			}
		}
		id := strings.TrimSpace(ch.ID)
		if id == "" || isLegacyAutoID(id) {
			for {
				candidate := fmt.Sprintf("#%d", nextID)
				nextID++
				if !used[candidate] {
					ch.ID = candidate
					break
				}
			}
		}
		ch.ID = strings.TrimSpace(ch.ID)
		used[ch.ID] = true
		if ch.Enabled == nil {
			enabled := true
			ch.Enabled = &enabled
		}
		if ch.Headers == nil {
			ch.Headers = map[string]string{}
		}
		if ch.ModelMappings == nil {
			ch.ModelMappings = map[string]string{}
		}
		if ch.HealthCheck != nil {
			if ch.HealthCheck.IntervalSeconds <= 0 {
				ch.HealthCheck.IntervalSeconds = c.HealthCheck.IntervalSeconds
			}
			if ch.HealthCheck.TimeoutSeconds <= 0 {
				ch.HealthCheck.TimeoutSeconds = c.HealthCheck.TimeoutSeconds
			}
			if strings.TrimSpace(ch.HealthCheck.Path) == "" {
				ch.HealthCheck.Path = c.HealthCheck.Path
			}
			if ch.HealthCheck.FailureThreshold <= 0 {
				ch.HealthCheck.FailureThreshold = c.HealthCheck.FailureThreshold
			}
			if ch.HealthCheck.SuccessThreshold <= 0 {
				ch.HealthCheck.SuccessThreshold = c.HealthCheck.SuccessThreshold
			}
		}
	}
	if c.FallbackModels == nil {
		c.FallbackModels = map[string]string{}
	}
	if c.UsageMaxRecords <= 0 {
		c.UsageMaxRecords = 500
	}
	c.CodexClientVersion = NormalizeCodexClientVersion(c.CodexClientVersion)
	c.CodexClientVersionSynced = NormalizeCodexClientVersion(c.CodexClientVersionSynced)
	if c.CodexVersionAutoSync == nil {
		enabled := true
		c.CodexVersionAutoSync = &enabled
	}
	if c.ModelMappings == nil {
		c.ModelMappings = []ModelMapping{}
	}
	for i := range c.ModelMappings {
		m := &c.ModelMappings[i]
		m.PlatformModel = strings.TrimSpace(m.PlatformModel)
		m.UpstreamModel = strings.TrimSpace(m.UpstreamModel)
		m.ChannelID = strings.TrimSpace(m.ChannelID)
	}
	if c.Tokens == nil {
		c.Tokens = []Token{}
	}
	for i := range c.Tokens {
		t := &c.Tokens[i]
		t.ID = strings.TrimSpace(t.ID)
		t.Name = strings.TrimSpace(t.Name)
		if t.ID == "" {
			t.ID = t.Name
		}
		if t.Enabled == nil {
			enabled := true
			t.Enabled = &enabled
		}
	}
	seenGroup := map[string]bool{}
	groups := make([]Group, 0, len(c.Groups))
	for _, g := range c.Groups {
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" || seenGroup[g.Name] {
			continue
		}
		seenGroup[g.Name] = true
		groups = append(groups, g)
	}
	if !seenGroup["default"] {
		groups = append([]Group{{Name: "default"}}, groups...)
	}
	c.Groups = groups
}

func (c *Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return errors.New("listen_addr cannot be empty")
	}
	seen := map[string]bool{}
	for _, ch := range c.Channels {
		if strings.TrimSpace(ch.ID) == "" {
			return errors.New("every channel needs an id")
		}
		if strings.TrimSpace(ch.BaseURL) == "" {
			return errors.New("channel " + ch.ID + " needs base_url")
		}
		switch ch.AuthType {
		case "", ChannelAuthNone:
		case ChannelAuthAPIKey:
			switch ch.APIKeyPlacement {
			case "", APIKeyPlacementBearer, APIKeyPlacementAuthorization:
			case APIKeyPlacementHeader:
				if strings.TrimSpace(ch.APIKeyHeader) == "" {
					return errors.New("channel " + ch.ID + " needs api_key_header")
				}
			case APIKeyPlacementQuery:
				if strings.TrimSpace(ch.APIKeyQuery) == "" {
					return errors.New("channel " + ch.ID + " needs api_key_query")
				}
			default:
				return errors.New("channel " + ch.ID + " has unsupported api_key_placement: " + ch.APIKeyPlacement)
			}
		case ChannelAuthCodex:
			if ch.CodexAuth == nil || (strings.TrimSpace(ch.CodexAuth.AccessToken) == "" && strings.TrimSpace(ch.CodexAuth.RefreshToken) == "") {
				return errors.New("channel " + ch.ID + " needs codex access_token or refresh_token")
			}
		default:
			return errors.New("channel " + ch.ID + " has unsupported auth_type: " + ch.AuthType)
		}
		if seen[ch.ID] {
			return errors.New("duplicate channel id: " + ch.ID)
		}
		seen[ch.ID] = true
	}
	for i, m := range c.ModelMappings {
		if m.PlatformModel == "" || m.UpstreamModel == "" {
			return errors.New("model mapping requires platform_model and upstream_model")
		}
		_ = i
	}
	return nil
}

func (c *Channel) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c *Channel) IsCodexAuth() bool {
	return strings.EqualFold(strings.TrimSpace(c.AuthType), ChannelAuthCodex)
}

func (c *Channel) IsNoAuth() bool {
	return strings.EqualFold(strings.TrimSpace(c.AuthType), ChannelAuthNone)
}

// ApplyAPIKeyAuthorization applies the configured API-key transport while
// preserving the legacy default of "Authorization: Bearer <key>".
func (c *Channel) ApplyAPIKeyAuthorization(req *http.Request) {
	if c == nil || req == nil || req.URL == nil || c.IsNoAuth() || c.IsCodexAuth() {
		return
	}
	key := strings.TrimSpace(c.APIKey)
	if key == "" {
		return
	}
	prefixValue := func() string {
		prefix := strings.TrimSpace(c.APIKeyPrefix)
		if prefix == "" {
			return key
		}
		return prefix + " " + key
	}
	switch strings.ToLower(strings.TrimSpace(c.APIKeyPlacement)) {
	case APIKeyPlacementAuthorization:
		req.Header.Set("Authorization", prefixValue())
	case APIKeyPlacementHeader:
		header := strings.TrimSpace(c.APIKeyHeader)
		if header == "" {
			header = "X-API-Key"
		}
		req.Header.Set(header, prefixValue())
	case APIKeyPlacementQuery:
		name := strings.TrimSpace(c.APIKeyQuery)
		if name == "" {
			name = "api_key"
		}
		query := req.URL.Query()
		query.Set(name, key)
		req.URL.RawQuery = query.Encode()
	default:
		prefix := strings.TrimSpace(c.APIKeyPrefix)
		if prefix == "" {
			prefix = "Bearer"
		}
		req.Header.Set("Authorization", prefix+" "+key)
	}
}

func (t *Token) IsEnabled() bool {
	return t.Enabled == nil || *t.Enabled
}

func (c *Channel) EffectiveHealth(global HealthCheck) HealthCheck {
	if c.HealthCheck == nil {
		return global
	}
	return *c.HealthCheck
}

func nextChannelNumber(existing map[string]bool) int {
	max := 0
	for id := range existing {
		id = strings.TrimSpace(id)
		if strings.HasPrefix(id, "#") {
			if n, err := strconv.Atoi(strings.TrimPrefix(id, "#")); err == nil && n > max {
				max = n
			}
		}
	}
	return max + 1
}

func isLegacyAutoID(id string) bool {
	if !strings.HasPrefix(id, "ch_") {
		return false
	}
	hexPart := strings.TrimPrefix(id, "ch_")
	if len(hexPart) != 8 {
		return false
	}
	for _, r := range hexPart {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

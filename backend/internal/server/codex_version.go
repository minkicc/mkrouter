package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/minkicc/mkrouter/backend/internal/config"
)

const (
	codexVersionSyncInterval   = 6 * time.Hour
	codexVersionSyncTimeout    = 30 * time.Second
	codexVersionSyncStartDelay = 5 * time.Second
	codexVersionRepo           = "openai/codex"
)

var (
	codexVersionLatestURL = "https://api.github.com/repos/" + codexVersionRepo + "/releases/latest"
	codexVersionListURL   = "https://api.github.com/repos/" + codexVersionRepo + "/releases?per_page=30"
	// Kept apart from the proxy client: release lookups must fail fast and
	// must never disturb upstream request budgeting.
	codexVersionClient = &http.Client{Timeout: codexVersionSyncTimeout}
)

type codexRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Start launches the background maintenance loops that need the full server
// (config persistence, engine replacement, and logging).
func (s *Server) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		timer := time.NewTimer(codexVersionSyncStartDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.syncCodexClientVersion(ctx)
		ticker := time.NewTicker(codexVersionSyncInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.syncCodexClientVersion(ctx)
			}
		}
	}()
}

// syncCodexClientVersion refreshes the advertised Codex client version from the
// upstream release feed. Failures only leave the previous, working version in
// place, so an offline machine keeps routing traffic.
func (s *Server) syncCodexClientVersion(ctx context.Context) {
	if s == nil || s.engine == nil {
		return
	}
	if !s.engine.Config().CodexVersionAutoSyncEnabled() {
		return
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, codexVersionSyncTimeout)
	defer cancel()
	latest, err := fetchLatestStableCodexClientVersion(timeoutCtx)
	if err != nil {
		s.logf("synchronize Codex client version: %v", err)
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	live := s.engine.Config()
	if !live.CodexVersionAutoSyncEnabled() {
		return
	}
	if synced := config.NormalizeCodexClientVersion(live.CodexClientVersionSynced); synced != "" &&
		config.CompareCodexClientVersions(latest, synced) <= 0 {
		return
	}
	live.CodexClientVersionSynced = latest
	if err := s.engine.ReplaceConfig(live); err != nil {
		s.logf("apply synchronized Codex client version: %v", err)
		return
	}
	if strings.TrimSpace(s.cfgPath) != "" {
		if err := s.engine.Config().Save(s.cfgPath); err != nil {
			s.logf("persist synchronized Codex client version: %v", err)
			return
		}
	}
	s.logf("synchronized Codex client version to %s", latest)
}

func (s *Server) logf(format string, args ...any) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Printf(format, args...)
}

func fetchLatestStableCodexClientVersion(ctx context.Context) (string, error) {
	// Prefer the dedicated latest-release endpoint, then fall back to the
	// release list so a rejected or missing "latest" tag is recoverable.
	if release, err := fetchCodexRelease(ctx, codexVersionLatestURL); err == nil {
		if version := stableCodexReleaseVersion([]codexRelease{release}); version != "" {
			return version, nil
		}
	}
	releases, err := fetchCodexReleases(ctx, codexVersionListURL)
	if err != nil {
		return "", err
	}
	version := stableCodexReleaseVersion(releases)
	if version == "" {
		return "", fmt.Errorf("no stable %s release found", config.CodexClientVersionTagPrefix)
	}
	return version, nil
}

func fetchCodexRelease(ctx context.Context, endpoint string) (codexRelease, error) {
	var release codexRelease
	if err := fetchCodexJSON(ctx, endpoint, &release); err != nil {
		return codexRelease{}, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return codexRelease{}, fmt.Errorf("release response from %s is missing tag_name", endpoint)
	}
	return release, nil
}

func fetchCodexReleases(ctx context.Context, endpoint string) ([]codexRelease, error) {
	var releases []codexRelease
	if err := fetchCodexJSON(ctx, endpoint, &releases); err != nil {
		return nil, err
	}
	return releases, nil
}

func fetchCodexJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mkrouter-codex-version-sync")
	resp, err := codexVersionClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("release feed returned status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

// stableCodexReleaseVersion picks the highest stable release version and
// ignores drafts, prereleases, and unrelated tags.
func stableCodexReleaseVersion(releases []codexRelease) string {
	latest := ""
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		tag := strings.TrimSpace(release.TagName)
		if !strings.HasPrefix(tag, config.CodexClientVersionTagPrefix) {
			continue
		}
		version := config.NormalizeCodexClientVersion(strings.TrimPrefix(tag, config.CodexClientVersionTagPrefix))
		if version == "" {
			continue
		}
		if latest == "" || config.CompareCodexClientVersions(version, latest) > 0 {
			latest = version
		}
	}
	return latest
}

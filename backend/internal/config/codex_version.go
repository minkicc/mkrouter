package config

import (
	"regexp"
	"strconv"
	"strings"
)

// CodexClientVersionFallback is the compiled Codex client version. It is only
// used when neither a synchronized nor a manually configured version exists,
// so an unreachable release feed never blocks Codex traffic.
const CodexClientVersionFallback = "0.155.1"

// CodexClientVersionTagPrefix is the tag prefix OpenAI uses for stable
// Codex CLI releases, for example "rust-v0.155.1".
const CodexClientVersionTagPrefix = "rust-v"

var codexClientVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`)

// NormalizeCodexClientVersion validates an official Codex version string and
// returns an empty string when the value is not usable.
func NormalizeCodexClientVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 || !codexClientVersionPattern.MatchString(value) {
		return ""
	}
	return value
}

// CompareCodexClientVersions orders dotted numeric versions.
func CompareCodexClientVersions(left, right string) int {
	leftParts := codexVersionParts(left)
	rightParts := codexVersionParts(right)
	for i := 0; i < len(leftParts); i++ {
		if leftParts[i] < rightParts[i] {
			return -1
		}
		if leftParts[i] > rightParts[i] {
			return 1
		}
	}
	return 0
}

func codexVersionParts(value string) [4]int {
	var parts [4]int
	for index, item := range strings.Split(value, ".") {
		if index >= len(parts) {
			break
		}
		parts[index], _ = strconv.Atoi(item)
	}
	return parts
}

// EffectiveCodexClientVersion is the single version resolver used by model
// discovery, health probes, and Codex inference. A manually configured version
// wins over the synchronized release, which wins over the compiled fallback.
func (c *Config) EffectiveCodexClientVersion() string {
	if c != nil {
		if version := NormalizeCodexClientVersion(c.CodexClientVersion); version != "" {
			return version
		}
		if version := NormalizeCodexClientVersion(c.CodexClientVersionSynced); version != "" {
			return version
		}
	}
	return CodexClientVersionFallback
}

// CodexVersionAutoSyncEnabled reports whether the release synchronizer should
// run. Absent configuration keeps the default of enabled.
func (c *Config) CodexVersionAutoSyncEnabled() bool {
	if c == nil || c.CodexVersionAutoSync == nil {
		return true
	}
	return *c.CodexVersionAutoSync
}

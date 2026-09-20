package routing

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type Status string

const (
	StatusUnknown   Status = "unknown"
	StatusHealthy   Status = "healthy"
	StatusUnhealthy Status = "unhealthy"
	StatusDisabled  Status = "disabled"
)

type Route struct {
	ID            string
	Name          string
	Models        []string
	ModelMappings map[string]string
	Priority      int
	Group         string
	Weight        int
	Price         float64
	Enabled       bool
	MaxRetries    int
	Status        Status
	ResponseTime  time.Duration
	Cooldown      bool
}

type ModelMapping struct {
	PlatformModel string
	UpstreamModel string
	ChannelID     string
	Enabled       *bool
}

func (m ModelMapping) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

type Group struct {
	Name     string
	Priority int
}

type Selection struct {
	Route         Route
	UpstreamModel string
}

// ResolveUpstreamModel applies channel-scoped mappings first. A mapping
// without a channel id only applies to channels that advertise its upstream
// model, so routing never needs to infer a provider from the channel URL.
func ResolveUpstreamModel(route *Route, requested string, mappings []ModelMapping) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}
	if route != nil {
		if mapped := strings.TrimSpace(route.ModelMappings[requested]); mapped != "" {
			return mapped, true
		}
	}
	for _, mapping := range mappings {
		if !mapping.IsEnabled() || strings.TrimSpace(mapping.PlatformModel) != requested {
			continue
		}
		upstream := strings.TrimSpace(mapping.UpstreamModel)
		channelID := strings.TrimSpace(mapping.ChannelID)
		if channelID != "" {
			if route == nil || channelID != route.ID {
				continue
			}
		} else if route == nil || !supportsModel(route.Models, upstream) {
			continue
		}
		return upstream, true
	}
	if route != nil && supportsModel(route.Models, requested) {
		return requested, true
	}
	return "", false
}

func supportsModel(models []string, requested string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return false
	}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == requested || model == "*" {
			return true
		}
	}
	return false
}

type candidate struct {
	route         Route
	upstreamModel string
	groupPriority int
}

func Select(requested string, routes []Route, mappings []ModelMapping, groups []Group, excluded map[string]bool) (*Selection, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return nil, errModelRequired
	}
	groupPriority := make(map[string]int, len(groups))
	for _, group := range groups {
		groupPriority[strings.TrimSpace(group.Name)] = group.Priority
	}
	candidates := make([]candidate, 0, len(routes))
	for _, route := range routes {
		if !route.Enabled || route.Status == StatusDisabled || route.Cooldown || excluded != nil && excluded[route.ID] {
			continue
		}
		upstream, ok := ResolveUpstreamModel(&route, requested, mappings)
		if !ok {
			continue
		}
		candidates = append(candidates, candidate{route: route, upstreamModel: upstream, groupPriority: groupPriority[strings.TrimSpace(route.Group)]})
	}
	if len(candidates) == 0 {
		return nil, errNoRoute
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidateLess(candidates[i], candidates[j]) })
	best := candidates[0]
	return &Selection{Route: best.route, UpstreamModel: best.upstreamModel}, nil
}

func candidateLess(left, right candidate) bool {
	if a, b := healthRank(left.route.Status), healthRank(right.route.Status); a != b {
		return a < b
	}
	if left.groupPriority != right.groupPriority {
		return left.groupPriority > right.groupPriority
	}
	if left.route.Priority != right.route.Priority {
		return left.route.Priority > right.route.Priority
	}
	if left.route.Price != right.route.Price {
		return left.route.Price < right.route.Price
	}
	if left.route.ResponseTime != right.route.ResponseTime {
		return left.route.ResponseTime < right.route.ResponseTime
	}
	if left.route.Weight != right.route.Weight {
		return left.route.Weight > right.route.Weight
	}
	return left.route.ID < right.route.ID
}

func healthRank(status Status) int {
	switch status {
	case StatusHealthy:
		return 0
	case StatusUnknown:
		return 1
	case StatusUnhealthy:
		return 2
	case StatusDisabled:
		return 3
	default:
		return 4
	}
}

func IsRetryableStatus(status int) bool {
	return status == 401 || status == 403 || status == 404 || status == 408 || status == 425 || status == 429 || status >= 500
}

// IsRetryableOnSameRoute reports errors where repeating the request against
// the same upstream is useful. Authentication, permission, and missing-route
// errors should move to another channel immediately instead.
func IsRetryableOnSameRoute(status int) bool {
	return status == 408 || status == 425 || status == 429 || status >= 500
}

func ResponseFailed(body []byte) bool {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return false
	}
	if looksLikeSSE(body) {
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload != "" && payload != "[DONE]" && jsonEventFailed([]byte(payload)) {
				return true
			}
		}
		return false
	}
	if jsonEventFailed(body) {
		return true
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	response, _ := payload["response"].(map[string]any)
	status, _ := response["status"].(string)
	return status == "failed" || status == "incomplete"
}

func IsRetryableError(status int, body []byte) bool {
	return IsRetryableStatus(status) || ResponseFailed(body)
}

// ResponseFailureDetail extracts a short human-readable explanation from an
// upstream failure payload (either a JSON body or an SSE stream) so a channel
// cooldown can report why it was taken out of rotation instead of a generic
// message. It returns "" when the payload carries no useful detail.
func ResponseFailureDetail(body []byte) string {
	for _, payload := range failurePayloads(body) {
		if detail := failureDetailFromPayload(payload); detail != "" {
			return truncateReason(detail, 200)
		}
	}
	return ""
}

// failurePayloads returns the JSON documents worth inspecting, newest last, so
// the most recent failure wins over an earlier partial event.
func failurePayloads(body []byte) [][]byte {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil
	}
	if !looksLikeSSE(body) {
		return [][]byte{body}
	}
	payloads := make([][]byte, 0, 4)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		payloads = append(payloads, []byte(payload))
	}
	return payloads
}

func failureDetailFromPayload(data []byte) string {
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	// "type" is an error code inside an error object, but only an event name at
	// the top level, so it counts as a code for the former and never the latter.
	if detail := failureDetailFromObject(nestedObject(payload, "error"), true); detail != "" {
		return detail
	}
	if detail := failureDetailFromObject(payload, false); detail != "" {
		return detail
	}
	if response, ok := payload["response"].(map[string]any); ok {
		if detail := failureDetailFromObject(nestedObject(response, "error"), true); detail != "" {
			return detail
		}
		return failureDetailFromObject(response, false)
	}
	return ""
}

func failureDetailFromObject(payload map[string]any, typeIsCode bool) string {
	if payload == nil {
		return ""
	}
	code := jsonText(payload["code"])
	if code == "" && typeIsCode {
		code = jsonText(payload["type"])
	}
	message := jsonText(payload["message"])
	switch {
	case code != "" && message != "" && code != message:
		return code + ": " + message
	case message != "":
		return message
	case code != "":
		return code
	}
	if detail := jsonText(payload["detail"]); detail != "" {
		return detail
	}
	if reason := jsonText(nestedObject(payload, "incomplete_details")["reason"]); reason != "" {
		return "incomplete: " + reason
	}
	if status := jsonText(payload["status"]); status == "failed" || status == "incomplete" {
		return status
	}
	return ""
}

func nestedObject(payload map[string]any, key string) map[string]any {
	if payload == nil {
		return nil
	}
	value, _ := payload[key].(map[string]any)
	return value
}

func jsonText(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func truncateReason(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit])
}

func looksLikeSSE(body []byte) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			return true
		}
	}
	return false
}

func jsonEventFailed(data []byte) bool {
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return false
	}
	typ, _ := payload["type"].(string)
	if typ == "response.failed" || typ == "error" || typ == "response.incomplete" {
		return true
	}
	if response, ok := payload["response"].(map[string]any); ok {
		status, _ := response["status"].(string)
		if status == "failed" || status == "incomplete" {
			return true
		}
	}
	_, hasError := payload["error"]
	return hasError
}

type routingError string

func (e routingError) Error() string { return string(e) }

const (
	errModelRequired routingError = "model is required"
	errNoRoute       routingError = "no available route for model"
)

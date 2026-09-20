package routing

import "testing"

func TestResponseFailureDetailFromSSE(t *testing.T) {
	body := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached for gpt-5.6-sol"}}}` + "\n\n"
	if got := ResponseFailureDetail([]byte(body)); got != "rate_limit_exceeded: Rate limit reached for gpt-5.6-sol" {
		t.Fatalf("detail = %q", got)
	}
}

func TestResponseFailureDetailFromJSONError(t *testing.T) {
	body := `{"error":{"type":"invalid_request_error","message":"model not found"}}`
	if got := ResponseFailureDetail([]byte(body)); got != "invalid_request_error: model not found" {
		t.Fatalf("detail = %q", got)
	}
}

func TestResponseFailureDetailFromIncompleteResponse(t *testing.T) {
	body := `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`
	if got := ResponseFailureDetail([]byte(body)); got != "incomplete: max_output_tokens" {
		t.Fatalf("detail = %q", got)
	}
}

func TestResponseFailureDetailIsEmptyForHealthyPayload(t *testing.T) {
	body := `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`
	if got := ResponseFailureDetail([]byte(body)); got != "" {
		t.Fatalf("detail = %q, want empty", got)
	}
}

func TestResponseFailureDetailTruncatesLongMessages(t *testing.T) {
	long := make([]rune, 500)
	for i := range long {
		long[i] = 'x'
	}
	body := `{"error":{"message":"` + string(long) + `"}}`
	if got := ResponseFailureDetail([]byte(body)); len([]rune(got)) != 200 {
		t.Fatalf("detail length = %d, want 200", len([]rune(got)))
	}
}

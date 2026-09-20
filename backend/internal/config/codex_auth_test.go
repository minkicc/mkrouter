package config

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseCodexAuthJSONNestedTokensAndJWTClaims(t *testing.T) {
	token := testCodexJWT(t, map[string]any{
		"sub":   "user-1",
		"email": "codex@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-1",
		},
	})
	raw, err := json.Marshal(map[string]any{
		"tokens": map[string]any{
			"access_token":  token,
			"refresh_token": "refresh-1",
			"id_token":      "id-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	auth, err := ParseCodexAuthJSON(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccessToken != token || auth.RefreshToken != "refresh-1" {
		t.Fatalf("tokens not parsed: %+v", auth)
	}
	if auth.AccountID != "account-1" || auth.UserID != "user-1" || auth.Email != "codex@example.com" {
		t.Fatalf("JWT claims not enriched: %+v", auth)
	}
	if auth.ClientID != CodexClientID {
		t.Fatalf("client id = %q", auth.ClientID)
	}
	if auth.UpdatedAt <= 0 {
		t.Fatalf("updated_at = %d", auth.UpdatedAt)
	}
	if _, ok := auth.ExpiresAtTime(); !ok {
		t.Fatalf("expiry not parsed: %+v", auth)
	}
}

func TestParseCodexAuthJSONPreservesCredentialRevision(t *testing.T) {
	auth, err := ParseCodexAuthJSON(`{"access_token":"access","updated_at":42}`)
	if err != nil {
		t.Fatal(err)
	}
	if auth.UpdatedAt != 42 {
		t.Fatalf("updated_at = %d, want 42", auth.UpdatedAt)
	}
}

func TestParseCodexAuthJSONRejectsRefreshOnly(t *testing.T) {
	if _, err := ParseCodexAuthJSON(`{"refresh_token":"refresh-only"}`); err == nil {
		t.Fatal("expected refresh-only JSON to require the dedicated authorization method")
	}
}

func TestParseCodexAuthJSONRejectsMissingTokens(t *testing.T) {
	if _, err := ParseCodexAuthJSON(`{"email":"codex@example.com"}`); err == nil {
		t.Fatal("expected missing token error")
	}
}

func TestParseCodexAuthInputAcceptsRawAccessToken(t *testing.T) {
	token := testCodexJWT(t, map[string]any{
		"sub":       "user-raw",
		"email":     "raw@example.com",
		"exp":       time.Now().Add(time.Hour).Unix(),
		"client_id": CodexClientID,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-raw",
		},
	})
	auth, err := ParseCodexAuthInput(token)
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccessToken != token || auth.AuthMode != CodexAuthModeAccessToken {
		t.Fatalf("raw auth = %+v", auth)
	}
	if auth.AccountID != "account-raw" || auth.Email != "raw@example.com" {
		t.Fatalf("raw JWT claims not enriched: %+v", auth)
	}
}

func TestParseCodexAuthInputRejectsChatGPTWebAccessToken(t *testing.T) {
	token := testCodexJWT(t, map[string]any{
		"exp":       time.Now().Add(time.Hour).Unix(),
		"client_id": "chatgpt-web-client",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-web",
		},
	})
	if _, err := ParseCodexAuthInput(token); err == nil {
		t.Fatal("expected ChatGPT web access token to be rejected")
	}
}

func TestParseCodexAuthInputRejectsSessionCredentialAndOAuthCallback(t *testing.T) {
	for _, input := range []string{
		"__Secure-next-auth.session-token=session-value",
		"sess-" + strings.Repeat("x", 90),
		"http://localhost:1455/auth/callback?code=ac_code&state=state",
		"ac_authorization-code",
	} {
		if _, err := ParseCodexAuthInput(input); err == nil {
			t.Fatalf("expected %q to be rejected", input)
		}
	}
}

func TestCodexAccessTokenAccountOverridesImportedAccount(t *testing.T) {
	token := testCodexJWT(t, map[string]any{
		"exp":       time.Now().Add(time.Hour).Unix(),
		"client_id": CodexClientID,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "account-from-token",
		},
	})
	auth, err := ParseCodexAuthJSON(`{"access_token":"` + token + `","account_id":"wrong@example.com"}`)
	if err != nil {
		t.Fatal(err)
	}
	if auth.AccountID != "account-from-token" {
		t.Fatalf("account id = %q", auth.AccountID)
	}
}

func TestCodexPersonalAccessTokenModeIsInferred(t *testing.T) {
	auth := &CodexAuth{AccessToken: "at-personal"}
	auth.Normalize()
	if auth.AuthMode != CodexAuthModePersonalAccessToken {
		t.Fatalf("auth mode = %q", auth.AuthMode)
	}
}

func TestLegacyCodexCredentialsRemainAuthJSONMode(t *testing.T) {
	auth := &CodexAuth{
		AccessToken:  "legacy-access",
		RefreshToken: "legacy-refresh",
	}
	auth.Normalize()
	if auth.AuthMode != CodexAuthModeAuthJSON {
		t.Fatalf("auth mode = %q", auth.AuthMode)
	}
}

func testCodexJWT(t *testing.T, claims map[string]any) string {
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

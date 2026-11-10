package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// Anthropic rejects models newer than the Claude Code version on the wire, so the
// stabilized fingerprint must follow the newest real client across credentials.
func TestApplyClaudeHeaders_SendsNewestClaudeCLIVersionSeenAcrossCredentials(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	stabilize := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{StabilizeDeviceProfile: &stabilize}}
	authA := &cliproxyauth.Auth{ID: "auth-newest-a", Attributes: map[string]string{"api_key": "key-newest-a", "cloak_mode": "always"}}
	authB := &cliproxyauth.Auth{ID: "auth-newest-b", Attributes: map[string]string{"api_key": "key-newest-b", "cloak_mode": "always"}}
	send := func(auth *cliproxyauth.Auth, userAgent string, confirmed bool) http.Header {
		t.Helper()
		req := newClaudeHeaderTestRequest(t, http.Header{
			"User-Agent":                  []string{userAgent},
			"X-Stainless-Package-Version": []string{"0.120.0"},
			"X-Stainless-Runtime-Version": []string{"v26.4.0"},
		})
		if confirmed { // the executor records confirmed clients at detection
			helps.ObserveClaudeCLIUserAgent(userAgent, cfg)
		}
		if errApply := applyClaudeHeaders(req, auth, auth.Attributes["api_key"], false, nil, nil, cfg, nil, confirmed); errApply != nil {
			t.Fatalf("applyClaudeHeaders() error = %v", errApply)
		}
		return req.Header
	}

	if got := send(authA, "lobe-chat/1.0", false).Get("User-Agent"); got != "claude-cli/2.1.321 (external, cli)" {
		t.Fatalf("before any Claude Code client, User-Agent = %q, want the built-in default", got)
	}
	if got := send(authA, "claude-cli/2.1.321 (external, cli)", true).Get("User-Agent"); got != "claude-cli/2.1.321 (external, cli)" {
		t.Fatalf("client on 2.1.321: User-Agent = %q", got)
	}
	if got := send(authB, "claude-cli/2.1.330 (external, cli)", true).Get("User-Agent"); got != "claude-cli/2.1.330 (external, cli)" {
		t.Fatalf("client on 2.1.330: User-Agent = %q", got)
	}
	// An older client on another credential, and a foreign client, never pull it back down.
	if got := send(authA, "claude-cli/2.1.299 (external, cli)", true).Get("User-Agent"); got != "claude-cli/2.1.330 (external, cli)" {
		t.Fatalf("older client after 2.1.330: User-Agent = %q", got)
	}
	if got := send(authA, "lobe-chat/1.0", false).Get("User-Agent"); got != "claude-cli/2.1.330 (external, cli)" {
		t.Fatalf("foreign client after 2.1.330: User-Agent = %q", got)
	}
	// Foreign clients claiming a newer version are not recorded.
	send(authB, "claude-cli/2.1.999 (external, cli)", false)
	if got := send(authB, "lobe-chat/1.0", false).Get("User-Agent"); got != "claude-cli/2.1.330 (external, cli)" {
		t.Fatalf("after an unconfirmed 2.1.999 claim: User-Agent = %q", got)
	}
}

func TestApplyClaudeHeaders_ConfiguredUserAgentIsAFloor(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	stabilize := true
	cfg := &config.Config{ClaudeHeaderDefaults: config.ClaudeHeaderDefaults{
		UserAgent:              "claude-cli/2.1.340 (external, cli)",
		StabilizeDeviceProfile: &stabilize,
	}}
	auth := &cliproxyauth.Auth{ID: "auth-floor", Attributes: map[string]string{"api_key": "key-floor"}}
	send := func(userAgent string) string {
		t.Helper()
		req := newClaudeHeaderTestRequest(t, http.Header{"User-Agent": []string{userAgent}})
		helps.ObserveClaudeCLIUserAgent(userAgent, cfg)
		if errApply := applyClaudeHeaders(req, auth, "key-floor", false, nil, nil, cfg, nil, true); errApply != nil {
			t.Fatalf("applyClaudeHeaders() error = %v", errApply)
		}
		return req.Header.Get("User-Agent")
	}
	if got := send("claude-cli/2.1.330 (external, cli)"); got != "claude-cli/2.1.340 (external, cli)" {
		t.Fatalf("client below the configured floor: User-Agent = %q", got)
	}
	if got := send("claude-cli/2.1.350 (external, cli)"); got != "claude-cli/2.1.350 (external, cli)" {
		t.Fatalf("client above the configured floor: User-Agent = %q", got)
	}
}

// Cloaked credentials rewrite the identity, so they must learn the client's
// version too, and the billing header must agree with the User-Agent on the
// very request that introduces it.
func TestClaudeExecutor_CloakedRequestSendsConfirmedClientVersionInUserAgentAndBilling(t *testing.T) {
	resetClaudeDeviceProfileCache()
	t.Cleanup(resetClaudeDeviceProfileCache)
	var upstreamBody []byte
	var upstreamHeaders http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	const userID = `{"device_id":"0000000000000000000000000000000000000000000000000000000000000000","account_uuid":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","session_id":"11111111-2222-4333-8444-555555555555"}`
	payload := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":` + fmt.Sprintf("%q", userID) + `}}`)
	auth := &cliproxyauth.Auth{
		ID: "auth-cloaked-newest",
		Attributes: map[string]string{
			"api_key":    "sk-ant-oat-cloaked-newest",
			"base_url":   server.URL,
			"cloak_mode": "always",
		},
		Metadata: claudeOAuthTestMetadata(),
	}
	headers := http.Header{
		"User-Agent":     {"claude-cli/2.1.340 (external, cli)"},
		"X-App":          {"cli"},
		"Anthropic-Beta": {"claude-code-20250219,interleaved-thinking-2025-05-14"},
	}
	_, errExecute := NewClaudeExecutor(&config.Config{}).Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: headers})
	if errExecute != nil {
		t.Fatalf("Execute() error = %v", errExecute)
	}
	if got := upstreamHeaders.Get("User-Agent"); got != "claude-cli/2.1.340 (external, cli)" {
		t.Fatalf("User-Agent = %q, want the confirmed client's 2.1.340", got)
	}
	if billing := ojson.GetBytes(upstreamBody, "system.0.text").String(); !strings.Contains(billing, "cc_version=2.1.340.") {
		t.Fatalf("billing header = %q, want cc_version 2.1.340", billing)
	}

	// A foreign client on another credential is cloaked and takes the learned version.
	foreignAuth := &cliproxyauth.Auth{
		ID:         "auth-cloaked-foreign",
		Attributes: map[string]string{"api_key": "sk-ant-oat-cloaked-foreign", "base_url": server.URL, "cloak_mode": "always"},
		Metadata:   claudeOAuthTestMetadata(),
	}
	_, errExecute = NewClaudeExecutor(&config.Config{}).Execute(context.Background(), foreignAuth, cliproxyexecutor.Request{
		Model:   "claude-opus-5",
		Payload: payload,
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: http.Header{"User-Agent": {"lobe-chat/1.0"}}})
	if errExecute != nil {
		t.Fatalf("foreign Execute() error = %v", errExecute)
	}
	if got := upstreamHeaders.Get("User-Agent"); got != "claude-cli/2.1.340 (external, cli)" {
		t.Fatalf("foreign User-Agent = %q, want the learned 2.1.340", got)
	}
	if billing := ojson.GetBytes(upstreamBody, "system.0.text").String(); !strings.Contains(billing, "cc_version=2.1.340.") {
		t.Fatalf("foreign billing header = %q, want cc_version 2.1.340", billing)
	}
}

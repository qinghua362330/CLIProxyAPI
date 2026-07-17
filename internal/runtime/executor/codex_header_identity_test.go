package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestFinalizeCodexRequestHeadersIDMatrix(t *testing.T) {
	tests := []struct {
		name        string
		target      http.Header
		source      http.Header
		promptCache string
		wantSession string
		wantThread  string
		wantClient  string
	}{
		{
			name: "distinct explicit IDs and client request ID",
			source: http.Header{
				"Session-Id":          []string{"session-a"},
				"Thread-Id":           []string{"thread-a"},
				"X-Client-Request-Id": []string{"request-a"},
			},
			promptCache: "cache-a",
			wantSession: "session-a",
			wantThread:  "thread-a",
			wantClient:  "request-a",
		},
		{
			name:        "custom path does not synthesize missing client request ID",
			source:      http.Header{"session-id": []string{"session-b"}, "thread-id": []string{"thread-b"}},
			wantSession: "session-b",
			wantThread:  "thread-b",
		},
		{
			name:        "session alone does not consume prompt fallback",
			source:      http.Header{"Session_id": []string{"session-c"}},
			promptCache: "cache-c",
			wantSession: "session-c",
		},
		{
			name:        "custom thread alone remains independent",
			source:      http.Header{"thread_id": []string{"thread-d"}},
			promptCache: "cache-d",
			wantThread:  "thread-d",
		},
		{
			name:        "custom lone prompt cache key stays body-only",
			promptCache: "cache-e",
		},
		{
			name:        "target overrides source per ID",
			target:      http.Header{"Session_id": []string{"target-session"}},
			source:      http.Header{"session-id": []string{"source-session"}, "thread-id": []string{"source-thread"}},
			wantSession: "target-session",
			wantThread:  "source-thread",
		},
		{
			name:       "explicit client request ID remains without session or thread",
			source:     http.Header{"X-Client-Request-Id": []string{"request-g"}},
			wantClient: "request-g",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.target.Clone()
			if target == nil {
				target = http.Header{}
			}
			finalizeCodexRequestHeaders(target, tc.source, tc.promptCache, false, false, false, "")

			if got := codexSessionHeaderValue(target); got != tc.wantSession {
				t.Fatalf("session-id = %q, want %q", got, tc.wantSession)
			}
			if got := codexThreadHeaderValue(target); got != tc.wantThread {
				t.Fatalf("thread-id = %q, want %q", got, tc.wantThread)
			}
			if got := codexClientRequestIDValue(target); got != tc.wantClient {
				t.Fatalf("x-client-request-id = %q, want %q", got, tc.wantClient)
			}
			for _, alias := range []string{"Session_id", "session_id", "Thread_id", "thread_id", "Conversation_id"} {
				if _, ok := target[alias]; ok {
					t.Fatalf("legacy alias %q remains in %#v", alias, target)
				}
			}
		})
	}
}

func TestFinalizeCodexRequestHeadersManagedOAuthIdentity(t *testing.T) {
	t.Run("client request id follows final thread", func(t *testing.T) {
		headers := http.Header{}
		source := http.Header{
			"session-id":          []string{"session-1"},
			"thread-id":           []string{"thread-1"},
			"x-client-request-id": []string{"must-not-win"},
		}

		finalizeCodexRequestHeaders(headers, source, "", true, true, true, codexUserAgent)

		if got := codexSessionHeaderValue(headers); got != "session-1" {
			t.Fatalf("session-id = %q, want session-1", got)
		}
		if got := codexThreadHeaderValue(headers); got != "thread-1" {
			t.Fatalf("thread-id = %q, want thread-1", got)
		}
		if got := codexClientRequestIDValue(headers); got != "thread-1" {
			t.Fatalf("x-client-request-id = %q, want thread-1", got)
		}
	})

	t.Run("missing identity receives uuid v7 fallback", func(t *testing.T) {
		headers := http.Header{}

		fallbackID := newCodexRequestIdentityID()
		finalizeCodexRequestHeaders(headers, nil, fallbackID, true, true, true, codexUserAgent)

		sessionID := codexSessionHeaderValue(headers)
		threadID := codexThreadHeaderValue(headers)
		if sessionID == "" || sessionID != threadID {
			t.Fatalf("fallback identity = session %q thread %q, want one shared non-empty id", sessionID, threadID)
		}
		parsed, err := uuid.Parse(threadID)
		if err != nil {
			t.Fatalf("fallback thread-id is not a UUID: %v", err)
		}
		if parsed.Version() != 7 {
			t.Fatalf("fallback UUID version = %d, want 7", parsed.Version())
		}
		if got := codexClientRequestIDValue(headers); got != threadID {
			t.Fatalf("x-client-request-id = %q, want thread-id %q", got, threadID)
		}
	})

	t.Run("custom upstream remains transparent", func(t *testing.T) {
		headers := http.Header{}

		finalizeCodexRequestHeaders(headers, nil, "cache-must-not-synthesize", false, false, false, "")

		if got := codexSessionHeaderValue(headers); got != "" {
			t.Fatalf("custom session-id = %q, want empty", got)
		}
		if got := codexThreadHeaderValue(headers); got != "" {
			t.Fatalf("custom thread-id = %q, want empty", got)
		}
		if got := codexClientRequestIDValue(headers); got != "" {
			t.Fatalf("custom x-client-request-id = %q, want empty", got)
		}
	})
}

func TestIdentityConfuseCompletesPartialIdentityWithoutPromptCache(t *testing.T) {
	headers := http.Header{
		"session-id":        []string{"raw-session"},
		"X-Codex-Window-Id": []string{"raw-session:0"},
	}
	cfg := &config.Config{
		Routing: config.RoutingConfig{SessionAffinity: true},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-1", Provider: "codex"}
	_, state := applyCodexManagedIdentityBody(cfg, auth, headers, "request-fallback", []byte(`{"model":"gpt-5.6"}`), []byte(`{"model":"gpt-5.6"}`))

	applyCodexIdentityConfuseHeaders(headers, &state)
	finalizeCodexRequestHeaders(headers, nil, "", true, true, true, codexUserAgent)

	sessionID := codexSessionHeaderValue(headers)
	threadID := codexThreadHeaderValue(headers)
	if sessionID == "raw-session" || threadID == "raw-session" {
		t.Fatalf("raw identity leaked: session %q thread %q", sessionID, threadID)
	}
	if sessionID == "" || sessionID != threadID {
		t.Fatalf("completed identity = session %q thread %q, want one shared confused id", sessionID, threadID)
	}
	if got := codexClientRequestIDValue(headers); got != threadID {
		t.Fatalf("x-client-request-id = %q, want thread-id %q", got, threadID)
	}
	if got := headerValueCaseInsensitive(headers, "X-Codex-Window-Id"); got != threadID+":0" {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", got, threadID+":0")
	}
}

func TestIdentityConfusePreservesPromptOverrideAndCompleteMetadataGraph(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{SessionAffinity: true},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-graph", Provider: "codex", Metadata: map[string]any{"access_token": "oauth-token"}}
	turnMetadata := `{"installation_id":"install-1","session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","window_id":"thread-1:7","forked_from_thread_id":"fork-1","parent_thread_id":"parent-1"}`
	payload := []byte(`{"model":"gpt-5.6","prompt_cache_key":"guardian:parent-1","client_metadata":{"x-codex-installation-id":"install-1","session_id":"session-1","thread_id":"thread-1","turn_id":"turn-1","x-codex-window-id":"thread-1:7","x-codex-parent-thread-id":"parent-1","x-codex-turn-metadata":` + strconv.Quote(turnMetadata) + `}}`)

	body, state := applyCodexIdentityConfuseBody(cfg, auth, payload, payload)
	if !state.enabled {
		t.Fatal("identity confusion was not enabled")
	}
	wantPrompt := codexIdentityConfuseUUID(auth.ID, "prompt-cache", "guardian:parent-1")
	wantSession := codexIdentityConfuseUUID(auth.ID, "session", "session-1")
	wantThread := codexIdentityConfuseUUID(auth.ID, "thread", "thread-1")
	wantParent := codexIdentityConfuseUUID(auth.ID, "thread", "parent-1")
	wantFork := codexIdentityConfuseUUID(auth.ID, "thread", "fork-1")
	wantTurn := codexIdentityConfuseUUID(auth.ID, "turn", "turn-1")
	wantInstallation := codexIdentityConfuseUUID(auth.ID, "installation", "install-1")

	assertJSONIdentity := func(path, want string) {
		t.Helper()
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, body)
		}
	}
	assertJSONIdentity("prompt_cache_key", wantPrompt)
	assertJSONIdentity("client_metadata.session_id", wantSession)
	assertJSONIdentity("client_metadata.thread_id", wantThread)
	assertJSONIdentity("client_metadata.turn_id", wantTurn)
	assertJSONIdentity("client_metadata.x-codex-window-id", wantThread+":7")
	assertJSONIdentity("client_metadata.x-codex-parent-thread-id", wantParent)
	assertJSONIdentity("client_metadata.x-codex-installation-id", wantInstallation)
	nested := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	for path, want := range map[string]string{
		"installation_id":       wantInstallation,
		"session_id":            wantSession,
		"thread_id":             wantThread,
		"turn_id":               wantTurn,
		"window_id":             wantThread + ":7",
		"forked_from_thread_id": wantFork,
		"parent_thread_id":      wantParent,
	} {
		if got := gjson.Get(nested, path).String(); got != want {
			t.Fatalf("nested %s = %q, want %q; metadata=%s", path, got, want, nested)
		}
	}

	headers := http.Header{
		"session-id":               []string{"session-1"},
		"thread-id":                []string{"thread-1"},
		"x-client-request-id":      []string{"raw-client"},
		"X-Codex-Installation-Id":  []string{"install-1"},
		"X-Codex-Window-Id":        []string{"thread-1:7"},
		"X-Codex-Parent-Thread-Id": []string{"parent-1"},
		"x-codex-turn-metadata":    []string{turnMetadata},
	}
	primeCodexRequestIdentityHeaders(headers, nil)
	applyCodexIdentityConfuseHeaders(headers, &state)
	finalizeCodexRequestHeaders(headers, nil, wantPrompt, true, true, true, codexUserAgent)

	if got := codexSessionHeaderValue(headers); got != wantSession {
		t.Fatalf("session-id = %q, want %q", got, wantSession)
	}
	if got := codexThreadHeaderValue(headers); got != wantThread {
		t.Fatalf("thread-id = %q, want %q", got, wantThread)
	}
	if got := codexClientRequestIDValue(headers); got != wantThread {
		t.Fatalf("x-client-request-id = %q, want thread %q", got, wantThread)
	}
	if got := headerValueCaseInsensitive(headers, "X-Codex-Installation-Id"); got != wantInstallation {
		t.Fatalf("X-Codex-Installation-Id = %q, want %q", got, wantInstallation)
	}
	if got := headerValueCaseInsensitive(headers, "X-Codex-Window-Id"); got != wantThread+":7" {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", got, wantThread+":7")
	}
	if got := headerValueCaseInsensitive(headers, "X-Codex-Parent-Thread-Id"); got != wantParent {
		t.Fatalf("X-Codex-Parent-Thread-Id = %q, want %q", got, wantParent)
	}
	headerTurnMetadata := headerValueCaseInsensitive(headers, "X-Codex-Turn-Metadata")
	for path, want := range map[string]string{
		"installation_id":       wantInstallation,
		"session_id":            wantSession,
		"thread_id":             wantThread,
		"turn_id":               wantTurn,
		"window_id":             wantThread + ":7",
		"forked_from_thread_id": wantFork,
		"parent_thread_id":      wantParent,
	} {
		if got := gjson.Get(headerTurnMetadata, path).String(); got != want {
			t.Fatalf("header turn metadata %s = %q, want %q; metadata=%s", path, got, want, headerTurnMetadata)
		}
	}
	firstPass := headers.Clone()
	applyCodexIdentityConfuseHeaders(headers, &state)
	if !reflect.DeepEqual(headers, firstPass) {
		t.Fatalf("identity confusion is not idempotent:\nfirst=%#v\nsecond=%#v", firstPass, headers)
	}
}

func TestFinalizeCodexRequestHeadersCompactOmitsClientRequestID(t *testing.T) {
	headers := http.Header{
		"session-id":          []string{"session-compact"},
		"thread-id":           []string{"thread-compact"},
		"x-client-request-id": []string{"must-be-removed"},
	}
	finalizeCodexRequestHeaders(headers, nil, "", true, false, true, codexUserAgent)
	if got := codexSessionHeaderValue(headers); got != "session-compact" {
		t.Fatalf("session-id = %q, want session-compact", got)
	}
	if got := codexThreadHeaderValue(headers); got != "thread-compact" {
		t.Fatalf("thread-id = %q, want thread-compact", got)
	}
	if got := codexClientRequestIDValue(headers); got != "" {
		t.Fatalf("compact x-client-request-id = %q, want absent", got)
	}
}

func TestCodexManagedIdentityUsesOneCanonicalMetadataSnapshot(t *testing.T) {
	auth := explicitOAuthAuth()
	turnMetadata := `{"installation_id":"nested-install","session_id":"nested-session","thread_id":"nested-thread","turn_id":"nested-turn","window_id":"nested-thread:7","parent_thread_id":"nested-parent"}`
	payload := []byte(`{"model":"gpt-5.6","prompt_cache_key":"guardian:parent","client_metadata":{"session_id":"flat-session","thread_id":"flat-thread","x-codex-installation-id":"flat-install","x-codex-window-id":"flat-thread:3","x-codex-turn-metadata":` + strconv.Quote(turnMetadata) + `}}`)
	sourceHeaders := http.Header{
		"session-id":               []string{"header-session"},
		"thread-id":                []string{"header-thread"},
		"x-client-request-id":      []string{"header-request"},
		"X-Codex-Installation-Id":  []string{"header-install"},
		"X-Codex-Window-Id":        []string{"header-thread:9"},
		"X-Codex-Parent-Thread-Id": []string{"header-parent"},
	}

	body, state := applyCodexManagedIdentityBody(nil, auth, sourceHeaders, "request-fallback", payload, payload)
	if !state.managed || state.enabled {
		t.Fatalf("state managed/enabled = %v/%v, want true/false", state.managed, state.enabled)
	}
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "guardian:parent" {
		t.Fatalf("prompt_cache_key = %q, want independent override", got)
	}
	for path, want := range map[string]string{
		"client_metadata.session_id":               "nested-session",
		"client_metadata.thread_id":                "nested-thread",
		"client_metadata.x-codex-installation-id":  "nested-install",
		"client_metadata.turn_id":                  "nested-turn",
		"client_metadata.x-codex-window-id":        "nested-thread:7",
		"client_metadata.x-codex-parent-thread-id": "nested-parent",
	} {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, body)
		}
	}
	nested := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
	for path, want := range map[string]string{
		"installation_id":  "nested-install",
		"session_id":       "nested-session",
		"thread_id":        "nested-thread",
		"turn_id":          "nested-turn",
		"window_id":        "nested-thread:7",
		"parent_thread_id": "nested-parent",
	} {
		if got := gjson.Get(nested, path).String(); got != want {
			t.Fatalf("nested %s = %q, want %q; metadata=%s", path, got, want, nested)
		}
	}

	headers := sourceHeaders.Clone()
	applyCodexIdentityConfuseHeaders(headers, &state)
	finalizeCodexRequestHeaders(headers, sourceHeaders, state.requestIdentityID, true, true, true, codexUserAgent)
	if got := codexSessionHeaderValue(headers); got != "nested-session" {
		t.Fatalf("session-id = %q, want nested-session", got)
	}
	if got := codexThreadHeaderValue(headers); got != "nested-thread" {
		t.Fatalf("thread-id = %q, want nested-thread", got)
	}
	if got := codexClientRequestIDValue(headers); got != "nested-thread" {
		t.Fatalf("x-client-request-id = %q, want nested-thread", got)
	}
	if got := headerValueCaseInsensitive(headers, "X-Codex-Window-Id"); got != "nested-thread:7" {
		t.Fatalf("X-Codex-Window-Id = %q, want nested-thread:7", got)
	}
}

func TestCodexManagedIdentityKeepsPromptOverrideSeparateFromGeneratedRequestIdentity(t *testing.T) {
	auth := explicitOAuthAuth()
	const requestIdentity = "019f0000-0000-7000-8000-000000000001"
	payload := []byte(`{"model":"gpt-5.6","prompt_cache_key":"guardian:parent-thread"}`)

	body, state := applyCodexManagedIdentityBody(nil, auth, nil, requestIdentity, payload, payload)
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "guardian:parent-thread" {
		t.Fatalf("prompt_cache_key = %q, want guardian override", got)
	}
	for path, want := range map[string]string{
		"client_metadata.session_id":        requestIdentity,
		"client_metadata.thread_id":         requestIdentity,
		"client_metadata.x-codex-window-id": requestIdentity + ":0",
	} {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, body)
		}
	}
	headers := http.Header{}
	applyCodexIdentityConfuseHeaders(headers, &state)
	finalizeCodexRequestHeaders(headers, nil, requestIdentity, true, true, true, codexUserAgent)
	if got := codexSessionHeaderValue(headers); got != requestIdentity {
		t.Fatalf("session-id = %q, want request identity", got)
	}
	if got := codexThreadHeaderValue(headers); got != requestIdentity {
		t.Fatalf("thread-id = %q, want request identity", got)
	}
	if got := codexClientRequestIDValue(headers); got != requestIdentity {
		t.Fatalf("x-client-request-id = %q, want request identity", got)
	}

	withoutOverride := []byte(`{"model":"gpt-5.6"}`)
	body, _ = applyCodexManagedIdentityBody(nil, auth, nil, requestIdentity, withoutOverride, withoutOverride)
	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != requestIdentity {
		t.Fatalf("default prompt_cache_key = %q, want thread/request identity", got)
	}
}

func TestCodexRequestIdentityIDStableAcrossRetries(t *testing.T) {
	assertStable := func(t *testing.T, req cliproxyexecutor.Request, opts *cliproxyexecutor.Options, stored func() string) {
		t.Helper()
		first := codexRequestIdentityID(req, opts)
		second := codexRequestIdentityID(req, opts)
		if first == "" || second != first {
			t.Fatalf("request identity changed across retries: first=%q second=%q", first, second)
		}
		if got := stored(); got != first {
			t.Fatalf("stored request identity = %q, want %q", got, first)
		}
		parsed, err := uuid.Parse(first)
		if err != nil || parsed.Version() != 7 {
			t.Fatalf("request identity = %q, want UUIDv7 (err=%v)", first, err)
		}
	}

	t.Run("allocates options metadata", func(t *testing.T) {
		req := cliproxyexecutor.Request{}
		opts := &cliproxyexecutor.Options{}
		assertStable(t, req, opts, func() string {
			return metadataString(opts.Metadata, cliproxyexecutor.CodexRequestIdentityMetadataKey)
		})
	})

	t.Run("preserves request metadata", func(t *testing.T) {
		metadata := map[string]any{}
		req := cliproxyexecutor.Request{Metadata: metadata}
		opts := &cliproxyexecutor.Options{}
		assertStable(t, req, opts, func() string {
			return metadataString(metadata, cliproxyexecutor.CodexRequestIdentityMetadataKey)
		})
	})
}

func TestCodexManagedIdentityDoesNotReuseHashesAcrossIdentityKinds(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{SessionAffinity: true},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-cross-kind"}
	raw := []byte(`{"model":"gpt-5.6","prompt_cache_key":"shared-id","client_metadata":{"session_id":"shared-id","thread_id":"thread-id"}}`)

	body, state := applyCodexManagedIdentityBody(cfg, auth, nil, "request-id", raw, raw)
	sessionID := gjson.GetBytes(body, "client_metadata.session_id").String()
	threadID := gjson.GetBytes(body, "client_metadata.thread_id").String()
	promptCacheKey := gjson.GetBytes(body, "prompt_cache_key").String()
	if sessionID == "" || threadID == "" || promptCacheKey == "" {
		t.Fatalf("managed identities must be non-empty: session=%q thread=%q prompt=%q", sessionID, threadID, promptCacheKey)
	}
	if sessionID == "shared-id" || threadID == "thread-id" || promptCacheKey == "shared-id" {
		t.Fatalf("raw identity leaked: session=%q thread=%q prompt=%q", sessionID, threadID, promptCacheKey)
	}
	if promptCacheKey == sessionID {
		t.Fatalf("explicit prompt cache override reused the session hash: %q", promptCacheKey)
	}
	if got, want := promptCacheKey, codexIdentityConfuseUUID(auth.ID, "prompt-cache", "shared-id"); got != want {
		t.Fatalf("prompt_cache_key = %q, want kind-scoped hash %q", got, want)
	}
	if len(state.identityIDs) < 3 {
		t.Fatalf("identity replacement count = %d, want independent session/thread/prompt mappings", len(state.identityIDs))
	}
}

func TestPairCodexClientIdentity(t *testing.T) {
	tests := []struct {
		name           string
		userAgent      string
		wantOriginator string
		wantUserAgent  string
		wantOK         bool
	}{
		{
			name:           "recognized leading product wins",
			userAgent:      "Codex_Exec/1.3.0 (Mac OS 26.2; arm64) (codex_vscode; 9.9)",
			wantOriginator: "codex_exec",
			wantUserAgent:  "codex_exec/1.3.0 (Mac OS 26.2; arm64) (codex_vscode; 9.9)",
			wantOK:         true,
		},
		{
			name:           "recognized final trailer repairs wrapper prefix",
			userAgent:      "host-wrapper/7.4 (Mac OS 26.2; arm64) (codex_vscode; 0.55.0)",
			wantOriginator: "codex_vscode",
			wantUserAgent:  "codex_vscode/7.4 (Mac OS 26.2; arm64) (codex_vscode; 0.55.0)",
			wantOK:         true,
		},
		{
			name:      "foreign identity is rejected",
			userAgent: "host-wrapper/7.4 (foreign; 1.0)",
		},
		{
			name:      "nested trailer is rejected",
			userAgent: "host-wrapper/7.4 ((codex_vscode; 0.55.0))",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			originator, userAgent, ok := pairCodexClientIdentity(tc.userAgent)
			if ok != tc.wantOK || originator != tc.wantOriginator || userAgent != tc.wantUserAgent {
				t.Fatalf("pair = (%q, %q, %v), want (%q, %q, %v)", originator, userAgent, ok, tc.wantOriginator, tc.wantUserAgent, tc.wantOK)
			}
		})
	}
}

func TestCodexOfficialIdentityTarget(t *testing.T) {
	valid := []string{
		"https://chatgpt.com/backend-api/codex/responses",
		"https://CHATGPT.com:443/backend-api/codex/responses/compact",
		"wss://chatgpt.com/backend-api/codex/responses",
	}
	invalid := []string{
		"http://chatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com.evil.test/backend-api/codex/responses",
		"https://user@chatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com:444/backend-api/codex/responses",
		"https://chatgpt.com/backend-api/codex/responses?x=1",
		"https://chatgpt.com/backend-api/codex/responses/",
		"https://chatgpt.com:/backend-api/codex/responses",
		"https://chatgpt.com/backend-api/codex/%72esponses",
		"https://chatgpt.com/backend-api/codex/alpha/search",
		"https://chatgpt.com/backend-api/codex/images/generations",
		"https://chatgpt.com/backend-api/codex/images/edits",
		"wss://chatgpt.com/backend-api/codex/responses/compact",
		"wss://chatgpt.com/backend-api/codex/images/generations",
	}
	for _, target := range valid {
		if !codexOfficialIdentityTarget(target) {
			t.Errorf("valid target rejected: %s", target)
		}
	}
	for _, target := range invalid {
		if codexOfficialIdentityTarget(target) {
			t.Errorf("invalid target accepted: %s", target)
		}
	}
}

func TestCodexOfficialOAuthTargetIncludesStandaloneImagesWithoutPairingResponsesIdentity(t *testing.T) {
	for _, target := range []string{
		"https://chatgpt.com/backend-api/codex/images/generations",
		"https://chatgpt.com/backend-api/codex/images/edits",
	} {
		if !codexOfficialOAuthTarget(target) {
			t.Errorf("official OAuth target rejected: %s", target)
		}
		if codexOfficialIdentityTarget(target) {
			t.Errorf("standalone Images target must not use Responses identity headers: %s", target)
		}
		if !codexShouldNormalizeOAuthClientIdentity(explicitOAuthAuth(), target) {
			t.Errorf("standalone Images target must normalize OAuth client identity: %s", target)
		}
		if codexShouldPairOAuthIdentity(explicitOAuthAuth(), target) {
			t.Errorf("standalone Images target must not pair session/thread identity: %s", target)
		}
	}
}

func TestCodexOAuthIdentityPairingRequiresPositiveOAuthAndOfficialOperation(t *testing.T) {
	officialResponses := "https://chatgpt.com" + codexOfficialResponsesPath
	officialCompact := "https://chatgpt.com" + codexOfficialCompactPath
	tests := []struct {
		name   string
		auth   *cliproxyauth.Auth
		target string
		want   bool
	}{
		{name: "nil auth", target: officialResponses},
		{name: "unknown auth", auth: &cliproxyauth.Auth{Provider: "codex"}, target: officialResponses},
		{name: "api key", auth: &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "key"}}, target: officialResponses},
		{name: "explicit oauth", auth: explicitOAuthAuth(), target: officialResponses, want: true},
		{name: "metadata oauth", auth: &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "token"}}, target: officialResponses, want: true},
		{name: "custom oauth", auth: explicitOAuthAuth(), target: "https://proxy.example/v1/responses"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexShouldPairOAuthIdentity(tc.auth, tc.target); got != tc.want {
				t.Fatalf("codexShouldPairOAuthIdentity() = %v, want %v", got, tc.want)
			}
		})
	}
	if !codexShouldPairClientRequestID(officialResponses) {
		t.Fatal("official Responses must pair x-client-request-id")
	}
	if codexShouldPairClientRequestID(officialCompact) {
		t.Fatal("official compact must omit x-client-request-id")
	}
}

func TestApplyCodexHeadersCustomOAuthPreservesRawClientIdentity(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://proxy.example.test/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "company-client/1.0",
		"Originator": "company-origin",
	}))
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"email": "user@example.com"}}

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("User-Agent"); got != "company-client/1.0" {
		t.Fatalf("User-Agent = %q, want raw custom-upstream value", got)
	}
	if got := req.Header.Get("Originator"); got != "company-origin" {
		t.Fatalf("Originator = %q, want raw custom-upstream value", got)
	}
}

func TestCodexRequestHeaderSourceExplicitEmptyIsAuthoritative(t *testing.T) {
	ctx := contextWithGinHeaders(map[string]string{"session-id": "gin-session"})
	if got := codexRequestHeaderSource(ctx, http.Header{}); got == nil || len(got) != 0 {
		t.Fatalf("explicit empty headers = %#v, want non-nil empty authoritative source", got)
	}
	if got := codexRequestHeaderSource(ctx, nil); codexSessionHeaderValue(got) != "gin-session" {
		t.Fatalf("nil explicit headers did not fall back to gin source: %#v", got)
	}
}

func TestApplyCodexManagedWebsocketHeadersPreservesDistinctIDs(t *testing.T) {
	explicit := http.Header{
		"Session-Id": []string{"session-1"},
		"Thread-Id":  []string{"thread-1"},
	}
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"email": "user@example.com"}}

	headers := applyCodexManagedWebsocketHeaders(
		context.Background(), http.Header{}, explicit, auth, "oauth-token", nil, "gpt-5.6", nil,
		"cache-must-not-win", "https://chatgpt.com"+codexOfficialResponsesPath,
	)

	if got := codexSessionHeaderValue(headers); got != "session-1" {
		t.Fatalf("session-id = %q, want session-1", got)
	}
	if got := codexThreadHeaderValue(headers); got != "thread-1" {
		t.Fatalf("thread-id = %q, want thread-1", got)
	}
	if got := codexClientRequestIDValue(headers); got != "thread-1" {
		t.Fatalf("x-client-request-id = %q, want final thread-id", got)
	}
}

func TestCodexHTTPExecutionPathsPreserveDistinctIDs(t *testing.T) {
	tests := []struct {
		name     string
		alt      string
		stream   bool
		payload  []byte
		response string
	}{
		{
			name:     "execute",
			payload:  []byte(`{"model":"gpt-5.6","prompt_cache_key":"cache-must-not-win","input":"hello"}`),
			response: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n",
		},
		{
			name:     "execute stream",
			stream:   true,
			payload:  []byte(`{"model":"gpt-5.6","prompt_cache_key":"cache-must-not-win","input":"hello"}`),
			response: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n",
		},
		{
			name:     "compact",
			alt:      "responses/compact",
			payload:  []byte(`{"model":"gpt-5.6","prompt_cache_key":"cache-must-not-win","input":[{"type":"message","role":"user","content":"history"},{"type":"compaction_trigger"}]}`),
			response: `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			captured := make(chan http.Header, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- r.Header.Clone()
				if tc.alt == "" {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
			opts := cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          tc.alt,
				Stream:       tc.stream,
				Headers: http.Header{
					"Session-Id": []string{"session-wire"},
					"Thread-Id":  []string{"thread-wire"},
				},
			}
			req := cliproxyexecutor.Request{Model: "gpt-5.6", Payload: tc.payload}
			if tc.stream {
				result, err := executor.ExecuteStream(context.Background(), auth, req, opts)
				if err != nil {
					t.Fatalf("ExecuteStream() error = %v", err)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream chunk error = %v", chunk.Err)
					}
				}
			} else {
				if _, err := executor.Execute(context.Background(), auth, req, opts); err != nil {
					t.Fatalf("Execute() error = %v", err)
				}
			}
			assertWireCodexIDs(t, <-captured, "session-wire", "thread-wire", "")
		})
	}
}

func TestCodexWebsocketExecutionPreservesDistinctIDs(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	captured := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, err = conn.ReadMessage(); err != nil {
			t.Errorf("read websocket request: %v", err)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`))
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6",
		Payload: []byte(`{"model":"gpt-5.6","prompt_cache_key":"cache-must-not-win","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
		Headers: http.Header{
			"Session-Id": []string{"session-ws"},
			"Thread-Id":  []string{"thread-ws"},
		},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	select {
	case headers := <-captured:
		assertWireCodexIDs(t, headers, "session-ws", "thread-ws", "")
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for websocket request headers")
	}
}

func assertWireCodexIDs(t *testing.T, headers http.Header, wantSession, wantThread, wantClient string) {
	t.Helper()
	if got := headers.Get("Session-Id"); got != wantSession {
		t.Fatalf("wire session-id = %q, want %q", got, wantSession)
	}
	if got := headers.Get("Thread-Id"); got != wantThread {
		t.Fatalf("wire thread-id = %q, want %q", got, wantThread)
	}
	if got := headers.Get("X-Client-Request-Id"); got != wantClient {
		t.Fatalf("wire x-client-request-id = %q, want %q", got, wantClient)
	}
}

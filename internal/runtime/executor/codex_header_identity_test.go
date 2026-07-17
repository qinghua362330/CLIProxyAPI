package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
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
			name:        "missing client request ID uses final thread",
			source:      http.Header{"session-id": []string{"session-b"}, "thread-id": []string{"thread-b"}},
			wantSession: "session-b",
			wantThread:  "thread-b",
			wantClient:  "thread-b",
		},
		{
			name:        "session alone does not consume prompt fallback",
			source:      http.Header{"Session_id": []string{"session-c"}},
			promptCache: "cache-c",
			wantSession: "session-c",
		},
		{
			name:        "thread alone remains independent and supplies client request ID",
			source:      http.Header{"thread_id": []string{"thread-d"}},
			promptCache: "cache-d",
			wantThread:  "thread-d",
			wantClient:  "thread-d",
		},
		{
			name:        "lone prompt cache key is dual compatibility fallback",
			promptCache: "cache-e",
			wantSession: "cache-e",
			wantThread:  "cache-e",
			wantClient:  "cache-e",
		},
		{
			name:        "target overrides source per ID",
			target:      http.Header{"Session_id": []string{"target-session"}},
			source:      http.Header{"session-id": []string{"source-session"}, "thread-id": []string{"source-thread"}},
			wantSession: "target-session",
			wantThread:  "source-thread",
			wantClient:  "source-thread",
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
			finalizeCodexRequestHeaders(target, tc.source, tc.promptCache, false, "")

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
		"https://chatgpt.com/backend-api/codex/images/generations",
		"https://chatgpt.com/backend-api/codex/images/edits",
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
			assertWireCodexIDs(t, <-captured, "session-wire", "thread-wire", "thread-wire")
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
		assertWireCodexIDs(t, headers, "session-ws", "thread-ws", "thread-ws")
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

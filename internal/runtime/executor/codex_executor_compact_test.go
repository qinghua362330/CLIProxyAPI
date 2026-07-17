package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCompactAddsDefaultInstructionsWithoutInjectingImageTool(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing instructions",
			payload: `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"history"},{"type":"compaction_trigger"}]}`,
		},
		{
			name:    "null instructions",
			payload: `{"model":"gpt-5.4","instructions":null,"input":[{"type":"message","role":"user","content":"history"},{"type":"compaction_trigger"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  "test",
			}}

			resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          "responses/compact",
				Stream:       false,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if gotPath != "/responses/compact" {
				t.Fatalf("path = %q, want %q", gotPath, "/responses/compact")
			}
			if instructions := gjson.GetBytes(gotBody, "instructions"); instructions.Type != gjson.String || instructions.String() != "" {
				t.Fatalf("instructions = %s, want empty string; body=%s", instructions.Raw, gotBody)
			}
			if gjson.GetBytes(gotBody, "tools").Exists() {
				t.Fatalf("compact request injected image_generation tool: %s", gotBody)
			}
			input := gjson.GetBytes(gotBody, "input").Array()
			if len(input) != 2 || input[1].Get("type").String() != "compaction_trigger" {
				t.Fatalf("compact input order changed: %s", gotBody)
			}
			if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
				t.Fatalf("payload = %s", string(resp.Payload))
			}
		})
	}
}

func TestCodexCompactDropsClientMetadataBodyAndKeepsCompatibilityHeaders(t *testing.T) {
	executor := NewCodexExecutor(&config.Config{})
	auth := explicitOAuthAuth()
	turnMetadata := `{"installation_id":"install-compact","session_id":"session-compact","thread_id":"thread-compact","turn_id":"turn-compact","window_id":"thread-compact:4","parent_thread_id":"parent-compact"}`
	payload := []byte(`{"model":"gpt-5.6","prompt_cache_key":"thread-compact","input":[],"client_metadata":{"session_id":"flat-session","thread_id":"flat-thread","x-codex-turn-metadata":` + strconv.Quote(turnMetadata) + `}}`)
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Headers: http.Header{
			"session-id":               []string{"header-session"},
			"thread-id":                []string{"header-thread"},
			"X-Codex-Installation-Id":  []string{"header-install"},
			"X-Codex-Window-Id":        []string{"header-thread:9"},
			"X-Codex-Parent-Thread-Id": []string{"header-parent"},
		},
		Metadata: map[string]any{cliproxyexecutor.CodexRequestIdentityMetadataKey: "request-compact"},
	}

	httpReq, body, state, err := executor.cacheHelper(
		context.Background(),
		opts.SourceFormat,
		"https://chatgpt.com"+codexOfficialCompactPath,
		auth,
		cliproxyexecutor.Request{Model: "gpt-5.6", Payload: payload},
		payload,
		payload,
		opts,
	)
	if err != nil {
		t.Fatalf("cacheHelper error: %v", err)
	}
	if gjson.GetBytes(body, "client_metadata").Exists() {
		t.Fatalf("compact body contains client_metadata: %s", body)
	}
	applyCodexManagedRequestHeaders(httpReq, auth, "oauth-token", false, executor.cfg, "gpt-5.6", opts.Headers, &state)
	if got := codexSessionHeaderValue(httpReq.Header); got != "session-compact" {
		t.Fatalf("session-id = %q, want session-compact", got)
	}
	if got := codexThreadHeaderValue(httpReq.Header); got != "thread-compact" {
		t.Fatalf("thread-id = %q, want thread-compact", got)
	}
	if got := codexClientRequestIDValue(httpReq.Header); got != "" {
		t.Fatalf("compact x-client-request-id = %q, want absent", got)
	}
	for header, want := range map[string]string{
		"X-Codex-Installation-Id":  "install-compact",
		"X-Codex-Window-Id":        "thread-compact:4",
		"X-Codex-Parent-Thread-Id": "parent-compact",
	} {
		if got := headerValueCaseInsensitive(httpReq.Header, header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
}

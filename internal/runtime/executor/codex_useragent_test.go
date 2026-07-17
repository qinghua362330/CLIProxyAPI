package executor

import (
	"net/http"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func codexOAuthAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
}

func TestApplyCodexWebsocketHeadersReplacesForeignClientUA(t *testing.T) {
	// Arrange: OAuth account + downstream client sends a non-Codex User-Agent, no config.
	ctx := contextWithGinHeaders(map[string]string{"User-Agent": "OpenAI/Python 1.2.3"})

	// Act
	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, codexOAuthAuth(), "", nil)

	// Assert: the foreign UA must NOT leak to the ChatGPT backend; canonical Codex UA is used.
	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %q, want canonical %q", got, codexUserAgent)
	}
}

func TestApplyCodexWebsocketHeadersKeepsOfficialClientUA(t *testing.T) {
	// Arrange: OAuth account + downstream client is a real Codex CLI.
	ctx := contextWithGinHeaders(map[string]string{"User-Agent": "codex_cli_rs/0.1.0"})

	// Act
	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, codexOAuthAuth(), "", nil)

	// Assert: a recognized Codex UA is forwarded verbatim.
	if got := headers.Get("User-Agent"); got != "codex_cli_rs/0.1.0" {
		t.Fatalf("User-Agent = %q, want %q", got, "codex_cli_rs/0.1.0")
	}
}

func TestApplyCodexHeadersDoesNotSetConnectionHeader(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, codexOAuthAuth(), "oauth-token", false, nil)

	if got := req.Header.Get("Connection"); got != "" {
		t.Fatalf("Connection = %q, want empty; HTTP/2 transport should manage connection reuse", got)
	}
}

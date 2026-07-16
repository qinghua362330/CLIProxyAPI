//go:build !cgo

package executor

import (
	"bytes"
	"context"
	"io"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCacheHelper_NoCGOPlaintextFallback(t *testing.T) {
	auth := &cliproxyauth.Auth{
		ID:       "oauth-no-cgo",
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
			"header:Content-Encoding":      "conflicting-override",
		},
	}
	rawJSON := []byte(`{"model":"gpt-5-codex","stream":true}`)
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: rawJSON}
	httpReq, upstreamBody, _, err := (&CodexExecutor{}).cacheHelper(
		context.Background(),
		sdktranslator.FromString("openai-response"),
		"https://chatgpt.com/backend-api/codex/responses",
		auth,
		req,
		req.Payload,
		rawJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper() error = %v", err)
	}
	applyCodexManagedRequestHeaders(httpReq, auth, "token", true, nil, req.Model, nil)

	wireBody, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if !bytes.Equal(wireBody, rawJSON) || !bytes.Equal(upstreamBody, rawJSON) {
		t.Fatalf("plaintext fallback mismatch: wire=%s upstream=%s raw=%s", wireBody, upstreamBody, rawJSON)
	}
	if got := httpReq.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if httpReq.ContentLength != int64(len(rawJSON)) {
		t.Fatalf("ContentLength = %d, want %d", httpReq.ContentLength, len(rawJSON))
	}
}

package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// codexAuthIsAPIKey preserves the legacy attribute-based API-key check used for
// Codex header selection.
func codexAuthIsAPIKey(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.TrimSpace(auth.Attributes["api_key"]) != ""
}

const (
	codexOfficialResponsesPath = "/backend-api/codex/responses"
	codexOfficialCompactPath   = "/backend-api/codex/responses/compact"
)

// codexShouldZstdBody follows OpenAI Codex rust-v0.144.5: request compression is
// enabled only for OAuth traffic using the official Responses client and backend.
// API-key/custom-backend captures are expected to be plaintext and do not
// contradict this rule. Compact uses a separate official plaintext path.
func codexShouldZstdBody(auth *cliproxyauth.Auth, cfg *config.Config, targetURL string) bool {
	if cfg != nil && cfg.Codex.RequestCompression != nil && !*cfg.Codex.RequestCompression {
		return false
	}
	return auth != nil && auth.AuthKind() == cliproxyauth.AuthKindOAuth && codexOfficialBackendOperation(targetURL) == codexOfficialResponsesPath
}

func codexOfficialBackendURL(targetURL string) bool {
	return codexOfficialBackendOperation(targetURL) != ""
}

func codexOfficialBackendOperation(targetURL string) string {
	parsed, err := url.Parse(targetURL)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Opaque != "" || parsed.User != nil {
		return ""
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawPath != "" {
		return ""
	}
	if !strings.EqualFold(parsed.Hostname(), "chatgpt.com") {
		return ""
	}
	port := parsed.Port()
	if port != "" && port != "443" {
		return ""
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return ""
	}
	switch parsed.Path {
	case codexOfficialResponsesPath, codexOfficialCompactPath:
		return parsed.Path
	default:
		return ""
	}
}

type codexBodyEncodingContextKey struct{}

type codexBodyEncodingState struct {
	managed    bool
	compressed bool
}

func codexManagedBodyEncoding(from sdktranslator.Format, targetURL string) bool {
	return !sourceFormatEqual(from, sdktranslator.FromString(codexOpenAIImageSourceFormat)) && codexOfficialBackendURL(targetURL)
}

func withCodexBodyEncodingState(ctx context.Context, state codexBodyEncodingState) context.Context {
	return context.WithValue(ctx, codexBodyEncodingContextKey{}, state)
}

func codexBodyEncodingStateFromRequest(req *http.Request) (codexBodyEncodingState, bool) {
	if req == nil {
		return codexBodyEncodingState{}, false
	}
	state, ok := req.Context().Value(codexBodyEncodingContextKey{}).(codexBodyEncodingState)
	return state, ok
}

func codexNewBodyRequest(ctx context.Context, targetURL string, plaintext []byte, managed bool, shouldCompress bool, compressor func([]byte) ([]byte, bool)) (*http.Request, error) {
	state := codexBodyEncodingState{managed: managed}
	wireBody := plaintext
	if managed && shouldCompress && compressor != nil {
		if compressed, ok := compressor(plaintext); ok {
			wireBody = compressed
			state.compressed = true
		}
	}

	req, err := http.NewRequestWithContext(withCodexBodyEncodingState(ctx, state), http.MethodPost, targetURL, bytes.NewReader(wireBody))
	if err != nil {
		return nil, err
	}
	finalizeCodexContentEncoding(req)
	return req, nil
}

// finalizeCodexContentEncoding restores the header/body invariant after auth
// custom headers and model overrides have run. Unmanaged requests are untouched.
func finalizeCodexContentEncoding(req *http.Request) {
	if req == nil {
		return
	}
	state, ok := codexBodyEncodingStateFromRequest(req)
	if !ok || !state.managed {
		return
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	if state.compressed {
		req.Header.Set("Content-Encoding", "zstd")
		return
	}
	req.Header.Del("Content-Encoding")
}

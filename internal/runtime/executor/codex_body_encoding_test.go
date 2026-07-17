//go:build cgo

package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

func decodeZstd(t *testing.T, src []byte) []byte {
	t.Helper()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("zstd.NewReader() error = %v", err)
	}
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(src, nil)
	if err != nil {
		t.Fatalf("DecodeAll() error = %v", err)
	}
	return decoded
}

func codexRequestCompressionConfig(value bool) *config.Config {
	return &config.Config{Codex: config.CodexConfig{RequestCompression: &value}}
}

func explicitOAuthAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "auth-oauth",
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
		},
	}
}

func legacyOAuthAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "auth-legacy-oauth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "oauth-token",
		},
	}
}

func legacyAPIKeyAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "auth-legacy-api-key",
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAPIKey: "sk-test",
		},
	}
}

func explicitAPIKeyAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "auth-explicit-api-key",
		Provider: "codex",
		Attributes: map[string]string{
			cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindAPIKey,
			cliproxyauth.AttributeAPIKey:   "sk-test",
		},
	}
}

func unknownAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "auth-unknown", Provider: "codex"}
}

func TestCodexOfficialBackendURL(t *testing.T) {
	tests := []struct {
		name      string
		targetURL string
		want      bool
	}{
		{name: "responses", targetURL: "https://chatgpt.com/backend-api/codex/responses", want: true},
		{name: "compact", targetURL: "https://chatgpt.com/backend-api/codex/responses/compact", want: true},
		{name: "default port", targetURL: "https://chatgpt.com:443/backend-api/codex/responses", want: true},
		{name: "case insensitive hostname", targetURL: "https://CHATGPT.COM/backend-api/codex/responses", want: true},
		{name: "case insensitive scheme", targetURL: "HTTPS://chatgpt.com/backend-api/codex/responses", want: true},
		{name: "base path", targetURL: "https://chatgpt.com/backend-api/codex"},
		{name: "responses trailing slash", targetURL: "https://chatgpt.com/backend-api/codex/responses/"},
		{name: "compact trailing slash", targetURL: "https://chatgpt.com/backend-api/codex/responses/compact/"},
		{name: "responses descendant", targetURL: "https://chatgpt.com/backend-api/codex/responses/other"},
		{name: "images generations", targetURL: "https://chatgpt.com/backend-api/codex/images/generations"},
		{name: "images edits", targetURL: "https://chatgpt.com/backend-api/codex/images/edits"},
		{name: "dot segment", targetURL: "https://chatgpt.com/backend-api/codex/./responses"},
		{name: "parent segment", targetURL: "https://chatgpt.com/backend-api/codex/responses/../responses"},
		{name: "repeated slash", targetURL: "https://chatgpt.com/backend-api/codex//responses"},
		{name: "query", targetURL: "https://chatgpt.com/backend-api/codex/responses?mode=test"},
		{name: "force query", targetURL: "https://chatgpt.com/backend-api/codex/responses?"},
		{name: "fragment", targetURL: "https://chatgpt.com/backend-api/codex/responses#fragment"},
		{name: "lookalike suffix", targetURL: "https://chatgpt.com.example/backend-api/codex/responses"},
		{name: "lookalike prefix", targetURL: "https://chatgpt.com.evil.test/backend-api/codex/responses"},
		{name: "userinfo", targetURL: "https://user@chatgpt.com/backend-api/codex/responses"},
		{name: "unexpected port", targetURL: "https://chatgpt.com:8443/backend-api/codex/responses"},
		{name: "empty port", targetURL: "https://chatgpt.com:/backend-api/codex/responses"},
		{name: "http", targetURL: "http://chatgpt.com/backend-api/codex/responses"},
		{name: "relative", targetURL: "/backend-api/codex/responses"},
		{name: "malformed port", targetURL: "https://chatgpt.com:not-a-port/backend-api/codex/responses"},
		{name: "path prefix lookalike", targetURL: "https://chatgpt.com/backend-api/codexevil/responses"},
		{name: "encoded path separator", targetURL: "https://chatgpt.com/backend-api/codex%2Fresponses"},
		{name: "wrong path", targetURL: "https://chatgpt.com/backend-api/responses"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexOfficialBackendURL(tt.targetURL); got != tt.want {
				t.Fatalf("codexOfficialBackendURL(%q) = %v, want %v", tt.targetURL, got, tt.want)
			}
		})
	}
}

func TestCodexShouldZstdBody(t *testing.T) {
	const officialURL = "https://chatgpt.com/backend-api/codex/responses"
	tests := []struct {
		name      string
		auth      *cliproxyauth.Auth
		cfg       *config.Config
		targetURL string
		want      bool
	}{
		{name: "explicit OAuth unset", auth: explicitOAuthAuth(), targetURL: officialURL, want: true},
		{name: "legacy OAuth true", auth: legacyOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: officialURL, want: true},
		{name: "OAuth false", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(false), targetURL: officialURL},
		{name: "OAuth compact unset", auth: explicitOAuthAuth(), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact"},
		{name: "OAuth compact true", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact"},
		{name: "legacy API key unset", auth: legacyAPIKeyAuth(), targetURL: officialURL},
		{name: "legacy API key true", auth: legacyAPIKeyAuth(), cfg: codexRequestCompressionConfig(true), targetURL: officialURL},
		{name: "legacy API key false", auth: legacyAPIKeyAuth(), cfg: codexRequestCompressionConfig(false), targetURL: officialURL},
		{name: "explicit API key unset", auth: explicitAPIKeyAuth(), targetURL: officialURL},
		{name: "explicit API key true", auth: explicitAPIKeyAuth(), cfg: codexRequestCompressionConfig(true), targetURL: officialURL},
		{name: "explicit API key false", auth: explicitAPIKeyAuth(), cfg: codexRequestCompressionConfig(false), targetURL: officialURL},
		{
			name: "explicit API key overrides OAuth metadata",
			auth: &cliproxyauth.Auth{
				Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindAPIKey},
				Metadata:   map[string]any{"access_token": "oauth-token"},
			},
			targetURL: officialURL,
		},
		{
			name: "explicit OAuth overrides legacy API key field",
			auth: &cliproxyauth.Auth{Attributes: map[string]string{
				cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
				cliproxyauth.AttributeAPIKey:   "sk-contradictory",
			}},
			targetURL: officialURL,
			want:      true,
		},
		{name: "unknown auth unset", auth: unknownAuth(), targetURL: officialURL},
		{name: "unknown auth true", auth: unknownAuth(), cfg: codexRequestCompressionConfig(true), targetURL: officialURL},
		{name: "unknown auth false", auth: unknownAuth(), cfg: codexRequestCompressionConfig(false), targetURL: officialURL},
		{name: "nil auth", targetURL: officialURL},
		{name: "OAuth custom provider", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "https://api.example.com/v1/responses"},
		{name: "OAuth non-HTTPS", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "http://chatgpt.com/backend-api/codex/responses"},
		{name: "OAuth malformed", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "://bad"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexShouldZstdBody(tt.auth, tt.cfg, tt.targetURL); got != tt.want {
				t.Fatalf("codexShouldZstdBody() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexZstdCompress(t *testing.T) {
	tests := []struct {
		name string
		src  []byte
	}{
		{name: "empty", src: []byte{}},
		{name: "JSON", src: []byte(`{"model":"gpt-5-codex","stream":true,"input":[{"role":"user","content":"hello"}]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			compressed, ok := codexZstdCompress(tt.src)
			if !ok {
				t.Fatal("codexZstdCompress() returned ok=false")
			}
			if !bytes.HasPrefix(compressed, zstdMagic) {
				t.Fatalf("compressed output missing zstd magic: % x", compressed[:min(4, len(compressed))])
			}
			if got := decodeZstd(t, compressed); !bytes.Equal(got, tt.src) {
				t.Fatalf("decoded body = %q, want %q", got, tt.src)
			}
		})
	}
}

func TestCodexZstdCompress_NoContentChecksum(t *testing.T) {
	const contentChecksumFlag = 0x04
	compressed, ok := codexZstdCompress([]byte(`{"model":"gpt-5-codex","stream":true}`))
	if !ok {
		t.Fatal("codexZstdCompress() returned ok=false")
	}
	if len(compressed) < 5 || !bytes.HasPrefix(compressed, zstdMagic) {
		t.Fatalf("not a valid zstd frame: % x", compressed[:min(8, len(compressed))])
	}
	if descriptor := compressed[4]; descriptor&contentChecksumFlag != 0 {
		t.Fatalf("Content_Checksum_flag set in frame descriptor 0x%02x", descriptor)
	}
}

func TestCodexZstdOfficialGolden(t *testing.T) {
	tests := []struct {
		size           int
		plainSHA256    string
		compressedSize int
		compressedSHA  string
	}{
		{size: 1_770, plainSHA256: "c6d5be9f15171fbbee8ba3c0291926ceb9314043cce5a76c6761762b69bfbfe1", compressedSize: 270, compressedSHA: "f7e8fe32bcf7cf4cbddf943d0a1e38b8fe17e49a2e0054a799aa03b826d841eb"},
		{size: 65_536, plainSHA256: "b2797a7998e01b1d4c7e9c0a31a0e1d3982028655eaa53615f50e24ece188773", compressedSize: 270, compressedSHA: "d97d07121d296c1a8f2c498d991fb1b8a4a2bc17c43ab03b546f6b989952c78b"},
		{size: 2_097_153, plainSHA256: "c823aff2b9a2da5bb9c21307f1d295c015402368f268a7710139543f68b6217b", compressedSize: 454, compressedSHA: "6800ee30812e9a787019b2e61b793ee830213e111b8259b6708cf51090f5e282"},
		{size: 3_145_728, plainSHA256: "98ed7e8054a2e2c2e542991ac2f923e9193aff935c63ad8e94ab5a902410818d", compressedSize: 549, compressedSHA: "0e31f92fc0db0881870188e56704f2216d88baaafc0d4d9e4630dcaa3036451e"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d_bytes", tt.size), func(t *testing.T) {
			plaintext := make([]byte, tt.size)
			for index := range plaintext {
				plaintext[index] = byte((index*31 + 17) % 251)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(plaintext)); got != tt.plainSHA256 {
				t.Fatalf("plaintext SHA-256 = %s, want %s", got, tt.plainSHA256)
			}

			compressed, ok := codexZstdCompress(plaintext)
			if !ok {
				t.Fatal("codexZstdCompress() returned ok=false")
			}
			if len(compressed) != tt.compressedSize {
				t.Fatalf("compressed length = %d, want %d", len(compressed), tt.compressedSize)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(compressed)); got != tt.compressedSHA {
				t.Fatalf("compressed SHA-256 = %s, want official Rust %s", got, tt.compressedSHA)
			}
			if !bytes.HasPrefix(compressed, codexOfficialZstdFramePrefix) {
				t.Fatalf("compressed prefix = % x, want % x", compressed[:min(len(compressed), len(codexOfficialZstdFramePrefix))], codexOfficialZstdFramePrefix)
			}
		})
	}
}

type testCodexZstdWriter struct {
	dst        io.Writer
	writeN     int
	writeErr   error
	closeErr   error
	closeCalls int
	wireOutput []byte
}

func (w *testCodexZstdWriter) Write(p []byte) (int, error) {
	if len(w.wireOutput) > 0 {
		_, _ = w.dst.Write(w.wireOutput)
	}
	return w.writeN, w.writeErr
}

func (w *testCodexZstdWriter) Close() error {
	w.closeCalls++
	return w.closeErr
}

func TestCodexZstdWriterFailures(t *testing.T) {
	src := []byte(`{"model":"gpt-5-codex","stream":true}`)
	tests := []struct {
		name       string
		writeN     int
		writeErr   error
		closeErr   error
		wireOutput []byte
		wantOK     bool
	}{
		{name: "partial write with error", writeN: 3, writeErr: errors.New("write failed"), wireOutput: []byte("partial")},
		{name: "short write without error", writeN: len(src) - 1, wireOutput: []byte("partial")},
		{name: "close error", writeN: len(src), closeErr: errors.New("close failed"), wireOutput: codexOfficialZstdFramePrefix},
		{name: "success", writeN: len(src), wireOutput: codexOfficialZstdFramePrefix, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var writer *testCodexZstdWriter
			compressed, ok := codexZstdCompressWithWriter(src, func(dst io.Writer) codexZstdWriteCloser {
				writer = &testCodexZstdWriter{
					dst:        dst,
					writeN:     tt.writeN,
					writeErr:   tt.writeErr,
					closeErr:   tt.closeErr,
					wireOutput: tt.wireOutput,
				}
				return writer
			})
			if writer == nil {
				t.Fatal("writer was not constructed")
			}
			if writer.closeCalls != 1 {
				t.Fatalf("Close() calls = %d, want 1", writer.closeCalls)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK && compressed != nil {
				t.Fatalf("failed compression published %x", compressed)
			}
			if tt.wantOK && !bytes.Equal(compressed, codexOfficialZstdFramePrefix) {
				t.Fatalf("compressed = %x, want %x", compressed, codexOfficialZstdFramePrefix)
			}
		})
	}
}

func TestCacheHelper_OAuth(t *testing.T) {
	executor := &CodexExecutor{}
	rawJSON := []byte(`{"model":"gpt-5-codex","stream":true,"input":[{"role":"user","content":"hello"}]}`)
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"model":"gpt-5-codex"}`)}
	httpReq, upstreamBody, _, err := executor.cacheHelper(
		context.Background(),
		sdktranslator.FromString("openai-response"),
		"https://chatgpt.com/backend-api/codex/responses",
		explicitOAuthAuth(),
		req,
		req.Payload,
		rawJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper() error = %v", err)
	}
	wireBody, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if got := httpReq.Header.Get("Content-Encoding"); got != "zstd" {
		t.Fatalf("Content-Encoding = %q, want zstd", got)
	}
	if httpReq.ContentLength != int64(len(wireBody)) {
		t.Fatalf("ContentLength = %d, want %d", httpReq.ContentLength, len(wireBody))
	}
	decoded := decodeZstd(t, wireBody)
	if !bytes.Equal(decoded, upstreamBody) {
		t.Fatalf("decoded/upstream body mismatch:\ndecoded=%s\nupstream=%s\nraw=%s", decoded, upstreamBody, rawJSON)
	}
	if !gjson.ValidBytes(decoded) {
		t.Fatalf("decoded body is not valid JSON: %s", decoded)
	}
	if got := gjson.GetBytes(decoded, "model").String(); got != "gpt-5-codex" {
		t.Fatalf("decoded model = %q, want gpt-5-codex", got)
	}
}

func TestCacheHelper_ImagesUnmanaged(t *testing.T) {
	rawJSON := []byte(`{"model":"gpt-image-2","stream":true}`)
	req := cliproxyexecutor.Request{Model: "gpt-image-2", Payload: rawJSON}
	httpReq, upstreamBody, _, err := (&CodexExecutor{}).cacheHelper(
		context.Background(), sdktranslator.FromString(codexOpenAIImageSourceFormat),
		"https://chatgpt.com/backend-api/codex/responses", explicitOAuthAuth(), req, req.Payload, rawJSON,
	)
	if err != nil {
		t.Fatalf("cacheHelper() error = %v", err)
	}
	wireBody, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if got := httpReq.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if !bytes.Equal(wireBody, upstreamBody) {
		t.Fatalf("image body mismatch: wire=%s upstream=%s raw=%s", wireBody, upstreamBody, rawJSON)
	}
	if httpReq.ContentLength != int64(len(upstreamBody)) {
		t.Fatalf("ContentLength = %d, want %d", httpReq.ContentLength, len(upstreamBody))
	}
	httpReq.Header.Set("Content-Encoding", "image-custom")
	finalizeCodexContentEncoding(httpReq)
	if got := httpReq.Header.Get("Content-Encoding"); got != "image-custom" {
		t.Fatalf("unmanaged image Content-Encoding = %q, want image-custom", got)
	}
}

func TestApplyCodexManagedRequestHeaders_ContentEncodingOverrides(t *testing.T) {
	const (
		officialURL = "https://chatgpt.com/backend-api/codex/responses"
		modelName   = "test-codex-content-encoding-override"
	)
	reg := registry.GetGlobalRegistry()
	clientID := "test-codex-body-encoding"
	reg.RegisterClient(clientID, "codex", []*registry.ModelInfo{{
		ID: modelName,
		Config: &registry.ModelConfig{OverrideHeader: map[string]string{
			"content-encoding":      "model-override",
			"x-test-model-override": "applied",
		}},
	}})
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	tests := []struct {
		name           string
		auth           *cliproxyauth.Auth
		targetURL      string
		applyModel     bool
		wantEncoding   string
		wantSignalName string
	}{
		{name: "OAuth auth custom header", auth: explicitOAuthAuth(), wantEncoding: "zstd", wantSignalName: "X-Test-Auth-Override"},
		{name: "API key auth custom header", auth: explicitAPIKeyAuth(), wantSignalName: "X-Test-Auth-Override"},
		{name: "OAuth model override", auth: explicitOAuthAuth(), applyModel: true, wantEncoding: "zstd", wantSignalName: "X-Test-Model-Override"},
		{name: "API key model override", auth: explicitAPIKeyAuth(), applyModel: true, wantSignalName: "X-Test-Model-Override"},
		{name: "OAuth compact auth custom header", auth: explicitOAuthAuth(), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact", wantSignalName: "X-Test-Auth-Override"},
		{name: "OAuth compact model override", auth: explicitOAuthAuth(), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact", applyModel: true, wantSignalName: "X-Test-Model-Override"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.applyModel {
				tt.auth.Attributes["header:Content-Encoding"] = "auth-override"
				tt.auth.Attributes["header:X-Test-Auth-Override"] = "applied"
			}
			rawJSON := []byte(`{"model":"gpt-5-codex","stream":true}`)
			req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: rawJSON}
			targetURL := tt.targetURL
			if targetURL == "" {
				targetURL = officialURL
			}
			httpReq, upstreamBody, _, err := (&CodexExecutor{}).cacheHelper(
				context.Background(), sdktranslator.FromString("openai-response"), targetURL,
				tt.auth, req, req.Payload, rawJSON,
			)
			if err != nil {
				t.Fatalf("cacheHelper() error = %v", err)
			}
			requestModel := "gpt-5-codex"
			if tt.applyModel {
				requestModel = modelName
			}
			applyCodexManagedRequestHeaders(httpReq, tt.auth, "token", true, nil, requestModel, nil, nil)
			if got := httpReq.Header.Get(tt.wantSignalName); got != "applied" {
				t.Fatalf("%s = %q, want applied", tt.wantSignalName, got)
			}
			if got := httpReq.Header.Get("Content-Encoding"); got != tt.wantEncoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			wireBody, err := io.ReadAll(httpReq.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if httpReq.ContentLength != int64(len(wireBody)) {
				t.Fatalf("ContentLength = %d, want %d", httpReq.ContentLength, len(wireBody))
			}
			decoded := wireBody
			if tt.wantEncoding == "zstd" {
				decoded = decodeZstd(t, wireBody)
			}
			if !bytes.Equal(decoded, upstreamBody) {
				t.Fatalf("body mismatch: decoded=%s upstream=%s raw=%s", decoded, upstreamBody, rawJSON)
			}
		})
	}
}

func TestCodexExecutorHttpRequest_ManagedContentEncodingWire(t *testing.T) {
	const officialURL = "https://chatgpt.com/backend-api/codex/responses"
	tests := []struct {
		name         string
		auth         *cliproxyauth.Auth
		targetURL    string
		wantEncoding string
	}{
		{name: "compressed OAuth", auth: explicitOAuthAuth(), wantEncoding: "zstd"},
		{name: "plaintext API key", auth: explicitAPIKeyAuth()},
		{name: "plaintext OAuth compact", auth: explicitOAuthAuth(), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotEncoding string
			var gotBody []byte
			var gotBodyErr error
			var gotContentLength int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEncoding = r.Header.Get("Content-Encoding")
				gotContentLength = r.ContentLength
				gotBody, gotBodyErr = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			tt.auth.Attributes["header:Content-Encoding"] = "conflicting-override"
			rawJSON := []byte(`{"model":"gpt-5-codex","stream":false,"input":[{"role":"user","content":"http request boundary"}]}`)
			req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: rawJSON}
			targetURL := tt.targetURL
			if targetURL == "" {
				targetURL = officialURL
			}
			httpReq, upstreamBody, _, err := (&CodexExecutor{}).cacheHelper(
				context.Background(), sdktranslator.FromString("openai-response"), targetURL,
				tt.auth, req, req.Payload, rawJSON,
			)
			if err != nil {
				t.Fatalf("cacheHelper() error = %v", err)
			}

			observerURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("url.Parse() error = %v", err)
			}
			httpReq.URL.Scheme = observerURL.Scheme
			httpReq.URL.Host = observerURL.Host
			httpReq.Host = ""
			response, err := (&CodexExecutor{}).HttpRequest(context.Background(), tt.auth, httpReq)
			if err != nil {
				t.Fatalf("HttpRequest() error = %v", err)
			}
			if _, errCopy := io.Copy(io.Discard, response.Body); errCopy != nil {
				t.Fatalf("Copy() error = %v", errCopy)
			}
			if errClose := response.Body.Close(); errClose != nil {
				t.Fatalf("Close() error = %v", errClose)
			}
			if gotBodyErr != nil {
				t.Fatalf("server ReadAll() error = %v", gotBodyErr)
			}

			if gotEncoding != tt.wantEncoding {
				t.Fatalf("server Content-Encoding = %q, want %q", gotEncoding, tt.wantEncoding)
			}
			if gotContentLength != int64(len(gotBody)) {
				t.Fatalf("server ContentLength = %d, want %d", gotContentLength, len(gotBody))
			}
			decoded := gotBody
			if tt.wantEncoding == "zstd" {
				decoded = decodeZstd(t, gotBody)
			}
			if !bytes.Equal(decoded, upstreamBody) {
				t.Fatalf("server body mismatch: decoded=%s upstream=%s raw=%s", decoded, upstreamBody, rawJSON)
			}
		})
	}
}

func TestCacheHelper_PlaintextBodies(t *testing.T) {
	const officialURL = "https://chatgpt.com/backend-api/codex/responses"
	tests := []struct {
		name      string
		auth      *cliproxyauth.Auth
		cfg       *config.Config
		targetURL string
	}{
		{name: "OAuth disabled", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(false), targetURL: officialURL},
		{name: "OAuth compact unset", auth: explicitOAuthAuth(), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact"},
		{name: "OAuth compact true", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact"},
		{name: "OAuth compact false", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(false), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact"},
		{name: "legacy API key unset", auth: legacyAPIKeyAuth(), targetURL: officialURL},
		{name: "legacy API key true", auth: legacyAPIKeyAuth(), cfg: codexRequestCompressionConfig(true), targetURL: officialURL},
		{name: "legacy API key false", auth: legacyAPIKeyAuth(), cfg: codexRequestCompressionConfig(false), targetURL: officialURL},
		{name: "explicit API key unset", auth: explicitAPIKeyAuth(), targetURL: officialURL},
		{name: "explicit API key true", auth: explicitAPIKeyAuth(), cfg: codexRequestCompressionConfig(true), targetURL: officialURL},
		{name: "explicit API key false", auth: explicitAPIKeyAuth(), cfg: codexRequestCompressionConfig(false), targetURL: officialURL},
		{name: "unknown auth", auth: unknownAuth(), cfg: codexRequestCompressionConfig(true), targetURL: officialURL},
		{name: "nil auth", cfg: codexRequestCompressionConfig(true), targetURL: officialURL},
		{name: "custom provider", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "https://api.example.com/v1/responses"},
		{name: "lookalike provider", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "https://chatgpt.com.example/backend-api/codex/responses"},
		{name: "non-HTTPS provider", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "http://chatgpt.com/backend-api/codex/responses"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &CodexExecutor{cfg: tt.cfg}
			rawJSON := []byte(`{"model":"gpt-5-codex","stream":true}`)
			req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: rawJSON}
			httpReq, upstreamBody, _, err := executor.cacheHelper(
				context.Background(), sdktranslator.FromString("openai-response"), tt.targetURL,
				tt.auth, req, req.Payload, rawJSON,
			)
			if err != nil {
				t.Fatalf("cacheHelper() error = %v", err)
			}
			wireBody, err := io.ReadAll(httpReq.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if got := httpReq.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want empty", got)
			}
			if !bytes.Equal(wireBody, upstreamBody) {
				t.Fatalf("plaintext body mismatch:\nwire=%s\nupstream=%s\nraw=%s", wireBody, upstreamBody, rawJSON)
			}
			if httpReq.ContentLength != int64(len(upstreamBody)) {
				t.Fatalf("ContentLength = %d, want %d", httpReq.ContentLength, len(upstreamBody))
			}
			if !gjson.ValidBytes(wireBody) || gjson.GetBytes(wireBody, "model").String() != "gpt-5-codex" {
				t.Fatalf("plaintext body is invalid: %s", wireBody)
			}
		})
	}
}

func TestCodexNewBodyRequest_EncoderFailureUsesPlaintext(t *testing.T) {
	rawJSON := []byte(`{"model":"gpt-5-codex","stream":true}`)
	httpReq, err := codexNewBodyRequest(
		context.Background(),
		"https://chatgpt.com/backend-api/codex/responses",
		rawJSON,
		true,
		true,
		func([]byte) ([]byte, bool) { return []byte("partial frame"), false },
	)
	if err != nil {
		t.Fatalf("codexNewBodyRequest() error = %v", err)
	}
	wireBody, err := io.ReadAll(httpReq.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if got := httpReq.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want empty", got)
	}
	if !bytes.Equal(wireBody, rawJSON) {
		t.Fatalf("plaintext body mismatch: wire=%s raw=%s", wireBody, rawJSON)
	}
	if httpReq.ContentLength != int64(len(rawJSON)) {
		t.Fatalf("ContentLength = %d, want %d", httpReq.ContentLength, len(rawJSON))
	}
}

func TestCacheHelper_WireServer(t *testing.T) {
	const officialURL = "https://chatgpt.com/backend-api/codex/responses"
	tests := []struct {
		name         string
		auth         *cliproxyauth.Auth
		cfg          *config.Config
		targetURL    string
		wantEncoding string
	}{
		{name: "OAuth default", auth: explicitOAuthAuth(), wantEncoding: "zstd"},
		{name: "legacy OAuth default", auth: legacyOAuthAuth(), wantEncoding: "zstd"},
		{name: "OAuth disabled", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(false)},
		{name: "API key true", auth: explicitAPIKeyAuth(), cfg: codexRequestCompressionConfig(true)},
		{name: "OAuth compact", auth: explicitOAuthAuth(), cfg: codexRequestCompressionConfig(true), targetURL: "https://chatgpt.com/backend-api/codex/responses/compact"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotEncoding string
			var gotBody []byte
			var gotBodyErr error
			var gotContentLength int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEncoding = r.Header.Get("Content-Encoding")
				gotContentLength = r.ContentLength
				gotBody, gotBodyErr = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			rawJSON := []byte(`{"model":"gpt-5-codex","stream":false,"input":[{"role":"user","content":"wire check"}]}`)
			req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: rawJSON}
			targetURL := tt.targetURL
			if targetURL == "" {
				targetURL = officialURL
			}
			httpReq, upstreamBody, _, err := (&CodexExecutor{cfg: tt.cfg}).cacheHelper(
				context.Background(), sdktranslator.FromString("openai-response"), targetURL,
				tt.auth, req, req.Payload, rawJSON,
			)
			if err != nil {
				t.Fatalf("cacheHelper() error = %v", err)
			}
			observerURL, err := url.Parse(server.URL)
			if err != nil {
				t.Fatalf("url.Parse() error = %v", err)
			}
			httpReq.URL.Scheme = observerURL.Scheme
			httpReq.URL.Host = observerURL.Host
			httpReq.Host = ""

			response, err := http.DefaultClient.Do(httpReq)
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			if _, errCopy := io.Copy(io.Discard, response.Body); errCopy != nil {
				t.Fatalf("Copy() error = %v", errCopy)
			}
			if errClose := response.Body.Close(); errClose != nil {
				t.Fatalf("Close() error = %v", errClose)
			}
			if gotBodyErr != nil {
				t.Fatalf("server ReadAll() error = %v", gotBodyErr)
			}

			if gotEncoding != tt.wantEncoding {
				t.Fatalf("server Content-Encoding = %q, want %q", gotEncoding, tt.wantEncoding)
			}
			if gotContentLength != int64(len(gotBody)) {
				t.Fatalf("server ContentLength = %d, want %d", gotContentLength, len(gotBody))
			}
			decoded := gotBody
			if tt.wantEncoding == "zstd" {
				decoded = decodeZstd(t, gotBody)
			}
			if !bytes.Equal(decoded, upstreamBody) {
				t.Fatalf("server body mismatch:\ndecoded=%s\nupstream=%s\nraw=%s", decoded, upstreamBody, rawJSON)
			}
			if !gjson.ValidBytes(decoded) || gjson.GetBytes(decoded, "model").String() != "gpt-5-codex" {
				t.Fatalf("decoded server body is invalid: %s", decoded)
			}
		})
	}
}

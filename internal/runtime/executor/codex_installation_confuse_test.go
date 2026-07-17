package executor

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// realTurnMetadata is a verbatim inbound x-codex-turn-metadata from production
// (家宽 /root/cpa/logs/error-v1-responses-2026-07-13T164856-42b850b4.log, sent by
// Codex Desktop/0.144.0-alpha.4). Note what the real client does and does not do:
// installation_id lives ONLY here, session_id == thread_id == the body's
// prompt_cache_key, window_id is that same id + ":0", and there is no
// client_metadata in the body at all.
const realTurnMetadata = `{"installation_id":"6ae1cc61-7e89-4a8c-8da2-982ddde8d432","session_id":"019f5a8a-cc68-7b92-89aa-17602176af46","thread_id":"019f5a8a-cc68-7b92-89aa-17602176af46","turn_id":"019f5aaa-1ca3-71f0-b3e4-de5b34ec347a","window_id":"019f5a8a-cc68-7b92-89aa-17602176af46:0","request_kind":"turn","thread_source":"user","sandbox":"none","turn_started_at_unix_ms":1783932525748,"workspace_kind":"projectless"}`

const (
	realInboundPromptCacheKey = "019f5a8a-cc68-7b92-89aa-17602176af46"
	realInboundInstallationID = "6ae1cc61-7e89-4a8c-8da2-982ddde8d432"
)

// outboundForAuth replays the real production inbound shape through the managed
// request path for one pool account and returns the outbound turn-metadata plus the
// wire session-id.
func outboundForAuth(t *testing.T, authID string) (turnMetadata string, sessionID string, body []byte) {
	t.Helper()

	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata", realTurnMetadata)
	ginCtx.Request.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	ginCtx.Request.Header.Set("Originator", "Codex Desktop")
	ctx := context.WithValue(context.Background(), "gin", ginCtx)

	cfg := &config.Config{
		Routing: config.RoutingConfig{SessionAffinity: true, Strategy: "round-robin"},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	executor := &CodexExecutor{cfg: cfg}
	auth := &cliproxyauth.Auth{ID: authID, Provider: "codex", Metadata: map[string]any{"access_token": "oauth-token"}}

	rawJSON := []byte(`{"model":"gpt-5.5","stream":true,"prompt_cache_key":"` + realInboundPromptCacheKey + `"}`)
	req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: rawJSON}

	httpReq, outBody, state, err := executor.cacheHelper(ctx,
		sdktranslator.FromString("openai-response"),
		"https://chatgpt.com"+codexOfficialResponsesPath,
		auth, req, req.Payload, rawJSON)
	if err != nil {
		t.Fatalf("cacheHelper(%s): %v", authID, err)
	}
	applyCodexManagedRequestHeaders(httpReq, auth, "oauth-token", true, cfg, req.Model, &state)

	sid := ""
	if v := httpReq.Header["session-id"]; len(v) > 0 {
		sid = v[0]
	}
	return httpReq.Header.Get("X-Codex-Turn-Metadata"), sid, outBody
}

// The raw installation_id must never reach the backend while identity-confuse is on:
// it is stable per install, so it survives every other per-account remap and lets the
// backend group the whole pool by it.
func TestIdentityConfuse_TurnMetadataInstallationIDIsRemapped(t *testing.T) {
	turnMetadata, _, _ := outboundForAuth(t, "pool-account-42")

	got := gjson.Get(turnMetadata, "installation_id").String()
	if got == realInboundInstallationID {
		t.Fatalf("raw installation_id reached the wire: %q", got)
	}
	if got == "" {
		t.Fatalf("installation_id was dropped; a real client always sends one")
	}
}

// The decisive property: two pool accounts serving the SAME downstream client must not
// share an installation_id. If they do, the id is a correlation anchor and the account
// pool is trivially joinable — which is the whole reason identity-confuse exists.
func TestIdentityConfuse_InstallationIDDiffersPerAccount(t *testing.T) {
	tmA, sidA, _ := outboundForAuth(t, "pool-account-42")
	tmB, sidB, _ := outboundForAuth(t, "pool-account-77")

	installA := gjson.Get(tmA, "installation_id").String()
	installB := gjson.Get(tmB, "installation_id").String()

	if installA == installB {
		t.Fatalf("two pool accounts emitted the same installation_id %q — the pool is joinable by it", installA)
	}
	// Sanity: the accounts really are being confused differently, so a passing
	// installation_id check cannot be an artifact of the fixture.
	if sidA == sidB {
		t.Fatalf("session-id did not vary per account (%q); fixture is not exercising identity-confuse", sidA)
	}
}

// Remapping installation_id must not disturb the identity graph the real client emits:
// session-id == thread-id == body prompt_cache_key == window_id prefix, all one value.
// (Live-captured from codex 0.142.5 and confirmed on real production traffic.)
func TestIdentityConfuse_TurnMetadataIdentityGraphStaysCoherent(t *testing.T) {
	turnMetadata, sessionID, body := outboundForAuth(t, "pool-account-42")

	if sessionID == "" {
		t.Fatalf("no session-id on the wire; a real client always sends one")
	}
	if sessionID == realInboundPromptCacheKey {
		t.Fatalf("raw session id reached the wire: %q", sessionID)
	}

	checks := map[string]string{
		"turn-metadata.session_id": gjson.Get(turnMetadata, "session_id").String(),
		"turn-metadata.thread_id":  gjson.Get(turnMetadata, "thread_id").String(),
		"body.prompt_cache_key":    gjson.GetBytes(body, "prompt_cache_key").String(),
	}
	for name, got := range checks {
		if got != sessionID {
			t.Errorf("%s = %q, want it to equal session-id %q", name, got, sessionID)
		}
	}
	if got, want := gjson.Get(turnMetadata, "window_id").String(), sessionID+":0"; got != want {
		t.Errorf("turn-metadata.window_id = %q, want %q", got, want)
	}
	// turn_id is per-turn and must be confused too, but must NOT collapse onto session-id.
	turnID := gjson.Get(turnMetadata, "turn_id").String()
	if turnID == "019f5aaa-1ca3-71f0-b3e4-de5b34ec347a" {
		t.Errorf("raw turn_id reached the wire: %q", turnID)
	}
	if turnID == sessionID {
		t.Errorf("turn_id collapsed onto session-id (%q); a real client varies it per turn", turnID)
	}
}

// A blank authID must not produce a "confused" value, because the hash would then be
// identical for every account — confused-looking but still a perfect anchor.
func TestIdentityConfuse_BlankAuthIDLeavesInstallationIDAlone(t *testing.T) {
	state := &codexIdentityConfuseState{enabled: true, authID: "", promptCacheKey: "confused", originalPromptCacheKey: realInboundPromptCacheKey}
	got := gjson.Get(applyCodexTurnMetadataIdentityConfuse(realTurnMetadata, state), "installation_id").String()
	if got != realInboundInstallationID {
		t.Fatalf("installation_id = %q with a blank authID; want it untouched (%q)", got, realInboundInstallationID)
	}
}

// The remapped installation_id must not merely differ from the original — it must look
// like one a real client could have generated. Codex generates it randomly, so a live
// capture shows version 4 (6ae1cc61-7e89-4a8c-…). uuid.NewSHA1 alone yields version 5,
// which no real install id has: a half-correct impersonation is its own tell.
func TestIdentityConfuse_InstallationIDKeepsRealClientUUIDVersion(t *testing.T) {
	if got, want := realInboundInstallationID[14], byte('4'); got != want {
		t.Fatalf("fixture drift: captured installation_id %q is version %c, not %c", realInboundInstallationID, got, want)
	}
	turnMetadata, _, _ := outboundForAuth(t, "pool-account-42")
	got := gjson.Get(turnMetadata, "installation_id").String()
	if len(got) != 36 {
		t.Fatalf("installation_id = %q, want a 36-char hyphenated UUID", got)
	}
	if got[14] != '4' {
		t.Fatalf("installation_id = %q (version %c); a real client only ever emits version 4", got, got[14])
	}
}

// A real install id never changes, so the remap must be deterministic: same account and
// same original id must always yield the same value. A per-request random one would
// defeat correlation but is itself a tell no real client produces.
func TestIdentityConfuse_InstallationIDIsStableAcrossRequests(t *testing.T) {
	first, _, _ := outboundForAuth(t, "pool-account-42")
	second, _, _ := outboundForAuth(t, "pool-account-42")

	a := gjson.Get(first, "installation_id").String()
	b := gjson.Get(second, "installation_id").String()
	if a != b {
		t.Fatalf("installation_id drifted between requests for one account: %q then %q", a, b)
	}
}

const (
	realInboundTurnID    = "019f5aaa-1ca3-71f0-b3e4-de5b34ec347a"
	realInboundSessionID = realInboundPromptCacheKey
)

func uuidMillisOf(t *testing.T, s string) int64 {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return codexUUIDMillis(u)
}

// A confused id must mirror the version of the id it replaces. A real client emits
// session/thread/turn as version 7 and installation_id as version 4; uuid.NewSHA1 alone
// emits version 5, which no real client ever produces.
func TestIdentityConfuse_MirrorsRealClientUUIDVersions(t *testing.T) {
	if got := realInboundSessionID[14]; got != '7' {
		t.Fatalf("fixture drift: captured session id is version %c, want 7", got)
	}
	if got := realInboundInstallationID[14]; got != '4' {
		t.Fatalf("fixture drift: captured installation id is version %c, want 4", got)
	}

	turnMetadata, sessionID, _ := outboundForAuth(t, "pool-account-42")
	for name, got := range map[string]string{
		"session-id":               sessionID,
		"turn-metadata.session_id": gjson.Get(turnMetadata, "session_id").String(),
		"turn-metadata.thread_id":  gjson.Get(turnMetadata, "thread_id").String(),
		"turn-metadata.turn_id":    gjson.Get(turnMetadata, "turn_id").String(),
	} {
		if len(got) != 36 || got[14] != '7' {
			t.Errorf("%s = %q (version %c), want version 7", name, got, got[14])
		}
	}
	if got := gjson.Get(turnMetadata, "installation_id").String(); got[14] != '4' {
		t.Errorf("installation_id = %q (version %c), want version 4", got, got[14])
	}
}

// A version-7 id's leading 48 bits are a real millisecond timestamp, so the confused one
// has to decode to a plausible instant. Reading a version-5 hash as v7 lands in the year
// 6335 — visible to anyone who parses the id.
func TestIdentityConfuse_V7TimestampStaysPlausible(t *testing.T) {
	realMillis := uuidMillisOf(t, realInboundSessionID)
	_, sessionID, _ := outboundForAuth(t, "pool-account-42")
	got := uuidMillisOf(t, sessionID)

	if got > realMillis {
		t.Errorf("confused session id is %d ms in the future of the original; a session cannot start after itself", got-realMillis)
	}
	if realMillis-got > codexIdentityConfuseMaxSkewMillis {
		t.Errorf("confused session id is %d ms before the original, beyond the %d ms bound", realMillis-got, codexIdentityConfuseMaxSkewMillis)
	}
}

// One shift per account, applied to every id in the request: a real client mints a turn
// after the session that contains it, and that ordering has to survive the remap.
func TestIdentityConfuse_V7PreservesSessionToTurnOrdering(t *testing.T) {
	turnMetadata, sessionID, _ := outboundForAuth(t, "pool-account-42")
	session := uuidMillisOf(t, sessionID)
	turn := uuidMillisOf(t, gjson.Get(turnMetadata, "turn_id").String())

	if turn <= session {
		t.Fatalf("turn (%d) is not after session (%d); the real client always mints it later", turn, session)
	}
	if want := uuidMillisOf(t, realInboundTurnID) - uuidMillisOf(t, realInboundSessionID); turn-session != want {
		t.Errorf("session-to-turn gap = %d ms, want the original %d ms", turn-session, want)
	}
}

// The shift must differ per account, or every account in the pool reports the identical
// millisecond and the timestamp becomes the anchor the id no longer is.
func TestIdentityConfuse_V7TimestampDiffersPerAccount(t *testing.T) {
	_, sidA, _ := outboundForAuth(t, "pool-account-42")
	_, sidB, _ := outboundForAuth(t, "pool-account-77")

	if a, b := uuidMillisOf(t, sidA), uuidMillisOf(t, sidB); a == b {
		t.Fatalf("two accounts reported the same session millisecond (%d)", a)
	}
}

// An original that is not a UUID has no shape to mirror; leave the hash alone rather than
// invent a version the caller never had.
func TestIdentityConfuse_NonUUIDOriginalKeepsHashShape(t *testing.T) {
	got := codexIdentityConfuseUUIDLike("pool-account-42", "prompt-cache", "not-a-uuid")
	if want := codexIdentityConfuseUUID("pool-account-42", "prompt-cache", "not-a-uuid"); got != want {
		t.Fatalf("codexIdentityConfuseUUIDLike = %q, want the plain hash %q", got, want)
	}
}

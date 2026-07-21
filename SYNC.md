# Fork maintenance & upstream sync (SYNC.md)

This fork is a **soft fork** of `router-for-me/CLIProxyAPI`: the Go module path is
kept unchanged and ~80% of the fingerprint-hardening logic lives in **new files**
under `internal/runtime/executor/helps/`, so upstream merges are low-friction. The
files that touch upstream code (the conflict surface) are kept to small call-site
hooks.

There are **two independent kinds of "keeping up"**:

1. **Merging upstream code** — automated below. `git merge upstream/main` works
   because the module path is preserved.
2. **Re-calibrating fingerprint *values*** — separate manual task, because the real
   `claude` / `codex` clients keep updating. See "Fingerprint re-capture".

---

## Automation (GitHub Actions)

| Workflow | Trigger | What it does |
|---|---|---|
| `.github/workflows/fork-ci.yml` | push / PR to `feat/fingerprint-hardening` or `main` | gofmt check + `go build ./...` + `go test ./...` (self-contained gate) |
| `.github/workflows/fork-sync.yml` | daily cron 03:00 UTC + manual | merge `upstream/main`; **clean+green → push**; **conflict → open a `sync/*` PR**; broken build/test → red run, nothing pushed |
| `.github/workflows/fork-image.yml` | push to mod branch / `v*` tag | build + push Docker image to `ghcr.io/<owner>/cliproxyapi` (`:latest`, `:<branch>`, `:sha`) using `GITHUB_TOKEN` — no extra secrets |
| `.github/workflows/fork-release.yml` | `v*` tag / manual | build Linux **dynamically linked** binaries (**CGO=1**, via the repo `Dockerfile`) amd64+arm64, package with config + systemd unit + `install.sh` + `DEPLOY.md`, publish to **GitHub Releases** + checksums |

Flow (day-to-day): `fork-sync` merges upstream → pushes → that push triggers
`fork-ci` (verify) and `fork-image` (publish image). Deploy is an optional,
commented step in `fork-image.yml` (fill in your SSH/registry target + secrets).

Flow (release): push a `v*` tag → `fork-release` (CGO=1 tarballs → GitHub
Releases) and `fork-image` (multi-arch image → GHCR) run in parallel. Both use
only `GITHUB_TOKEN`.

### One-time setup on the fork (you must do these in the GitHub UI)

1. **Settings → Actions → General → Workflow permissions →** select **"Read and
   write permissions"** (lets `fork-sync` push merges and open PRs, and
   `fork-image` publish to GHCR).
2. **Settings → Actions → General →** ensure Actions are **enabled** for the fork.
3. **Disable interfering upstream workflows on the fork** (Actions tab → pick the
   workflow → ⋯ → Disable). Disabling via the UI does not delete the files, so
   upstream merges stay clean.
   - **Must disable** `release` and `docker-image`: upstream `release.yaml` triggers
     on `tags: ['*']` (every tag) and would race `fork-release` for the same GitHub
     Release; `docker-image.yml` needs DockerHub secrets this fork doesn't have and
     will fail. `fork-release` + `fork-image` replace both.
   - **Should disable** (upstream-governance, mis-fire here): `pr-path-guard`,
     `agents-md-guard`, `auto-retarget-main-pr-to-dev`.
4. First image publish makes the GHCR package **private** by default — make it
   public under the repo's *Packages* if you want to `docker pull` without auth.
5. (Optional) adjust the cron cadence in `fork-sync.yml`. Daily is a good default;
   weekly means fewer, larger merges.

---

## Releasing (打 tag 出产物)

Cut a versioned release — this produces the deployable server artifacts:

```bash
git checkout feat/fingerprint-hardening
git pull
# choose a version; keeping upstream's vX.Y.Z base + a fork suffix is tidy:
git tag v7.2.49-fp1 -m "fingerprint-hardening release"
git push origin v7.2.49-fp1
```

That tag triggers, in parallel (both `GITHUB_TOKEN`-only):
- **`fork-release`** → `CLIProxyAPI_<ver>_linux_amd64.tar.gz` + `_aarch64.tar.gz`
  (**CGO=1, dynamically linked**, built from the repo `Dockerfile`) + `checksums.txt`
  attached to the GitHub Release. Each tarball contains the binary,
  `config.example.yaml`, the systemd unit, `install.sh`, and `DEPLOY.md` —
  `sudo ./install.sh` on the server does the rest.

  > **Never build releases with `CGO_ENABLED=0`.** It compiles fine and fails
  > silently at runtime: Go `.so` plugins require CGO + dynamic linking (a static
  > build disables the plugin host and reports `X-CPA-SUPPORT-PLUGIN:0`), and
  > `codex_body_encoding_nocgo.go` deliberately falls back to **plaintext** bodies —
  > i.e. the codex zstd fingerprint silently stops working. `fork-release.yml`
  > asserts dynamic linking for this reason. Note a macOS host **cannot**
  > cross-compile this (no linux CGO toolchain), so always release via CI.
  > glibc baseline = debian bookworm (2.36).
- **`fork-image`** → multi-arch image at `ghcr.io/<owner>/cliproxyapi:<tag>`.

Manual dry-run without tagging: Actions → `fork-release` → *Run workflow* builds
the tarballs as run **artifacts** (no Release published).

> Prerequisite: upstream `release` + `docker-image` disabled (see step 3 above),
> and "Read and write" workflow permissions (step 1).

---

## Manual merge (fallback / when you want control)

```bash
git fetch upstream
git checkout feat/fingerprint-hardening
git merge upstream/main            # or: git rebase upstream/main
# resolve conflicts if any (see conflict-prone files below)
gofmt -w . && go build ./... && go test ./...
FP_VERIFY=1 go test ./internal/runtime/executor/helps/ -run TestFingerprintAgainstReporter  # fingerprint safety net
git push
```

### Conflict-prone files (where this fork edits upstream code)

**Measured, not guessed.** Get the real list before every merge — fork size does
not predict conflicts, because git merges disjoint regions of the same file fine:

```bash
git merge-tree --write-tree --name-only main upstream/main
# exit 0 = clean; non-zero = the listed files conflict
```

As of 2026-07-17 (main `29d4cbea`, 9 commits behind), merging **all** of upstream
yields exactly **two** conflicting files — and they are precisely our two in-scope
areas, which is the fork working as intended:

1. `sdk/cliproxy/auth/conductor.go` — the **FBT stack** (+463/−97, our largest
   divergence). See "FBT vs upstream RequestScopedError" below **before** resolving.
2. `internal/runtime/executor/codex_executor.go` — codex header/identity + zstd hook

Everything else **auto-merges**, including files both sides edit:
`utls_client.go`, `codex_websockets_executor.go`, `claude_executor.go`,
`kimi_executor.go`, `xai_executor.go`, `go.mod`, `go.sum`, `internal/config/config.go`,
`sdk/api/handlers/handlers.go`, `sdk/cliproxy/executor/types.go`, `sdk/pluginapi/types.go`.

> **The auto-merge is the dangerous half, not the conflict.** A conflict stops you.
> A silent auto-merge of two semantically colliding designs compiles, passes both
> test suites, and ships a behavior change. Verified: see below.

The fork's other ~24 changed files are **new files** (fingerprint-observatory
plugin, `codex_body_encoding*.go`, fork CI, `deploy/`, `docs/`) and can never conflict.
`codex_body_encoding*.go` is the pattern to copy: fingerprint logic in a new file,
one call site in upstream code.

**Escape hatch:** every feature has a `disable-*` config flag. If upstream
refactors a subsystem and re-applying a hook is hard, disable that feature, merge
clean, and re-apply the hook later.

### Post-merge checklist
- [ ] `gofmt -l .` clean
- [ ] `go build ./...` and `go build -o /tmp/x ./cmd/server`
- [ ] `go test ./...` green
- [ ] `FP_VERIFY=1 go test ./internal/runtime/executor/helps/ -run TestFingerprintAgainstReporter` → node/h1 JA3 `44f88fca…`, chatgpt h2 OK
- [ ] fingerprint values still current (see below)
- [ ] **FBT suite green** — and read the next section first; green is **not** sufficient here:
      `go test ./sdk/cliproxy/auth/ -run 'TestMarkResult_|TestReadStreamBootstrap_|TestDrainAndCoolOnStatus_|TestExecuteStream_.*FirstByteTimeout|TestFirstByteTimeoutExhausted|TestManager_SetTemporaryCooldown' -count=1`

---

## FBT vs upstream `RequestScopedError` — a deliberate, permanent divergence

**Read this before resolving any `conductor.go` conflict, and before "tidying up"
anything it names.** Analysis dated 2026-07-17 against upstream `09da52ad`.

Upstream and this fork independently built mechanisms for the same question —
*which failures should make a credential unavailable?* They are **not compatible**,
and this fork **keeps its own, permanently**.

### Why we cannot adopt upstream's mechanism (proven, not preferred)

1. **Upstream has no first-byte timeout at all.** `grep -nE "time.After|NewTimer|AfterFunc|FirstByte"`
   over upstream `conductor.go` returns one hit: a cooldown timer. Upstream's
   `readStreamBootstrap` blocks on a bare `select { <-ctx.Done() | <-ch }` with no
   deadline. Our +463/−97 buys a **capability upstream lacks**, not a duplicate.
2. **Converting our FBT error is structurally impossible.** `auth.Error` has **one**
   `Code` field. `IsFirstByteTimeoutExhausted` keys on `Code == "stream_first_byte_timeout_exhausted"`;
   upstream's `IsRequestScoped` keys on `Code == "request_scoped"`. Mutually exclusive:
   stamping request-scoped **destroys the FBT identity**.
3. **Upstream has nowhere to hook.** Its `discardStreamChunks` is literally
   `go func() { for range ch {} }()` — empty body, `chunk.Err` never read, at all 7
   abandonment sites. It never *observes* a late status, so no classifier could act on
   one. Our `drainAndCoolOnStatus` is the structural inverse.

Of our 14 FBT rules, **12 are inexpressible** via `IsRequestScoped()` (it is a static
property of an error *type*; ours is computed from ambient state at
`conductor.go:2102` and has **no error type at all**). Take upstream's *classifier*
if useful; **never take upstream's *structure***.

### The contract collision (the important part)

For the **same physical event** — a stream that closes before the first payload —
the two projects assert **opposite** contracts:

| | Upstream | This fork |
|---|---|---|
| Verdict | request-scoped: **keep the credential available**, no cooldown, no failover | `empty_stream`: **cool + rotate** |
| Where | `TestManager_RequestScopedErrorStopsCredentialFallbackWithoutSuspendingAuth` (`conductor_overrides_test.go:1131`) | `conductor.go:2235` |
| Basis | "the request's fault" | production: blackholed connections (uTLS h2 `ClientConn` exhaustion) |

Our basis is measured, not theoretical — daily `fbt-check.sh` FBT-504 series:
`07-13: 0 · 07-14: 14 · 07-15: 218 · 07-16: 83 · 07-17: 0`. The FBT stack plus the
h2 leak fix drove user-visible failures to zero.

> ### The rule: **a green upstream test here means we lost.**
>
> Making `TestManager_RequestScopedErrorStopsCredentialFallbackWithoutSuspendingAuth`
> pass *requires* adopting no-cool/no-failover for this class — i.e. it is upstream's
> formal assertion that our 218→0 hardening is a bug. `main` is PR-gated on
> `build`/`build-test`, so **CI pressure pushes you into the regression**. When that
> test goes red after a merge, that is the divergence working. Invert or delete the
> `incomplete` subtests to assert cool+rotate, citing the FBT-504 series — so every
> future merge **re-litigates this loudly instead of erasing it silently**.

### Landmines (verified by running the merge, 2026-07-17)

- **`markResultFromError` (`conductor.go:1741`) must stay inline.** It builds
  `&Error{Message:…, HTTPStatus: upstreamStatusCode(err)}` with **no `Code`**. That
  Code-stripping is the *only* reason a drained late 429 still cools (rule R5). The
  obvious DRY cleanup — routing it through upstream's `resultErrorFromError` — would
  stamp it request-scoped and **silently delete R5**. If it must serve the direct
  bootstrap path too, **split it**; do not unify it.
- **The naive merge is green.** Resolving both conflicts `--ours` builds clean, and
  **both** our 16 FBT tests and upstream's new codex tests pass — while
  `newCodexIncompleteStreamError()` (always 408, always request-scoped) silently
  reclassifies transport failures. Our suite is blind to it. Only upstream's test
  catches it, and "fixing" that test is the regression.
- **Layer disagreement.** `408` is in `handlers.go`'s `bootstrapEligible` replay list,
  while post-merge the conductor treats it as terminal. Handler wants to replay, the
  conductor refuses to failover. Needs a test.
- **Silent widening.** Upstream moved the `MarkResult` guard from
  `isRequestScopedNotFoundResultError` (404 only) to `isRequestScopedResultError`,
  removing the transient cooldown for `500`/`"status":"UNKNOWN"` and `422`. Note
  `auth.Failed++` / `recordRecentRequest` still run **outside** the guard, so
  request-scoped failures skew scheduler selection across the pool with **no cooldown
  to match**.

### Merging `09da52ad` specifically

Worth taking for its **translator payload only** — `case "response.completed",
"response.incomplete": terminalSuccess = true`, which we lack (our `main` handles
only `response.completed`) and which auto-merges. That is #3055's real fix and it
makes `max_output_tokens` truncation a **success**, not an error.

Do **not** take `newCodexIncompleteStreamError()`. A genuine `response.incomplete`
sets `terminalSuccess` and returns, so the emit is reachable **only** from transport
failure — exactly the class our `empty_stream` branch must keep cooling and rotating.
Resolve the `conductor.go` hunk `--ours`.

---

## Fingerprint re-capture (the other maintenance)

Upstream code merges do **not** keep fingerprint *values* current — the real
clients themselves change (UA/pkg versions, header order, Accept-Encoding…). When
`claude` / `codex` update, re-capture and update the constants/pools.

Method (all local, nothing leaves the machine; redact/delete captures after):

```bash
# 1. tiny local raw-TCP server that logs request bytes (see git history of this
#    session for capture_server.py), on 127.0.0.1:PORT
# 2. Claude:
ANTHROPIC_BASE_URL=http://127.0.0.1:PORT claude -p "hi" --model claude-3-5-haiku-20241022
# 3. Codex (custom/OAuth provider):
codex exec --skip-git-repo-check -c 'model_providers.<name>.base_url="http://127.0.0.1:PORT/v1"' "hi" </dev/null
```

Then update, if changed:
- `helps/device_profile_pool.go` — `claudeProfilePool` (cli/pkg/node versions), `codexUAPool`
- `helps/claude_device_profile.go` — default UA / package / runtime constants
- `claude_executor.go` — `Accept-Encoding` value, `X-Stainless-Timeout` default
- `helps/utls_h1_ordered.go` — `headerWireCasing` / `transportHeaderTrailer` if the
  order or casing changed (assert with `TestWriteOrderedRequest_MatchesRealCaptureOrder`)
- `codex_executor.go` — `codexUserAgent` / `codexOriginator`

TLS JA3/JA4 (`helps/utls_profiles.go`) rarely changes (Node/OpenSSL). Verify with
the `FP_VERIFY=1` test against `tls.peet.ws`.

---

## Known follow-up (documented, not yet done for stability)
- ~~**Codex `thread_id` alignment**~~ — **done** (2026-07-17). `setCodexSessionThreadHeaders`
  (`codex_websockets_executor.go`) now takes `sessionID`/`threadID` independently, deletes
  every alias (`session_id`, `Thread_id`, `Conversation_id`, `Conversation-Id`, …) and emits
  only lowercase `session-id` / `thread-id`. `Conversation_id` survives **read-only**, as a
  downstream input alias for the reasoning-replay session key (`codex_executor.go`) — it is
  never forwarded upstream.
- chatgpt.com HTTP/2 SETTINGS fingerprint; Gemini host profile.

---

## Scope policy (what belongs in this fork)

The fork diverges on **exactly two** things. Anything else should track upstream —
every extra line is merge tax paid forever.

1. **Fingerprint** — making our wire traffic byte-identical to the real `codex` /
   `claude` client (headers, body shape/encoding, TLS/JA3, header order, UA,
   session-id/thread-id, identity-confuse, device profiles).
2. **FBT / error handling** — the first-byte-timeout stack and which failures may
   change credential availability. See the section above.

Audited 2026-07-17 (77 files, +8145/−418, 96 commits): the policy mostly holds.
About half the changed files are **new files** and cost nothing. Notes:

- `kimi_executor.go` **is in scope** despite us not using kimi: upstream hardcodes the
  device id `"cli-proxy-api-device"` when kimi-cli's real one is absent — an identical
  literal across every CPA install worldwide, i.e. a self-identifying beacon. We derive a
  hostname-stable UUID instead. (Auto-merges; upstream `106270be` touches a different hunk.)
- `xai_executor.go` is **out of scope** — a functional fix (`xai.x-search-response-filter`
  for grok multi-turn truncation), not fingerprint or FBT. Cost is ~0 (auto-merges), so it
  is tolerated rather than reverted.
- **Prefer upstreaming over forking.** A fix that is generally useful should become a PR to
  `router-for-me/CLIProxyAPI` — that removes fork surface permanently. Precedent: the uTLS
  h2 connection-leak fix (PR #4369). The kimi device-id beacon is a good candidate.

> **Watch upstream's direction.** `106270be` rewrote the kimi headers from
> `User-Agent: KimiCLI/1.10.6` to `User-Agent: CLIProxyAPI/<version>`, changing the comment
> from *"Headers match kimi-cli client for compatibility"* to *"Headers identify CLIProxyAPI"*.
> Upstream is **deliberately de-impersonating**. That is the opposite of this fork's purpose.
> If that policy ever reaches the codex path, merges stop being mechanical and become a
> fork-or-leave decision — the fork's real long-term risk, larger than any conflict count.

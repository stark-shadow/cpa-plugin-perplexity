# Perplexity — a CLIProxyAPI native provider plugin

Expose a signed-in **Perplexity web** session through CPA's OpenAI-compatible chat API.
This is a standalone personal repository, `github.com/lzt404/cpa-plugin-perplexity`.
It is not the Perplexity API (Sonar) and does not use API keys or browser OAuth.

## Features and limits

- Non-streaming `chat.completion` responses and incremental `chat.completion.chunk` deltas.
- Text-only prompts, conversation flattening, rune-safe truncation, optional source links.
  Multimodal arrays keep text parts and ignore `image_url` parts. Tools, attachments,
  sampling controls (`temperature`, `max_tokens`), and cross-request thread continuation
  are not supported; accepted sampling fields do not change the upstream web request.
- Catalog discovery with a six-hour memory/disk cache and four shipped fallback models.
- Session-cookie import, validity checks, refresh probes, normalized quota metrics.
- Browser-like headers and the host's standard HTTP transport; **no TLS fingerprint library**.
- Usage and token counting are **estimates: ceil(Unicode characters / 4)**, not billing data.
  `executor.count_tokens` puts `{input_tokens: N}` in the SDK executor response payload
  (and exposes `input_tokens` at the RPC result level for direct callers).

Web endpoints can change. Pro/Research and explicit models may require a paid plan;
advertising a model does not grant access. Use only accounts you are authorized to use.
All local tests use hermetic fake host callbacks; no live-account validation is claimed.

## Requirements and install

CPA v8 with native ABI 1 / schema 10 and host HTTP operation, stream, and auth-storage
callbacks. The SDK is pinned to `v8.0.61` in `go.mod`, verified with `go list -m -versions`
on November 9, 2026. Source builds require Go 1.26.10+, a C compiler and Python 3.

1. Build or obtain the platform ZIP. Its root contains exactly `perplexity.so`,
   `manifest.json`, both READMEs and `LICENSE`. The macOS `.so` is a Mach-O dynamic library.
2. Place `perplexity.so` in CPA's plugin directory. Configure and restart CPA:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    perplexity:
      enabled: true
      priority: 0
      language: en-US
      sources: [web]                 # web, scholar, social
      include_sources: true
      catalog_ttl: 6h
      request_timeout: 180s          # raise to e.g. 10m for deep research
      max_prompt_chars: 48000        # oldest text removed, with [...truncated...]
      max_concurrency: 2             # concurrent upstream asks per session
      pro_query_limit: 0            # unknown; do not invent a denominator
      research_query_limit: 0
      exclude_models: []            # exact public or internal model IDs
      base_url: https://www.perplexity.ai
      user_agent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
```

Unknown YAML keys, duplicate keys, invalid values and additional documents are rejected
at registration/reconfiguration. The host-injected `enabled`, `priority`, `store` keys
are accepted. `max_prompt_chars` must fit the 17-character truncation marker (minimum 17).
`base_url` must be an HTTP(S) URL with a host, without userinfo/query/fragment; a mirror path prefix is allowed.
Point mirrors only at trusted servers: requests carry the account cookie.

## Import a signed-in session

Open `/v0/resource/plugins/perplexity/manage` on the CPA host. This static panel is public,
but all of its management operations require normal CPA management authentication.
Enter the management key if needed; it is kept only in page memory, not browser storage.

Sign in to `perplexity.ai` in your browser. In DevTools → Application → Cookies, copy
`__Secure-next-auth.session-token`. Paste the token, a full `Cookie:` header, cookie text,
or a JSON cookie object into the panel. It clears the textarea before sending and never
prints the token back. Do not send cookies through chat, screenshots, logs or issue reports.

`auth.login.start` deliberately returns `unsupported_operation` (501): server-side OAuth
cannot capture the browser cookie. Import is the actual sign-in path. Sessions are refreshed
by probing `/api/auth/session`, not by minting new tokens. Expired sessions need re-import.

## Management API

All paths below require CPA management authentication under `/v0/management`:

| Method | Path | Body / result |
| --- | --- | --- |
| POST | `/plugins/perplexity/auth/import` | `{session_token?, cookies?, label?}` → 201 `{email, plan, file}` |
| GET | `/plugins/perplexity/auth/status` | `{accounts:[{file,email,plan,validated_at,valid}]}` |
| POST | `/plugins/perplexity/auth/test` | `{file}` → one account validity record |
| GET | `/plugins/perplexity/catalog` | `{tier, models}` |

`cookies` can be cookie text or a JSON object. `session_token`, when set, overrides the
session cookie in the jar. Extra cookies are preserved. Saved names are deterministic:
`perplexity-<first 12 hex SHA-256 characters of session>.json`. Import validates the session
then saves through `host.auth.save`, allowing CPA to discover the auth file normally.
Remove accounts through CPA auth-file management; no plugin DELETE route is provided.
Each validity probe has an eight-second timeout; one failed probe does not fail the status
response. `valid: false` can mean expired **or temporarily unreachable**, not proof of expiry.

Static browser resources `/manage`, `/panel.css`, `/panel.js` live under
`/v0/resource/plugins/perplexity/`, with no CDN or external scripts.

## Models and execution

| Public ID | Upstream mode | Model preference |
| --- | --- | --- |
| `pplx-auto` | concise | turbo |
| `pplx-pro` | copilot | pplx_pro |
| `pplx-reasoning` | copilot | pplx_reasoning |
| `pplx-deep-research` | copilot | pplx_alpha |
| `pplx-<internal>` | copilot | catalog internal ID |

The four base models are first, followed by tier-aware search/research models sorted by
label and ID. Max accounts see all tiers; ASI, browser-agent, agentic-research and other
non-chat modes are not advertised. Configured exclusions also prevent execution.
The catalog is persisted best-effort as `<auth_dir>/.perplexity-catalog.json` with mode 0600.
Fetch failures fall back to the shipped list. Single user messages pass through unchanged;
multi-turn prompts become `System: ...\n\nUser: ...\n\nAssistant: ...` transcripts.

Non-streaming responses include a `perplexity` object with `thread_url` and source links.
When enabled, sources append as `\n\n---\n**Sources**\n- [title](url)` in either mode.
The plugin sends **bare JSON** via `host.stream.emit`; CPA's OpenAI translator and handler
add SSE framing and the `[DONE]` marker. Empty, malformed, or failed upstream answers are
errors, not empty successes. Mid-stream errors close with a structured `error_detail`.

Upstream 401/403 or a LOGIN upsell becomes `account_unavailable` (401, naming the import
route); 429 becomes `upstream_rate_limited` with Retry-After seconds; protocol failures are
502; transport timeouts are 504. Client cancellation is `client_closed` (499), not an
account penalty. Refresh transport failures are retryable service failures (503), not
`reauth_required`; only an invalid session produces the latter (401).

## Quota

A session probe supplies the plan. `/rest/rate-limit/all` and `/rest/user/settings` supply
Pro, Research, Labs, agentic research, upload and page counts. Paid credits are queried only
after a valid session probe and failure is tolerated. Metrics retain the raw known counts.
Pro/Research buckets appear only if `pro_query_limit` / `research_query_limit` is configured
above zero, with `remainingFraction = remaining / configured_limit`. No monthly limit is
invented. These configured denominators are operator assumptions, not upstream promises.

## Build, tests and releases

```sh
export PATH="$HOME/.local/go/bin:$PATH"
bash build.sh
# dist/1.0.0-linux-amd64/perplexity.so
go test -race -v ./...
python3 acceptance/abi_smoke.py dist/1.0.0-linux-amd64/perplexity.so
```

`build.sh` runs hermetic race tests first, then
`CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/<version>-<goos>-<goarch>/perplexity.so .`,
removes the generated header, packages the five root members and writes ZIP checksums.
`CPA_CORE=/path/to/CLIProxyAPI bash build.sh` optionally creates `.build/go.work` for a
local SDK checkout without modifying `go.mod`. No absolute local paths are committed.
`make build`, `make test`, `make package`, `make clean` are available.

The tag workflow builds Linux amd64/arm64 (arm64 with `aarch64-linux-gnu-gcc`) and macOS
amd64/arm64, packages `perplexity_<version>_<goos>_<goarch>.zip`, and publishes `checksums.txt`.
Tests run on the build host architecture even during cross-compilation. Tags must match
the manifest version. See `acceptance/REPORT.md` for executed checks and outstanding live
account/platform acceptance, including missing local reference material.

## License

MIT. The build uses the Go standard library and the published CPA SDK plus yaml.v3.
No dependencies, hosting targets or credentials are copied from the reference plugin.

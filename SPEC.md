# cpa-plugin-perplexity — build spec

A CLIProxyAPI (CPA) native plugin that exposes **Perplexity web (perplexity.ai)** as an
OpenAI-compatible provider. It is a *standalone plugin repository* on the personal
account: `github.com/stark-shadow/cpa-plugin-perplexity`. It is **not** part of
EasyCLIProxyAPI and not part of any `router-for-me` repo.

Read `/tmp/omc-ref/go/` (upstream plugin `omc-workbuddy-oauth`) as the reference for
ABI wiring, dispatch, streaming pump, and management routes. Read
`~/cpa-project/cpa-plugin-quota-balance/` as the reference for repo scaffold,
`build.sh`, `package.py`, `manifest.json`, tests, and the `host.http.do` bridge.

Everything below marked **VERIFIED** was measured live from the E5 host on
2026-11-09 with `curl_cffi` impersonating Chrome and with Go's stdlib transport.

---

## 1. Repository layout

```
main.go            C-ABI entry: cliproxy_plugin_init / <id>Call / <id>Free / <id>Shutdown
dispatch.go        method table + registration payload
config.go          strict YAML config (KnownFields(true), single document)
auth.go            credential model, parse/login/import/refresh
catalog.go         model catalog fetch, cache, OpenAI model mapping
executor.go        OpenAI chat -> Perplexity /rest/sse/perplexity_ask, stream + non-stream
sse.go             incremental SSE decoder + answer/source accumulator
quota.go           /rest/rate-limit/all + /rest/user/settings -> normalized quota
management.go      authenticated management routes + browser panel resources
state.go           auth file persistence helpers
policy_test.go     unit tests for dispatch, config, sse parsing, model mapping
go.mod             module github.com/stark-shadow/cpa-plugin-perplexity
manifest.json | build.sh | package.py | Makefile | README.md | README.zh-CN.md | LICENSE
acceptance/REPORT.md | acceptance/matrix.json
.github/workflows/build-and-release.yml
```

IDs: plugin id `perplexity`, provider/credential key `perplexity`, dynamic library
`perplexity.so`. Module path `github.com/stark-shadow/cpa-plugin-perplexity`.
Pin SDK: newest published tag of `github.com/router-for-me/CLIProxyAPI/v8`
(currently v8.0.59; verify with `go list -m -versions`). `manifest.json`
schema_version 10, abi_version 1, author `lzt404`,
repository `https://github.com/stark-shadow/cpa-plugin-perplexity`.

`go` is at `$HOME/.local/go/bin/go` in a non-login SSH shell.

---

## 2. Upstream contract (VERIFIED)

Base URL `https://www.perplexity.ai`. Version parameter `2.18`.

### 2.1 Anti-bot
- Plain browser-like `User-Agent` (Chrome 136+) plus standard `sec-ch-ua` /
  `sec-fetch-*` headers is enough from this host for the **REST endpoints**.
- Go's stdlib `http.Client` with those headers returns 200 for `/`, the model
  catalog, and a working SSE answer. **No uTLS, no curl_cffi, no cf_clearance
  cookie is required.** Do not add a TLS fingerprint library.
- Send: `User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36`,
  `Accept-Language: en-US,en;q=0.9`, `sec-ch-ua: "Chromium";v="136", "Not=A?Brand";v="24"`,
  `sec-ch-ua-mobile: ?0`, `sec-ch-ua-platform: "Windows"`.

### 2.2 Session probe
`GET /api/auth/session`
- Unauthenticated: HTTP 200, body `{}`.
- Authenticated (cookie `__Secure-next-auth.session-token=<token>`): JSON with a
  `user` object. Expected fields (defensive decode, all optional):
  `user.id`, `user.name`, `user.email`, `user.subscription_tier` (`pro`/`max`/
  `free`), and possibly `user.emailVerified`. Treat any non-empty `user` with an
  `id` or `email` as a valid session; treat `{}` as invalid.

### 2.3 Ask (SSE)
`POST /rest/sse/perplexity_ask`
Headers:
```
accept: text/event-stream
content-type: application/json
origin: https://www.perplexity.ai
referer: https://www.perplexity.ai/
accept-language: en-US,en;q=0.9
x-perplexity-request-endpoint: https://www.perplexity.ai/rest/sse/perplexity_ask
x-perplexity-request-reason: ask-query-state-provider
x-perplexity-request-try-number: 1
x-request-id: <frontend uuid>
Cookie: __Secure-next-auth.session-token=...
<the browser headers from 2.1>
```
Body (VERIFIED working unauthenticated; same shape authenticated):
```json
{"query_str":"<text>","params":{
  "attachments":[],
  "frontend_context_uuid":"<uuid4>",
  "frontend_uuid":"<uuid4>",
  "is_incognito":false,
  "language":"en-US",
  "last_backend_uuid":null,
  "mode":"concise",
  "model_preference":"turbo",
  "query_source":"home",
  "source":"default",
  "sources":["web"],
  "version":"2.18"
}}
```
`mode`: `"concise"` for the free/auto path, `"copilot"` for Pro/Research and any
explicit model. `model_preference` is the internal catalog id (`turbo`,
`pplx_pro`, `pplx_reasoning`, `pplx_alpha`, `gpt6_astra`, `claude50sonnet`, …).

SSE events: `event: message` frames carrying one JSON object, then
`event: end_of_stream` with `data: {}`. Some deployments also send a bare
`data: [DONE]`.

A `message` frame contains (defensive decode):
- `backend_uuid`, `thread_url_slug`, `cursor` — thread identity.
- `text_completed` (bool) and `final` (bool) — answer finished.
- `status` — `"COMPLETED"` when done; `FAILED`/`ERROR`/`CANCELLED` mean failure.
- `blocks`: array. Two shapes matter:
  - `{"markdown_block":{"progress":"IN_PROGRESS|DONE","chunks":["...","..."],
     "chunk_starting_offset":0,"answer":"<full text>"}}`
     `chunks` is either a **full snapshot** (offset 0) or only the pieces appended
     at `chunk_starting_offset`; maintain a cumulative chunk array exactly as the
     reference accumulator does. If `answer` is present it is authoritative.
  - `{"web_result_block":{"web_results":[{"title","url","snippet",...}]}}`
     Use for source links.
- `text`: legacy array of workflow steps; `step_type == "FINAL"` carries
  `content.answer` (a JSON string or plain string).
- `error` — non-empty means a failed generation.
- `upsell_information` — when it exists with `upsell_type` `LOGIN`, the request
  was served anonymously (the answer is a signup teaser). Treat that, when a
  credential was supplied, as `account_unavailable` (401) so the credential is
  surfaced as broken instead of returning "Sign up and repeat your request."

When the stream ends without any answer text, return an error, never an empty
completion.

### 2.4 Model catalog
`GET /rest/models/config/v2?version=2.18&source=default` → JSON. Relevant parts:
- `models`: map `internal_id -> {label, description, mode, provider}`. Modes seen:
  `search` (110), `asi` (31), `research` (2), `study`, `studio`,
  `browser_agent`, `agentic_research`.
- `search_config`: array of `{label, description, subscription_tier:
  "pro"|"max"|null, non_reasoning_model, reasoning_model, is_default}`. This is
  the **tier-aware** list; prefer it for the catalog, and fall back to `models`
  with `mode` `search`/`research` when `search_config` is absent.
- `default_models`: `{search:"turbo", research:"pplx_alpha", ...}`.

Cache the catalog in memory for `catalog_ttl` (default `6h`) and persist to
`<auth_dir>/.perplexity-catalog.json` best-effort. On any fetch failure fall back
to the shipped list:
`pplx-auto -> (concise, turbo)`,
`pplx-pro -> (copilot, pplx_pro)`,
`pplx-reasoning -> (copilot, pplx_reasoning)`,
`pplx-deep-research -> (copilot, pplx_alpha)`.

### 2.5 Quota
`GET /rest/rate-limit/all` → JSON:
```json
{"free_queries":{"available":true,"remaining_detail":{"kind":"not_provided"}},
 "model_specific_limits":{},
 "remaining_agentic_research":0,"remaining_labs":0,
 "remaining_pro":0,"remaining_research":0,
 "sources":{"source_to_limit":{"<source>":{"monthly_limit":0,"remaining":0}, ...}}}
```
`GET /rest/user/settings` → `{pages_limit, upload_limit, create_limit,
max_files_per_user, ...}`.
Both work unauthenticated. Authenticated, `remaining_*` reflect the account.
`GET /rest/billing/credits` requires auth and is for paid credits — call it only
when the session is valid, and tolerate failure.

Quota mapping (`QuotaFetchResponse`):
- `Subscription`: `Plan` from the session tier (`pro`/`max`/`free`).
- `Summary` metrics (numbers): `pro_searches_remaining`, `research_remaining`,
  `labs_remaining`, `agentic_research_remaining`, `uploads_remaining`
  (`upload_limit`), `pages_remaining` (`pages_limit`).
- `Groups`: one group `Perplexity` with buckets only where a denominator is
  known. Pro/research monthly limits are not published by the endpoint, so use
  the optional config `pro_query_limit` / `research_query_limit` (default `0` =
  unknown); when `0`, emit no bucket for that window instead of inventing a
  fraction. When set, `remainingFraction = remaining/limit` and
  `Description` names the raw counts.

---

## 3. Credential model

`StorageJSON` of one auth file:
```json
{
  "type": "perplexity",
  "session_token": "<cookie value>",
  "cookies": {"__Secure-next-auth.session-token": "..."},  // full jar when imported
  "email": "user@example.com",
  "name": "Display Name",
  "account_id": "....",
  "plan": "pro",
  "validated_at": "2026-11-09T07:00:00Z"
}
```
- `session_token` is authoritative; `cookies` may carry extra cookies
  (`cf_clearance`, `__Host-pplx-last-active-account`) and must be sent verbatim.
- Parse accepts the plain token, `name=value; name2=value2` cookie text, a JSON
  object of cookies, or an already-serialized credential. Also accept a full
  `Cookie:` header value.
- Auth label: `email` when known, else `Perplexity (<first 6 of account id>)`,
  else `Perplexity`.
- Auth id/file name: `perplexity-<sha256(session_token)[:12]>.json`, so re-import
  of the same session updates the same file.
- Metadata: `{"email":..., "type":"perplexity"}`; `oauth_provider` unset.
- **Secret hygiene**: never log tokens or cookie values; error strings must not
  contain them.

### 3.1 Sign-in
CPA's browser OAuth cannot capture a cookie server-side, so:
- v1.1.0 supersedes the original unsupported-login behavior with a companion MV3
  extension using `chrome.cookies`, not OAuth/PKCE. See
  `acceptance/LOGIN-EVIDENCE.md` for the supplied upstream evidence.
- `auth.login.start` derives the core origin from `BaseURL` and returns the plugin
  `/login?state=…` page. State is purpose-bound, sealed, under 128 characters, and
  contains only a 16-byte flow nonce and expiry, never a cookie.
- `auth.login.poll` remains pending until the authenticated import route validates
  and saves the session with that state, then returns AuthData. Flows live only in
  bounded process memory, expire after ten minutes, and do not survive a restart.
- Manual paste import remains unchanged. The extension is shipped from `extension/`
  separately, never in the five-member plugin ZIP. Direct authenticated fetch falls
  back to a checked, same-origin `/manage` postMessage import when fetch is blocked.

### 3.2 Parse / refresh
- `auth.parse`: recognize `"type":"perplexity"` (or a bare token when the file is
  named `perplexity*.json`), validate shape only (no network), return AuthData.
- `auth.refresh`: re-probe `/api/auth/session`. On success update
  `email`/`name`/`account_id`/`plan`/`validated_at` and return AuthData with
  `NextRefreshAfter` = now + 6h. On `{}` return an error whose message says the
  session expired and must be imported again; dispatch maps it to
  `reauth_required` (401). On transport failure return 503 without marking the
  credential dead.

---

## 4. Model provider

Capabilities:
```json
{"auth_provider": true, "model_provider": true, "executor": true,
 "executor_model_scope": "oauth", "executor_input_formats": ["openai"],
 "executor_output_formats": ["openai"], "quota_provider": true,
 "management_api": true}
```
`model.static` and `model.for_auth` return the same catalog for provider
`perplexity`:
- Always: `pplx-auto`, `pplx-pro`, `pplx-reasoning`, `pplx-deep-research`.
- From the tier-aware catalog: `pplx-<internal_id>` for every `search`/`research`
  model whose `subscription_tier` is `null`, `pro`, or compatible with the
  credential tier (`max` credentials see all). Never advertise an `asi` /
  `browser_agent` / `agentic_research` model as a chat model; skip those modes.
- `ModelInfo`: `ID`, `Object: "model"`, `OwnedBy: "perplexity"`,
  `DisplayName: label`, `Name: internal_id`, `Description`, `Type: "chat"`,
  `SupportedGenerationMethods: ["chat.completions"]`,
  `SupportedInputModalities: ["text"]`, `SupportedOutputModalities: ["text"]`.
- Deterministic ordering (auto/pro/reasoning/deep-research first, then label).

Mapping `pplx-*` back to upstream:
| model id | mode | model_preference |
|---|---|---|
| `pplx-auto` | concise | turbo |
| `pplx-pro` | copilot | pplx_pro |
| `pplx-reasoning` | copilot | pplx_reasoning |
| `pplx-deep-research` | copilot | pplx_alpha |
| `pplx-<internal>` | copilot | `<internal>` |

Unknown model → `unknown_model` 400. A `pplx-` prefix is stripped when the host
sends a prefixed alias.

---

## 5. Management API + panel

Register under `/v0/management` (paths relative, plugin prefix included):
- `POST /plugins/perplexity/auth/import`
  body `{"session_token":"...", "cookies":"name=value; ...", "label":"optional", "state":"optional"}`
  (or a `?state=` query; conflicting states are rejected)
  → validate against `/api/auth/session` → persist via
  `host.auth.save` (`{"name":"perplexity-<hash>.json","json":<credential>}`) →
  `201` with `{email, plan, file}`. Invalid session → `400` with a message that
  says the cookie is not a signed-in Perplexity session. Token never echoed.
- `GET /plugins/perplexity/auth/login/poll?state=…` → sanitized
  `{status, message, email}` for the browser; normal management authentication.
- `GET /plugins/perplexity/auth/status`
  → `{"accounts":[{"file","email","plan","validated_at","valid"}]}` (live probe,
  8s timeout, never fails the whole request).
- `POST /plugins/perplexity/auth/test` body `{"file":"..."}` → probe one account.
- `GET /plugins/perplexity/catalog` → the advertised model list + tier.
- `DELETE` is out of scope for v1; removal stays a normal CPA auth-file delete.

Browser resources (`/v0/resource/plugins/perplexity/`), unauthenticated:
- `/login` — self-contained extension setup/capture page, management key field,
  Perplexity login button, manual fallback preserving state, two-second status
  polling and a connected-email success message. CSP hashes authorize inline CSS/JS.
- `/manage` — small self-contained HTML panel (inline CSS/JS, no external CDN):
  a textarea for the cookie, an Import button, a table of accounts with
  Valid/Expired state and a Test button, and a note explaining how to copy the
  `__Secure-next-auth.session-token` cookie from DevTools. The page calls the
  management routes with `credentials: "same-origin"`; it must not print the
  token back.
- `/panel.css`, `/panel.js` served as separate resources if it keeps the handler
  clean.

---

## 6. Executor

### 6.1 Request
`ExecutorRequest.Payload` is an OpenAI `chat/completions` body (the host declares
input format `openai`). Steps:
1. Decode `{model, messages, stream, max_tokens?, temperature?}` strictly enough
   to reject a non-object or missing messages.
2. Resolve `model` → (mode, model_preference); unknown → 400.
3. Flatten messages to a single `query_str`:
   - If there is exactly one user message and no system: use its text.
   - Otherwise build a transcript: `System: ...\n\nUser: ...\n\nAssistant: ...\n\nUser: ...`
     (roles lowercased; multimodal content arrays: keep `text` parts, ignore
     `image_url` parts and note it in the description).
   - Cap the prompt at `max_prompt_chars` (default 48000); truncate from the
     front with a `[...truncated...]` marker, never split a UTF-8 rune.
4. Build the upstream body (§2.3) with fresh uuid4s and the credential cookie.
5. `mode` selection: `concise` only for `pplx-auto`; `copilot` for everything
   else. If the credential plan is `free`, `copilot` requests will be refused by
   upstream — map that to `account_unavailable`/429 as the upstream says.

### 6.2 Non-streaming (`executor.execute`)
Use the host `HTTPClient.Do` bridge (or `host.http.do` via RPC; follow
quota-balance's `hostCall`). Read the SSE body, accumulate the answer, and return
an OpenAI completion JSON:
```json
{"id":"chatcmpl-...","object":"chat.completion","created":<unix>,
 "model":"<requested model>",
 "choices":[{"index":0,"message":{"role":"assistant","content":"<answer>"},
             "finish_reason":"stop"}],
 "usage":{"prompt_tokens":N,"completion_tokens":M,"total_tokens":N+M},
 "perplexity":{"thread_url":"https://www.perplexity.ai/search/<slug>",
               "sources":[{"title":"...","url":"..."}]}}
```
Usage is estimated (`ceil(chars/4)`), which is acceptable for a web-scraped
provider; say so in the README. Append a `Sources` markdown list to `content`
only when `include_sources` config is true (default true), formatted as
`\n\n---\n**Sources**\n- [title](url)`.

### 6.3 Streaming (`executor.execute_stream`)
Follow `omc-workbuddy-oauth`'s pump:
- `host.http.do_stream` (`{"host_callback_id":..., "request":{...}}`) opens the
  upstream stream and returns `{status_code, headers, stream_id}`.
- Non-2xx or `content-type: application/json` → drain, classify, return a
  structured error before answering the client.
- Read `host.http.stream_read` in a loop; feed bytes to an incremental SSE
  decoder; compute the **delta** of the cumulative answer on every frame and
  emit one OpenAI chunk:
  `{"id":"chatcmpl-...","object":"chat.completion.chunk","created":...,
    "model":"<model>","choices":[{"index":0,"delta":{"content":"<delta>"},
    "finish_reason":null}]}`
  First chunk carries `delta":{"role":"assistant"}`. A final chunk carries
  `delta":{},"finish_reason":"stop"`.
- Emit bare JSON payloads via `host.stream.emit` — the host adds `data: ` and the
  `[DONE]` marker (verify in `internal/pluginhost/adapters_executors.go` and the
  openai translator; if the host expects the plugin to include SSE framing, do
  that instead and document it in a code comment). Close with
  `host.stream.close`; on a mid-stream failure pass `error_detail` so CPA can
  route on the status.
- If no text can be produced, close the stream with an error rather than a
  silent empty answer.
- Honour client cancellation: a read error containing `context canceled` /
  `client closed` maps to 499 (`client_closed`), so the credential is not
  penalised.

Error mapping (both modes): 401/403 from upstream → `account_unavailable` 503 or
401 with a message naming the import route; 429 → `upstream_rate_limited` with
`Retry-After` forwarded when present; login upsell inside a 200 stream → 401;
protocol garbage → 502 `upstream_protocol_error`; transport timeout → 504;
anything else → 502.

`executor.count_tokens`: return `{input_tokens: estimate}` from the request; no
network.

### 6.4 Config
Strict YAML under `plugins.configs.perplexity` (allow the host-injected
`enabled`, `priority`, `store` keys like quota-balance's `rawSettings`):
- `language` (string, default `en-US`)
- `sources` (array, default `["web"]`; allowed `web`, `scholar`, `social`)
- `include_sources` (bool, default true)
- `catalog_ttl` (string duration, default `6h`)
- `request_timeout` (string duration, default `180s`; deep research needs more —
  document raising it)
- `max_prompt_chars` (integer, default 48000)
- `max_concurrency` (integer, default 2, bounds simultaneous upstream asks per
  credential)
- `pro_query_limit`, `research_query_limit` (integer, default 0 = unknown)
- `exclude_models` (array, never advertised)
- `base_url` (string, default `https://www.perplexity.ai`, for a future mirror;
  validate scheme+host)
- `user_agent` (string, default the Chrome UA in §2.1)

---

## 7. Build, test, package

Follow the skill `cpa-native-plugin` (sections 2–6) and
`cpa-plugin-quota-balance`'s `build.sh`/`package.py`:
- `build.sh`: optional `CPA_CORE` via a temporary `.build/go.work`; then
  `go test -race -v ./...`, then
  `CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o
  dist/<version>-<goos>-<goarch>/perplexity.so .`, strip the generated `.h`.
- Tests must cover: registration payload, strict config parsing, cookie/token
  parsing (incl. rejection of junk), model id mapping both directions, SSE
  accumulation (snapshot, offset append, legacy `text`, upsell, failure),
  delta streaming, quota mapping, and error classification. No live network in
  `go test`.
- `package.py <stage> <zip>`: zip root contains exactly
  `perplexity.so` + `manifest.json` + READMEs + LICENSE. CI builds
  `linux/amd64`, `linux/arm64` (`CC=aarch64-linux-gnu-gcc`), `darwin/arm64`,
  `darwin/amd64`, names zips `perplexity_<version>_<goos>_<goarch>.zip`, and
  publishes `checksums.txt`. Workflow triggers on `v*` tags.
- `Makefile`: `build`, `test`, `package`, `clean`.

Write `acceptance/matrix.json` (cases: register, config, import, model list,
non-stream answer, stream answer, quota) with `pass`/`fail` per case and
`acceptance/REPORT.md` stating exactly what was executed locally versus what
still needs a live account. Do not claim live-account results that were not run.

---

## 8. Deliverable for this run

1. Full repo content in `~/cpa-project/cpa-plugin-perplexity`, committed
   (author `lzt404`, message like `feat: Perplexity web provider plugin`).
2. `build.sh` passes: `go test -race ./...` prints `ok`, and a linux/amd64
   `perplexity.so` exists.
3. Report: files created, test output tail, the exact `go build` command result,
   and any spec item you could not complete with the reason.

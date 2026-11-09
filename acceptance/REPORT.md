# Local acceptance report

Date: **2026-11-09**, host platform **linux/amd64**.
Plugin: `perplexity` **1.1.0**, native ABI **1**, RPC schema **10**.

## v1.1.0 cookie-capture login — this run

Implemented real `auth.login.start` / `auth.login.poll`, a public `/login` page,
and the separately shipped unpacked Chrome/Edge MV3 `extension/`. The existing
manual paste route is preserved. Login state is a 66-character AES-GCM sealed
blob with an authenticated purpose, a 16-byte nonce and short expiry; no cookie
is included. At most 64 flows live in memory, including completed AuthData, and
restart/shutdown or ten-minute expiry ends them. Import only completes a state
after session validation and successful host storage. A separate authenticated
browser polling route returns status/email, never credential storage.

The extension attempts a Bearer-authenticated POST first. A fetch/network/CORS
failure opens the configured same-origin `/manage` page, with a one-use in-memory
postMessage handoff checked against source, origin, configured base and state.
HTTP errors are shown directly; no authentication or CORS policy is weakened.
Management key/base settings persist in local extension storage; cookies do not.
The plugin ZIP still contains exactly five root members and no extension files.

The reference `/tmp/omc-ref/go/state.go` **is available for this run**; its compact
AES-GCM state approach was adapted with a purpose and a process-local key. The
historical missing-reference notes below describe v1.0.0, not this run.
The task owner's verified upstream facts were accepted without re-probing or
reverse engineering: see [LOGIN-EVIDENCE.md](LOGIN-EVIDENCE.md).

Executed locally, hermetically (no live account or upstream HTTP):

- `export PATH=$HOME/.local/go/bin:$PATH; bash build.sh`: full `go test -race -v
  ./...` reports `PASS` / `ok github.com/stark-shadow/cpa-plugin-perplexity`, then builds
  and packages **v1.1.0 linux/amd64**, exit **0**.
- `node --test extension/tests/*.test.cjs`: 15 passing fake-browser/VM tests,
  covering cookie selection, state association, host/frame rejection, persisted
  settings, exact/redacted HTTP errors, CORS fallback, one-use/expired handoffs,
  same-origin panel import, the content-script hook and manual fallback.
- Go login tests cover state round-trip/purpose/tampering/length, origin derivation,
  pending → saved import → repeated success, query/JSON state, rejected conflicts
  and concurrent imports, failure/retry, storage failure, expiry/restart/eviction,
  CSP hash authorization and unchanged token/Cookie-header/JSON paste forms.
- `python3 acceptance/abi_smoke.py dist/1.1.0-linux-amd64/perplexity.so`: **PASS**,
  including real native ABI login start → pending → fake-host import → success.
- Archive inspection, `file`, and `nm -D --defined-only`: exactly the five plugin
  ZIP root members, Linux x86-64 ELF shared library, four required ABI exports.
- `go test -race -count=3 ./...`, `go vet ./...`, JavaScript syntax checks with
  `node --check`, `bash -n build.sh`, Python bytecode compilation and
  `git diff --check` also pass. Go commands use the PATH export shown above.

Exact shared-library build command (exit **0**):

```sh
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/1.1.0-linux-amd64/perplexity.so .
```

Artifacts: `dist/1.1.0-linux-amd64/perplexity.so`,
`dist/perplexity_1.1.0_linux_amd64.zip`, `dist/artifacts/checksums.txt`.

**Not run for v1.1.0:** a live signed-in account, actual Chrome/Edge unpacked-load
and HttpOnly-cookie capture, real CPA/browser CORS behavior, a live login or model
request, non-linux/amd64 builds, publication, installation, deployment or remote
push. Browser tests mock extension APIs/DOM; they are not a real-browser result.
Perplexity does not expose the required public OAuth/device/PKCE credential, so
that protocol cannot be implemented; this is explicitly a cookie capture/import.

## Historical v1.0.0 record

The remaining sections retain the original v1.0.0 acceptance record. Its extra
commands, paths and missing references are historical, not claims of rerunning
those commands for v1.1.0.

## Scope and missing required references

`SPEC.md` was read end to end. The quota-balance scaffold, host-call bridge,
policy tests and README structure were read without modifying that repository.
The authoritative local `sdk/pluginapi/types.go` and `sdk/pluginabi/types.go`
were inspected, as were the actual host registration, HTTP-operation, streaming,
OpenAI translator and OpenAI handler implementations. Published SDK types and
`pluginabi.Method*` constants are used rather than copied public Go structures.

Two requested readings could not be completed:

- **`/tmp/omc-ref/go/` does not exist on this host.** Therefore its `main.go`,
  `dispatch.go`, `host_http.go`, `executor.go`, `completion.go`, `state.go` and
  `auth.go` could not be read. Alternate local copies were not found in the
  reference/project locations searched. The actual host implementation and
  quota-balance bridge were used to verify the ABI/RPC contracts instead.
- **The `cpa-native-plugin` skill is not installed/available.** Its sections 2–6
  could not be read, so compliance with unseen skill text is not claimed.
  Build/test/package behavior follows the explicit SPEC and available scaffold.

No hosting target, auth file, browser session, token or cookie was copied from a
reference project. No Perplexity endpoint was re-probed. The SPEC's VERIFIED
upstream facts were accepted as supplied; no TLS-fingerprint dependency was added.

## SDK resolution

Executed `go list -m -versions github.com/router-for-me/CLIProxyAPI/v8`.
The newest returned published tag was **v8.0.61** (v8.0.59 was not the newest).
Executed `go get github.com/router-for-me/CLIProxyAPI/v8@v8.0.61 gopkg.in/yaml.v3@v3.0.1`
and `go mod tidy`; `go.mod` pins that SDK, and `go.sum` is included.
The module selects Go **1.26.10** through the installed Go toolchain mechanism.

## Executed checks

All Go tests are hermetic: HTTP, HTTP streams, authentication storage and output
streams are in-process fake host callbacks. They never connect to Perplexity or
any other live service. SDK module resolution is a build dependency operation,
not an upstream acceptance probe.

1. `export PATH=$HOME/.local/go/bin:$PATH; bash build.sh`:
   race tests, c-shared library build, header removal, ZIP packaging and checksums.
2. `go test -race -count=3 ./...`: repeated race testing, including per-credential
   concurrency, operation cancellation, stream shutdown and SDK wire response shapes.
3. `CPA_CORE=$HOME/cpa-platform/CLIProxyAPI bash build.sh`: verified the optional
   temporary `.build/go.work` integration against the local host checkout.
   A final ordinary build restores the delivered artifact to the pinned published SDK.
4. `go vet ./...`, `GOWORK=off go mod verify`, `bash -n build.sh`,
   `python3 -m py_compile package.py acceptance/abi_smoke.py`.
5. `python3 acceptance/abi_smoke.py dist/1.0.0-linux-amd64/perplexity.so`:
   loads the real shared library with a fake C host, validates exported functions,
   rejects wrong ABI/null pointers, verifies the 1 MiB boundary and strict YAML,
   performs real plugin-to-host catalog callbacks, verifies use of the host free
   function, and checks idempotent shutdown. No network is used.
6. `file`, `nm -D --defined-only`, `python3 -m zipfile -l` on the produced artifacts:
   Linux x86-64 ELF shared object; all four required exports; ZIP contains exactly
   the five required root entries, and the generated C header is absent.
   `sha256sum -c checksums.txt` in `dist/artifacts` also passes.

Test coverage includes registration, strict single-document config, credential
forms/junk rejection, stable auth names, session refresh/expiry, catalog mapping
both ways/tier filtering/exclusions/cache/fallback, SSE snapshot and offset
accumulation, authoritative answers, legacy FINAL steps, sources, LOGIN upsells,
failed/garbage/empty answers, streaming deltas, cancellation, 429 Retry-After,
timeouts, quota with/without denominators, management routes/resources, SDK wire
response shapes, shutdown/panic recovery, concurrency and exact ZIP membership.

The successful test output ends with `PASS` and
`ok github.com/stark-shadow/cpa-plugin-perplexity`; no race report is emitted.

Exact library build command (exit **0**):

```sh
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o dist/1.0.0-linux-amd64/perplexity.so .
```

Delivered native artifact:
`/home/lzt404/cpa-project/cpa-plugin-perplexity/dist/1.0.0-linux-amd64/perplexity.so`.
ZIP: `dist/perplexity_1.0.0_linux_amd64.zip`.
ZIP checksum file: `dist/artifacts/checksums.txt`.
Build outputs and temporary workspaces are ignored, not committed.

## Not executed / remaining live acceptance

- **No live signed-in account** was used. Import validation, Pro/Max model access,
  real non-stream/stream answers, long deep research, expired live sessions, current
  quota counters and paid-credit availability still require an authorized account.
- **No live CPA server or browser panel end-to-end session** was launched. Route
  registration, management handling, resources and host bridge were verified by
  tests and the native smoke harness, not by a running management UI.
- **Linux arm64 and macOS arm64/amd64 builds were not run locally.** This is a Linux
  amd64 host and `aarch64-linux-gnu-gcc` is not installed. The four-target tag
  workflow is provided, including the arm64 compiler install and checksums release.
- **No GitHub tag/release workflow or publication was run.** No remote push,
  deployment, hosting changes or account modifications were performed.

The `pass` entries in `matrix.json` mean **hermetic local acceptance only**.
Every live-account field is explicitly `not_run`; none implies a live result.

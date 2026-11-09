# Local acceptance report

Date: **2026-11-09**, host platform **linux/amd64**.
Plugin: `perplexity` **1.0.0**, native ABI **1**, RPC schema **10**.

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
`ok github.com/lzt404/cpa-plugin-perplexity`; no race report is emitted.

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

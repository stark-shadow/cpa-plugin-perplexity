# Authentication evidence supplied for v1.1.0

Date: **November 9, 2026**. These are the task owner's already-verified findings
from this host, accepted as input, **not live probes repeated by this change**.
No private cookie, token, internal secret or account data is reproduced here.

## No device endpoint or OAuth client

- The desktop client is a **ToDesktop Electron wrapper** loading
  `https://perplexity.ai`; it has no separate desktop credential.
- The inspected wrapper bundle was `/tmp/main.js`; the website bundle snapshot
  consisted of **69 SPA chunks** in `/tmp/pplxjs/` on the verification host.
- The tested device-code endpoint candidates returned **404**. The provided
  verification found no public OAuth client credential and no PKCE or
  `redirect_uri` support. The task did not supply the complete candidate URL list,
  so this report does not invent one or claim exhaustive endpoint discovery.
- A Codex-style **PKCE implementation is therefore not available**. The same
  browser session can instead be captured by an authorized extension.

## HttpOnly session cookie

- The signed-in session is `__Secure-next-auth.session-token`.
- Its **HttpOnly** flag prevents page JavaScript, including a bookmarklet, from
  reading it. The extension uses `chrome.cookies` with explicit host permission
  instead; the cookie is not exposed in the UI or sent in a URL.

## Server-only internal secret

- `/api/auth/callback/pplx-jwt-to-cookie` exists but requires the header
  `internal_request_secret`.
- The supplied scan found **zero `internal_request` matches** across all 69 SPA
  chunks and the wrapper's `main.js`; the required secret is not a client credential.
- This is a server-to-server endpoint. The plugin/extension neither attempts to
  reverse it nor fabricates a secret, OAuth client or successful authorization.

## Acceptance boundary

The bundle paths are transient host references, not shipped evidence archives.
No new upstream request, real browser cookie capture or signed-in account login
was executed for v1.1.0. Local acceptance uses fake host/browser APIs only; see
[REPORT.md](REPORT.md) and [the extension security note](../extension/README.md).

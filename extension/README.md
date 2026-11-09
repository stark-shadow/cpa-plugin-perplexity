# CPA Perplexity Session Capture (Chrome / Edge, MV3)

1. Clone/download this repository; the extension is the **`extension/` directory**,
   not the plugin ZIP. Install plugin v1.1.0 in CPA separately.
2. Open `chrome://extensions` or `edge://extensions`, enable **Developer mode**,
   choose **Load unpacked**, and select `extension/`.
3. Open the extension popup and configure your CPA origin (default
   `http://127.0.0.1:8317`) and management key. Both settings persist in
   `chrome.storage.local`; they are not synced. Only HTTP `127.0.0.1` and
   `localhost` origins are supported by the supplied host permissions.
4. Sign in at `https://www.perplexity.ai/`. Start Perplexity login in CPA, refresh
   the login page if the extension was just installed, enter the management key,
   and click **我已登录，捕获会话**. Or leave the CPA login tab active and click
   **捕获并导入** in the popup; its state is associated automatically. A popup
   import from any other tab is a normal standalone import.

## Transport and fallback

The extension's service worker reads only the session cookie,
`__Host-pplx-last-active-account`, and `cf_clearance` using `chrome.cookies`.
It first POSTs to `/v0/management/plugins/perplexity/auth/import` using
`Authorization: Bearer <management key>`. It shows the returned email/plan or the
server's error (credential values are redacted if an error echoes them).

If the direct fetch fails (including CORS/network rejection), it opens the plugin
`/manage` page at the **configured CPA origin**, preserving any login state.
The service worker holds a one-use handoff in memory for at most 60 seconds. The
content script relays it to that tab with `window.postMessage`; `/manage` checks
`event.source`, `event.origin`, the configured base origin and login state, then
makes the authenticated **same-origin** POST. No CORS changes, anonymous
management endpoints, externally connectable extension, or CPA auth bypass are
introduced. An HTTP error such as 401 is shown directly, not retried via fallback.
Fallback failures and success appear **in the newly opened management tab**.
If CPA is unreachable or the extension worker restarts during the handoff, fix the
connection and capture again; a URL never contains cookies or the management key.

`/login` polls a normally authenticated, sanitized management status endpoint every
two seconds when a management key is entered. The host separately polls the native
login ABI. Pending flows expire after ten minutes or a plugin restart. Manual paste
import remains available from `/manage`, including with a login state's query.

## Security

**The session cookie is a full Perplexity account credential, like a password.**
Only install code you trust. Never share it in logs, screenshots, issue reports,
chat, filenames or URLs. Capture never displays the cookie or writes it to
extension storage; CPA persists it through its existing private auth-file storage.
The management key is saved in plaintext in local extension storage, so protect
your browser profile and remove it/uninstall the extension on shared machines.
The unauthenticated resource page itself is not authorization: a valid management
key is still required. Close fallback tabs after use. Do not capture on an
untrusted CPA instance/origin; the configured instance receives the credential.

The wrapper has no independent OAuth credential or device/PKCE flow; the
HttpOnly cookie cannot be read by page JavaScript or a bookmarklet. See the
repository's [verified authentication evidence](../acceptance/LOGIN-EVIDENCE.md).

Hermetic extension/bridge tests (no browser, network or account):
`node --test extension/tests/*.test.cjs` from the repository root. Real Chrome/Edge
cookie permissions, live sign-in and CORS behavior still need browser acceptance.

"use strict";

if (["/v0/resource/plugins/perplexity/login", "/v0/resource/plugins/perplexity/manage"].includes(location.pathname) && window === window.top) {
  let handoffRequested = false;
  const post = (message) => window.postMessage({source: "perplexity-extension", ...message}, location.origin);
  window.addEventListener("message", async (event) => {
    const data = event.data;
    if (event.source !== window || event.origin !== location.origin || !data || data.source !== "perplexity-page") return;
    if (data.type === "bridge-ready" && !handoffRequested && location.pathname.endsWith("/manage") && location.hash.startsWith("#capture=")) {
      handoffRequested = true;
      try {
        const result = await chrome.runtime.sendMessage({type: "handoff"});
        if (result.ok) post({type: "import", ...result});
        else post({type: "handoff-error", error: result.error});
      } catch (_) { post({type: "handoff-error", error: "Extension disconnected. Capture again from the popup."}); }
    }
    if (data.type === "capture" && location.pathname.endsWith("/login") && typeof data.requestId === "string") {
      try {
        const result = await chrome.runtime.sendMessage({type: "capture", state: data.state, managementKey: data.managementKey});
        post({type: "capture-result", requestId: data.requestId, ...result});
      } catch (_) { post({type: "capture-result", requestId: data.requestId, ok: false, error: "Extension disconnected. Reload this page."}); }
    }
  });
  post({type: "bridge-ready"});
}

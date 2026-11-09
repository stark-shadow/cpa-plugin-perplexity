"use strict";
importScripts("shared.js");

const handoffs = new Map();
const managePath = "/v0/resource/plugins/perplexity/manage";
const loginPath = "/v0/resource/plugins/perplexity/login";

function pageURL(sender, base) {
  if (!sender.tab || typeof sender.tab.id !== "number" || sender.frameId !== 0) throw new Error("Only the CPA top-level page can request capture.");
  const page = new URL(sender.url);
  if (page.origin !== base || ![managePath, loginPath].includes(page.pathname)) {
    throw new Error("This page does not match the CPA base URL configured in the extension popup.");
  }
  return page;
}

function cleanupHandoffs() {
  for (const [tab, handoff] of handoffs) {
    if (handoff.expires <= Date.now()) handoffs.delete(tab);
  }
}

async function openHandoff(base, managementKey, payload) {
  cleanupHandoffs();
  if (handoffs.size >= 16) throw new Error("Too many pending imports. Close unused import tabs and retry in one minute.");
  const tab = await chrome.tabs.create({url: "about:blank"});
  const requestId = crypto.randomUUID();
  handoffs.set(tab.id, {base, managementKey, payload, requestId, expires: Date.now() + 60000});
  const url = new URL(managePath, base);
  if (payload.state) url.searchParams.set("state", payload.state);
  url.hash = "capture=" + requestId;
  try {
    await chrome.tabs.update(tab.id, {url: url.href});
  } catch (error) {
    handoffs.delete(tab.id);throw error;
  }
  setTimeout(() => handoffs.delete(tab.id), 60000);
  return {ok: true, fallback: true};
}

async function handleMessage(message, sender) {
  if (!message || !["capture", "handoff"].includes(message.type)) throw new Error("Unknown extension request.");
  const settings = await chrome.storage.local.get({baseURL: PerplexityCapture.defaultBase, managementKey: ""});
  const base = PerplexityCapture.baseOrigin(settings.baseURL);
  if (message.type === "handoff") {
    const page = pageURL(sender, base);
    cleanupHandoffs();
    const handoff = handoffs.get(sender.tab.id);
    if (!handoff || handoff.base !== base || page.pathname !== managePath || page.hash !== "#capture=" + handoff.requestId ||
        (page.searchParams.get("state") || "") !== (handoff.payload.state || "")) {
      throw new Error("Capture handoff expired or the extension restarted. Capture again from the extension popup.");
    }
    handoffs.delete(sender.tab.id);
    return {ok: true, baseOrigin: base, requestId: handoff.requestId, managementKey: handoff.managementKey, payload: handoff.payload};
  }
  let state = "";
  let managementKey = settings.managementKey.trim();
  if (sender.tab) {
    const page = pageURL(sender, base);
    state = page.searchParams.get("state") || "";
    if (message.state !== state) throw new Error("Login state mismatch.");
    if (typeof message.managementKey === "string" && message.managementKey.trim()) managementKey = message.managementKey.trim();
  } else {
    if (sender.url !== chrome.runtime.getURL("popup.html")) throw new Error("Capture must start in the extension popup or CPA login page.");
    const [tab] = await chrome.tabs.query({active: true, currentWindow: true});
    if (tab && tab.url) {
      const page = new URL(tab.url);
      if (page.origin === base && [managePath, loginPath].includes(page.pathname)) state = page.searchParams.get("state") || "";
    }
  }
  if (!managementKey) throw new Error("请填写 CPA 管理密钥。");
  const payload = PerplexityCapture.cookiePayload(await chrome.cookies.getAll({domain: "perplexity.ai"}));
  if (state) payload.state = state;
  const secrets = [...Object.values(payload.cookies), managementKey];
  try {
    let response;
    try {
      response = await fetch(base + "/v0/management/plugins/perplexity/auth/import", {
        method: "POST", headers: {"Content-Type": "application/json", Authorization: "Bearer " + managementKey},
        body: JSON.stringify(payload), credentials: "omit", cache: "no-store", redirect: "error", signal: AbortSignal.timeout(12000)
      });
    } catch (_) {
      return await openHandoff(base, managementKey, payload);
    }
    return await PerplexityCapture.importResponse(response, secrets);
  } catch (error) {
    throw new Error(PerplexityCapture.redact(error.message, secrets));
  }
}

chrome.runtime.onMessage.addListener((message, sender, respond) => {
  handleMessage(message, sender).then(respond, (error) => respond({ok: false, error: error.message}));
  return true;
});

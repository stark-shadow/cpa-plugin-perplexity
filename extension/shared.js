"use strict";

const PerplexityCapture = (() => {
  const defaultBase = "http://127.0.0.1:8317";
  const sessionCookie = "__Secure-next-auth.session-token";
  const cookieNames = [sessionCookie, "__Host-pplx-last-active-account", "cf_clearance"];

  function baseOrigin(value) {
    const base = new URL(value || defaultBase);
    if (base.protocol !== "http:" || !["127.0.0.1", "localhost"].includes(base.hostname) ||
        base.username || base.password || base.search || base.hash || base.pathname !== "/") {
      throw new Error("CPA base URL must be an HTTP loopback origin, for example http://127.0.0.1:8317.");
    }
    return base.origin;
  }

  function cookiePayload(cookies) {
    const selected = {};
    for (const name of cookieNames) {
      const matches = cookies.filter((cookie) => cookie.name === name &&
        ["perplexity.ai", ".perplexity.ai", "www.perplexity.ai", ".www.perplexity.ai"].includes(cookie.domain) && cookie.path === "/");
      matches.sort((first, second) => Number(second.domain.includes("www.")) - Number(first.domain.includes("www.")));
      if (matches.length) selected[name] = matches[0].value;
    }
    if (!selected[sessionCookie]) {
      throw new Error("未找到已登录会话。请先在 https://www.perplexity.ai/ 登录，再重试。");
    }
    return {session_token: selected[sessionCookie], cookies: selected};
  }

  function redact(message, secrets) {
    let text = String(message);
    for (const secret of secrets.filter(Boolean).sort((first, second) => second.length - first.length)) {
      text = text.replaceAll(secret, "[redacted]");
    }
    return text;
  }

  async function importResponse(response, secrets) {
    const text = await response.text();
    let result;
    try { result = JSON.parse(text); } catch (_) { result = null; }
    if (!response.ok) {
      const detail = result && (result.error || result.message || result.Message);
      const message = typeof detail === "string" ? detail : detail && (detail.message || detail.Message);
      throw new Error(redact(message || text || ("HTTP " + response.status), secrets));
    }
    if (!result || typeof result.email !== "string" || typeof result.plan !== "string") {
      throw new Error("CPA returned an invalid import response.");
    }
    return {ok: true, email: redact(result.email, secrets), plan: redact(result.plan, secrets)};
  }

  return {defaultBase, baseOrigin, cookiePayload, redact, importResponse};
})();

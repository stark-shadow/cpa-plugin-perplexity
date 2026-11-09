package main

const loginHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>连接 Perplexity</title><style>` + panelCSS + `</style></head>
<body><main><h1>连接 Perplexity</h1><p>通过浏览器扩展捕获已登录会话，不是 OAuth 授权。Cookie 等同完整账号密码，页面不会显示 Cookie。</p>
<section><h2>1. 安装伴随扩展并登录</h2><p>在 Chrome / Edge 的扩展管理中启用开发者模式，点击「加载已解压的扩展程序」，选择本仓库的 <code>extension/</code> 目录。安装后请刷新本页，并在扩展弹窗中配置当前 CPA 地址。</p><button id="open">打开 Perplexity 登录</button></section>
<section><h2>2. 捕获会话</h2><label for="key">CPA 管理密钥</label><input id="key" type="password" autocomplete="off"><p>密钥仅留在本页内存；管理请求仍需正常 CPA 认证。</p><button id="capture">我已登录，捕获会话</button><p>没有扩展？可在 <a id="manual" href="./manage">手动导入面板</a> 粘贴会话。</p></section>
<p id="message" role="status" aria-live="polite">等待登录并捕获会话。</p></main><script>` + loginJS + `</script></body></html>`

const loginJS = `"use strict";
const byId = (id) => document.getElementById(id);
const state = new URLSearchParams(location.search).get("state") || "";
const manual = new URL("./manage",location.href);
if (state) manual.searchParams.set("state",state);
byId("manual").href = manual.href;
let extensionReady = false;
let completed = false;
let polling = false;
let captureTimer;
let interval;
function connected(email) {
  if (completed) return;
  completed = true;clearInterval(interval);clearTimeout(captureTimer);
  byId("message").textContent = "已连接 " + (email || "Perplexity") + "。登录成功，可以关闭此页。";
  byId("capture").disabled = true;
  setTimeout(() => window.close(),2000);
}
window.addEventListener("message", (event) => {
  const data = event.data;
  if (event.source !== window || event.origin !== location.origin || !data || data.source !== "perplexity-extension") return;
  if (data.type === "bridge-ready") {
    extensionReady = true;window.postMessage({source:"perplexity-page",type:"bridge-ready"},location.origin);
  }
  if (data.type === "capture-result" && data.requestId === "login-capture") {
    clearTimeout(captureTimer);byId("capture").disabled = false;
    if (data.ok && !data.fallback) connected(data.email);
    else byId("message").textContent = data.fallback ? "已打开同源导入面板，请等待会话导入。" : (data.error || "捕获失败，请使用手动导入面板。");
  }
});
byId("open").onclick = () => window.open("https://www.perplexity.ai/","_blank","noopener,noreferrer");
byId("capture").onclick = () => {
  if (!extensionReady) { byId("message").textContent = "未检测到扩展。请安装后刷新，或使用手动导入面板粘贴 Cookie。";return; }
  if (!state) { byId("message").textContent = "缺少登录状态，请从 CPA 重新发起登录。";return; }
  byId("capture").disabled = true;
  byId("message").textContent = "正在捕获并导入…";
  window.postMessage({source:"perplexity-page",type:"capture",requestId:"login-capture",state,managementKey:byId("key").value.trim()},location.origin);
  captureTimer = setTimeout(() => { byId("capture").disabled = false;byId("message").textContent = "扩展未响应，请检查扩展设置或使用手动导入面板。"; },15000);
};
async function poll() {
  if (polling || completed || !state || !byId("key").value.trim()) return;
  polling = true;
  try {
    // The core, not this plugin, owns the CPA OAuth session: only
    // /v8/management/oauth/status saves the credential and returns "ok". Polling the
    // plugin route would report success while nothing was persisted.
    const response = await fetch("/v8/management/oauth/status?state=" + encodeURIComponent(state),{headers:{Authorization:"Bearer " + byId("key").value.trim()},credentials:"same-origin",cache:"no-store"});
    if (!response.ok) { byId("message").textContent = "请检查 CPA 管理密钥；状态查询需要管理认证。";return; }
    const result = await response.json();
    if (result.status === "ok") connected("");
    if (result.status === "wait") { byId("message").textContent = "等待捕获会话…"; return; }
    if (result.status === "error") { clearInterval(interval);byId("capture").disabled = true;byId("message").textContent = (result.error || "登录已过期或插件已重启，请从 CPA 重新发起登录。"); }
  } catch (_) { byId("message").textContent = "暂时无法连接 CPA，正在重试…"; }
  finally { polling = false; }
}
interval = setInterval(poll,2000);
byId("key").addEventListener("change",poll);
poll();`

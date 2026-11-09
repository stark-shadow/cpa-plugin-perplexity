"use strict";

const byId = (id) => document.getElementById(id);
const ready = chrome.storage.local.get({baseURL: PerplexityCapture.defaultBase, managementKey: ""}).then((settings) => {
  byId("base").value = settings.baseURL;
  byId("key").value = settings.managementKey;
});

async function saveSettings() {
  await ready;
  const baseURL = PerplexityCapture.baseOrigin(byId("base").value.trim());
  await chrome.storage.local.set({baseURL, managementKey: byId("key").value.trim()});
}

for (const field of ["base", "key"]) {
  byId(field).addEventListener("change", () => saveSettings().catch((error) => { byId("message").textContent = error.message; }));
}

byId("capture").onclick = async () => {
  byId("capture").disabled = true;
  byId("message").textContent = "正在捕获并导入…";
  try {
    await saveSettings();
    const result = await chrome.runtime.sendMessage({type: "capture"});
    if (!result.ok) throw new Error(result.error);
    byId("message").textContent = result.fallback ? "直接请求受阻，已打开同源管理面板完成导入。" : "已连接 " + (result.email || "Perplexity") + " / " + result.plan;
  } catch (error) { byId("message").textContent = error.message; }
  finally { byId("capture").disabled = false; }
};

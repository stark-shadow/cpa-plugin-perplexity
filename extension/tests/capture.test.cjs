"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const {test} = require("node:test");
const {randomUUID} = require("node:crypto");

const extension = path.resolve(__dirname, "..");
const base = "http://127.0.0.1:8317";
const loginPath = "/v0/resource/plugins/perplexity/login";
const managePath = "/v0/resource/plugins/perplexity/manage";
const token = "hermetic-session-token-0000000000000000";
const cookieName = "__Secure-next-auth.session-token";
const key = "hermetic-management-key";
const cookie = (name, value, domain = ".perplexity.ai") => ({name, value, domain, path: "/", httpOnly: true});
const plain = (value) => JSON.parse(JSON.stringify(value));
const tick = () => new Promise((resolve) => setImmediate(resolve));

function worker(options = {}) {
  const requests = [], tabs = [], writes = [], timers = [];
  let listener;
  let now = Date.now();
  const storage = {baseURL: base, managementKey: key, ...options.settings};
  const context = vm.createContext({
    URL, AbortSignal, crypto: {randomUUID},
    Date: {now: () => now},
    setTimeout: (callback, delay) => { timers.push({callback, delay}); },
    chrome: {
      storage: {local: {get: async (defaults) => ({...defaults, ...storage}), set: async (values) => writes.push(values)}},
      cookies: {getAll: async (filter) => {
        assert.deepEqual(plain(filter), {domain: "perplexity.ai"});
        return options.cookies || [cookie(cookieName, token), cookie("cf_clearance", "clearance-secret"), cookie("__Host-pplx-last-active-account", "account-secret"), cookie("irrelevant", "never-imported")];
      }},
      tabs: {
        query: async () => options.activeTab ? [options.activeTab] : [],
        create: async (details) => { tabs.push(plain(details));return {id: 42}; },
        update: async (identifier, details) => { assert.equal(identifier, 42);tabs.push(plain(details)); }
      },
      runtime: {getURL: (file) => "chrome-extension://hermetic/" + file, onMessage: {addListener: (callback) => { listener = callback; }}}
    },
    fetch: async (url, request) => {
      requests.push({url, ...plain(request)});
      if (options.fetch) return options.fetch(url, request);
      return Response.json({email: "test@example.invalid", plan: "pro", file: "perplexity-hash.json"}, {status: 201});
    }
  });
  context.importScripts = (file) => vm.runInContext(fs.readFileSync(path.join(extension, file), "utf8"), context);
  vm.runInContext(fs.readFileSync(path.join(extension, "background.js"), "utf8"), context);
  return {
    context, requests, tabs, writes, timers,
    advance: (milliseconds) => { now += milliseconds; },
    send: (message, sender = {url: "chrome-extension://hermetic/popup.html"}) => new Promise((resolve) => {
      assert.equal(listener(message, sender, resolve), true);
    }),
    shared: vm.runInContext("PerplexityCapture", context)
  };
}

test("MV3 manifest grants only specified hosts and permissions", () => {
  const manifest = JSON.parse(fs.readFileSync(path.join(extension, "manifest.json")));
  assert.equal(manifest.manifest_version, 3);
  assert.deepEqual(manifest.permissions, ["cookies", "storage", "activeTab"]);
  assert.deepEqual(manifest.host_permissions, ["https://www.perplexity.ai/*", "http://127.0.0.1/*", "http://localhost/*"]);
  assert.ok(manifest.background.service_worker);
  assert.ok(manifest.action.default_popup);
  assert.equal(manifest.externally_connectable, undefined);
});

test("capture imports only allowed cookies with management authentication", async () => {
  const host = worker();
  const result = await host.send({type: "capture"});
  assert.deepEqual(plain(result), {ok: true, email: "test@example.invalid", plan: "pro"});
  const request = host.requests[0];
  assert.equal(request.url, base + "/v0/management/plugins/perplexity/auth/import");
  assert.equal(request.headers.Authorization, "Bearer " + key);
  assert.equal(request.method, "POST");
  assert.equal(request.redirect, "error");
  const payload = JSON.parse(request.body);
  assert.equal(payload.session_token, token);
  assert.deepEqual(Object.keys(payload.cookies).sort(), [cookieName, "cf_clearance", "__Host-pplx-last-active-account"].sort());
  assert.equal(JSON.stringify(result).includes(token), false);
  assert.deepEqual(host.writes, []);
});

test("cookie selection rejects missing sessions and ignores unrelated hosts", async () => {
  const host = worker({cookies: [cookie(cookieName, token, "attacker.invalid")]});
  const result = await host.send({type: "capture"});
  assert.equal(result.ok, false);
  assert.match(result.error, /未找到已登录会话/);
  assert.equal(host.requests.length, 0);
  assert.equal(JSON.stringify(result).includes(token), false);
  const payload = host.shared.cookiePayload([cookie(cookieName, "parent-session"), cookie(cookieName, "www-session", "www.perplexity.ai")]);
  assert.equal(payload.session_token, "www-session");
});

test("popup and page capture preserve login state; other ports and frames are rejected", async () => {
  const host = worker({activeTab: {url: base + loginPath + "?state=opaque-state"}});
  assert.equal((await host.send({type: "capture"})).ok, true);
  assert.equal(JSON.parse(host.requests[0].body).state, "opaque-state");
  const sender = {url: base + loginPath + "?state=opaque-state", tab: {id: 7}, frameId: 0};
  assert.equal((await host.send({type: "capture", state: "opaque-state", managementKey: "page-key"}, sender)).ok, true);
  assert.equal(host.requests[1].headers.Authorization, "Bearer page-key");
  for (const invalid of [{...sender, frameId: 1}, {...sender, url: "http://127.0.0.1:9999" + loginPath}, {...sender, url: base + "/unrelated"}]) {
    assert.equal((await host.send({type: "capture", state: "opaque-state"}, invalid)).ok, false);
  }
  assert.equal((await host.send({type: "capture", state: "wrong-state"}, sender)).ok, false);
  assert.equal(host.requests.length, 2);
});

test("HTTP errors are exact, redacted, and do not trigger CORS fallback", async () => {
  const host = worker({fetch: async () => Response.json({error: {message: "Unauthorized " + key + " " + token + " clearance-secret"}}, {status: 401})});
  const result = await host.send({type: "capture"});
  assert.deepEqual(plain(result), {ok: false, error: "Unauthorized [redacted] [redacted] [redacted]"});
  assert.deepEqual(host.tabs, []);
});

test("network failure opens same-origin, one-use postMessage handoff without URL secrets", async () => {
  const host = worker({activeTab: {url: base + loginPath + "?state=opaque-state"}, fetch: async () => { throw new TypeError("CORS blocked"); }});
  assert.deepEqual(plain(await host.send({type: "capture"})), {ok: true, fallback: true});
  const destination = host.tabs[1].url;
  assert.equal(destination.includes(token), false);
  assert.equal(destination.includes(key), false);
  const page = new URL(destination);
  assert.equal(page.origin, base);
  assert.equal(page.pathname, managePath);
  assert.equal(page.searchParams.get("state"), "opaque-state");
  const sender = {url: destination, tab: {id: 42}, frameId: 0};
  const stolen = await host.send({type: "handoff"}, {...sender, tab: {id: 43}});
  assert.equal(stolen.ok, false);
  const handoff = await host.send({type: "handoff"}, sender);
  assert.equal(handoff.ok, true);
  assert.equal(handoff.baseOrigin, base);
  assert.equal(handoff.payload.session_token, token);
  assert.equal(handoff.managementKey, key);
  assert.equal((await host.send({type: "handoff"}, sender)).ok, false);
  assert.equal(host.timers[0].delay, 60000);
});

test("expired handoff cannot be resumed", async () => {
  const host = worker({fetch: async () => { throw new TypeError("Offline"); }});
  await host.send({type: "capture"});
  host.advance(60001);
  const result = await host.send({type: "handoff"}, {url: host.tabs[1].url, tab: {id: 42}, frameId: 0});
  assert.equal(result.ok, false);
  assert.match(result.error, /expired/);
});

test("base URL rejects off-host or secret-bearing destinations", () => {
  const {shared} = worker();
  for (const invalid of ["https://localhost:8317", "http://attacker.invalid", "http://user:secret@localhost:8317", base + "/prefix", base + "?secret=value", base + "#fragment"]) {
    assert.throws(() => shared.baseOrigin(invalid));
  }
  assert.equal(shared.baseOrigin("http://localhost:9000/"), "http://localhost:9000");
});

function pageHarness(script, pathname = managePath) {
  const elements = new Map();
  const messages = [], requests = [], listeners = [], intervals = [];
  const window = {addEventListener: (type, callback) => { if (type === "message") listeners.push(callback); }, postMessage: (data, origin) => messages.push({data, origin}), close: () => {}};
  window.top = window;
  const location = new URL(base + pathname + "?state=opaque-state");
  const context = vm.createContext({
    URL, URLSearchParams, window, location,
    document: {getElementById: (id) => {
      if (!elements.has(id)) elements.set(id, {value: "", textContent: "", replaceChildren: () => {}, addEventListener: () => {}});
      return elements.get(id);
    }},
    setInterval: (callback, delay) => { intervals.push({callback, delay});return 1; }, clearInterval: () => {}, setTimeout: () => 1, clearTimeout: () => {},
    fetch: async (url, request) => {
      requests.push({url, ...plain(request)});
      return Response.json(url.endsWith("/auth/status") ? {accounts: []} : {email: "test@example.invalid", plan: "pro"}, {status: 200});
    }
  });
  vm.runInContext(script, context);
  return {context, window, location, messages, requests, elements, intervals, dispatch: async (data, origin = base, source = window) => {
    for (const listener of listeners) await listener({data, origin, source});
    await tick();
  }};
}

test("manage bridge verifies source, origin, configured base and state before import", async () => {
  const source = fs.readFileSync(path.join(extension, "..", "management.go"), "utf8").match(/const panelJS = `([\s\S]*)`/)[1];
  const host = pageHarness(source);
  await tick();
  host.requests.length = 0;
  const data = {source: "perplexity-extension", type: "import", requestId: "handoff", baseOrigin: base, managementKey: key, payload: {session_token: token, cookies: {[cookieName]: token}, state: "opaque-state"}};
  await host.dispatch({...data}, "http://localhost:8317");
  await host.dispatch({...data}, base, {});
  await host.dispatch({...data, baseOrigin: "http://localhost:8317"});
  await host.dispatch({...data, payload: {...data.payload, state: "wrong-state"}});
  assert.equal(host.requests.length, 0);
  await host.dispatch({...data, payload: {...data.payload}});
  assert.equal(host.requests.length, 1);
  assert.equal(host.requests[0].credentials, "same-origin");
  assert.equal(host.requests[0].headers.Authorization, "Bearer " + key);
  assert.equal(host.elements.get("message").textContent, "已连接 test@example.invalid / pro");
  assert.equal(JSON.stringify(host.messages).includes(token), false);
});

test("manage fallback shows exact HTTP errors without echoing cookie values", async () => {
  const source = fs.readFileSync(path.join(extension, "..", "management.go"), "utf8").match(/const panelJS = `([\s\S]*)`/)[1];
  const host = pageHarness(source);
  await tick();
  host.context.fetch = async () => Response.json({error: {message: "Rejected " + token + " " + key}}, {status: 401});
  await host.dispatch({source: "perplexity-extension", type: "import", requestId: "handoff", baseOrigin: base, managementKey: key, payload: {session_token: token, cookies: {[cookieName]: token}, state: "opaque-state"}});
  assert.equal(host.elements.get("message").textContent, "Rejected [redacted] [redacted]");
  assert.equal(JSON.stringify(host.messages).includes(token), false);
  assert.equal(host.messages.at(-1).data.ok, false);
});

test("content script bridges page capture and fallback only to its own origin", async () => {
  const host = pageHarness("", loginPath);
  const sent = [];
  host.context.chrome = {runtime: {sendMessage: async (data) => { sent.push(data);return {ok: true, email: "test@example.invalid", plan: "pro"}; }}};
  vm.runInContext(fs.readFileSync(path.join(extension, "content.js"), "utf8"), host.context);
  const request = {source: "perplexity-page", type: "capture", requestId: "capture-id", state: "opaque-state", managementKey: key};
  await host.dispatch(request, "http://localhost:8317");
  assert.equal(sent.length, 0);
  await host.dispatch(request);
  assert.equal(sent.length, 1);
  assert.equal(host.messages.at(-1).origin, base);
  assert.equal(host.messages.at(-1).data.type, "capture-result");
});

test("content script relays a handoff only after same-origin panel readiness", async () => {
  const host = pageHarness("");
  host.location.hash = "capture=handoff-id";
  let requests = 0;
  host.context.chrome = {runtime: {sendMessage: async () => {
    requests++;
    return {ok: true, requestId: "handoff-id", baseOrigin: base, managementKey: key, payload: {session_token: token, state: "opaque-state"}};
  }}};
  vm.runInContext(fs.readFileSync(path.join(extension, "content.js"), "utf8"), host.context);
  await host.dispatch({source: "perplexity-page", type: "bridge-ready"}, "http://localhost:8317");
  assert.equal(requests, 0);
  await host.dispatch({source: "perplexity-page", type: "bridge-ready"});
  await host.dispatch({source: "perplexity-page", type: "bridge-ready"});
  assert.equal(requests, 1);
  assert.equal(host.messages.at(-1).origin, base);
  assert.equal(host.messages.at(-1).data.type, "import");
});

test("login page explains manual fallback and preserves state", async () => {
  const source = fs.readFileSync(path.join(extension, "..", "login_page.go"), "utf8").match(/const loginJS = `([\s\S]*)`/)[1];
  const host = pageHarness(source, loginPath);
  host.elements.get("capture").onclick();
  assert.match(host.elements.get("message").textContent, /未检测到扩展/);
  assert.equal(new URL(host.elements.get("manual").href).searchParams.get("state"), "opaque-state");
  await host.dispatch({source: "perplexity-extension", type: "bridge-ready"});
  host.elements.get("capture").onclick();
  assert.equal(host.messages.at(-1).data.type, "capture");
  assert.equal(host.messages.at(-1).data.state, "opaque-state");
});

test("login page polls sanitized status every two seconds with normal management auth", async () => {
  const source = fs.readFileSync(path.join(extension, "..", "login_page.go"), "utf8").match(/const loginJS = `([\s\S]*)`/)[1];
  const host = pageHarness(source, loginPath);
  assert.equal(host.intervals[0].delay, 2000);
  host.elements.get("key").value = key;
  host.context.fetch = async (url, request) => {
    assert.equal(url, "/v0/management/plugins/perplexity/auth/login/poll?state=opaque-state");
    assert.equal(request.headers.Authorization, "Bearer " + key);
    assert.equal(request.credentials, "same-origin");
    return Response.json({status: "success", email: "test@example.invalid"});
  };
  await host.intervals[0].callback();
  assert.match(host.elements.get("message").textContent, /已连接 test@example.invalid/);
  assert.equal(host.elements.get("capture").disabled, true);
});

test("popup persists only origin and management key and never displays session token", async () => {
  const host = pageHarness("");
  const writes = [];
  const {shared} = worker();
  host.context.PerplexityCapture = shared;
  host.context.chrome = {
    storage: {local: {get: async () => ({baseURL: base, managementKey: key}), set: async (values) => writes.push(plain(values))}},
    runtime: {sendMessage: async () => ({ok: true, email: "test@example.invalid", plan: "pro"})}
  };
  vm.runInContext(fs.readFileSync(path.join(extension, "popup.js"), "utf8"), host.context);
  await tick();
  await host.elements.get("capture").onclick();
  assert.deepEqual(writes, [{baseURL: base, managementKey: key}]);
  assert.equal(host.elements.get("message").textContent, "已连接 test@example.invalid / pro");
  assert.equal(host.elements.get("message").textContent.includes(token), false);
});

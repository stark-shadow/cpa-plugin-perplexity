package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const managementPrefix = "/plugins/perplexity"

func managementRegistration() pluginapi.ManagementRegistrationResponse {
	return pluginapi.ManagementRegistrationResponse{
		Routes: []pluginapi.ManagementRoute{
			{Method: http.MethodPost, Path: managementPrefix + "/auth/import"},
			{Method: http.MethodGet, Path: managementPrefix + "/auth/status"},
			{Method: http.MethodPost, Path: managementPrefix + "/auth/test"},
			{Method: http.MethodGet, Path: managementPrefix + "/catalog"},
		},
		Resources: []pluginapi.ResourceRoute{
			{Path: "/manage", Menu: "Perplexity", Description: "Import and test Perplexity web sessions"},
			{Path: "/panel.css"}, {Path: "/panel.js"},
		},
	}
}

func managementJSON(status int, value any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(value)
	return pluginapi.ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}}, Body: body}
}

func managementError(err error) pluginapi.ManagementResponse {
	var detail *pluginabi.Error
	if !errors.As(err, &detail) {
		detail = classifyError(err)
	}
	response := managementJSON(detail.HTTPStatus, map[string]any{"error": detail})
	if detail.RetryAfterSeconds > 0 {
		response.Headers.Set("Retry-After", strconv.Itoa(detail.RetryAfterSeconds))
	}
	return response
}

func decodeManagement(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return pluginabi.NewError("invalid_request", "Invalid Perplexity management request", 400)
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return pluginabi.NewError("invalid_request", "Expected one management request object", 400)
	}
	return nil
}

func importedCredential(raw []byte) (credential, error) {
	var request struct {
		SessionToken string          `json:"session_token"`
		Cookies      json.RawMessage `json:"cookies"`
		Label        string          `json:"label"`
	}
	if err := decodeManagement(raw, &request); err != nil {
		return credential{}, err
	}
	cred := credential{Type: pluginID, SessionToken: request.SessionToken, Label: request.Label}
	if len(request.Cookies) > 0 && string(request.Cookies) != "null" {
		var text string
		if json.Unmarshal(request.Cookies, &text) == nil {
			if cred.SessionToken == "" {
				parsed, err := parseCredential([]byte(text))
				if err != nil {
					return credential{}, err
				}
				cred.SessionToken, cred.Cookies = parsed.SessionToken, parsed.Cookies
			} else {
				var err error
				cred.Cookies, err = parseCookieText(text)
				if err != nil {
					return credential{}, err
				}
			}
		} else if json.Unmarshal(request.Cookies, &cred.Cookies) != nil {
			return credential{}, invalidCredential()
		}
	}
	return validateCredential(cred)
}

type storedAccount struct {
	File string
	Cred *credential
}

type accountStatus struct {
	File        string `json:"file"`
	Email       string `json:"email"`
	Plan        string `json:"plan"`
	ValidatedAt string `json:"validated_at"`
	Valid       bool   `json:"valid"`
}

func storedAccounts(ctx context.Context, callback hostCallbackFunc) ([]storedAccount, error) {
	if callback == nil {
		return nil, pluginabi.NewError("storage_error", "Host auth storage unavailable", 503)
	}
	raw, err := callback(ctx, pluginabi.MethodHostAuthList, struct{}{})
	if err != nil {
		return nil, pluginabi.NewError("storage_error", "Unable to list Perplexity accounts", 503)
	}
	var response struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return nil, protocolError()
	}
	accounts := make([]storedAccount, 0)
	for _, file := range response.Files {
		if file.Provider != pluginID && file.Type != pluginID {
			continue
		}
		account := storedAccount{File: filepath.Base(file.Name)}
		index := file.AuthIndex
		if index == "" {
			index = file.ID
		}
		data, getErr := callback(ctx, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: index})
		if getErr == nil {
			var record pluginapi.HostAuthGetResponse
			if json.Unmarshal(data, &record) == nil {
				if cred, parseErr := parseCredential(record.JSON); parseErr == nil {
					account.Cred = &cred
				}
			}
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func testAccount(ctx context.Context, client sender, account storedAccount) accountStatus {
	result := accountStatus{File: account.File}
	if account.Cred == nil {
		return result
	}
	cred := *account.Cred
	result.Email, result.Plan, result.ValidatedAt = cred.Email, cred.Plan, cred.ValidatedAt
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if updated, err := probeSession(probeCtx, client, cred); err == nil {
		result.Valid, result.Email, result.Plan, result.ValidatedAt = true, updated.Email, updated.Plan, updated.ValidatedAt
	}
	return result
}

func (rt *pluginRuntime) managementHandle(request pluginapi.ManagementRequest, id string, callback hostCallbackFunc) pluginapi.ManagementResponse {
	resourcePath := strings.TrimPrefix(request.Path, "/v0/resource/plugins/perplexity")
	if request.Method == http.MethodGet {
		content, contentType := "", ""
		switch resourcePath {
		case "/manage":
			content, contentType = panelHTML, "text/html; charset=utf-8"
		case "/panel.css":
			content, contentType = panelCSS, "text/css; charset=utf-8"
		case "/panel.js":
			content, contentType = panelJS, "application/javascript; charset=utf-8"
		}
		if contentType != "" {
			return pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {contentType}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"}}, Body: []byte(content)}
		}
	}
	path := strings.TrimPrefix(request.Path, "/v0/management")
	cfg, _ := rt.snapshot()
	ctx, cancel := rt.requestContext(8 * time.Second)
	defer cancel()
	client := sender{callback: callback, id: id, cfg: cfg}
	switch {
	case request.Method == http.MethodPost && path == managementPrefix+"/auth/import":
		cred, err := importedCredential(request.Body)
		if err != nil {
			return managementError(err)
		}
		validated, err := probeSession(ctx, client, cred)
		if err != nil {
			var detail *pluginabi.Error
			if errors.As(err, &detail) && detail.HTTPStatus == 401 {
				return managementError(pluginabi.NewError("invalid_session", "The cookie is not a signed-in Perplexity session", 400))
			}
			return managementError(err)
		}
		file, err := saveCredential(ctx, callback, validated)
		if err != nil {
			return managementError(err)
		}
		return managementJSON(201, map[string]string{"email": validated.Email, "plan": validated.Plan, "file": file})
	case request.Method == http.MethodGet && path == managementPrefix+"/auth/status":
		accounts, err := storedAccounts(ctx, callback)
		if err != nil {
			return managementError(err)
		}
		status := make([]accountStatus, 0, len(accounts))
		for _, account := range accounts {
			status = append(status, testAccount(ctx, client, account))
		}
		return managementJSON(200, map[string]any{"accounts": status})
	case request.Method == http.MethodPost && path == managementPrefix+"/auth/test":
		var input struct {
			File string `json:"file"`
		}
		if err := decodeManagement(request.Body, &input); err != nil {
			return managementError(err)
		}
		accounts, err := storedAccounts(ctx, callback)
		if err != nil {
			return managementError(err)
		}
		for _, account := range accounts {
			if account.File == input.File {
				return managementJSON(200, testAccount(ctx, client, account))
			}
		}
		return managementError(pluginabi.NewError("not_found", "Perplexity account not found", 404))
	case request.Method == http.MethodGet && path == managementPrefix+"/catalog":
		tier := "pro"
		if accounts, err := storedAccounts(ctx, callback); err == nil {
			for _, account := range accounts {
				if account.Cred != nil && account.Cred.Plan == "max" {
					tier = "max"
				}
			}
		}
		catalog := rt.catalogFor(ctx, client)
		return managementJSON(200, map[string]any{"tier": tier, "models": modelInfos(mappedModels(catalog, tier, cfg))})
	default:
		return managementError(pluginabi.NewError("not_found", "Perplexity route not found", 404))
	}
}

const panelHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Perplexity sessions</title><link rel="stylesheet" href="./panel.css"><script src="./panel.js" defer></script></head>
<body><main><h1>Perplexity sessions</h1><p>Import a signed-in browser session. Cookies stay private and are never displayed back.</p>
<section><h2>1. Connect to management</h2><label for="key">CPA management key (if required)</label><input id="key" type="password" autocomplete="off"><button id="refresh">Refresh accounts</button><p>The key stays only in this page's memory. Management endpoints require normal CPA authentication.</p></section>
<section><h2>2. Import a session</h2><p>Sign in to perplexity.ai, open DevTools → Application → Cookies → https://www.perplexity.ai, and copy the <code>__Secure-next-auth.session-token</code> value. You can also paste a full Cookie header or cookie JSON object. Treat it like a password.</p><label for="cookie">Session token or cookies</label><textarea id="cookie" rows="5" autocomplete="off" spellcheck="false"></textarea><button id="import">Import session</button></section>
<p id="message" role="status" aria-live="polite"></p><section><h2>Accounts</h2><table><thead><tr><th>Account / file</th><th>Plan</th><th>State</th><th>Action</th></tr></thead><tbody id="accounts"></tbody></table><p>Remove accounts using CPA's normal auth-file management. Expired sessions must be imported again; browser OAuth is not supported.</p></section></main></body></html>`

const panelCSS = `:root{color-scheme:light dark;font:16px/1.5 system-ui,sans-serif}body{margin:0;background:light-dark(#f5f7f8,#10191c);color:light-dark(#172e35,#e3eeee)}main{max-width:850px;margin:3rem auto;padding:0 1.2rem}h1{font-size:2.2rem;letter-spacing:-.04em}section{background:light-dark(#fff,#18262b);border:1px solid #748f9455;border-radius:14px;padding:1.5rem;margin:1.2rem 0}h2{margin-top:0;font-size:1.1rem}p{color:light-dark(#496068,#b5cace)}label{display:block;font-weight:600}input,textarea{box-sizing:border-box;width:100%;padding:.7rem;border:1px solid #718e94;border-radius:7px;margin:.5rem 0;font:inherit}button{padding:.65rem 1.1rem;border:0;border-radius:7px;background:#176e78;color:white;cursor:pointer}button:disabled{opacity:.5}table{width:100%;text-align:left;border-collapse:collapse}td,th{padding:.6rem;border-bottom:1px solid #748f9444;overflow-wrap:anywhere}code{overflow-wrap:anywhere}#message{min-height:1.5rem}`

const panelJS = `"use strict";
const byId = (id) => document.getElementById(id);
const base = "/v0/management/plugins/perplexity";
async function api(path, body) {
  const headers = {"Content-Type":"application/json"};
  const key = byId("key").value.trim();
  if (key) headers.Authorization = "Bearer " + key;
  const response = await fetch(base + path, {method:body ? "POST" : "GET",headers,credentials:"same-origin",cache:"no-store",body:body ? JSON.stringify(body) : undefined});
  if (!response.ok) throw new Error(response.status === 401 ? "Check your CPA management key." : "Request failed. Check the session and try again.");
  return response.json();
}
async function refresh() {
  const result = await api("/auth/status");
  const table = byId("accounts");
  table.replaceChildren();
  for (const account of result.accounts) {
    const row = document.createElement("tr");
    for (const value of [account.email || account.file,account.plan || "unknown",account.valid ? "Valid" : "Expired / unavailable"]) {
      const cell = document.createElement("td");cell.textContent = value;row.append(cell);
    }
    const action = document.createElement("td");const button = document.createElement("button");button.textContent = "Test";
    button.onclick = () => run(async () => { const tested = await api("/auth/test",{file:account.file});byId("message").textContent = tested.valid ? "Session is valid." : "Session expired or temporarily unavailable.";await refresh(); });
    action.append(button);row.append(action);table.append(row);
  }
}
async function run(work) {
  try { await work(); } catch (error) { byId("message").textContent = error.message; }
}
byId("refresh").onclick = () => run(refresh);
byId("import").onclick = () => run(async () => {
  let cookies = byId("cookie").value.trim();byId("cookie").value = "";
  if (!cookies) { byId("message").textContent = "Paste a signed-in session first.";return; }
  if (cookies.startsWith("{")) { try { cookies = JSON.parse(cookies); } catch (_) { throw new Error("Invalid cookie JSON."); } }
  await api("/auth/import",{cookies});cookies = "";byId("message").textContent = "Session imported.";await refresh();
});
run(refresh);`

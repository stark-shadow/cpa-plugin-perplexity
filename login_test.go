package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func startTestLogin(t *testing.T, rt *pluginRuntime) pluginapi.AuthLoginStartResponse {
	t.Helper()
	value, err := rt.dispatch(pluginabi.MethodAuthLoginStart, marshal(pluginapi.AuthLoginStartRequest{BaseURL: "http://127.0.0.1:8317/v0/management/oauth/callback"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	return value.(pluginapi.AuthLoginStartResponse)
}

func pollTestLogin(t *testing.T, rt *pluginRuntime, state string) pluginapi.AuthLoginPollResponse {
	t.Helper()
	value, err := rt.dispatch(pluginabi.MethodAuthLoginPoll, marshal(pluginapi.AuthLoginPollRequest{State: state}), nil)
	if err != nil {
		t.Fatal(err)
	}
	return value.(pluginapi.AuthLoginPollResponse)
}

func TestLoginStateSealingAndPurpose(t *testing.T) {
	key, nonce := make([]byte, 32), make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	state := loginState{Kind: cookieLoginKind, Expires: time.Now().Add(time.Minute).Unix(), Nonce: nonce}
	sealed, err := sealState(key, state)
	if err != nil || len(sealed) > 128 {
		t.Fatal("state did not fit CPA state limit")
	}
	opened, err := openState(key, sealed)
	if err != nil || !reflect.DeepEqual(opened, state) {
		t.Fatal("state round trip failed")
	}
	second, err := sealState(key, state)
	if err != nil || second == sealed {
		t.Fatal("sealing did not use a fresh nonce")
	}
	for _, invalid := range []string{"", strings.Repeat("a", 129), "!" + sealed[1:], sealed[:len(sealed)-2]} {
		if _, err := openState(key, invalid); err == nil {
			t.Fatal("invalid state accepted")
		}
	}
	otherKey := bytes.Repeat([]byte{1}, 32)
	if _, err := openState(otherKey, sealed); err == nil {
		t.Fatal("state accepted under a different key")
	}
	tampered, _ := base64.RawURLEncoding.DecodeString(sealed)
	tampered[len(tampered)-1] ^= 1
	if _, err := openState(key, base64.RawURLEncoding.EncodeToString(tampered)); err == nil {
		t.Fatal("modified authentication tag accepted")
	}
	for _, invalid := range []loginState{{Kind: 0, Nonce: nonce, Expires: state.Expires}, {Kind: 1, Nonce: nonce[:15], Expires: state.Expires}, {Kind: 1, Nonce: nonce, Expires: 1 << 32}} {
		if _, err := sealState(key, invalid); err == nil {
			t.Fatal("invalid state shape accepted")
		}
	}
	rt := testRuntime(t)
	rt.stateKey = key
	rt.pending[hex.EncodeToString(nonce)] = &pendingFlow{Expires: state.Expires}
	state.Kind = 2
	wrongPurpose, err := sealState(key, state)
	if err != nil {
		t.Fatal(err)
	}
	if pollTestLogin(t, rt, wrongPurpose).Status != pluginapi.AuthLoginStatusError {
		t.Fatal("another state's purpose was accepted as login")
	}
}

func TestLoginStartOrigin(t *testing.T) {
	for _, base := range []string{"http://127.0.0.1:8317/v0/management/oauth/callback?ignored=1#fragment", "https://cpa.example.invalid:9443/nested/callback", "http://[::1]:8317/callback"} {
		rt := testRuntime(t)
		started, err := rt.startLogin(pluginapi.AuthLoginStartRequest{BaseURL: base})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(started.URL)
		original, _ := url.Parse(base)
		if err != nil || parsed.Scheme != original.Scheme || parsed.Host != original.Host || parsed.Path != "/v0/resource/plugins/perplexity/login" || parsed.Query().Get("state") != started.State || len(parsed.Query()) != 1 || parsed.Fragment != "" {
			t.Fatal("login URL did not derive only the core origin")
		}
		if len(started.State) > maxStateLength || started.Provider != pluginID || time.Until(started.ExpiresAt) > loginLifetime || time.Until(started.ExpiresAt) < 9*time.Minute {
			t.Fatal("invalid login start response")
		}
		if pollTestLogin(t, rt, started.State).Status != pluginapi.AuthLoginStatusPending {
			t.Fatal("new login not pending")
		}
	}
	for _, base := range []string{"", "/relative", "ftp://localhost", "http://user:password@localhost/", "http:///callback", "https://localhost:bad/"} {
		_, err := testRuntime(t).startLogin(pluginapi.AuthLoginStartRequest{BaseURL: base})
		errorDetail(t, err, "invalid_request", 400)
	}
}

func TestLoginImportCompletion(t *testing.T) {
	for _, location := range []string{"query", "json", "both"} {
		t.Run(location, func(t *testing.T) {
			rt, host := testRuntime(t), newFakeHost()
			started := startTestLogin(t, rt)
			body := map[string]any{"session_token": testToken, "cookies": map[string]string{"cf_clearance": "cookie-secret"}}
			query := url.Values{}
			if location != "json" {
				query.Set("state", started.State)
			}
			if location != "query" {
				body["state"] = started.State
			}
			request := pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/perplexity/auth/import", Query: query, Body: marshal(body)}
			response := rt.managementHandle(request, "callback", host.call)
			if response.StatusCode != 201 || bytes.Contains(response.Body, []byte(testToken)) || bytes.Contains(response.Body, []byte("cookie-secret")) {
				t.Fatal("state import failed or leaked a secret")
			}
			for range 2 {
				result := pollTestLogin(t, rt, started.State)
				if result.Status != pluginapi.AuthLoginStatusSuccess || result.Auth.Label != "test@example.invalid" || !bytes.Equal(result.Auth.StorageJSON, host.saved[result.Auth.FileName]) {
					t.Fatal("poll did not return persisted auth")
				}
			}
			status := rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementPrefix + "/auth/login/poll", Query: url.Values{"state": {started.State}}}, "", nil)
			if status.StatusCode != 200 || !bytes.Contains(status.Body, []byte(`"status":"success"`)) || !bytes.Contains(status.Body, []byte("test@example.invalid")) || bytes.Contains(status.Body, []byte(testToken)) || bytes.Contains(status.Body, []byte("cookie-secret")) || bytes.Contains(status.Body, []byte("StorageJSON")) {
				t.Fatal("browser poll did not return sanitized status")
			}
			if rt.managementHandle(request, "callback", host.call).StatusCode != 409 {
				t.Fatal("completed flow accepted a second import")
			}
		})
	}
}

func TestLoginImportFailuresAndRetry(t *testing.T) {
	rt, host := testRuntime(t), newFakeHost()
	started := startTestLogin(t, rt)
	request := pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementPrefix + "/auth/import", Query: url.Values{"state": {started.State}}, Body: marshal(map[string]string{"session_token": testToken, "state": "other-state"})}
	if rt.managementHandle(request, "", host.call).StatusCode != 400 || len(host.saved) != 0 {
		t.Fatal("conflicting states accepted")
	}
	request.Body = marshal(map[string]string{"session_token": testToken})
	host.http = func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("{}")}, nil
	}
	if rt.managementHandle(request, "", host.call).StatusCode != 400 || pollTestLogin(t, rt, started.State).Status != pluginapi.AuthLoginStatusPending {
		t.Fatal("invalid session completed flow")
	}
	host.http = nil
	if rt.managementHandle(request, "", host.call).StatusCode != 201 {
		t.Fatal("failed import could not be retried")
	}
}

func TestLoginImportStorageFailureStaysPending(t *testing.T) {
	rt, host := testRuntime(t), newFakeHost()
	started := startTestLogin(t, rt)
	request := pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementPrefix + "/auth/import", Body: marshal(map[string]string{"session_token": testToken, "state": started.State})}
	callback := func(ctx context.Context, method string, input any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthSave {
			return nil, errors.New("storage unavailable")
		}
		return host.call(ctx, method, input)
	}
	if rt.managementHandle(request, "", callback).StatusCode != 503 || pollTestLogin(t, rt, started.State).Status != pluginapi.AuthLoginStatusPending {
		t.Fatal("unsaved credential completed login")
	}
	if rt.managementHandle(request, "", host.call).StatusCode != 201 {
		t.Fatal("storage failure prevented retry")
	}
}

func TestLoginResourceCSP(t *testing.T) {
	rt := testRuntime(t)
	response := rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/perplexity/login"}, "", nil)
	policy := response.Headers.Get("Content-Security-Policy")
	for _, inline := range []string{panelCSS, loginJS} {
		digest := sha256.Sum256([]byte(inline))
		if !strings.Contains(policy, "'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'") {
			t.Fatal("inline resource lacks CSP hash authorization")
		}
	}
	if response.StatusCode != 200 || strings.Contains(policy, "unsafe-inline") || response.Headers.Get("Cache-Control") != "no-store" || response.Headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("login resource security headers mismatch")
	}
	for _, text := range []string{"打开 Perplexity 登录", "我已登录，捕获会话", "extension/", "id=\"key\""} {
		if !bytes.Contains(response.Body, []byte(text)) {
			t.Fatal("login page missing required UI")
		}
	}
}

func TestLoginExpiryRestartAndBoundedCleanup(t *testing.T) {
	rt := testRuntime(t)
	started := startTestLogin(t, rt)
	for _, current := range []*pluginRuntime{testRuntime(t), rt} {
		if current == rt {
			clear(rt.pending)
		}
		result := pollTestLogin(t, current, started.State)
		if result.Status != pluginapi.AuthLoginStatusError || !strings.Contains(result.Message, "expired") {
			t.Fatal("lost flow not reported as expired")
		}
	}
	state := loginState{Kind: cookieLoginKind, Expires: time.Now().Add(-time.Second).Unix(), Nonce: bytes.Repeat([]byte{3}, 16)}
	expired, err := sealState(rt.stateKey, state)
	if err != nil {
		t.Fatal(err)
	}
	rt.pending[hex.EncodeToString(state.Nonce)] = &pendingFlow{Expires: state.Expires}
	if pollTestLogin(t, rt, expired).Status != pluginapi.AuthLoginStatusError || len(rt.pending) != 0 {
		t.Fatal("expired state not removed")
	}
	request := pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementPrefix + "/auth/import", Query: url.Values{"state": {expired}}, Body: marshal(map[string]string{"session_token": testToken})}
	if rt.managementHandle(request, "", nil).StatusCode != 410 {
		t.Fatal("expired import reached upstream")
	}
	old := startTestLogin(t, rt)
	for _, flow := range rt.pending {
		flow.Expires--
	}
	for range maxPendingFlows + 3 {
		startTestLogin(t, rt)
	}
	if len(rt.pending) != maxPendingFlows || pollTestLogin(t, rt, old.State).Status != pluginapi.AuthLoginStatusError {
		t.Fatal("flow bound did not evict old state")
	}
	rt.stop()
	rt.configure(defaultSettings())
	if len(rt.pending) != 0 || len(rt.stateKey) != 0 {
		t.Fatal("shutdown retained pending credentials")
	}
}

func TestConcurrentLoginImportClaim(t *testing.T) {
	rt := testRuntime(t)
	started := startTestLogin(t, rt)
	finish, err := rt.claimLoginImport(started.State)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			if _, err := rt.claimLoginImport(started.State); err == nil {
				t.Error("parallel import accepted")
			}
			pollTestLogin(t, rt, started.State)
		})
	}
	workers.Wait()
	if err := finish(nil); err != nil {
		t.Fatal(err)
	}
	finish, err = rt.claimLoginImport(started.State)
	if err != nil {
		t.Fatal("aborted claim not released")
	}
	auth := credential{Type: pluginID, SessionToken: testToken}.authData()
	if err := finish(&auth); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyPasteImportForms(t *testing.T) {
	forms := []any{testToken, "Cookie: " + sessionCookie + "=" + testToken + "; cf_clearance=cookie-secret", map[string]string{sessionCookie: testToken, "cf_clearance": "cookie-secret"}}
	for _, cookies := range forms {
		rt, host := testRuntime(t), newFakeHost()
		response := rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementPrefix + "/auth/import", Body: marshal(map[string]any{"cookies": cookies})}, "", host.call)
		if response.StatusCode != 201 || len(host.saved) != 1 {
			t.Fatal("manual paste import changed")
		}
		var cred credential
		if json.Unmarshal(host.saved[credentialName(testToken)], &cred) != nil || cred.SessionToken != testToken {
			t.Fatal("manual credential not persisted")
		}
	}
}

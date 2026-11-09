package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const sessionCookie = "__Secure-next-auth.session-token"

type credential struct {
	Type         string            `json:"type"`
	SessionToken string            `json:"session_token"`
	Cookies      map[string]string `json:"cookies,omitempty"`
	Email        string            `json:"email,omitempty"`
	Name         string            `json:"name,omitempty"`
	AccountID    string            `json:"account_id,omitempty"`
	Plan         string            `json:"plan,omitempty"`
	ValidatedAt  string            `json:"validated_at,omitempty"`
	Label        string            `json:"label,omitempty"`
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._~%+/\-]{20,}={0,2}$`)
var cookieNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

func invalidCredential() *pluginabi.Error {
	return pluginabi.NewError("invalid_credential", "Provide a Perplexity session token or Cookie header containing the signed-in session cookie", 400)
}

func validateCredential(cred credential) (credential, error) {
	if cred.Type != "" && cred.Type != pluginID {
		return credential{}, invalidCredential()
	}
	if cred.SessionToken == "" {
		cred.SessionToken = cred.Cookies[sessionCookie]
	}
	if len(cred.SessionToken) > 16384 || !tokenPattern.MatchString(cred.SessionToken) {
		return credential{}, invalidCredential()
	}
	for name, value := range cred.Cookies {
		if !cookieNamePattern.MatchString(name) {
			return credential{}, invalidCredential()
		}
		for _, char := range value {
			if char < 0x21 || char > 0x7e || strings.ContainsRune(";\"\\,", char) {
				return credential{}, invalidCredential()
			}
		}
	}
	cred.Type = pluginID
	if cred.Cookies != nil {
		cred.Cookies[sessionCookie] = cred.SessionToken
	}
	return cred, nil
}

func parseCredential(raw []byte) (credential, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return credential{}, invalidCredential()
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return credential{}, invalidCredential()
		}
		return parseCredential([]byte(text))
	}
	if raw[0] == '{' {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return credential{}, invalidCredential()
		}
		if fields["type"] != nil || fields["session_token"] != nil || fields["cookies"] != nil {
			var cred credential
			if json.Unmarshal(raw, &cred) != nil {
				return credential{}, invalidCredential()
			}
			return validateCredential(cred)
		}
		var cookies map[string]string
		if json.Unmarshal(raw, &cookies) != nil {
			return credential{}, invalidCredential()
		}
		return validateCredential(credential{Cookies: cookies})
	}
	text := string(raw)
	if strings.HasPrefix(strings.ToLower(text), "cookie:") {
		text = strings.TrimSpace(text[len("cookie:"):])
	}
	if tokenPattern.MatchString(text) && !strings.HasPrefix(text, sessionCookie+"=") {
		return validateCredential(credential{SessionToken: text})
	}
	cookies, err := parseCookieText(text)
	if err != nil {
		return credential{}, err
	}
	return validateCredential(credential{Cookies: cookies})
}

func parseCookieText(text string) (map[string]string, error) {
	if strings.HasPrefix(strings.ToLower(text), "cookie:") {
		text = strings.TrimSpace(text[len("cookie:"):])
	}
	cookies := make(map[string]string)
	for _, pair := range strings.Split(text, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, value, found := strings.Cut(pair, "=")
		if !found || name == "" {
			return nil, invalidCredential()
		}
		if _, exists := cookies[name]; exists {
			return nil, invalidCredential()
		}
		cookies[name] = value
	}
	return cookies, nil
}

func (cred credential) cookieHeader() string {
	names := make([]string, 0, len(cred.Cookies))
	for name := range cred.Cookies {
		if name != sessionCookie {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	pairs := []string{sessionCookie + "=" + cred.SessionToken}
	for _, name := range names {
		pairs = append(pairs, name+"="+cred.Cookies[name])
	}
	return strings.Join(pairs, "; ")
}

func (cred credential) authData() pluginapi.AuthData {
	label := cred.Email
	if label == "" && cred.AccountID != "" {
		account := []rune(cred.AccountID)
		label = "Perplexity (" + string(account[:min(6, len(account))]) + ")"
	}
	if label == "" {
		label = "Perplexity"
	}
	if cred.Label != "" {
		label = cred.Label
	}
	storage, _ := json.Marshal(cred)
	name := credentialName(cred.SessionToken)
	return pluginapi.AuthData{Provider: pluginID, ID: name, FileName: name, Label: label, StorageJSON: storage, Metadata: map[string]any{"email": cred.Email, "type": pluginID}}
}

func parseAuth(request pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	var marker struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(request.RawJSON, &marker)
	file := request.FileName
	if file == "" {
		file = filepath.Base(request.Path)
	}
	named := strings.HasPrefix(file, "perplexity") && strings.HasSuffix(file, ".json")
	if marker.Type != pluginID && !(marker.Type == "" && named) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	cred, err := parseCredential(request.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{}, err
	}
	return pluginapi.AuthParseResponse{Handled: true, Auth: cred.authData()}, nil
}

func probeSession(ctx context.Context, client sender, cred credential) (credential, error) {
	var session struct {
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
			Plan  string `json:"subscription_tier"`
		} `json:"user"`
	}
	if err := client.getJSON(ctx, "/api/auth/session", &cred, &session); err != nil {
		return credential{}, err
	}
	if session.User.ID == "" && session.User.Email == "" {
		return credential{}, accountError()
	}
	cred.Email, cred.Name, cred.AccountID, cred.Plan = session.User.Email, session.User.Name, session.User.ID, session.User.Plan
	if cred.Plan == "" {
		cred.Plan = "free"
	}
	cred.ValidatedAt = time.Now().UTC().Format(time.RFC3339)
	return cred, nil
}

func (rt *pluginRuntime) refreshAuth(request pluginapi.AuthRefreshRequest, id string, callback hostCallbackFunc) (pluginapi.AuthRefreshResponse, error) {
	cred, err := parseCredential(request.StorageJSON)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, pluginabi.NewError("reauth_required", "Perplexity session expired; import the session cookie again in the plugin panel", 401)
	}
	rt.rememberHost(request.Host)
	cfg, _ := rt.snapshot()
	ctx, cancel := rt.requestContext(8 * time.Second)
	defer cancel()
	updated, err := probeSession(ctx, sender{callback: callback, id: id, cfg: cfg}, cred)
	if err != nil {
		if detail, ok := err.(*pluginabi.Error); ok && detail.HTTPStatus == 401 {
			return pluginapi.AuthRefreshResponse{}, pluginabi.NewError("reauth_required", "Perplexity session expired; import the session cookie again in the plugin panel", 401)
		}
		return pluginapi.AuthRefreshResponse{}, pluginabi.NewError("upstream_unavailable", "Unable to validate Perplexity session; try again later", 503)
	}
	auth := updated.authData()
	auth.NextRefreshAfter = time.Now().Add(6 * time.Hour)
	return pluginapi.AuthRefreshResponse{Auth: auth, NextRefreshAfter: auth.NextRefreshAfter}, nil
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const testToken = "hermetic-session-token-0000000000000000"
const testSession = `{"user":{"id":"account-12345678","name":"Test User","email":"test@example.invalid","subscription_tier":"pro"}}`
const testCatalog = `{"models":{"gpt6_astra":{"label":"Astra","mode":"search"},"claude50sonnet":{"label":"Sonnet","mode":"search"},"max_only":{"label":"Max","mode":"research"},"asi":{"mode":"asi"},"agent":{"mode":"agentic_research"}},"search_config":[{"label":"Astra","subscription_tier":null,"non_reasoning_model":"gpt6_astra"},{"label":"Sonnet","subscription_tier":"pro","reasoning_model":"claude50sonnet"},{"label":"Max","subscription_tier":"max","reasoning_model":"max_only"},{"label":"Not chat","non_reasoning_model":"asi"},{"label":"Not chat either","non_reasoning_model":"agent"}]}`

type fakeHost struct {
	mu         sync.Mutex
	requests   []pluginapi.HTTPRequest
	methods    []string
	http       func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
	open       hostStreamResponse
	chunks     []hostStreamRead
	readErr    error
	read       func() (json.RawMessage, error)
	emitted    [][]byte
	closed     chan streamClose
	saved      map[string]json.RawMessage
	opCount    int
	streamGone int
}

func newFakeHost() *fakeHost {
	return &fakeHost{open: hostStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "upstream"}, closed: make(chan streamClose, 8), saved: map[string]json.RawMessage{}}
}

func marshal(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func (host *fakeHost) call(ctx context.Context, method string, value any) (json.RawMessage, error) {
	host.mu.Lock()
	host.methods = append(host.methods, method)
	switch method {
	case pluginabi.MethodHostHTTPOperationOpen:
		host.opCount++
		result := marshal(map[string]string{"operation_id": fmt.Sprintf("operation-%d", host.opCount)})
		host.mu.Unlock()
		return result, nil
	case pluginabi.MethodHostHTTPCancel:
		host.mu.Unlock()
		return marshal(struct{}{}), nil
	case pluginabi.MethodHostHTTPDo, pluginabi.MethodHostHTTPDoStream:
		var wrapped hostHTTPRequest
		if json.Unmarshal(marshal(value), &wrapped) != nil {
			host.mu.Unlock()
			return nil, errors.New("bad test HTTP request")
		}
		host.requests = append(host.requests, wrapped.Request)
		if method == pluginabi.MethodHostHTTPDoStream {
			response := host.open
			host.mu.Unlock()
			return marshal(response), nil
		}
		handler := host.http
		host.mu.Unlock()
		if handler != nil {
			response, err := handler(wrapped.Request)
			return marshal(response), err
		}
		switch {
		case strings.Contains(wrapped.Request.URL, "/api/auth/session"):
			return marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testSession)}), nil
		case strings.Contains(wrapped.Request.URL, "/models/config/"):
			return marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testCatalog)}), nil
		default:
			return marshal(pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, Body: []byte(answerSSE("Hello"))}), nil
		}
	case pluginabi.MethodHostHTTPStreamRead:
		if host.read != nil {
			read := host.read
			host.mu.Unlock()
			return read()
		}
		if host.readErr != nil {
			err := host.readErr
			host.mu.Unlock()
			return nil, err
		}
		chunk := hostStreamRead{Done: true}
		if len(host.chunks) > 0 {
			chunk, host.chunks = host.chunks[0], host.chunks[1:]
		}
		host.mu.Unlock()
		return marshal(chunk), nil
	case pluginabi.MethodHostHTTPStreamClose:
		host.streamGone++
		host.mu.Unlock()
		return marshal(struct{}{}), nil
	case pluginabi.MethodHostStreamEmit:
		chunk := value.(streamEmit)
		host.emitted = append(host.emitted, bytes.Clone(chunk.Payload))
		host.mu.Unlock()
		return marshal(struct{}{}), nil
	case pluginabi.MethodHostStreamClose:
		host.mu.Unlock()
		host.closed <- value.(streamClose)
		return marshal(struct{}{}), nil
	case pluginabi.MethodHostAuthSave:
		request := value.(pluginapi.HostAuthSaveRequest)
		host.saved[request.Name] = bytes.Clone(request.JSON)
		host.mu.Unlock()
		return marshal(pluginapi.HostAuthSaveResponse{Name: request.Name}), nil
	case pluginabi.MethodHostAuthList:
		files := make([]pluginapi.HostAuthFileEntry, 0, len(host.saved))
		for name := range host.saved {
			files = append(files, pluginapi.HostAuthFileEntry{Name: name, AuthIndex: name, Provider: pluginID})
		}
		host.mu.Unlock()
		return marshal(map[string]any{"files": files}), nil
	case pluginabi.MethodHostAuthGet:
		request := value.(pluginapi.HostAuthGetRequest)
		data := host.saved[request.AuthIndex]
		host.mu.Unlock()
		return marshal(pluginapi.HostAuthGetResponse{Name: request.AuthIndex, JSON: data}), nil
	default:
		host.mu.Unlock()
		return nil, errors.New("unexpected host method")
	}
}

func testRuntime(t *testing.T) *pluginRuntime {
	t.Helper()
	rt := newRuntime()
	t.Cleanup(rt.stop)
	return rt
}

func testCredential() []byte {
	return marshal(credential{Type: pluginID, SessionToken: testToken, Plan: "pro"})
}

func testExecutor() pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{Model: "pplx-auto", Format: "openai", StorageJSON: testCredential(), Payload: []byte(`{"model":"pplx-auto","messages":[{"role":"user","content":"Hello?"}]}`)}
}

func eventJSON(value string) string { return "event: message\ndata: " + value + "\n\n" }

func answerSSE(answer string) string {
	frame := map[string]any{"blocks": []any{map[string]any{"markdown_block": map[string]any{"answer": answer}}}}
	return eventJSON(string(marshal(frame))) + "event: end_of_stream\ndata: {}\n\n"
}

func errorDetail(t *testing.T, err error, code string, status int) *pluginabi.Error {
	t.Helper()
	var detail *pluginabi.Error
	if !errors.As(err, &detail) || detail.Code != code || detail.HTTPStatus != status {
		t.Fatalf("error = %#v, want %s/%d", err, code, status)
	}
	if strings.Contains(detail.Message, testToken) || strings.Contains(detail.Message, "cookie-secret") {
		t.Fatal("error leaked credential")
	}
	return detail
}

func TestRegistrationAndDispatch(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	raw := marshal(map[string]any{"schema_version": 1, "config_yaml": []byte("enabled: true\npriority: 3\nstore: {channel: stable}\n")})
	envelopeRaw := rpcEnvelope(pluginabi.MethodPluginRegister, raw, rt.dispatch, host.call)
	var envelope pluginabi.Envelope
	if json.Unmarshal(envelopeRaw, &envelope) != nil || !envelope.OK {
		t.Fatalf("registration: %s", envelopeRaw)
	}
	var result struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]any     `json:"capabilities"`
	}
	if json.Unmarshal(envelope.Result, &result) != nil || result.SchemaVersion != pluginabi.SchemaVersion || result.Metadata.Author != "lzt404" || result.Metadata.GitHubRepository != pluginRepo {
		t.Fatal("registration metadata mismatch")
	}
	for _, name := range []string{"auth_provider", "model_provider", "executor", "quota_provider", "management_api"} {
		if result.Capabilities[name] != true {
			t.Fatalf("missing capability %s", name)
		}
	}
	for _, name := range []string{"executor_input_formats", "executor_output_formats"} {
		if !reflect.DeepEqual(result.Capabilities[name], []any{"openai"}) {
			t.Fatalf("invalid %s", name)
		}
	}
	if result.Capabilities["executor_model_scope"] != "oauth" || len(result.Metadata.ConfigFields) != 12 {
		t.Fatal("wrong executor scope/config fields")
	}
	for _, method := range []string{pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier} {
		value, err := rt.dispatch(method, []byte("{}"), host.call)
		if err != nil || !bytes.Contains(marshal(value), []byte(`"identifier":"perplexity"`)) {
			t.Fatalf("identifier method %s failed", method)
		}
	}
	_, err := rt.dispatch(pluginabi.MethodAuthLoginStart, nil, host.call)
	errorDetail(t, err, "invalid_request", 400)
	_, err = rt.dispatch("not-a-method", nil, host.call)
	errorDetail(t, err, "method_not_found", 404)
	_, err = rt.dispatch(pluginabi.MethodExecutorExecute, []byte("[]"), host.call)
	errorDetail(t, err, "invalid_request", 400)
	for _, yaml := range []string{"typo: true", "language: en\n---\n{}"} {
		_, err = rt.dispatch(pluginabi.MethodPluginRegister, marshal(map[string]any{"config_yaml": []byte(yaml)}), host.call)
		errorDetail(t, err, "invalid_config", 400)
	}
}

func TestRPCSafety(t *testing.T) {
	panicHandler := func(string, []byte, hostCallbackFunc) (any, error) { panic(testToken) }
	for _, raw := range [][]byte{nil, make([]byte, maxRPCPayload+1)} {
		encoded := rpcEnvelope(pluginabi.MethodPluginRegister, raw, panicHandler, nil)
		var envelope pluginabi.Envelope
		if json.Unmarshal(encoded, &envelope) != nil || envelope.OK || envelope.Error == nil || bytes.Contains(encoded, []byte(testToken)) {
			t.Fatal("unsafe error envelope")
		}
	}
	encoded := rpcEnvelope("", nil, func(string, []byte, hostCallbackFunc) (any, error) { return make(chan int), nil }, nil)
	var envelope pluginabi.Envelope
	if json.Unmarshal(encoded, &envelope) != nil || envelope.OK || envelope.Error.Code != "internal_error" {
		t.Fatal("serialization failure not enveloped")
	}
}

func TestStrictConfig(t *testing.T) {
	for _, input := range []string{"", "{}", "enabled: true\npriority: 0\nstore: {}", "sources: [scholar, social]\ninclude_sources: false\nrequest_timeout: 10m", "base_url: https://mirror.invalid/perplexity"} {
		cfg, err := parseSettings([]byte(input))
		if err != nil || cfg.MaxConcurrency != 2 || cfg.TTL != 6*time.Hour {
			t.Fatalf("valid config rejected: %q", input)
		}
	}
	for _, input := range []string{
		"unknown: " + testToken, "language: en\n---\nlanguage: zh", "{}\n---\n", "{}\n---\n{}\n---\n{}",
		"sources: [video]", "sources: []", "catalog_ttl: bad", "catalog_ttl: 0s", "request_timeout: -1s",
		"max_concurrency: 0", "max_prompt_chars: 1", "pro_query_limit: -1", "research_query_limit: -1",
		"base_url: ftp://example.invalid", "base_url: https:///missing", "base_url: https://user:pass@example.invalid", "base_url: https://example.invalid?x=1",
		"user_agent: \"bad\\r\\nheader\"", "language: ''", "max_concurrency: false", "include_sources: maybe", "language: en\nlanguage: zh", "[]",
	} {
		_, err := parseSettings([]byte(input))
		errorDetail(t, err, "invalid_config", 400)
	}
}

func TestMirrorRequestOrigin(t *testing.T) {
	cfg, err := parseSettings([]byte("base_url: https://mirror.invalid/perplexity/"))
	if err != nil {
		t.Fatal(err)
	}
	request := (sender{cfg: cfg}).request(http.MethodGet, "/api/auth/session", nil, nil)
	if request.URL != "https://mirror.invalid/perplexity/api/auth/session" || request.Headers.Get("Origin") != "https://mirror.invalid" || request.Headers.Get("Referer") != "https://mirror.invalid/perplexity/" {
		t.Fatal("mirror prefix changed the HTTP origin")
	}
}

func TestCredentialParsing(t *testing.T) {
	inputs := []string{
		testToken, string(marshal(testToken)), sessionCookie + "=" + testToken,
		"Cookie: " + sessionCookie + "=" + testToken + "; cf_clearance=cookie-secret; __Host-pplx-last-active-account=acct",
		string(marshal(map[string]string{sessionCookie: testToken, "cf_clearance": "cookie-secret"})),
		string(marshal(credential{Type: pluginID, SessionToken: testToken, Cookies: map[string]string{sessionCookie: "stale", "cf_clearance": "cookie-secret"}})),
	}
	for _, input := range inputs {
		cred, err := parseCredential([]byte(input))
		if err != nil || cred.SessionToken != testToken || cred.Type != pluginID || !strings.HasPrefix(cred.cookieHeader(), sessionCookie+"="+testToken) {
			t.Fatal("valid credential rejected")
		}
		if cred.Cookies["cf_clearance"] == "cookie-secret" && !strings.Contains(cred.cookieHeader(), "cf_clearance=cookie-secret") {
			t.Fatal("extra cookie not preserved")
		}
	}
	for _, input := range []string{"", "junk", "hello world, this is not a token", "{}", "[]", "null", "other=" + testToken, "type=perplexity", `{"type":"other","session_token":"` + testToken + `"}`, `{"cookies":false}`, "Cookie: " + sessionCookie + "=" + testToken + "; bad=foo\r\nCookie: secret", sessionCookie + "=" + testToken + "; " + sessionCookie + "=" + testToken} {
		_, err := parseCredential([]byte(input))
		errorDetail(t, err, "invalid_credential", 400)
	}
	cred, _ := parseCredential(testCredential())
	auth := cred.authData()
	if auth.ID != credentialName(testToken) || auth.FileName != auth.ID || auth.Label != "Perplexity" || len(auth.Metadata) != 2 || auth.Metadata["oauth_provider"] != nil {
		t.Fatal("incorrect auth record")
	}
	cred.AccountID = "abcdefghi"
	if cred.authData().Label != "Perplexity (abcdef)" {
		t.Fatal("account label mismatch")
	}
	cred.Email = "email@example.invalid"
	if cred.authData().Label != cred.Email {
		t.Fatal("email label mismatch")
	}
	parsed, err := parseAuth(pluginapi.AuthParseRequest{FileName: "perplexity-test.json", RawJSON: []byte(testToken)})
	if err != nil || !parsed.Handled {
		t.Fatal("named bare token not recognized")
	}
	parsed, err = parseAuth(pluginapi.AuthParseRequest{FileName: "other.json", RawJSON: []byte(testToken)})
	if err != nil || parsed.Handled {
		t.Fatal("unrelated auth was claimed")
	}
	parsed, err = parseAuth(pluginapi.AuthParseRequest{FileName: "other.json", RawJSON: testCredential()})
	if err != nil || !parsed.Handled {
		t.Fatal("typed credential not recognized")
	}
}

func TestCatalogMapping(t *testing.T) {
	var catalog modelCatalog
	if json.Unmarshal([]byte(testCatalog), &catalog) != nil {
		t.Fatal("bad fixture")
	}
	cfg := defaultSettings()
	models := mappedModels(catalog, "pro", cfg)
	if len(models) != 6 || models[0].ID != "pplx-auto" || models[4].ID != "pplx-gpt6_astra" || models[5].ID != "pplx-claude50sonnet" {
		t.Fatalf("model order/filter wrong: %#v", models)
	}
	if len(mappedModels(catalog, "max", cfg)) != 7 {
		t.Fatal("max tier missing model")
	}
	for _, mapping := range models {
		for _, id := range []string{mapping.ID, "perplexity/" + mapping.ID, mapping.Preference, "pplx-" + mapping.Preference} {
			got, err := resolveModel(id, models)
			if err != nil || got.Preference != mapping.Preference || got.Mode != mapping.Mode {
				t.Fatalf("round-trip failed for %s", id)
			}
		}
	}
	_, err := resolveModel("pplx-unpublished", models)
	errorDetail(t, err, "unknown_model", 400)
	cfg.ExcludeModels = []string{"pplx-pro", "gpt6_astra"}
	models = mappedModels(catalog, "pro", cfg)
	if len(models) != 4 {
		t.Fatal("excluded model advertised")
	}
	_, err = resolveModel("pplx-gpt6_astra", models)
	errorDetail(t, err, "unknown_model", 400)
	catalog.SearchConfig = nil
	if len(mappedModels(catalog, "pro", defaultSettings())) != 7 {
		t.Fatal("catalog fallback modes incorrect")
	}
	for _, info := range modelInfos(models) {
		if info.OwnedBy != pluginID || info.Object != "model" || info.Type != "chat" || info.Name == "" || !reflect.DeepEqual(info.SupportedInputModalities, []string{"text"}) || !reflect.DeepEqual(info.SupportedGenerationMethods, []string{"chat.completions"}) {
			t.Fatal("model info contract mismatch")
		}
	}
}

func TestCatalogCacheAndFallback(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	dir := t.TempDir()
	request := pluginapi.StaticModelRequest{Host: pluginapi.HostConfigSummary{AuthDir: dir}}
	for range 2 {
		models := rt.models(request.Host, "pro", "callback", host.call)
		if len(models.Models) != 6 {
			t.Fatal("model catalog fetch failed")
		}
	}
	if len(host.requests) != 1 {
		t.Fatal("catalog not cached")
	}
	// The snapshot must stay out of the credential directory: CPA treats every
	// top-level *.json in auth-dir as an auth file.
	info, err := os.Stat(filepath.Join(dir, ".perplexity-cache", "catalog.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("catalog not persisted privately")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 || !entries[0].IsDir() {
		t.Fatal("catalog cache must not add a top-level file to auth-dir")
	}
	second := testRuntime(t)
	second.models(request.Host, "pro", "callback", host.call)
	if len(host.requests) != 1 {
		t.Fatal("persisted catalog not reused")
	}
	fallback := testRuntime(t)
	host.http = func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{}, errors.New("offline " + testToken)
	}
	if len(fallback.models(pluginapi.HostConfigSummary{}, "pro", "callback", host.call).Models) != 4 {
		t.Fatal("offline catalog fallback missing")
	}
}

func TestSSEAccumulation(t *testing.T) {
	payload := ": heartbeat\r\n\r\n" + strings.ReplaceAll(eventJSON(`{"blocks":[{"markdown_block":{"chunks":["Hel"],"chunk_starting_offset":0}}]}`), "\n", "\r\n") +
		eventJSON(`{"blocks":[{"markdown_block":{"chunks":["Hel","lo"],"chunk_starting_offset":0}}]}`) +
		eventJSON(`{"blocks":[{"markdown_block":{"chunks":[" 🌍"],"chunk_starting_offset":2}},{"web_result_block":{"web_results":[{"title":"Example","url":"https://example.invalid"},{"title":"duplicate","url":"https://example.invalid"}]}}]}`) +
		eventJSON(`{"backend_uuid":"backend","thread_url_slug":"thread","status":"COMPLETED","blocks":[{"markdown_block":{"answer":"Hello 🌍!"}}]}`) + "data: [DONE]\n\n"
	for _, size := range []int{1, 2, 7, 5000} {
		decoder := sseDecoder{}
		acc := answerAccumulator{}
		for offset := 0; offset < len(payload); offset += size {
			if err := consumeEvents(&decoder, &acc, []byte(payload[offset:min(offset+size, len(payload))]), false, nil); err != nil {
				t.Fatalf("incremental SSE failed: %v", err)
			}
		}
		if err := consumeEvents(&decoder, &acc, nil, true, nil); err != nil || acc.Answer != "Hello 🌍!" || !acc.Ended || len(acc.Sources) != 1 || acc.ThreadSlug != "thread" {
			t.Fatalf("bad accumulation: %#v", acc)
		}
		if acc.sourcesMarkdown() != "\n\n---\n**Sources**\n- [Example](https://example.invalid)" {
			t.Fatal("source formatting mismatch")
		}
	}
	for _, answer := range []any{"legacy", `{"answer":"legacy"}`, map[string]string{"answer": "legacy"}} {
		acc := answerAccumulator{}
		frame := map[string]any{"text": []any{map[string]any{"step_type": "FINAL", "content": map[string]any{"answer": answer}}}}
		if err := acc.apply(sseEvent{Data: string(marshal(frame))}); err != nil || acc.Answer != "legacy" {
			t.Fatal("legacy final answer failed")
		}
	}
	for _, fixture := range []struct {
		body string
		code string
	}{
		{`{"upsell_information":{"upsell_type":"LOGIN"}}`, "account_unavailable"},
		{`{"status":"FAILED"}`, "upstream_error"}, {`{"status":"ERROR"}`, "upstream_error"}, {`{"status":"CANCELLED"}`, "upstream_error"},
		{`{"error":"` + testToken + `"}`, "upstream_error"}, {`invalid`, "upstream_protocol_error"},
		{`{"blocks":[{"markdown_block":{"chunks":["gap"],"chunk_starting_offset":5}}]}`, "upstream_protocol_error"},
	} {
		acc := answerAccumulator{Authenticated: true}
		status := 502
		if fixture.code == "account_unavailable" {
			status = 401
		}
		errorDetail(t, acc.apply(sseEvent{Data: fixture.body}), fixture.code, status)
	}
	decoder := sseDecoder{}
	_, err := decoder.feed(bytes.Repeat([]byte("x"), maxSSEFrame+1), false)
	errorDetail(t, err, "upstream_protocol_error", 502)
}

func TestPromptFlatteningAndCount(t *testing.T) {
	_, prompt, err := parseChat([]byte(`{"messages":[{"role":"USER","content":[{"type":"text","text":"one"},{"type":"image_url","image_url":{"url":"https://ignored.invalid"}},{"type":"text","text":"two"}]}]}`), 48000)
	if err != nil || prompt != "onetwo" {
		t.Fatal("text parts not flattened")
	}
	_, prompt, err = parseChat([]byte(`{"messages":[{"role":"SYSTEM","content":"Be brief"},{"role":"user","content":"Hi"},{"role":"assistant","content":"Hello"},{"role":"user","content":"Next"}]}`), 48000)
	if err != nil || prompt != "System: Be brief\n\nUser: Hi\n\nAssistant: Hello\n\nUser: Next" {
		t.Fatal("transcript mismatch")
	}
	_, prompt, err = parseChat(marshal(map[string]any{"messages": []any{map[string]string{"role": "user", "content": strings.Repeat("界", 100)}}}), 32)
	if err != nil || !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) != 32 || !strings.HasPrefix(prompt, "[...truncated...]") {
		t.Fatal("rune-safe prompt cap failed")
	}
	for _, raw := range []string{"null", "[]", "{}", `{"messages":[]}`, `{"messages":[{"role":"user","content":{}}]}`, `{"messages":[{"role":"bogus","content":"Hi"}]}`} {
		_, _, err = parseChat([]byte(raw), 48000)
		errorDetail(t, err, "invalid_request", 400)
	}
	rt := testRuntime(t)
	value, err := rt.dispatch(pluginabi.MethodExecutorCountTokens, marshal(executorRPC{ExecutorRequest: testExecutor()}), func(context.Context, string, any) (json.RawMessage, error) {
		t.Fatal("token counting used network")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		pluginapi.ExecutorResponse
		InputTokens int `json:"input_tokens"`
	}
	if json.Unmarshal(marshal(value), &result) != nil || result.InputTokens != 2 || string(result.Payload) != `{"input_tokens":2}` {
		t.Fatal("host token-count payload mismatch")
	}
}

func TestExecuteCompletion(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	host.http = func(request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.Contains(request.URL, "/models/config/") {
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testCatalog)}, nil
		}
		for name, expected := range map[string]string{"User-Agent": browserUA, "Cookie": sessionCookie + "=" + testToken, "Accept": "text/event-stream", "Origin": "https://www.perplexity.ai", "X-Perplexity-Request-Reason": "ask-query-state-provider", "Sec-Ch-Ua-Mobile": "?0"} {
			if request.Headers.Get(name) != expected {
				t.Errorf("incorrect request header %s", name)
			}
		}
		var body struct {
			Query  string         `json:"query_str"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(request.Body, &body) != nil || body.Query != "Hello?" || body.Params["model_preference"] != "turbo" || body.Params["mode"] != "concise" || body.Params["version"] != "2.18" || body.Params["last_backend_uuid"] != nil {
			t.Error("upstream request body mismatch")
		}
		for _, name := range []string{"frontend_uuid", "frontend_context_uuid"} {
			id, ok := body.Params[name].(string)
			if !ok || len(id) != 36 || id[14] != '4' {
				t.Errorf("bad UUID field %s", name)
			}
		}
		bodyText := eventJSON(`{"thread_url_slug":"test-thread","blocks":[{"markdown_block":{"answer":"Answer"}},{"web_result_block":{"web_results":[{"title":"Example","url":"https://example.invalid"}]}}]}`) + "event: end_of_stream\ndata: {}\n\n"
		return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}, "Set-Cookie": {"secret=not-forwarded"}}, Body: []byte(bodyText)}, nil
	}
	response, err := rt.execute(testExecutor(), "callback", host.call)
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		Object  string `json:"object"`
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Input  int `json:"prompt_tokens"`
			Output int `json:"completion_tokens"`
			Total  int `json:"total_tokens"`
		} `json:"usage"`
		Perplexity struct {
			ThreadURL string       `json:"thread_url"`
			Sources   []sourceLink `json:"sources"`
		} `json:"perplexity"`
	}
	if json.Unmarshal(response.Payload, &completion) != nil || completion.Object != "chat.completion" || completion.Model != "pplx-auto" || !strings.HasPrefix(completion.ID, "chatcmpl-") || len(completion.Choices) != 1 {
		t.Fatal("invalid chat.completion")
	}
	choice := completion.Choices[0]
	if choice.Message.Role != "assistant" || choice.Finish != "stop" || !strings.Contains(choice.Message.Content, "**Sources**") || completion.Usage.Total != completion.Usage.Input+completion.Usage.Output || completion.Perplexity.ThreadURL != "https://www.perplexity.ai/search/test-thread" || len(completion.Perplexity.Sources) != 1 {
		t.Fatal("completion fields incorrect")
	}
	if response.Headers.Get("Set-Cookie") != "" || bytes.Contains(response.Payload, []byte(testToken)) {
		t.Fatal("completion leaked credential/header")
	}
	cfg := defaultSettings()
	cfg.IncludeSources = false
	rt.configure(cfg)
	response, err = rt.execute(testExecutor(), "callback", host.call)
	if err != nil || bytes.Contains(response.Payload, []byte("**Sources**")) {
		t.Fatal("include_sources=false ignored")
	}
	request := testExecutor()
	request.Model = "pplx-unknown"
	_, err = rt.execute(request, "callback", host.call)
	errorDetail(t, err, "unknown_model", 400)
}

func waitClose(t *testing.T, rt *pluginRuntime, host *fakeHost) streamClose {
	t.Helper()
	select {
	case closed := <-host.closed:
		rt.workers.Wait()
		return closed
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close")
		return streamClose{}
	}
}

func TestStreamingDeltas(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	payload := eventJSON(`{"blocks":[{"markdown_block":{"chunks":["Hel"],"chunk_starting_offset":0}}]}`) +
		eventJSON(`{"blocks":[{"markdown_block":{"chunks":["Hel","lo"],"chunk_starting_offset":0}}]}`) +
		eventJSON(`{"blocks":[{"markdown_block":{"chunks":[" 🌍"],"chunk_starting_offset":2}},{"web_result_block":{"web_results":[{"title":"Source","url":"https://example.invalid"}]}}]}`) + "data: [DONE]\n\n"
	for offset := 0; offset < len(payload); offset += 7 {
		host.chunks = append(host.chunks, hostStreamRead{Payload: []byte(payload[offset:min(offset+7, len(payload))])})
	}
	request := executorRPC{ExecutorRequest: testExecutor(), StreamID: "output", callbackContext: callbackContext{HostCallbackID: "callback"}}
	if _, err := rt.dispatch(pluginabi.MethodExecutorExecuteStream, marshal(request), host.call); err != nil {
		t.Fatal(err)
	}
	closed := waitClose(t, rt, host)
	if closed.ErrorDetail != nil || closed.StreamID != "output" || host.streamGone != 1 {
		t.Fatal("stream cleanup failed")
	}
	if len(host.emitted) != 6 {
		t.Fatalf("got %d chunks, expected role + 3 deltas + sources + stop", len(host.emitted))
	}
	var text strings.Builder
	for index, raw := range host.emitted {
		if !json.Valid(raw) || bytes.HasPrefix(raw, []byte("data:")) || bytes.Contains(raw, []byte(testToken)) {
			t.Fatal("host emit must contain bare secret-free JSON")
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta  map[string]string `json:"delta"`
				Finish *string           `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &chunk) != nil || chunk.Object != "chat.completion.chunk" || len(chunk.Choices) != 1 {
			t.Fatal("invalid chunk")
		}
		choice := chunk.Choices[0]
		if index == 0 && choice.Delta["role"] != "assistant" {
			t.Fatal("first chunk missing assistant role")
		}
		if index == len(host.emitted)-1 && (len(choice.Delta) != 0 || choice.Finish == nil || *choice.Finish != "stop") {
			t.Fatal("missing final stop chunk")
		}
		text.WriteString(choice.Delta["content"])
	}
	if text.String() != "Hello 🌍\n\n---\n**Sources**\n- [Source](https://example.invalid)" {
		t.Fatal("cumulative snapshots emitted twice")
	}
	_, err := answerDelta("Hello", "Rewritten")
	errorDetail(t, err, "upstream_protocol_error", 502)
}

func TestStreamFailures(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		status  int
		ctype   string
		payload string
		readErr error
		code    string
		want    int
		sync    bool
	}{
		{name: "rate", status: 429, payload: testToken, code: "upstream_rate_limited", want: 429, sync: true},
		{name: "json", status: 200, ctype: "application/json; charset=utf-8", payload: `{"error":"` + testToken + `"}`, code: "upstream_protocol_error", want: 502, sync: true},
		{name: "login-json", status: 200, ctype: "application/json", payload: `{"upsell_information":{"upsell_type":"LOGIN"}}`, code: "account_unavailable", want: 401, sync: true},
		{name: "forbidden", status: 403, payload: testToken, code: "account_unavailable", want: 401, sync: true},
		{name: "empty", status: 200, payload: "data: [DONE]\n\n", code: "upstream_protocol_error", want: 502},
		{name: "garbage", status: 200, payload: "data: not-json\n\n", code: "upstream_protocol_error", want: 502},
		{name: "upsell", status: 200, payload: eventJSON(`{"upsell_information":{"upsell_type":"LOGIN"}}`), code: "account_unavailable", want: 401},
		{name: "midstream", status: 200, payload: eventJSON(`{"blocks":[{"markdown_block":{"answer":"Hi"}}]}`) + eventJSON(`{"error":"`+testToken+`"}`), code: "upstream_error", want: 502},
		{name: "client-cancel", status: 200, readErr: errors.New("context canceled " + testToken), code: "client_closed", want: 499},
		{name: "client-closed", status: 200, readErr: errors.New("client closed " + testToken), code: "client_closed", want: 499},
		{name: "timeout", status: 200, readErr: context.DeadlineExceeded, code: "upstream_timeout", want: 504},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			rt := testRuntime(t)
			host := newFakeHost()
			host.open.StatusCode = fixture.status
			if fixture.ctype != "" {
				host.open.Headers.Set("Content-Type", fixture.ctype)
			}
			host.open.Headers.Set("Retry-After", "17")
			host.readErr = fixture.readErr
			host.chunks = []hostStreamRead{{Payload: []byte(fixture.payload), Done: true}}
			_, err := rt.executeStream(testExecutor(), "callback", "output", host.call)
			if fixture.sync {
				detail := errorDetail(t, err, fixture.code, fixture.want)
				if fixture.status == 429 && detail.RetryAfterSeconds != 17 {
					t.Fatal("Retry-After not forwarded")
				}
				if len(host.emitted) != 0 || host.streamGone != 1 {
					t.Fatal("error response emitted content or leaked stream")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				closed := waitClose(t, rt, host)
				errorDetail(t, closed.ErrorDetail, fixture.code, fixture.want)
				if host.streamGone != 1 {
					t.Fatal("upstream stream leaked")
				}
				if fixture.name != "midstream" && len(host.emitted) != 0 {
					t.Fatal("role emitted before an answer was available")
				}
			}
		})
	}
}

func TestNonStreamFailuresAndClassification(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 504} {
		detail := statusError(status, http.Header{"Retry-After": {"23"}})
		wantCode, wantStatus := "upstream_error", 502
		switch status {
		case 401, 403:
			wantCode, wantStatus = "account_unavailable", 401
		case 429:
			wantCode, wantStatus = "upstream_rate_limited", 429
		case 504:
			wantCode, wantStatus = "upstream_timeout", 504
		}
		errorDetail(t, detail, wantCode, wantStatus)
	}
	for _, fixture := range []struct {
		body  string
		ctype string
		code  string
		want  int
	}{
		{"data: [DONE]\n\n", "text/event-stream", "upstream_protocol_error", 502},
		{eventJSON(`{"upsell_information":{"upsell_type":"LOGIN"}}`), "text/event-stream", "account_unavailable", 401},
		{`{"error":"` + testToken + `"}`, "application/json", "upstream_protocol_error", 502},
		{"data: garbage\n\n", "text/event-stream", "upstream_protocol_error", 502},
	} {
		rt := testRuntime(t)
		host := newFakeHost()
		host.http = func(request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			if strings.Contains(request.URL, "/models/config/") {
				return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(testCatalog)}, nil
			}
			return pluginapi.HTTPResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {fixture.ctype}}, Body: []byte(fixture.body)}, nil
		}
		_, err := rt.execute(testExecutor(), "callback", host.call)
		errorDetail(t, err, fixture.code, fixture.want)
	}
	errorDetail(t, classifyError(errors.New("timeout "+testToken)), "upstream_timeout", 504)
	errorDetail(t, classifyError(errors.New("transport "+testToken)), "upstream_error", 502)
	response := managementError(statusError(429, http.Header{"Retry-After": {"20"}}))
	if response.Headers.Get("Retry-After") != "20" {
		t.Fatal("management rate header missing")
	}
}

func TestQuotaNormalizationAndFetch(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	host.http = func(request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		body := "{}"
		switch {
		case strings.HasSuffix(request.URL, "/api/auth/session"):
			body = testSession
		case strings.HasSuffix(request.URL, "/rest/rate-limit/all"):
			body = `{"remaining_pro":25,"remaining_research":3,"remaining_labs":1,"remaining_agentic_research":2}`
		case strings.HasSuffix(request.URL, "/rest/user/settings"):
			body = `{"upload_limit":7,"pages_limit":9}`
		case strings.HasSuffix(request.URL, "/rest/billing/credits"):
			return pluginapi.HTTPResponse{StatusCode: 403}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
	}
	request := pluginapi.QuotaFetchRequest{Provider: pluginID, StorageJSON: testCredential()}
	result, err := rt.fetchQuota(request, "callback", host.call)
	if err != nil || result.Subscription.Plan != "pro" || len(result.Summary) != 6 || len(result.Groups) != 1 || len(result.Groups[0].Buckets) != 0 {
		t.Fatalf("unknown quota denominator fabricated: %#v, %v", result, err)
	}
	cfg := defaultSettings()
	cfg.ProQueryLimit, cfg.ResearchQueryLimit = 100, 10
	rt.configure(cfg)
	result, err = rt.fetchQuota(request, "callback", host.call)
	if err != nil || len(result.Groups[0].Buckets) != 2 || result.Groups[0].Buckets[0].RemainingFraction != 0.25 || result.Groups[0].Buckets[1].RemainingFraction != 0.3 || !strings.Contains(result.Groups[0].Buckets[0].Description, "25 / 100") {
		t.Fatal("known quota denominator mapping failed")
	}
	invalid := newFakeHost()
	invalid.http = func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("{}")}, nil
	}
	_, err = rt.fetchQuota(request, "callback", invalid.call)
	errorDetail(t, err, "account_unavailable", 401)
	if len(invalid.requests) != 1 || !strings.HasSuffix(invalid.requests[0].URL, "/api/auth/session") {
		t.Fatal("billing called without valid session")
	}
}

func TestManagementImportStatusAndRefresh(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	registration := managementRegistration()
	if len(registration.Routes) != 5 || len(registration.Resources) != 4 {
		t.Fatal("route contract mismatch")
	}
	for _, route := range registration.Routes {
		if !strings.HasPrefix(route.Path, managementPrefix+"/") || route.Menu != "" {
			t.Fatal("management route escaped authentication prefix")
		}
	}
	for range 2 {
		response := rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/perplexity/auth/import", Body: marshal(map[string]any{"session_token": testToken, "cookies": "cf_clearance=cookie-secret", "label": "Test"})}, "callback", host.call)
		if response.StatusCode != 201 || bytes.Contains(response.Body, []byte(testToken)) || bytes.Contains(response.Body, []byte("cookie-secret")) {
			t.Fatalf("unsafe or failed import: %s", response.Body)
		}
	}
	if len(host.saved) != 1 || host.saved[credentialName(testToken)] == nil {
		t.Fatal("reimport did not update deterministic auth file")
	}
	for _, path := range []string{"/auth/status", "/catalog"} {
		response := rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/perplexity" + path}, "callback", host.call)
		if response.StatusCode != 200 || bytes.Contains(response.Body, []byte(testToken)) || bytes.Contains(response.Body, []byte("cookie-secret")) {
			t.Fatal("unsafe/failed management response")
		}
	}
	response := rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementPrefix + "/auth/test", Body: marshal(map[string]string{"file": credentialName(testToken)})}, "callback", host.call)
	if response.StatusCode != 200 || !bytes.Contains(response.Body, []byte(`"valid":true`)) {
		t.Fatal("account test failed")
	}
	refreshed, err := rt.refreshAuth(pluginapi.AuthRefreshRequest{StorageJSON: testCredential()}, "callback", host.call)
	if err != nil || refreshed.Auth.Label != "test@example.invalid" || time.Until(refreshed.NextRefreshAfter) < 5*time.Hour || refreshed.Auth.NextRefreshAfter != refreshed.NextRefreshAfter {
		t.Fatal("refresh failed")
	}
	for _, resource := range registration.Resources {
		response = rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/perplexity" + resource.Path}, "", nil)
		if response.StatusCode != 200 || len(response.Body) == 0 || response.Headers.Get("Content-Security-Policy") == "" {
			t.Fatal("public resource unavailable")
		}
	}
	if !strings.Contains(panelJS, `credentials:"same-origin"`) || strings.Contains(panelJS, "console.log") || strings.Contains(panelHTML, "https://cdn") {
		t.Fatal("panel privacy/self-contained policy failed")
	}
	for _, input := range []string{`{"cookies":"Cookie: ` + sessionCookie + `=` + testToken + `"}`, `{"cookies":{"` + sessionCookie + `":"` + testToken + `"}}`} {
		cred, err := importedCredential([]byte(input))
		if err != nil || cred.SessionToken != testToken {
			t.Fatal("cookie import form not supported")
		}
	}
}

func TestExpiredSessionAndTransportFailures(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	host.http = func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("{}")}, nil
	}
	_, err := rt.refreshAuth(pluginapi.AuthRefreshRequest{StorageJSON: testCredential()}, "callback", host.call)
	errorDetail(t, err, "reauth_required", 401)
	response := rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodPost, Path: managementPrefix + "/auth/import", Body: marshal(map[string]string{"session_token": testToken})}, "callback", host.call)
	if response.StatusCode != 400 || !bytes.Contains(response.Body, []byte("not a signed-in Perplexity session")) || len(host.saved) != 0 {
		t.Fatal("invalid session imported")
	}
	host.saved[credentialName(testToken)] = testCredential()
	host.saved["perplexity-corrupt.json"] = []byte(`{"type":"perplexity"}`)
	host.http = func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{}, errors.New("transport " + testToken)
	}
	_, err = rt.refreshAuth(pluginapi.AuthRefreshRequest{StorageJSON: testCredential()}, "callback", host.call)
	errorDetail(t, err, "upstream_unavailable", 503)
	response = rt.managementHandle(pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementPrefix + "/auth/status"}, "callback", host.call)
	if response.StatusCode != 200 || bytes.Contains(response.Body, []byte(testToken)) || !bytes.Contains(response.Body, []byte(`"valid":false`)) {
		t.Fatal("one broken account failed status request")
	}
}

func TestConcurrencyLimitAndCancellation(t *testing.T) {
	rt := testRuntime(t)
	cfg := defaultSettings()
	cfg.MaxConcurrency = 2
	rt.configure(cfg)
	var running atomic.Int32
	var peak atomic.Int32
	var workers sync.WaitGroup
	for range 12 {
		workers.Go(func() {
			release, err := rt.acquire(context.Background(), testToken)
			if err != nil {
				t.Error(err)
				return
			}
			current := running.Add(1)
			for previous := peak.Load(); current > previous && !peak.CompareAndSwap(previous, current); previous = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			running.Add(-1)
			release()
			release()
		})
	}
	workers.Wait()
	if peak.Load() > 2 || peak.Load() == 0 {
		t.Fatal("credential concurrency bound exceeded")
	}
	releaseOne, _ := rt.acquire(context.Background(), testToken)
	releaseTwo, _ := rt.acquire(context.Background(), testToken)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rt.acquire(ctx, testToken)
	errorDetail(t, err, "client_closed", 499)
	deadline, stop := context.WithTimeout(context.Background(), time.Millisecond)
	defer stop()
	_, err = rt.acquire(deadline, testToken)
	errorDetail(t, err, "upstream_timeout", 504)
	releaseOne()
	releaseTwo()
	if len(rt.active) != 0 {
		t.Fatal("credential semaphore leaked")
	}
}

func TestTransportOperationCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	cancelled := make(chan struct{})
	var once sync.Once
	callback := func(_ context.Context, method string, payload any) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostHTTPOperationOpen:
			return marshal(map[string]string{"operation_id": "operation"}), nil
		case pluginabi.MethodHostHTTPDo:
			<-cancelled
			return nil, context.Canceled
		case pluginabi.MethodHostHTTPCancel:
			once.Do(func() { close(cancelled) })
			return marshal(struct{}{}), nil
		default:
			return nil, errors.New("unexpected test callback")
		}
	}
	client := sender{callback: callback, cfg: defaultSettings(), id: "callback"}
	_, err := client.do(ctx, client.request(http.MethodGet, "/api/auth/session", nil, nil))
	errorDetail(t, err, "upstream_timeout", 504)
}

func TestShutdownCancelsPumpAndRecoversPanic(t *testing.T) {
	for _, panicRead := range []bool{false, true} {
		rt := testRuntime(t)
		host := newFakeHost()
		readStarted := make(chan struct{})
		cancelled := make(chan struct{})
		var once sync.Once
		callback := func(ctx context.Context, method string, payload any) (json.RawMessage, error) {
			if method == pluginabi.MethodHostHTTPCancel {
				request := payload.(map[string]string)
				if request["operation_id"] == "operation-2" {
					once.Do(func() { close(cancelled) })
				}
			}
			return host.call(ctx, method, payload)
		}
		host.read = func() (json.RawMessage, error) {
			close(readStarted)
			if panicRead {
				panic(testToken)
			}
			<-cancelled
			return nil, context.Canceled
		}
		if _, err := rt.executeStream(testExecutor(), "callback", "output", callback); err != nil {
			t.Fatal(err)
		}
		<-readStarted
		rt.stop()
		closed := waitClose(t, rt, host)
		if panicRead {
			errorDetail(t, closed.ErrorDetail, "internal_error", 500)
		} else {
			errorDetail(t, closed.ErrorDetail, "client_closed", 499)
		}
	}
}

func TestQuiesceDrainsUnaryRPC(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	finished := make(chan error, 1)
	var once sync.Once
	callback := func(ctx context.Context, method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostHTTPDo {
			request := payload.(hostHTTPRequest)
			if strings.Contains(request.Request.URL, "/perplexity_ask") {
				close(entered)
				<-cancelled
				return nil, context.Canceled
			}
		}
		if method == pluginabi.MethodHostHTTPCancel && payload.(map[string]string)["operation_id"] == "operation-2" {
			once.Do(func() { close(cancelled) })
		}
		return host.call(ctx, method, payload)
	}
	go func() {
		_, err := rt.dispatch(pluginabi.MethodExecutorExecute, marshal(executorRPC{ExecutorRequest: testExecutor()}), callback)
		finished <- err
	}()
	<-entered
	if _, err := rt.dispatch(pluginabi.MethodPluginQuiesce, nil, callback); err != nil {
		t.Fatal(err)
	}
	errorDetail(t, <-finished, "client_closed", 499)
	_, err := rt.dispatch(pluginabi.MethodAuthIdentifier, nil, callback)
	errorDetail(t, err, "client_closed", 499)
	if _, err = rt.dispatch(pluginabi.MethodPluginRegister, []byte("{}"), callback); err != nil {
		t.Fatal("could not register after quiesce")
	}
	if _, err = rt.dispatch(pluginabi.MethodAuthIdentifier, nil, callback); err != nil {
		t.Fatal("re-registered runtime remained closed")
	}
}

func TestSDKRPCResponseShapes(t *testing.T) {
	rt := testRuntime(t)
	host := newFakeHost()
	host.http = func(request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		body := "{}"
		switch {
		case strings.Contains(request.URL, "/api/auth/session"):
			body = testSession
		case strings.Contains(request.URL, "/models/config/"):
			body = testCatalog
		case strings.Contains(request.URL, "/perplexity_ask"):
			return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(answerSSE("Hello"))}, nil
		}
		return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}, nil
	}
	for _, fixture := range []struct {
		method  string
		request any
		target  any
	}{
		{pluginabi.MethodAuthParse, pluginapi.AuthParseRequest{RawJSON: testCredential()}, &pluginapi.AuthParseResponse{}},
		{pluginabi.MethodAuthRefresh, struct {
			pluginapi.AuthRefreshRequest
			callbackContext
		}{AuthRefreshRequest: pluginapi.AuthRefreshRequest{StorageJSON: testCredential()}, callbackContext: callbackContext{HostCallbackID: "callback"}}, &pluginapi.AuthRefreshResponse{}},
		{pluginabi.MethodModelStatic, pluginapi.StaticModelRequest{}, &pluginapi.ModelResponse{}},
		{pluginabi.MethodModelForAuth, struct {
			pluginapi.AuthModelRequest
			callbackContext
		}{AuthModelRequest: pluginapi.AuthModelRequest{StorageJSON: testCredential()}, callbackContext: callbackContext{HostCallbackID: "callback"}}, &pluginapi.ModelResponse{}},
		{pluginabi.MethodExecutorExecute, executorRPC{ExecutorRequest: testExecutor(), callbackContext: callbackContext{HostCallbackID: "callback"}}, &pluginapi.ExecutorResponse{}},
		{pluginabi.MethodQuotaDescribe, pluginapi.QuotaDescribeRequest{}, &pluginapi.QuotaDescribeResponse{}},
		{pluginabi.MethodQuotaFetch, struct {
			pluginapi.QuotaFetchRequest
			callbackContext
		}{QuotaFetchRequest: pluginapi.QuotaFetchRequest{StorageJSON: testCredential()}, callbackContext: callbackContext{HostCallbackID: "callback"}}, &pluginapi.QuotaFetchResponse{}},
		{pluginabi.MethodManagementRegister, pluginapi.ManagementRegistrationRequest{}, &pluginapi.ManagementRegistrationResponse{}},
		{pluginabi.MethodManagementHandle, struct {
			pluginapi.ManagementRequest
			callbackContext
		}{ManagementRequest: pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementPrefix + "/catalog"}, callbackContext: callbackContext{HostCallbackID: "callback"}}, &pluginapi.ManagementResponse{}},
	} {
		t.Run(fixture.method, func(t *testing.T) {
			raw := rpcEnvelope(fixture.method, marshal(fixture.request), rt.dispatch, host.call)
			var envelope pluginabi.Envelope
			if json.Unmarshal(raw, &envelope) != nil || !envelope.OK || json.Unmarshal(envelope.Result, fixture.target) != nil {
				t.Fatalf("SDK wire round-trip failed for %s", fixture.method)
			}
		})
	}
}

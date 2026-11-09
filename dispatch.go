package main

import (
	"encoding/json"
	"errors"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginID      = "perplexity"
	pluginVersion = "1.1.0"
	pluginRepo    = "https://github.com/lzt404/cpa-plugin-perplexity"
	maxRPCPayload = 1 << 20
)

type dispatcher func(string, []byte, hostCallbackFunc) (any, error)

func rpcEnvelope(method string, raw []byte, handle dispatcher, callback hostCallbackFunc) (encoded []byte) {
	defer func() {
		if recover() != nil {
			encoded, _ = pluginabi.NewErrorEnvelope("internal_error", "Perplexity plugin internal error", 500)
		}
	}()
	var value any
	var err error
	if len(raw) > maxRPCPayload {
		err = pluginabi.NewError("invalid_request", "RPC payload exceeds 1 MiB", 400)
	} else {
		value, err = handle(method, raw, callback)
	}
	envelope := pluginabi.Envelope{OK: err == nil}
	if err != nil {
		if !errors.As(err, &envelope.Error) {
			envelope.Error = classifyError(err)
		}
	} else {
		envelope.Result, err = json.Marshal(value)
		if err != nil {
			envelope.OK = false
			envelope.Error = pluginabi.NewError("internal_error", "Perplexity response encoding failed", 500)
		}
	}
	encoded, _ = json.Marshal(envelope)
	return encoded
}

func decodeRPC(raw []byte, target any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil || json.Unmarshal(raw, target) != nil {
		return pluginabi.NewError("invalid_request", "Invalid Perplexity RPC request", 400)
	}
	return nil
}

type callbackContext struct {
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type executorRPC struct {
	pluginapi.ExecutorRequest
	callbackContext
	StreamID string `json:"stream_id,omitempty"`
}

func (rt *pluginRuntime) dispatch(method string, raw []byte, callback hostCallbackFunc) (any, error) {
	if method != pluginabi.MethodPluginRegister && method != pluginabi.MethodPluginReconfigure && method != pluginabi.MethodPluginQuiesce && method != pluginabi.MethodPluginShutdown {
		done, err := rt.beginCall()
		if err != nil {
			return nil, err
		}
		defer done()
	}
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var request struct {
			ConfigYAML    []byte `json:"config_yaml"`
			SchemaVersion uint32 `json:"schema_version"`
		}
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		cfg, err := parseSettings(request.ConfigYAML)
		if err != nil {
			return nil, err
		}
		rt.configure(cfg)
		return registration(), nil
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		rt.stop()
		return struct{}{}, nil
	case pluginabi.MethodAuthIdentifier, pluginabi.MethodExecutorIdentifier, pluginabi.MethodQuotaIdentifier:
		return map[string]any{"identifier": pluginID, "credential_providers": []string{pluginID}}, nil
	case pluginabi.MethodAuthLoginStart:
		var request pluginapi.AuthLoginStartRequest
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		return rt.startLogin(request)
	case pluginabi.MethodAuthLoginPoll:
		var request pluginapi.AuthLoginPollRequest
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		return rt.pollLogin(request)
	case pluginabi.MethodQuotaReset, pluginabi.MethodExecutorHTTPRequest:
		return nil, pluginabi.NewError("unsupported_operation", "Perplexity quota reset and arbitrary HTTP are not supported", 501)
	case pluginabi.MethodAuthParse:
		var request pluginapi.AuthParseRequest
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		rt.rememberHost(request.Host)
		return parseAuth(request)
	case pluginabi.MethodAuthRefresh:
		var request struct {
			pluginapi.AuthRefreshRequest
			callbackContext
		}
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		return rt.refreshAuth(request.AuthRefreshRequest, request.HostCallbackID, callback)
	case pluginabi.MethodModelStatic:
		var request pluginapi.StaticModelRequest
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		return rt.models(request.Host, "pro", "", callback), nil
	case pluginabi.MethodModelForAuth:
		var request struct {
			pluginapi.AuthModelRequest
			callbackContext
		}
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		cred, err := parseCredential(request.StorageJSON)
		if err != nil {
			return nil, err
		}
		return rt.models(request.Host, cred.Plan, request.HostCallbackID, callback), nil
	case pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream, pluginabi.MethodExecutorCountTokens:
		var request executorRPC
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		if request.Format != "" && request.Format != "openai" {
			return nil, pluginabi.NewError("invalid_request", "Perplexity executor requires OpenAI chat format", 400)
		}
		switch method {
		case pluginabi.MethodExecutorExecute:
			return rt.execute(request.ExecutorRequest, request.HostCallbackID, callback)
		case pluginabi.MethodExecutorExecuteStream:
			return rt.executeStream(request.ExecutorRequest, request.HostCallbackID, request.StreamID, callback)
		default:
			cfg, _ := rt.snapshot()
			return countTokens(request.ExecutorRequest, cfg.MaxPromptChars)
		}
	case pluginabi.MethodQuotaDescribe:
		return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{pluginID}, DisplayName: "Perplexity", SupportsReset: false}, nil
	case pluginabi.MethodQuotaFetch:
		var request struct {
			pluginapi.QuotaFetchRequest
			callbackContext
		}
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		return rt.fetchQuota(request.QuotaFetchRequest, request.HostCallbackID, callback)
	case pluginabi.MethodManagementRegister:
		return managementRegistration(), nil
	case pluginabi.MethodManagementHandle:
		var request struct {
			pluginapi.ManagementRequest
			callbackContext
		}
		if err := decodeRPC(raw, &request); err != nil {
			return nil, err
		}
		return rt.managementHandle(request.ManagementRequest, request.HostCallbackID, callback), nil
	default:
		return nil, pluginabi.NewError("method_not_found", "Unsupported Perplexity plugin method", 404)
	}
}

func registration() any {
	cfg := defaultSettings()
	fields := []pluginapi.ConfigField{
		{Name: "language", Type: pluginapi.ConfigFieldTypeString, Default: cfg.Language, Description: "Answer language."},
		{Name: "sources", Type: pluginapi.ConfigFieldTypeArray, Default: cfg.Sources, Description: "Search sources: web, scholar, social."},
		{Name: "include_sources", Type: pluginapi.ConfigFieldTypeBoolean, Default: true, Description: "Append a Sources markdown list."},
		{Name: "catalog_ttl", Type: pluginapi.ConfigFieldTypeString, Default: cfg.CatalogTTL, Description: "Model catalog cache duration."},
		{Name: "request_timeout", Type: pluginapi.ConfigFieldTypeString, Default: cfg.RequestTimeout, Description: "Total request timeout; increase for deep research."},
		{Name: "max_prompt_chars", Type: pluginapi.ConfigFieldTypeNumber, Default: cfg.MaxPromptChars, Description: "Prompt rune limit; oldest text is truncated."},
		{Name: "max_concurrency", Type: pluginapi.ConfigFieldTypeNumber, Default: cfg.MaxConcurrency, Description: "Simultaneous asks per credential."},
		{Name: "pro_query_limit", Type: pluginapi.ConfigFieldTypeNumber, Default: 0, Description: "Known Pro quota denominator; zero means unknown."},
		{Name: "research_query_limit", Type: pluginapi.ConfigFieldTypeNumber, Default: 0, Description: "Known Research quota denominator; zero means unknown."},
		{Name: "exclude_models", Type: pluginapi.ConfigFieldTypeArray, Default: []string{}, Description: "Model IDs or internal IDs to hide and reject."},
		{Name: "base_url", Type: pluginapi.ConfigFieldTypeString, Default: cfg.BaseURL, Description: "Upstream origin; HTTP(S) scheme and host required."},
		{Name: "user_agent", Type: pluginapi.ConfigFieldTypeString, Default: cfg.UserAgent, Description: "Browser-like User-Agent."},
	}
	return struct {
		SchemaVersion uint32             `json:"schema_version"`
		Metadata      pluginapi.Metadata `json:"metadata"`
		Capabilities  map[string]any     `json:"capabilities"`
	}{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      pluginapi.Metadata{Name: "Perplexity", Version: pluginVersion, Author: "lzt404", GitHubRepository: pluginRepo, ConfigFields: fields},
		Capabilities: map[string]any{"auth_provider": true, "model_provider": true, "executor": true,
			"executor_model_scope": pluginapi.ExecutorModelScopeOAuth, "executor_input_formats": []string{"openai"}, "executor_output_formats": []string{"openai"}, "quota_provider": true, "management_api": true},
	}
}

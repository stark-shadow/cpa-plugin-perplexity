package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type sender struct {
	callback hostCallbackFunc
	id       string
	cfg      settings
}

type hostHTTPRequest struct {
	HostCallbackID string                `json:"host_callback_id,omitempty"`
	OperationID    string                `json:"operation_id,omitempty"`
	Request        pluginapi.HTTPRequest `json:"request"`
}

type hostStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostStreamRead struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func (client sender) request(method, path string, cred *credential, body []byte) pluginapi.HTTPRequest {
	base, _ := url.Parse(client.cfg.BaseURL)
	origin := base.Scheme + "://" + base.Host
	headers := http.Header{
		"User-Agent": {client.cfg.UserAgent}, "Accept-Language": {"en-US,en;q=0.9"},
		"Sec-Ch-Ua":        {`"Chromium";v="136", "Not=A?Brand";v="24"`},
		"Sec-Ch-Ua-Mobile": {"?0"}, "Sec-Ch-Ua-Platform": {`"Windows"`},
		"Sec-Fetch-Dest": {"empty"}, "Sec-Fetch-Mode": {"cors"}, "Sec-Fetch-Site": {"same-origin"},
		"Accept": {"application/json"}, "Origin": {origin}, "Referer": {client.cfg.BaseURL + "/"},
	}
	if cred != nil {
		headers.Set("Cookie", cred.cookieHeader())
	}
	return pluginapi.HTTPRequest{Method: method, URL: client.cfg.BaseURL + path, Headers: headers, Body: body}
}

func (client sender) invoke(ctx context.Context, method string, request pluginapi.HTTPRequest) (json.RawMessage, func(), error) {
	if client.callback == nil {
		return nil, func() {}, pluginabi.NewError("upstream_unavailable", "Host HTTP callback unavailable", 503)
	}
	if ctx.Err() != nil {
		return nil, func() {}, classifyError(ctx.Err())
	}
	opened, err := client.callback(ctx, pluginabi.MethodHostHTTPOperationOpen, map[string]string{"host_callback_id": client.id})
	if err != nil {
		return nil, func() {}, classifyError(err)
	}
	var operation struct {
		ID string `json:"operation_id"`
	}
	if json.Unmarshal(opened, &operation) != nil || operation.ID == "" {
		return nil, func() {}, protocolError()
	}
	var once sync.Once
	cancelOperation := func() {
		once.Do(func() {
			_, _ = client.callback(context.Background(), pluginabi.MethodHostHTTPCancel, map[string]string{"host_callback_id": client.id, "operation_id": operation.ID})
		})
	}
	stop := context.AfterFunc(ctx, cancelOperation)
	cleanup := func() {
		stop()
		cancelOperation()
	}
	raw, err := client.callback(ctx, method, hostHTTPRequest{HostCallbackID: client.id, OperationID: operation.ID, Request: request})
	if err != nil || ctx.Err() != nil {
		cleanup()
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, func() {}, classifyError(err)
	}
	return raw, cleanup, nil
}

func (client sender) do(ctx context.Context, request pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	raw, cleanup, err := client.invoke(ctx, pluginabi.MethodHostHTTPDo, request)
	defer cleanup()
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var response pluginapi.HTTPResponse
	if json.Unmarshal(raw, &response) != nil || response.StatusCode < 100 {
		return response, protocolError()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response, statusError(response.StatusCode, response.Headers)
	}
	return response, nil
}

func (client sender) open(ctx context.Context, request pluginapi.HTTPRequest) (hostStreamResponse, func(), error) {
	raw, finish, err := client.invoke(ctx, pluginabi.MethodHostHTTPDoStream, request)
	if err != nil {
		return hostStreamResponse{}, finish, err
	}
	var response hostStreamResponse
	if json.Unmarshal(raw, &response) != nil || response.StreamID == "" {
		finish()
		return response, func() {}, protocolError()
	}
	cleanup := func() {
		finish()
		_, _ = client.callback(context.Background(), pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": response.StreamID})
	}
	return response, cleanup, nil
}

func (client sender) read(ctx context.Context, streamID string) (hostStreamRead, error) {
	if ctx.Err() != nil {
		return hostStreamRead{}, classifyError(ctx.Err())
	}
	raw, err := client.callback(ctx, pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": streamID})
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return hostStreamRead{}, classifyError(err)
	}
	var chunk hostStreamRead
	if json.Unmarshal(raw, &chunk) != nil {
		return chunk, protocolError()
	}
	if chunk.Error != "" {
		return chunk, classifyError(errors.New(chunk.Error))
	}
	return chunk, nil
}

func (client sender) getJSON(ctx context.Context, path string, cred *credential, target any) error {
	response, err := client.do(ctx, client.request(http.MethodGet, path, cred, nil))
	if err != nil {
		return err
	}
	if json.Unmarshal(response.Body, target) != nil || string(response.Body) == "null" {
		return protocolError()
	}
	return nil
}

func protocolError() *pluginabi.Error {
	return pluginabi.NewError("upstream_protocol_error", "Perplexity returned an invalid or empty answer", 502)
}

func accountError() *pluginabi.Error {
	return pluginabi.NewError("account_unavailable", "Perplexity session unavailable; import a signed-in cookie at /v0/management/plugins/perplexity/auth/import", 401)
}

func statusError(status int, headers http.Header) *pluginabi.Error {
	switch status {
	case 401, 403:
		return accountError()
	case 429:
		err := pluginabi.NewError("upstream_rate_limited", "Perplexity rate limit reached", 429)
		err.Retryable = true
		retry := headers.Get("Retry-After")
		if seconds, parseErr := strconv.Atoi(retry); parseErr == nil && seconds > 0 {
			err.RetryAfterSeconds = seconds
		} else if date, parseErr := http.ParseTime(retry); parseErr == nil && time.Until(date) > 0 {
			err.RetryAfterSeconds = int(time.Until(date).Seconds()) + 1
		}
		return err
	case 408, 504:
		return pluginabi.NewError("upstream_timeout", "Perplexity request timed out", 504)
	default:
		return pluginabi.NewError("upstream_error", "Perplexity upstream request failed", 502)
	}
}

func classifyError(err error) *pluginabi.Error {
	if err == nil {
		return protocolError()
	}
	message := strings.ToLower(err.Error())
	if errors.Is(err, context.Canceled) || strings.Contains(message, "context canceled") || strings.Contains(message, "client closed") {
		return pluginabi.NewError("client_closed", "Perplexity request cancelled by client", 499)
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(message, "timeout") || strings.Contains(message, "timed out") || strings.Contains(message, "deadline exceeded") || (errors.As(err, &timeout) && timeout.Timeout()) {
		return pluginabi.NewError("upstream_timeout", "Perplexity request timed out", 504)
	}
	return pluginabi.NewError("upstream_error", "Perplexity transport request failed", 502)
}

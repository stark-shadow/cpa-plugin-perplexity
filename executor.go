package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

const truncationMarker = "[...truncated...]"

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	MaxTokens   *int          `json:"max_tokens"`
	Temperature *float64      `json:"temperature"`
}

func parseChat(payload []byte, limit int) (chatRequest, string, error) {
	invalid := func() (chatRequest, string, error) {
		return chatRequest{}, "", pluginabi.NewError("invalid_request", "Expected an OpenAI chat object with non-empty text messages", 400)
	}
	var request chatRequest
	if !strings.HasPrefix(strings.TrimSpace(string(payload)), "{") || json.Unmarshal(payload, &request) != nil || len(request.Messages) == 0 {
		return invalid()
	}
	parts := make([]string, 0, len(request.Messages))
	for _, message := range request.Messages {
		role := strings.ToLower(message.Role)
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			return invalid()
		}
		text := ""
		if json.Unmarshal(message.Content, &text) != nil {
			var content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(message.Content, &content) != nil {
				return invalid()
			}
			var builder strings.Builder
			for _, item := range content {
				if item.Type == "text" {
					builder.WriteString(item.Text)
				}
			}
			text = builder.String()
		}
		if len(request.Messages) == 1 && role == "user" {
			parts = append(parts, text)
		} else {
			parts = append(parts, strings.ToUpper(role[:1])+role[1:]+": "+text)
		}
	}
	prompt := strings.Join(parts, "\n\n")
	if strings.TrimSpace(prompt) == "" || !utf8.ValidString(prompt) {
		return invalid()
	}
	if utf8.RuneCountInString(prompt) > limit {
		runes := []rune(prompt)
		keep := max(0, limit-len(truncationMarker))
		prompt = truncationMarker + string(runes[len(runes)-keep:])
	}
	return request, prompt, nil
}

func estimateTokens(text string) int { return (utf8.RuneCountInString(text) + 3) / 4 }

func uuid4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", pluginabi.NewError("internal_error", "Unable to initialize Perplexity request", 500)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(value[:])
	return raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:], nil
}

type preparedAsk struct {
	Request pluginapi.HTTPRequest
	Model   string
	Prompt  string
	Cred    credential
	ID      string
	Created int64
}

func (rt *pluginRuntime) prepareAsk(ctx context.Context, client sender, request pluginapi.ExecutorRequest) (preparedAsk, error) {
	chat, prompt, err := parseChat(request.Payload, client.cfg.MaxPromptChars)
	if err != nil {
		return preparedAsk{}, err
	}
	model := request.Model
	if model == "" {
		model = chat.Model
	}
	cred, err := parseCredential(request.StorageJSON)
	if err != nil {
		return preparedAsk{}, accountError()
	}
	catalog := rt.catalogFor(ctx, client)
	mapping, err := resolveModel(model, mappedModels(catalog, cred.Plan, client.cfg))
	if err != nil {
		return preparedAsk{}, err
	}
	frontend, err := uuid4()
	if err != nil {
		return preparedAsk{}, err
	}
	contextUUID, err := uuid4()
	if err != nil {
		return preparedAsk{}, err
	}
	body, _ := json.Marshal(map[string]any{"query_str": prompt, "params": map[string]any{
		"attachments": []any{}, "frontend_context_uuid": contextUUID, "frontend_uuid": frontend,
		"is_incognito": false, "language": client.cfg.Language, "last_backend_uuid": nil,
		"mode": mapping.Mode, "model_preference": mapping.Preference, "query_source": "home",
		"source": "default", "sources": client.cfg.Sources, "version": "2.18",
	}})
	upstream := client.request(http.MethodPost, "/rest/sse/perplexity_ask", &cred, body)
	upstream.Headers.Set("Accept", "text/event-stream")
	upstream.Headers.Set("Content-Type", "application/json")
	upstream.Headers.Set("X-Perplexity-Request-Endpoint", upstream.URL)
	upstream.Headers.Set("X-Perplexity-Request-Reason", "ask-query-state-provider")
	upstream.Headers.Set("X-Perplexity-Request-Try-Number", "1")
	upstream.Headers.Set("X-Request-Id", frontend)
	return preparedAsk{Request: upstream, Model: model, Prompt: prompt, Cred: cred, ID: "chatcmpl-" + frontend, Created: time.Now().Unix()}, nil
}

func consumeEvents(decoder *sseDecoder, acc *answerAccumulator, payload []byte, final bool, onAnswer func(string) error) error {
	events, err := decoder.feed(payload, final)
	if err != nil {
		return err
	}
	for _, event := range events {
		if err = acc.apply(event); err != nil {
			return err
		}
		if onAnswer != nil {
			if err = onAnswer(acc.Answer); err != nil {
				return err
			}
		}
		if acc.Ended {
			break
		}
	}
	return nil
}

func jsonAnswerError(body []byte) error {
	var frame answerFrame
	if json.Unmarshal(body, &frame) == nil && frame.Upsell != nil && strings.EqualFold(frame.Upsell.Type, "LOGIN") {
		return accountError()
	}
	return protocolError()
}

func (rt *pluginRuntime) execute(request pluginapi.ExecutorRequest, id string, callback hostCallbackFunc) (pluginapi.ExecutorResponse, error) {
	cfg, _ := rt.snapshot()
	ctx, cancel := rt.requestContext(cfg.Timeout)
	defer cancel()
	client := sender{callback: callback, id: id, cfg: cfg}
	ask, err := rt.prepareAsk(ctx, client, request)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	release, err := rt.acquire(ctx, ask.Cred.SessionToken)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	defer release()
	response, err := client.do(ctx, ask.Request)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if strings.Contains(strings.ToLower(response.Headers.Get("Content-Type")), "application/json") {
		return pluginapi.ExecutorResponse{}, jsonAnswerError(response.Body)
	}
	acc := answerAccumulator{Authenticated: true, Sources: []sourceLink{}}
	if err = consumeEvents(&sseDecoder{}, &acc, response.Body, true, nil); err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if strings.TrimSpace(acc.Answer) == "" {
		return pluginapi.ExecutorResponse{}, protocolError()
	}
	content := acc.Answer
	if cfg.IncludeSources {
		content += acc.sourcesMarkdown()
	}
	input, output := estimateTokens(ask.Prompt), estimateTokens(content)
	threadURL := ""
	if acc.ThreadSlug != "" {
		threadURL = cfg.BaseURL + "/search/" + url.PathEscape(acc.ThreadSlug)
	}
	completion, _ := json.Marshal(map[string]any{
		"id": ask.ID, "object": "chat.completion", "created": ask.Created, "model": ask.Model,
		"choices":    []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":      map[string]int{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output},
		"perplexity": map[string]any{"thread_url": threadURL, "sources": acc.Sources},
	})
	return pluginapi.ExecutorResponse{Payload: completion, Headers: http.Header{"Content-Type": {"application/json"}}}, nil
}

type streamEmit struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload"`
}

type streamClose struct {
	StreamID    string           `json:"stream_id"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

func streamChunk(ask preparedAsk, delta map[string]string, finish any) []byte {
	encoded, _ := json.Marshal(map[string]any{
		"id": ask.ID, "object": "chat.completion.chunk", "created": ask.Created, "model": ask.Model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return encoded
}

func drainStream(ctx context.Context, client sender, response hostStreamResponse) ([]byte, error) {
	var body []byte
	for {
		chunk, err := client.read(ctx, response.StreamID)
		if err != nil {
			return body, err
		}
		if len(body)+len(chunk.Payload) > maxAnswerSize {
			return nil, protocolError()
		}
		body = append(body, chunk.Payload...)
		if chunk.Done {
			return body, nil
		}
	}
}

func (rt *pluginRuntime) executeStream(request pluginapi.ExecutorRequest, callbackID, streamID string, callback hostCallbackFunc) (any, error) {
	if streamID == "" {
		return nil, pluginabi.NewError("invalid_request", "Host output stream is required", 400)
	}
	cfg, _ := rt.snapshot()
	ctx, cancel := rt.requestContext(cfg.Timeout)
	client := sender{callback: callback, id: callbackID, cfg: cfg}
	ask, err := rt.prepareAsk(ctx, client, request)
	if err != nil {
		cancel()
		return nil, err
	}
	release, err := rt.acquire(ctx, ask.Cred.SessionToken)
	if err != nil {
		cancel()
		return nil, err
	}
	response, cleanup, err := client.open(ctx, ask.Request)
	if err != nil {
		cleanup()
		release()
		cancel()
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(strings.ToLower(response.Headers.Get("Content-Type")), "application/json") {
		body, drainErr := drainStream(ctx, client, response)
		cleanup()
		release()
		cancel()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, statusError(response.StatusCode, response.Headers)
		}
		if drainErr != nil {
			return nil, drainErr
		}
		return nil, jsonAnswerError(body)
	}
	err = rt.startWorker(func() {
		defer cleanup()
		defer release()
		defer cancel()
		var failure *pluginabi.Error
		defer func() {
			if recover() != nil {
				failure = pluginabi.NewError("internal_error", "Perplexity stream failed", 500)
			}
			_, _ = callback(context.Background(), pluginabi.MethodHostStreamClose, streamClose{StreamID: streamID, ErrorDetail: failure})
		}()
		if pumpErr := pumpStream(ctx, client, response.StreamID, streamID, ask); pumpErr != nil {
			if !errors.As(pumpErr, &failure) {
				failure = classifyError(pumpErr)
			}
		}
	})
	if err != nil {
		cleanup()
		release()
		cancel()
		return nil, err
	}
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

func pumpStream(ctx context.Context, client sender, upstreamID, outputID string, ask preparedAsk) error {
	decoder := sseDecoder{}
	acc := answerAccumulator{Authenticated: true}
	emitted, started := "", false
	emit := func(delta map[string]string, finish any) error {
		_, err := client.callback(ctx, pluginabi.MethodHostStreamEmit, streamEmit{StreamID: outputID, Payload: streamChunk(ask, delta, finish)})
		if err != nil {
			return classifyError(err)
		}
		return nil
	}
	onAnswer := func(answer string) error {
		delta, err := answerDelta(emitted, answer)
		if err != nil || delta == "" {
			return err
		}
		if !started {
			if err = emit(map[string]string{"role": "assistant"}, nil); err != nil {
				return err
			}
			started = true
		}
		if err = emit(map[string]string{"content": delta}, nil); err != nil {
			return err
		}
		emitted = answer
		return nil
	}
	for {
		chunk, err := client.read(ctx, upstreamID)
		if err != nil {
			return err
		}
		if err = consumeEvents(&decoder, &acc, chunk.Payload, chunk.Done, onAnswer); err != nil {
			return err
		}
		if acc.Ended || chunk.Done {
			break
		}
	}
	if !started || strings.TrimSpace(acc.Answer) == "" {
		return protocolError()
	}
	if client.cfg.IncludeSources {
		if sources := acc.sourcesMarkdown(); sources != "" {
			if err := emit(map[string]string{"content": sources}, nil); err != nil {
				return err
			}
		}
	}
	return emit(map[string]string{}, "stop")
}

func countTokens(request pluginapi.ExecutorRequest, limit int) (any, error) {
	_, prompt, err := parseChat(request.Payload, limit)
	if err != nil {
		return nil, err
	}
	count := estimateTokens(prompt)
	payload, _ := json.Marshal(map[string]int{"input_tokens": count})
	return struct {
		pluginapi.ExecutorResponse
		InputTokens int `json:"input_tokens"`
	}{ExecutorResponse: pluginapi.ExecutorResponse{Payload: payload}, InputTokens: count}, nil
}

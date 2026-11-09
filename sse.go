package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

const maxSSEFrame = 4 << 20
const maxAnswerSize = 8 << 20

type sseEvent struct {
	Name string
	Data string
}

type sseDecoder struct {
	line      []byte
	data      []string
	name      string
	skipLF    bool
	frameSize int
}

func (decoder *sseDecoder) finishLine(events *[]sseEvent) {
	line := strings.TrimPrefix(string(decoder.line), "\ufeff")
	decoder.line = decoder.line[:0]
	if line == "" {
		if len(decoder.data) > 0 || decoder.name == "end_of_stream" {
			*events = append(*events, sseEvent{Name: decoder.name, Data: strings.Join(decoder.data, "\n")})
		}
		decoder.data, decoder.name, decoder.frameSize = nil, "", 0
		return
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		decoder.name = value
	case "data":
		decoder.data = append(decoder.data, value)
	}
}

func (decoder *sseDecoder) feed(payload []byte, final bool) ([]sseEvent, error) {
	var events []sseEvent
	for _, char := range payload {
		if decoder.skipLF {
			decoder.skipLF = false
			if char == '\n' {
				continue
			}
		}
		decoder.frameSize++
		if decoder.frameSize > maxSSEFrame {
			return nil, protocolError()
		}
		if char == '\r' || char == '\n' {
			decoder.finishLine(&events)
			decoder.skipLF = char == '\r'
		} else {
			decoder.line = append(decoder.line, char)
		}
	}
	if final {
		if len(decoder.line) > 0 {
			decoder.finishLine(&events)
		}
		decoder.finishLine(&events)
	}
	return events, nil
}

type sourceLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type markdownBlock struct {
	Progress string   `json:"progress"`
	Chunks   []string `json:"chunks"`
	Offset   int      `json:"chunk_starting_offset"`
	Answer   *string  `json:"answer"`
}

type answerBlock struct {
	Markdown *markdownBlock `json:"markdown_block"`
	Web      *struct {
		Results []sourceLink `json:"web_results"`
	} `json:"web_result_block"`
}

type answerFrame struct {
	BackendUUID string          `json:"backend_uuid"`
	ThreadSlug  string          `json:"thread_url_slug"`
	Cursor      json.RawMessage `json:"cursor"`
	Completed   bool            `json:"text_completed"`
	Final       bool            `json:"final"`
	Status      string          `json:"status"`
	Blocks      []answerBlock   `json:"blocks"`
	Text        json.RawMessage `json:"text"`
	Error       json.RawMessage `json:"error"`
	Upsell      *struct {
		Type string `json:"upsell_type"`
	} `json:"upsell_information"`
}

type markdownState struct {
	Chunks []string
	Answer string
}

type answerAccumulator struct {
	Authenticated bool
	Answer        string
	Sources       []sourceLink
	ThreadSlug    string
	BackendUUID   string
	Ended         bool
	markdown      map[int]markdownState
	seenSources   map[string]bool
}

func (acc *answerAccumulator) apply(event sseEvent) error {
	if acc.Ended {
		return nil
	}
	if event.Name == "end_of_stream" || strings.TrimSpace(event.Data) == "[DONE]" {
		acc.Ended = true
		return nil
	}
	if event.Name != "message" && event.Name != "" && event.Name != "error" {
		return nil
	}
	var frame answerFrame
	if !strings.HasPrefix(strings.TrimSpace(event.Data), "{") || json.Unmarshal([]byte(event.Data), &frame) != nil {
		return protocolError()
	}
	if frame.Upsell != nil && strings.EqualFold(frame.Upsell.Type, "LOGIN") && acc.Authenticated {
		return accountError()
	}
	status := strings.ToUpper(frame.Status)
	if status == "FAILED" || status == "ERROR" || status == "CANCELLED" || nonemptyError(frame.Error) || event.Name == "error" {
		return pluginabi.NewError("upstream_error", "Perplexity generation failed", 502)
	}
	if frame.ThreadSlug != "" {
		acc.ThreadSlug = frame.ThreadSlug
	}
	if frame.BackendUUID != "" {
		acc.BackendUUID = frame.BackendUUID
	}
	if acc.markdown == nil {
		acc.markdown = map[int]markdownState{}
		acc.seenSources = map[string]bool{}
	}
	updated := false
	for index, block := range frame.Blocks {
		if block.Markdown != nil {
			markdown := block.Markdown
			state := acc.markdown[index]
			if markdown.Offset < 0 || markdown.Offset > len(state.Chunks) {
				if markdown.Answer == nil {
					return protocolError()
				}
			} else if markdown.Chunks != nil {
				state.Chunks = append(state.Chunks[:markdown.Offset], markdown.Chunks...)
				state.Answer = strings.Join(state.Chunks, "")
			}
			if markdown.Answer != nil {
				state.Answer = *markdown.Answer
			}
			acc.markdown[index] = state
			updated = true
		}
		if block.Web != nil {
			for _, source := range block.Web.Results {
				parsed, err := url.Parse(source.URL)
				if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || acc.seenSources[source.URL] {
					continue
				}
				acc.seenSources[source.URL] = true
				acc.Sources = append(acc.Sources, source)
			}
		}
	}
	if updated {
		indices := make([]int, 0, len(acc.markdown))
		for index := range acc.markdown {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		var answer strings.Builder
		for _, index := range indices {
			answer.WriteString(acc.markdown[index].Answer)
		}
		acc.Answer = answer.String()
	} else if len(frame.Text) > 0 && !bytes.Equal(frame.Text, []byte("null")) {
		answer, err := legacyAnswer(frame.Text)
		if err != nil {
			return err
		}
		if answer != "" {
			acc.Answer = answer
		}
	}
	if len(acc.Answer) > maxAnswerSize || !utf8.ValidString(acc.Answer) {
		return protocolError()
	}
	return nil
}

func nonemptyError(raw json.RawMessage) bool {
	text := strings.TrimSpace(string(raw))
	return text != "" && text != "null" && text != `""` && text != "{}" && text != "[]" && text != "false"
}

func legacyAnswer(raw json.RawMessage) (string, error) {
	if len(raw) > 0 && raw[0] == '"' {
		var decoded string
		if json.Unmarshal(raw, &decoded) != nil {
			return "", protocolError()
		}
		raw = []byte(decoded)
	}
	var steps []struct {
		Type    string `json:"step_type"`
		Content struct {
			Answer json.RawMessage `json:"answer"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &steps) != nil {
		return "", protocolError()
	}
	answer := ""
	for _, step := range steps {
		if step.Type != "FINAL" || len(step.Content.Answer) == 0 {
			continue
		}
		var plain string
		if json.Unmarshal(step.Content.Answer, &plain) == nil {
			var nested struct {
				Answer string `json:"answer"`
			}
			if strings.HasPrefix(strings.TrimSpace(plain), "{") && json.Unmarshal([]byte(plain), &nested) == nil && nested.Answer != "" {
				plain = nested.Answer
			}
			answer = plain
		} else {
			var nested struct {
				Answer string `json:"answer"`
			}
			if json.Unmarshal(step.Content.Answer, &nested) != nil {
				return "", protocolError()
			}
			answer = nested.Answer
		}
	}
	return answer, nil
}

func answerDelta(previous, current string) (string, error) {
	if !strings.HasPrefix(current, previous) {
		return "", protocolError()
	}
	return current[len(previous):], nil
}

func (acc *answerAccumulator) sourcesMarkdown() string {
	if len(acc.Sources) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("\n\n---\n**Sources**")
	for _, source := range acc.Sources {
		title := strings.NewReplacer("\n", " ", "\r", " ", "[", "\\[", "]", "\\]").Replace(source.Title)
		if title == "" {
			title = source.URL
		}
		link := strings.NewReplacer("(", "%28", ")", "%29", "\n", "%0A", "\r", "%0D").Replace(source.URL)
		builder.WriteString("\n- [" + title + "](" + link + ")")
	}
	return builder.String()
}

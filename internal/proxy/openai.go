package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ========== OpenAI Chat Completions / Responses compatibility ==========

type OpenAIErrorResponse struct {
	Error OpenAIError `json:"error"`
}

type OpenAIError struct {
	Message string      `json:"message"`
	Type    string      `json:"type,omitempty"`
	Param   string      `json:"param,omitempty"`
	Code    interface{} `json:"code,omitempty"`
}

type OpenAIFunctionDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
	Strict      bool        `json:"strict,omitempty"`
}

type OpenAITool struct {
	Type        string                    `json:"type"`
	Name        string                    `json:"name,omitempty"`
	Description string                    `json:"description,omitempty"`
	Parameters  interface{}               `json:"parameters,omitempty"`
	Strict      bool                      `json:"strict,omitempty"`
	Function    *OpenAIFunctionDefinition `json:"function,omitempty"`
}

type OpenAIChatToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIChatToolCall struct {
	ID       string                     `json:"id"`
	Type     string                     `json:"type"`
	Function OpenAIChatToolCallFunction `json:"function"`
}

type OpenAIChatMessage struct {
	Role       string               `json:"role"`
	Content    interface{}          `json:"content,omitempty"`
	Name       string               `json:"name,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIChatToolCall `json:"tool_calls,omitempty"`
}

type OpenAIJSONSchemaConfig struct {
	Name        string      `json:"name,omitempty"`
	Description string      `json:"description,omitempty"`
	Schema      interface{} `json:"schema,omitempty"`
	Strict      bool        `json:"strict,omitempty"`
}

type OpenAIChatResponseFormat struct {
	Type       string                  `json:"type,omitempty"`
	JSONSchema *OpenAIJSONSchemaConfig `json:"json_schema,omitempty"`
}

type OpenAIChatStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type OpenAIChatCompletionRequest struct {
	Model               string                     `json:"model"`
	Messages            []OpenAIChatMessage        `json:"messages"`
	Stream              bool                       `json:"stream,omitempty"`
	Tools               []OpenAITool               `json:"tools,omitempty"`
	ToolChoice          interface{}                `json:"tool_choice,omitempty"`
	ResponseFormat      *OpenAIChatResponseFormat  `json:"response_format,omitempty"`
	StreamOptions       *OpenAIChatStreamOptions   `json:"stream_options,omitempty"`
	Temperature         *float64                   `json:"temperature,omitempty"`
	TopP                *float64                   `json:"top_p,omitempty"`
	MaxTokens           int                        `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                        `json:"max_completion_tokens,omitempty"`
	Functions           []OpenAIFunctionDefinition `json:"functions,omitempty"`
	FunctionCall        interface{}                `json:"function_call,omitempty"`
	Metadata            map[string]interface{}     `json:"metadata,omitempty"`
	N                   int                        `json:"n,omitempty"`
}

type OpenAIChatCompletionChoice struct {
	Index        int                    `json:"index"`
	Message      map[string]interface{} `json:"message"`
	FinishReason *string                `json:"finish_reason"`
}

type OpenAIChatCompletionResponse struct {
	ID      string                       `json:"id"`
	Object  string                       `json:"object"`
	Created int64                        `json:"created"`
	Model   string                       `json:"model"`
	Choices []OpenAIChatCompletionChoice `json:"choices"`
	Usage   map[string]int               `json:"usage,omitempty"`
}

type OpenAIResponsesTextConfig struct {
	Format *OpenAIChatResponseFormat `json:"format,omitempty"`
}

type OpenAIResponsesRequest struct {
	Model              string                     `json:"model"`
	Input              interface{}                `json:"input"`
	Stream             bool                       `json:"stream,omitempty"`
	Tools              []OpenAITool               `json:"tools,omitempty"`
	ToolChoice         interface{}                `json:"tool_choice,omitempty"`
	Instructions       string                     `json:"instructions,omitempty"`
	Text               *OpenAIResponsesTextConfig `json:"text,omitempty"`
	Temperature        *float64                   `json:"temperature,omitempty"`
	TopP               *float64                   `json:"top_p,omitempty"`
	MaxOutputTokens    int                        `json:"max_output_tokens,omitempty"`
	PreviousResponseID string                     `json:"previous_response_id,omitempty"`
	Metadata           map[string]interface{}     `json:"metadata,omitempty"`
}

type anthropicInvocationError struct {
	Status  int
	Message string
	Type    string
}

type anthropicSSEFrame struct {
	Event string
	Data  json.RawMessage
}

type anthropicStreamBridgeWriter struct {
	header  http.Header
	status  int
	mu      sync.Mutex
	buffer  strings.Builder
	errBody bytes.Buffer
	frames  chan string
}

func newAnthropicStreamBridgeWriter() *anthropicStreamBridgeWriter {
	return &anthropicStreamBridgeWriter{
		header: make(http.Header),
		frames: make(chan string, 64),
	}
}

func (w *anthropicStreamBridgeWriter) Header() http.Header {
	return w.header
}

func (w *anthropicStreamBridgeWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = statusCode
	}
}

func (w *anthropicStreamBridgeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status != http.StatusOK {
		_, _ = w.errBody.Write(p)
		w.mu.Unlock()
		return len(p), nil
	}
	_, _ = w.buffer.WriteString(string(p))
	var frames []string
	for {
		buf := w.buffer.String()
		idx := strings.Index(buf, "\n\n")
		if idx < 0 {
			break
		}
		frame := buf[:idx]
		rest := buf[idx+2:]
		w.buffer.Reset()
		w.buffer.WriteString(rest)
		frames = append(frames, frame)
	}
	w.mu.Unlock()

	for _, frame := range frames {
		w.frames <- frame
	}
	return len(p), nil
}

func (w *anthropicStreamBridgeWriter) Flush() {}

func (w *anthropicStreamBridgeWriter) Close() {
	w.mu.Lock()
	leftover := strings.TrimSpace(w.buffer.String())
	w.buffer.Reset()
	w.mu.Unlock()
	if leftover != "" {
		w.frames <- leftover
	}
	close(w.frames)
}

func (w *anthropicStreamBridgeWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *anthropicStreamBridgeWriter) ErrorBody() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.errBody.Bytes()...)
}

type openAIChatStreamToolState struct {
	ToolIndex int
	CallID    string
	Name      string
}

type openAIChatStreamTranscoder struct {
	w            http.ResponseWriter
	flusher      http.Flusher
	id           string
	model        string
	created      int64
	includeUsage bool
	inputTokens  int
	outputTokens int
	sentRole     bool
	done         bool
	nextTool     int
	toolBlocks   map[int]openAIChatStreamToolState
}

func newOpenAIChatStreamTranscoder(w http.ResponseWriter, flusher http.Flusher, id, model string, created int64, includeUsage bool) *openAIChatStreamTranscoder {
	return &openAIChatStreamTranscoder{
		w:            w,
		flusher:      flusher,
		id:           id,
		model:        model,
		created:      created,
		includeUsage: includeUsage,
		toolBlocks:   make(map[int]openAIChatStreamToolState),
	}
}

func (t *openAIChatStreamTranscoder) emitChunk(delta map[string]interface{}, finishReason *string, usage map[string]int) error {
	payload := map[string]interface{}{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []map[string]interface{}{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	return sendOpenAISSE(t.w, t.flusher, payload)
}

func (t *openAIChatStreamTranscoder) emitUsageChunk() error {
	if !t.includeUsage {
		return nil
	}
	payload := map[string]interface{}{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"created": t.created,
		"model":   t.model,
		"choices": []interface{}{},
		"usage": map[string]int{
			"prompt_tokens":     t.inputTokens,
			"completion_tokens": t.outputTokens,
			"total_tokens":      t.inputTokens + t.outputTokens,
		},
	}
	return sendOpenAISSE(t.w, t.flusher, payload)
}

func (t *openAIChatStreamTranscoder) HandleFrame(frame anthropicSSEFrame) error {
	var payload map[string]interface{}
	if len(frame.Data) > 0 {
		if err := json.Unmarshal(frame.Data, &payload); err != nil {
			return err
		}
	}

	switch frame.Event {
	case "message_start":
		if message, ok := payload["message"].(map[string]interface{}); ok {
			if usage, ok := message["usage"].(map[string]interface{}); ok {
				t.inputTokens = intValue(usage["input_tokens"])
			}
		}
		if !t.sentRole {
			t.sentRole = true
			return t.emitChunk(map[string]interface{}{"role": "assistant"}, nil, nil)
		}
	case "content_block_start":
		index := intValue(payload["index"])
		block, _ := payload["content_block"].(map[string]interface{})
		if blockType, _ := block["type"].(string); blockType == "tool_use" {
			state := openAIChatStreamToolState{
				ToolIndex: t.nextTool,
				CallID:    stringValue(block["id"]),
				Name:      stringValue(block["name"]),
			}
			t.nextTool++
			t.toolBlocks[index] = state
			return t.emitChunk(map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index": state.ToolIndex,
					"id":    state.CallID,
					"type":  "function",
					"function": map[string]interface{}{
						"name":      state.Name,
						"arguments": "",
					},
				}},
			}, nil, nil)
		}
	case "content_block_delta":
		index := intValue(payload["index"])
		delta, _ := payload["delta"].(map[string]interface{})
		switch stringValue(delta["type"]) {
		case "text_delta":
			text := stringValue(delta["text"])
			if text == "" {
				return nil
			}
			return t.emitChunk(map[string]interface{}{"content": text}, nil, nil)
		case "input_json_delta":
			state, ok := t.toolBlocks[index]
			if !ok {
				return nil
			}
			return t.emitChunk(map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index": state.ToolIndex,
					"function": map[string]interface{}{
						"arguments": stringValue(delta["partial_json"]),
					},
				}},
			}, nil, nil)
		}
	case "message_delta":
		delta, _ := payload["delta"].(map[string]interface{})
		stop := mapAnthropicStopReasonToOpenAI(stringValue(delta["stop_reason"]))
		if usage, ok := payload["usage"].(map[string]interface{}); ok {
			t.outputTokens = intValue(usage["output_tokens"])
		}
		if err := t.emitChunk(map[string]interface{}{}, &stop, nil); err != nil {
			return err
		}
		return t.emitUsageChunk()
	case "message_stop":
		if t.done {
			return nil
		}
		t.done = true
		_, err := io.WriteString(t.w, "data: [DONE]\n\n")
		if err == nil {
			t.flusher.Flush()
		}
		return err
	}
	return nil
}

type responsesStreamToolItem struct {
	ItemID      string
	OutputIndex int
	CallID      string
	Name        string
	Arguments   strings.Builder
	Done        bool
}

type openAIResponsesStreamTranscoder struct {
	w               http.ResponseWriter
	flusher         http.Flusher
	responseID      string
	model           string
	created         int64
	inputTokens     int
	outputTokens    int
	createdSent     bool
	completedSent   bool
	nextOutputIndex int
	messageStarted  bool
	messageItemID   string
	messageIndex    int
	messageText     strings.Builder
	toolBlocks      map[int]*responsesStreamToolItem
	toolItems       []*responsesStreamToolItem
}

func newOpenAIResponsesStreamTranscoder(w http.ResponseWriter, flusher http.Flusher, responseID, model string, created int64) *openAIResponsesStreamTranscoder {
	return &openAIResponsesStreamTranscoder{
		w:          w,
		flusher:    flusher,
		responseID: responseID,
		model:      model,
		created:    created,
		toolBlocks: make(map[int]*responsesStreamToolItem),
	}
}

func (t *openAIResponsesStreamTranscoder) emit(eventType string, payload map[string]interface{}) error {
	payload["type"] = eventType
	return sendOpenAISSEEvent(t.w, t.flusher, eventType, payload)
}

func (t *openAIResponsesStreamTranscoder) ensureCreated() error {
	if t.createdSent {
		return nil
	}
	t.createdSent = true
	return t.emit("response.created", map[string]interface{}{
		"response": map[string]interface{}{
			"id":         t.responseID,
			"object":     "response",
			"created_at": t.created,
			"status":     "in_progress",
			"model":      t.model,
			"output":     []interface{}{},
		},
	})
}

func (t *openAIResponsesStreamTranscoder) ensureMessageItem() error {
	if t.messageStarted {
		return nil
	}
	if err := t.ensureCreated(); err != nil {
		return err
	}
	t.messageStarted = true
	t.messageIndex = t.nextOutputIndex
	t.nextOutputIndex++
	t.messageItemID = "msg_" + compactUUID()
	return t.emit("response.output_item.added", map[string]interface{}{
		"response_id":  t.responseID,
		"output_index": t.messageIndex,
		"item": map[string]interface{}{
			"id":     t.messageItemID,
			"type":   "message",
			"status": "in_progress",
			"role":   "assistant",
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        "",
				"annotations": []interface{}{},
			}},
		},
	})
}

func (t *openAIResponsesStreamTranscoder) buildFinalResponseObject() map[string]interface{} {
	output := make([]map[string]interface{}, 0, 1+len(t.toolItems))
	if t.messageStarted {
		output = append(output, map[string]interface{}{
			"id":     t.messageItemID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        t.messageText.String(),
				"annotations": []interface{}{},
			}},
		})
	}
	for _, item := range t.toolItems {
		output = append(output, map[string]interface{}{
			"id":        item.ItemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   item.CallID,
			"name":      item.Name,
			"arguments": item.Arguments.String(),
		})
	}
	return map[string]interface{}{
		"id":         t.responseID,
		"object":     "response",
		"created_at": t.created,
		"status":     "completed",
		"model":      t.model,
		"output":     output,
		"usage": map[string]int{
			"input_tokens":  t.inputTokens,
			"output_tokens": t.outputTokens,
			"total_tokens":  t.inputTokens + t.outputTokens,
		},
	}
}

func (t *openAIResponsesStreamTranscoder) finalizeMessageItem() error {
	if !t.messageStarted {
		return nil
	}
	if err := t.emit("response.output_text.done", map[string]interface{}{
		"response_id":   t.responseID,
		"output_index":  t.messageIndex,
		"content_index": 0,
		"text":          t.messageText.String(),
	}); err != nil {
		return err
	}
	return t.emit("response.output_item.done", map[string]interface{}{
		"response_id":  t.responseID,
		"output_index": t.messageIndex,
		"item": map[string]interface{}{
			"id":     t.messageItemID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        t.messageText.String(),
				"annotations": []interface{}{},
			}},
		},
	})
}

func (t *openAIResponsesStreamTranscoder) finalizeToolItem(index int) error {
	item, ok := t.toolBlocks[index]
	if !ok || item.Done {
		return nil
	}
	item.Done = true
	if err := t.emit("response.function_call_arguments.done", map[string]interface{}{
		"response_id":  t.responseID,
		"output_index": item.OutputIndex,
		"item_id":      item.ItemID,
		"arguments":    item.Arguments.String(),
	}); err != nil {
		return err
	}
	return t.emit("response.output_item.done", map[string]interface{}{
		"response_id":  t.responseID,
		"output_index": item.OutputIndex,
		"item": map[string]interface{}{
			"id":        item.ItemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   item.CallID,
			"name":      item.Name,
			"arguments": item.Arguments.String(),
		},
	})
}

func (t *openAIResponsesStreamTranscoder) HandleFrame(frame anthropicSSEFrame) error {
	var payload map[string]interface{}
	if len(frame.Data) > 0 {
		if err := json.Unmarshal(frame.Data, &payload); err != nil {
			return err
		}
	}

	switch frame.Event {
	case "message_start":
		if message, ok := payload["message"].(map[string]interface{}); ok {
			if usage, ok := message["usage"].(map[string]interface{}); ok {
				t.inputTokens = intValue(usage["input_tokens"])
			}
		}
		return t.ensureCreated()
	case "content_block_start":
		if err := t.ensureCreated(); err != nil {
			return err
		}
		index := intValue(payload["index"])
		block, _ := payload["content_block"].(map[string]interface{})
		if stringValue(block["type"]) == "tool_use" {
			item := &responsesStreamToolItem{
				ItemID:      "fc_" + compactUUID(),
				OutputIndex: t.nextOutputIndex,
				CallID:      stringValue(block["id"]),
				Name:        stringValue(block["name"]),
			}
			t.nextOutputIndex++
			t.toolBlocks[index] = item
			t.toolItems = append(t.toolItems, item)
			return t.emit("response.output_item.added", map[string]interface{}{
				"response_id":  t.responseID,
				"output_index": item.OutputIndex,
				"item": map[string]interface{}{
					"id":        item.ItemID,
					"type":      "function_call",
					"status":    "in_progress",
					"call_id":   item.CallID,
					"name":      item.Name,
					"arguments": "",
				},
			})
		}
	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]interface{})
		switch stringValue(delta["type"]) {
		case "text_delta":
			text := stringValue(delta["text"])
			if text == "" {
				return nil
			}
			if err := t.ensureMessageItem(); err != nil {
				return err
			}
			t.messageText.WriteString(text)
			return t.emit("response.output_text.delta", map[string]interface{}{
				"response_id":   t.responseID,
				"output_index":  t.messageIndex,
				"content_index": 0,
				"delta":         text,
			})
		case "input_json_delta":
			index := intValue(payload["index"])
			item, ok := t.toolBlocks[index]
			if !ok {
				return nil
			}
			chunk := stringValue(delta["partial_json"])
			item.Arguments.WriteString(chunk)
			return t.emit("response.function_call_arguments.delta", map[string]interface{}{
				"response_id":  t.responseID,
				"output_index": item.OutputIndex,
				"item_id":      item.ItemID,
				"delta":        chunk,
			})
		}
	case "content_block_stop":
		return t.finalizeToolItem(intValue(payload["index"]))
	case "message_delta":
		if usage, ok := payload["usage"].(map[string]interface{}); ok {
			t.outputTokens = intValue(usage["output_tokens"])
		}
		if err := t.finalizeMessageItem(); err != nil {
			return err
		}
		if t.completedSent {
			return nil
		}
		t.completedSent = true
		return t.emit("response.completed", map[string]interface{}{
			"response": t.buildFinalResponseObject(),
		})
	case "message_stop":
		return nil
	}
	return nil
}

func HandleOpenAIChatCompletions(pool *AccountPool) http.HandlerFunc {
	anthropicHandler := HandleAnthropicMessages(pool)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "failed to read request body: "+err.Error(), "invalid_request_error", "")
			return
		}
		if len(bodyBytes) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "request body is required", "invalid_request_error", "")
			return
		}
		LogAPIInputJSONBytes("openai-chat", "incoming /v1/chat/completions request", bodyBytes)

		var req OpenAIChatCompletionRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error", "")
			return
		}
		anthReq, err := convertOpenAIChatCompletionRequest(&req)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}

		respID := "chatcmpl_" + compactUUID()
		created := time.Now().Unix()
		if req.Stream {
			streamAnthropicAsOpenAIChat(w, r, anthropicHandler, anthReq, respID, created, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
			return
		}

		anthResp, invErr := invokeAnthropicNonStream(anthropicHandler, r, anthReq)
		if invErr != nil {
			writeOpenAIError(w, invErr.Status, invErr.Message, invErr.Type, "")
			return
		}

		resp := buildOpenAIChatCompletionResponse(respID, created, anthReq.Model, anthResp)
		LogAPIOutputJSON(respID, "openai chat completions response", resp)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func HandleOpenAIResponses(pool *AccountPool) http.HandlerFunc {
	anthropicHandler := HandleAnthropicMessages(pool)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "failed to read request body: "+err.Error(), "invalid_request_error", "")
			return
		}
		if len(bodyBytes) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "request body is required", "invalid_request_error", "")
			return
		}
		LogAPIInputJSONBytes("openai-resp", "incoming /v1/responses request", bodyBytes)

		var req OpenAIResponsesRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error", "")
			return
		}
		anthReq, err := convertOpenAIResponsesRequest(&req)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}

		respID := "resp_" + compactUUID()
		created := time.Now().Unix()
		if req.Stream {
			streamAnthropicAsOpenAIResponses(w, r, anthropicHandler, anthReq, respID, created)
			return
		}

		anthResp, invErr := invokeAnthropicNonStream(anthropicHandler, r, anthReq)
		if invErr != nil {
			writeOpenAIError(w, invErr.Status, invErr.Message, invErr.Type, "")
			return
		}

		resp := buildOpenAIResponsesResponse(respID, created, anthReq.Model, anthResp)
		LogAPIOutputJSON(respID, "openai responses response", resp)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func streamAnthropicAsOpenAIChat(w http.ResponseWriter, r *http.Request, anthropicHandler http.HandlerFunc, anthropicReq *AnthropicRequest, responseID string, created int64, includeUsage bool) {
	bridge := newAnthropicStreamBridgeWriter()
	innerReq, err := newAnthropicBridgeRequest(r, anthropicReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "api_error", "")
		return
	}
	go func() {
		anthropicHandler(bridge, innerReq)
		bridge.Close()
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming not supported", "api_error", "")
		return
	}
	transcoder := newOpenAIChatStreamTranscoder(w, flusher, responseID, anthropicReq.Model, created, includeUsage)
	headersSent := false
	for raw := range bridge.frames {
		if bridge.Status() != http.StatusOK {
			continue
		}
		frame, err := parseAnthropicSSEFrame(raw)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to parse Anthropic stream: "+err.Error(), "api_error", "")
			return
		}
		if !headersSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			headersSent = true
		}
		if err := transcoder.HandleFrame(frame); err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to map Anthropic stream: "+err.Error(), "api_error", "")
			return
		}
	}

	if bridge.Status() != http.StatusOK {
		invErr := parseAnthropicInvocationError(bridge.Status(), bridge.ErrorBody())
		writeOpenAIError(w, invErr.Status, invErr.Message, invErr.Type, "")
	}
}

func streamAnthropicAsOpenAIResponses(w http.ResponseWriter, r *http.Request, anthropicHandler http.HandlerFunc, anthropicReq *AnthropicRequest, responseID string, created int64) {
	bridge := newAnthropicStreamBridgeWriter()
	innerReq, err := newAnthropicBridgeRequest(r, anthropicReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "api_error", "")
		return
	}
	go func() {
		anthropicHandler(bridge, innerReq)
		bridge.Close()
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming not supported", "api_error", "")
		return
	}
	transcoder := newOpenAIResponsesStreamTranscoder(w, flusher, responseID, anthropicReq.Model, created)
	headersSent := false
	for raw := range bridge.frames {
		if bridge.Status() != http.StatusOK {
			continue
		}
		frame, err := parseAnthropicSSEFrame(raw)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to parse Anthropic stream: "+err.Error(), "api_error", "")
			return
		}
		if !headersSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			headersSent = true
		}
		if err := transcoder.HandleFrame(frame); err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "failed to map Anthropic stream: "+err.Error(), "api_error", "")
			return
		}
	}

	if bridge.Status() != http.StatusOK {
		invErr := parseAnthropicInvocationError(bridge.Status(), bridge.ErrorBody())
		writeOpenAIError(w, invErr.Status, invErr.Message, invErr.Type, "")
	}
}

func invokeAnthropicNonStream(anthropicHandler http.HandlerFunc, sourceReq *http.Request, anthropicReq *AnthropicRequest) (*AnthropicResponse, *anthropicInvocationError) {
	innerReq, err := newAnthropicBridgeRequest(sourceReq, anthropicReq)
	if err != nil {
		return nil, &anthropicInvocationError{Status: http.StatusInternalServerError, Message: err.Error(), Type: "api_error"}
	}
	rr := httptest.NewRecorder()
	anthropicHandler(rr, innerReq)
	if rr.Code != http.StatusOK {
		invErr := parseAnthropicInvocationError(rr.Code, rr.Body.Bytes())
		return nil, &invErr
	}
	var anthResp AnthropicResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &anthResp); err != nil {
		return nil, &anthropicInvocationError{Status: http.StatusBadGateway, Message: "invalid anthropic response: " + err.Error(), Type: "api_error"}
	}
	return &anthResp, nil
}

func newAnthropicBridgeRequest(sourceReq *http.Request, anthropicReq *AnthropicRequest) (*http.Request, error) {
	body, err := json.Marshal(anthropicReq)
	if err != nil {
		return nil, err
	}
	innerReq, err := http.NewRequestWithContext(sourceReq.Context(), http.MethodPost, "/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	innerReq.Header.Set("Content-Type", "application/json")
	copySelectedHeaders(sourceReq.Header, innerReq.Header, "X-Web-Search", "X-Workspace-Search")
	return innerReq, nil
}

func copySelectedHeaders(src, dst http.Header, names ...string) {
	for _, name := range names {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

func parseAnthropicInvocationError(status int, body []byte) anthropicInvocationError {
	invErr := anthropicInvocationError{Status: status, Message: strings.TrimSpace(string(body)), Type: "api_error"}
	var payload struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error.Message != "" {
		invErr.Message = payload.Error.Message
		if payload.Error.Type != "" {
			invErr.Type = payload.Error.Type
		}
	}
	return invErr
}

func parseAnthropicSSEFrame(raw string) (anthropicSSEFrame, error) {
	var frame anthropicSSEFrame
	lines := strings.Split(raw, "\n")
	var dataLines []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "event: "):
			frame.Event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		}
	}
	if frame.Event == "" {
		return frame, fmt.Errorf("missing event in frame %q", raw)
	}
	if len(dataLines) > 0 {
		frame.Data = json.RawMessage(strings.Join(dataLines, "\n"))
	}
	return frame, nil
}

func sendOpenAISSE(w http.ResponseWriter, flusher http.Flusher, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func sendOpenAISSEEvent(w http.ResponseWriter, flusher http.Flusher, eventType string, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func buildOpenAIChatCompletionResponse(responseID string, created int64, model string, anthResp *AnthropicResponse) OpenAIChatCompletionResponse {
	text, toolCalls := extractAnthropicTextAndToolCalls(anthResp.Content)
	finishReason := mapAnthropicStopReasonToOpenAI(stringValueOrDefault(anthResp.StopReason, "end_turn"))
	message := map[string]interface{}{
		"role": "assistant",
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if text == "" && len(toolCalls) > 0 {
		message["content"] = nil
	} else {
		message["content"] = text
	}
	resp := OpenAIChatCompletionResponse{
		ID:      responseID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []OpenAIChatCompletionChoice{{
			Index:        0,
			Message:      message,
			FinishReason: &finishReason,
		}},
	}
	if anthResp.Usage != nil {
		resp.Usage = map[string]int{
			"prompt_tokens":     anthResp.Usage.InputTokens,
			"completion_tokens": anthResp.Usage.OutputTokens,
			"total_tokens":      anthResp.Usage.InputTokens + anthResp.Usage.OutputTokens,
		}
	}
	return resp
}

func buildOpenAIResponsesResponse(responseID string, created int64, model string, anthResp *AnthropicResponse) map[string]interface{} {
	text, toolCalls := extractAnthropicTextAndToolCalls(anthResp.Content)
	output := make([]map[string]interface{}, 0, 1+len(toolCalls))
	if text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]interface{}{
			"id":     "msg_" + compactUUID(),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []map[string]interface{}{{
				"type":        "output_text",
				"text":        text,
				"annotations": []interface{}{},
			}},
		})
	}
	for _, toolCall := range toolCalls {
		output = append(output, map[string]interface{}{
			"id":        "fc_" + compactUUID(),
			"type":      "function_call",
			"status":    "completed",
			"call_id":   toolCall.ID,
			"name":      toolCall.Function.Name,
			"arguments": toolCall.Function.Arguments,
		})
	}
	resp := map[string]interface{}{
		"id":         responseID,
		"object":     "response",
		"created_at": created,
		"status":     "completed",
		"model":      model,
		"output":     output,
	}
	if anthResp.Usage != nil {
		resp["usage"] = map[string]int{
			"input_tokens":  anthResp.Usage.InputTokens,
			"output_tokens": anthResp.Usage.OutputTokens,
			"total_tokens":  anthResp.Usage.InputTokens + anthResp.Usage.OutputTokens,
		}
	}
	return resp
}

func extractAnthropicTextAndToolCalls(blocks []AnthropicContentBlock) (string, []OpenAIChatToolCall) {
	var text strings.Builder
	var toolCalls []OpenAIChatToolCall
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			toolCalls = append(toolCalls, OpenAIChatToolCall{
				ID:   block.ID,
				Type: "function",
				Function: OpenAIChatToolCallFunction{
					Name:      block.Name,
					Arguments: string(block.Input),
				},
			})
		}
	}
	return text.String(), toolCalls
}

func convertOpenAIChatCompletionRequest(req *OpenAIChatCompletionRequest) (*AnthropicRequest, error) {
	if req.N > 1 {
		return nil, fmt.Errorf("n > 1 is not supported")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages is required")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = AppConfig.Proxy.DefaultModel
	}
	tools, err := convertOpenAITools(req.Tools, req.Functions)
	if err != nil {
		return nil, err
	}
	system, messages, err := convertOpenAIChatMessagesToAnthropic(req.Messages)
	if err != nil {
		return nil, err
	}
	anthReq := &AnthropicRequest{
		Model:        model,
		MaxTokens:    firstNonZero(req.MaxCompletionTokens, req.MaxTokens),
		System:       system,
		Messages:     messages,
		Stream:       req.Stream,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		Tools:        tools,
		ToolChoice:   normalizeOpenAIToolChoice(req.ToolChoice, req.FunctionCall),
		OutputConfig: convertOpenAIResponseFormat(req.ResponseFormat),
		Metadata:     req.Metadata,
	}
	return anthReq, nil
}

func convertOpenAIResponsesRequest(req *OpenAIResponsesRequest) (*AnthropicRequest, error) {
	if strings.TrimSpace(req.PreviousResponseID) != "" {
		return nil, fmt.Errorf("previous_response_id is not supported in this stateless Responses bridge")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = AppConfig.Proxy.DefaultModel
	}
	tools, err := convertOpenAITools(req.Tools, nil)
	if err != nil {
		return nil, err
	}
	system, messages, err := convertOpenAIResponsesInputToAnthropic(req.Instructions, req.Input)
	if err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	return &AnthropicRequest{
		Model:        model,
		MaxTokens:    req.MaxOutputTokens,
		System:       system,
		Messages:     messages,
		Stream:       req.Stream,
		Temperature:  req.Temperature,
		TopP:         req.TopP,
		Tools:        tools,
		ToolChoice:   req.ToolChoice,
		OutputConfig: convertOpenAIResponsesTextFormat(req.Text),
		Metadata:     req.Metadata,
	}, nil
}

func convertOpenAITools(tools []OpenAITool, functions []OpenAIFunctionDefinition) ([]AnthropicTool, error) {
	if len(tools) == 0 && len(functions) == 0 {
		return nil, nil
	}
	var anthropicTools []AnthropicTool
	if len(tools) == 0 {
		for _, fn := range functions {
			anthropicTools = append(anthropicTools, AnthropicTool{
				Name:        fn.Name,
				Description: fn.Description,
				InputSchema: ensureJSONSchemaObject(fn.Parameters),
			})
		}
		return anthropicTools, nil
	}
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			fn := tool.Function
			if fn == nil {
				fn = &OpenAIFunctionDefinition{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  tool.Parameters,
				}
			}
			if strings.TrimSpace(fn.Name) == "" {
				return nil, fmt.Errorf("tool.function.name is required")
			}
			anthropicTools = append(anthropicTools, AnthropicTool{
				Name:        fn.Name,
				Description: fn.Description,
				InputSchema: ensureJSONSchemaObject(fn.Parameters),
			})
		default:
			return nil, fmt.Errorf("unsupported tool type %q", tool.Type)
		}
	}
	return anthropicTools, nil
}

func normalizeOpenAIToolChoice(toolChoice interface{}, functionCall interface{}) interface{} {
	if toolChoice != nil {
		return toolChoice
	}
	if functionCall == nil {
		return nil
	}
	switch v := functionCall.(type) {
	case string:
		return v
	case map[string]interface{}:
		if name, ok := v["name"].(string); ok && name != "" {
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}
		}
	}
	return functionCall
}

func convertOpenAIChatMessagesToAnthropic(messages []OpenAIChatMessage) (interface{}, []AnthropicMessage, error) {
	var systemParts []string
	var anthropicMsgs []AnthropicMessage
	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		switch role {
		case "system", "developer":
			text, err := flattenOpenAIContentText(msg.Content)
			if err != nil {
				return nil, nil, err
			}
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			content, err := convertOpenAIContentToAnthropicBlocks(msg.Content, false)
			if err != nil {
				return nil, nil, err
			}
			anthropicMsgs = append(anthropicMsgs, AnthropicMessage{Role: "user", Content: content})
		case "assistant":
			content, err := convertOpenAIAssistantMessageToAnthropicContent(msg)
			if err != nil {
				return nil, nil, err
			}
			anthropicMsgs = append(anthropicMsgs, AnthropicMessage{Role: "assistant", Content: content})
		case "tool":
			content, err := convertOpenAIToolMessageToAnthropicContent(msg.Content, msg.ToolCallID)
			if err != nil {
				return nil, nil, err
			}
			anthropicMsgs = append(anthropicMsgs, AnthropicMessage{Role: "user", Content: content})
		default:
			return nil, nil, fmt.Errorf("unsupported message role %q", role)
		}
	}
	var system interface{}
	if len(systemParts) > 0 {
		system = strings.Join(systemParts, "\n\n")
	}
	return system, anthropicMsgs, nil
}

func convertOpenAIResponsesInputToAnthropic(instructions string, input interface{}) (interface{}, []AnthropicMessage, error) {
	var systemParts []string
	if trimmed := strings.TrimSpace(instructions); trimmed != "" {
		systemParts = append(systemParts, trimmed)
	}
	var anthropicMsgs []AnthropicMessage
	flushPendingUser := func(parts []interface{}) {
		if len(parts) > 0 {
			anthropicMsgs = append(anthropicMsgs, AnthropicMessage{Role: "user", Content: parts})
		}
	}

	switch v := input.(type) {
	case string:
		anthropicMsgs = append(anthropicMsgs, AnthropicMessage{Role: "user", Content: v})
	case map[string]interface{}:
		return convertOpenAIResponsesInputToAnthropic(instructions, []interface{}{v})
	case []interface{}:
		var pendingUserParts []interface{}
		for _, raw := range v {
			item, ok := raw.(map[string]interface{})
			if !ok {
				return nil, nil, fmt.Errorf("unsupported responses input item %T", raw)
			}
			role := stringValue(item["role"])
			itemType := stringValue(item["type"])
			switch {
			case role != "":
				flushPendingUser(pendingUserParts)
				pendingUserParts = nil
				msg, err := convertGenericOpenAIMessageToAnthropic(role, item)
				if err != nil {
					return nil, nil, err
				}
				if role == "system" || role == "developer" {
					if text := stringValue(msg.Content); text != "" {
						systemParts = append(systemParts, text)
					}
					continue
				}
				anthropicMsgs = append(anthropicMsgs, msg)
			case itemType == "message":
				flushPendingUser(pendingUserParts)
				pendingUserParts = nil
				msg, err := convertGenericOpenAIMessageToAnthropic(stringValue(item["role"]), item)
				if err != nil {
					return nil, nil, err
				}
				anthropicMsgs = append(anthropicMsgs, msg)
			case itemType == "function_call_output":
				flushPendingUser(pendingUserParts)
				pendingUserParts = nil
				content, err := convertOpenAIToolOutputToAnthropicContent(item)
				if err != nil {
					return nil, nil, err
				}
				anthropicMsgs = append(anthropicMsgs, AnthropicMessage{Role: "user", Content: content})
			case isOpenAIInputContentType(itemType):
				blocks, err := convertOpenAIContentItemsToAnthropicBlocks([]interface{}{item}, true)
				if err != nil {
					return nil, nil, err
				}
				pendingUserParts = append(pendingUserParts, blocks...)
			default:
				return nil, nil, fmt.Errorf("unsupported responses input item type %q", itemType)
			}
		}
		flushPendingUser(pendingUserParts)
	default:
		return nil, nil, fmt.Errorf("unsupported input type %T", input)
	}

	var system interface{}
	if len(systemParts) > 0 {
		system = strings.Join(systemParts, "\n\n")
	}
	return system, anthropicMsgs, nil
}

func convertGenericOpenAIMessageToAnthropic(role string, item map[string]interface{}) (AnthropicMessage, error) {
	msg := OpenAIChatMessage{
		Role:       role,
		Content:    item["content"],
		ToolCallID: stringValue(item["tool_call_id"]),
	}
	if rawToolCalls, ok := item["tool_calls"].([]interface{}); ok {
		for _, raw := range rawToolCalls {
			callMap, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			msg.ToolCalls = append(msg.ToolCalls, OpenAIChatToolCall{
				ID:   stringValue(callMap["id"]),
				Type: stringValue(callMap["type"]),
				Function: OpenAIChatToolCallFunction{
					Name:      stringValue(nestedMapValue(callMap, "function", "name")),
					Arguments: stringValue(nestedMapValue(callMap, "function", "arguments")),
				},
			})
		}
	}
	switch role {
	case "system", "developer":
		text, err := flattenOpenAIContentText(msg.Content)
		if err != nil {
			return AnthropicMessage{}, err
		}
		return AnthropicMessage{Role: role, Content: text}, nil
	case "user":
		content, err := convertOpenAIContentToAnthropicBlocks(msg.Content, false)
		if err != nil {
			return AnthropicMessage{}, err
		}
		return AnthropicMessage{Role: "user", Content: content}, nil
	case "assistant":
		content, err := convertOpenAIAssistantMessageToAnthropicContent(msg)
		if err != nil {
			return AnthropicMessage{}, err
		}
		return AnthropicMessage{Role: "assistant", Content: content}, nil
	case "tool":
		content, err := convertOpenAIToolMessageToAnthropicContent(msg.Content, msg.ToolCallID)
		if err != nil {
			return AnthropicMessage{}, err
		}
		return AnthropicMessage{Role: "user", Content: content}, nil
	default:
		return AnthropicMessage{}, fmt.Errorf("unsupported role %q", role)
	}
}

func convertOpenAIAssistantMessageToAnthropicContent(msg OpenAIChatMessage) (interface{}, error) {
	var blocks []interface{}
	textBlocks, err := convertOpenAIContentToAnthropicBlocks(msg.Content, true)
	if err != nil {
		return nil, err
	}
	blocks = append(blocks, textBlocks...)
	for _, call := range msg.ToolCalls {
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("assistant tool call %s has invalid JSON arguments", call.Function.Name)
		}
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    defaultString(call.ID, "call_"+compactUUID()),
			"name":  call.Function.Name,
			"input": json.RawMessage(args),
		})
	}
	if len(blocks) == 0 {
		return "", nil
	}
	return blocks, nil
}

func convertOpenAIToolMessageToAnthropicContent(content interface{}, toolCallID string) (interface{}, error) {
	if strings.TrimSpace(toolCallID) == "" {
		return nil, fmt.Errorf("tool_call_id is required for tool messages")
	}
	toolContent, err := convertOpenAIToolResultContent(content)
	if err != nil {
		return nil, err
	}
	return []interface{}{map[string]interface{}{
		"type":        "tool_result",
		"tool_use_id": toolCallID,
		"content":     toolContent,
	}}, nil
}

func convertOpenAIToolOutputToAnthropicContent(item map[string]interface{}) (interface{}, error) {
	callID := stringValue(item["call_id"])
	if callID == "" {
		callID = stringValue(item["tool_call_id"])
	}
	if callID == "" {
		return nil, fmt.Errorf("function_call_output.call_id is required")
	}
	toolContent, err := convertOpenAIToolResultContent(item["output"])
	if err != nil {
		return nil, err
	}
	return []interface{}{map[string]interface{}{
		"type":        "tool_result",
		"tool_use_id": callID,
		"content":     toolContent,
	}}, nil
}

func convertOpenAIToolResultContent(content interface{}) (interface{}, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []interface{}:
		var parts []map[string]interface{}
		for _, raw := range v {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			text := stringValue(item["text"])
			if text == "" {
				text = stringValue(item["content"])
			}
			if text == "" {
				continue
			}
			parts = append(parts, map[string]interface{}{"type": "text", "text": text})
		}
		if len(parts) == 0 {
			return "", nil
		}
		result := make([]interface{}, 0, len(parts))
		for _, part := range parts {
			result = append(result, part)
		}
		return result, nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return string(raw), nil
	}
}

func convertOpenAIContentToAnthropicBlocks(content interface{}, allowEmpty bool) ([]interface{}, error) {
	switch v := content.(type) {
	case nil:
		if allowEmpty {
			return nil, nil
		}
		return []interface{}{}, nil
	case string:
		if v == "" {
			return nil, nil
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": v}}, nil
	case []interface{}:
		return convertOpenAIContentItemsToAnthropicBlocks(v, allowEmpty)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": string(raw)}}, nil
	}
}

func convertOpenAIContentItemsToAnthropicBlocks(items []interface{}, allowEmpty bool) ([]interface{}, error) {
	var blocks []interface{}
	for _, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		typeName := stringValue(item["type"])
		switch typeName {
		case "text", "input_text", "output_text":
			text := stringValue(item["text"])
			if text == "" {
				text = stringValue(item["content"])
			}
			if text != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": text})
			}
		case "image_url":
			source, err := convertOpenAIImageSource(item["image_url"])
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, map[string]interface{}{"type": "image", "source": source})
		case "input_image":
			imagePayload := item["image_url"]
			if imagePayload == nil {
				imagePayload = item["url"]
			}
			if imagePayload == nil {
				imagePayload = item["image"]
			}
			source, err := convertOpenAIImageSource(imagePayload)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, map[string]interface{}{"type": "image", "source": source})
		case "file", "input_file":
			source, err := convertOpenAIFileSource(item)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, map[string]interface{}{"type": "document", "source": source})
		default:
			if typeName == "" && stringValue(item["text"]) != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": stringValue(item["text"])})
				continue
			}
			return nil, fmt.Errorf("unsupported content item type %q", typeName)
		}
	}
	if len(blocks) == 0 && !allowEmpty {
		return []interface{}{}, nil
	}
	return blocks, nil
}

func convertOpenAIImageSource(value interface{}) (map[string]interface{}, error) {
	switch v := value.(type) {
	case string:
		return normalizeOpenAIURLSource(v, "image/png")
	case map[string]interface{}:
		if fileID := stringValue(v["file_id"]); fileID != "" {
			return nil, fmt.Errorf("image file_id %q is not supported without the Files API", fileID)
		}
		url := stringValue(v["url"])
		if url == "" {
			url = stringValue(v["image_url"])
		}
		if url == "" {
			return nil, fmt.Errorf("image_url.url is required")
		}
		return normalizeOpenAIURLSource(url, "image/png")
	default:
		return nil, fmt.Errorf("unsupported image payload %T", value)
	}
}

func convertOpenAIFileSource(item map[string]interface{}) (map[string]interface{}, error) {
	payload := item
	if nested, ok := item["file"].(map[string]interface{}); ok {
		payload = nested
	}
	if fileID := stringValue(payload["file_id"]); fileID != "" {
		return nil, fmt.Errorf("file_id %q is not supported without the Files API", fileID)
	}
	if dataURL := stringValue(payload["url"]); dataURL != "" {
		return normalizeOpenAIURLSource(dataURL, detectMediaTypeFromPayload(payload, "application/pdf"))
	}
	fileData := stringValue(payload["file_data"])
	if fileData == "" {
		fileData = stringValue(payload["data"])
	}
	if fileData == "" {
		return nil, fmt.Errorf("file_data or url is required for file inputs")
	}
	if parsed, ok := parseDataURL(fileData); ok {
		return map[string]interface{}{
			"type":       "base64",
			"media_type": parsed.MediaType,
			"data":       parsed.Data,
		}, nil
	}
	mediaType := detectMediaTypeFromPayload(payload, "application/pdf")
	if _, err := base64.StdEncoding.DecodeString(fileData); err != nil {
		return nil, fmt.Errorf("file_data must be base64: %w", err)
	}
	return map[string]interface{}{
		"type":       "base64",
		"media_type": mediaType,
		"data":       fileData,
	}, nil
}

func normalizeOpenAIURLSource(raw string, defaultMediaType string) (map[string]interface{}, error) {
	if parsed, ok := parseDataURL(raw); ok {
		return map[string]interface{}{
			"type":       "base64",
			"media_type": parsed.MediaType,
			"data":       parsed.Data,
		}, nil
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return nil, fmt.Errorf("unsupported URL source %q", truncateForLog(raw, 60))
	}
	return map[string]interface{}{
		"type": "url",
		"url":  raw,
	}, nil
}

type parsedDataURL struct {
	MediaType string
	Data      string
}

func parseDataURL(raw string) (parsedDataURL, bool) {
	if !strings.HasPrefix(raw, "data:") {
		return parsedDataURL{}, false
	}
	comma := strings.Index(raw, ",")
	if comma <= 5 {
		return parsedDataURL{}, false
	}
	header := raw[5:comma]
	data := raw[comma+1:]
	if !strings.Contains(header, ";base64") {
		return parsedDataURL{}, false
	}
	mediaType := strings.TrimSuffix(header, ";base64")
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return parsedDataURL{MediaType: mediaType, Data: data}, true
}

func detectMediaTypeFromPayload(payload map[string]interface{}, fallback string) string {
	for _, key := range []string{"mime_type", "media_type", "content_type"} {
		if value := stringValue(payload[key]); value != "" {
			return value
		}
	}
	if filename := stringValue(payload["filename"]); filename != "" {
		if detected := mime.TypeByExtension(filepath.Ext(filename)); detected != "" {
			return detected
		}
	}
	return fallback
}

func flattenOpenAIContentText(content interface{}) (string, error) {
	switch v := content.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case []interface{}:
		var sb strings.Builder
		for _, raw := range v {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			typeName := stringValue(item["type"])
			switch typeName {
			case "text", "input_text", "output_text":
				sb.WriteString(stringValue(item["text"]))
			case "image_url", "input_image", "file", "input_file":
				// 非文本内容留给专门的内容块转换处理；系统消息里忽略。
			default:
				if typeName == "" && stringValue(item["text"]) != "" {
					sb.WriteString(stringValue(item["text"]))
					continue
				}
				return "", fmt.Errorf("unsupported content item type %q in text-only context", typeName)
			}
		}
		return sb.String(), nil
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	}
}

func convertOpenAIResponseFormat(format *OpenAIChatResponseFormat) *AnthropicOutputConfig {
	if format == nil {
		return nil
	}
	switch format.Type {
	case "json_schema":
		if format.JSONSchema == nil || format.JSONSchema.Schema == nil {
			return nil
		}
		return &AnthropicOutputConfig{Format: &AnthropicOutputFormat{Type: "json_schema", Schema: format.JSONSchema.Schema}}
	case "json_object":
		return &AnthropicOutputConfig{Format: &AnthropicOutputFormat{Type: "json_schema", Schema: looseJSONObjectSchema()}}
	default:
		return nil
	}
}

func convertOpenAIResponsesTextFormat(textCfg *OpenAIResponsesTextConfig) *AnthropicOutputConfig {
	if textCfg == nil {
		return nil
	}
	return convertOpenAIResponseFormat(textCfg.Format)
}

func looseJSONObjectSchema() interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"additionalProperties": true,
	}
}

func ensureJSONSchemaObject(schema interface{}) interface{} {
	if schema == nil {
		return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	return schema
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errType, param string) {
	payload := OpenAIErrorResponse{Error: OpenAIError{Message: message, Type: errType, Param: param}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func mapAnthropicStopReasonToOpenAI(stopReason string) string {
	switch stopReason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

func compactUUID() string {
	return strings.ReplaceAll(generateUUIDv4(), "-", "")
}

func intValue(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	}
	return 0
}

func stringValue(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case json.RawMessage:
		return string(s)
	default:
		return ""
	}
}

func stringValueOrDefault(v *string, fallback string) string {
	if v == nil || *v == "" {
		return fallback
	}
	return *v
}

func nestedMapValue(m map[string]interface{}, key string, nested string) interface{} {
	nv, ok := m[key].(map[string]interface{})
	if !ok {
		return nil
	}
	return nv[nested]
}

func defaultString(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func firstNonZero(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func isOpenAIInputContentType(typeName string) bool {
	switch typeName {
	case "input_text", "input_image", "input_file", "text", "image_url", "file":
		return true
	default:
		return false
	}
}

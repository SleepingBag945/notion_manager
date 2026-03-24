package proxy

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConvertOpenAIChatCompletionRequest_WithFilesToolsAndJSONSchema(t *testing.T) {
	pdfData := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 mock"))
	imageData := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	req := &OpenAIChatCompletionRequest{
		Model: "gpt-5.4",
		Messages: []OpenAIChatMessage{
			{Role: "developer", Content: "Always answer in Chinese."},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "分析这个文件"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64," + imageData}},
				map[string]interface{}{"type": "file", "file": map[string]interface{}{"filename": "spec.pdf", "file_data": pdfData}},
			}},
		},
		Tools: []OpenAITool{{
			Type: "function",
			Function: &OpenAIFunctionDefinition{
				Name:        "Read",
				Description: "Read a file",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"path": map[string]interface{}{"type": "string"},
					},
				},
			},
		}},
		ToolChoice: map[string]interface{}{
			"type":     "function",
			"function": map[string]interface{}{"name": "Read"},
		},
		ResponseFormat: &OpenAIChatResponseFormat{
			Type:       "json_schema",
			JSONSchema: &OpenAIJSONSchemaConfig{Schema: map[string]interface{}{"type": "object"}},
		},
	}

	anthReq, err := convertOpenAIChatCompletionRequest(req)
	if err != nil {
		t.Fatalf("convertOpenAIChatCompletionRequest() error = %v", err)
	}
	if anthReq.Model != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", anthReq.Model)
	}
	if anthReq.System != "Always answer in Chinese." {
		t.Fatalf("system = %#v", anthReq.System)
	}
	if len(anthReq.Tools) != 1 || anthReq.Tools[0].Name != "Read" {
		t.Fatalf("tools = %#v", anthReq.Tools)
	}
	if anthReq.OutputConfig == nil || anthReq.OutputConfig.Format == nil || anthReq.OutputConfig.Format.Type != "json_schema" {
		t.Fatalf("output_config = %#v", anthReq.OutputConfig)
	}
	if len(anthReq.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(anthReq.Messages))
	}
	blocks, ok := anthReq.Messages[0].Content.([]interface{})
	if !ok || len(blocks) != 3 {
		t.Fatalf("content blocks = %#v", anthReq.Messages[0].Content)
	}
	first := blocks[0].(map[string]interface{})
	if first["type"] != "text" {
		t.Fatalf("first block = %#v", first)
	}
	second := blocks[1].(map[string]interface{})
	if second["type"] != "image" {
		t.Fatalf("second block = %#v", second)
	}
	third := blocks[2].(map[string]interface{})
	if third["type"] != "document" {
		t.Fatalf("third block = %#v", third)
	}
}

func TestConvertOpenAIResponsesRequest_WithFunctionCallOutput(t *testing.T) {
	req := &OpenAIResponsesRequest{
		Model:        "gpt-5.4",
		Instructions: "Return JSON only.",
		Input: []interface{}{
			map[string]interface{}{"type": "input_text", "text": "hello"},
			map[string]interface{}{"type": "function_call_output", "call_id": "call_123", "output": "done"},
		},
		Text: &OpenAIResponsesTextConfig{Format: &OpenAIChatResponseFormat{Type: "json_object"}},
	}

	anthReq, err := convertOpenAIResponsesRequest(req)
	if err != nil {
		t.Fatalf("convertOpenAIResponsesRequest() error = %v", err)
	}
	if anthReq.System != "Return JSON only." {
		t.Fatalf("system = %#v", anthReq.System)
	}
	if anthReq.OutputConfig == nil || anthReq.OutputConfig.Format == nil || anthReq.OutputConfig.Format.Type != "json_schema" {
		t.Fatalf("output_config = %#v", anthReq.OutputConfig)
	}
	if len(anthReq.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(anthReq.Messages))
	}
	firstBlocks := anthReq.Messages[0].Content.([]interface{})
	if firstBlocks[0].(map[string]interface{})["type"] != "text" {
		t.Fatalf("first message blocks = %#v", firstBlocks)
	}
	secondBlocks := anthReq.Messages[1].Content.([]interface{})
	toolResult := secondBlocks[0].(map[string]interface{})
	if toolResult["type"] != "tool_result" || toolResult["tool_use_id"] != "call_123" {
		t.Fatalf("tool result = %#v", toolResult)
	}
}

func TestBuildOpenAIChatCompletionResponse_FromAnthropicBlocks(t *testing.T) {
	stopReason := "tool_use"
	resp := buildOpenAIChatCompletionResponse("chatcmpl_test", 123, "gpt-5.4", &AnthropicResponse{
		Content: []AnthropicContentBlock{
			{Type: "text", Text: "先读文件"},
			{Type: "tool_use", ID: "call_1", Name: "Read", Input: json.RawMessage(`{"path":"README.md"}`)},
		},
		StopReason: &stopReason,
		Usage:      &AnthropicUsage{InputTokens: 10, OutputTokens: 5},
	})

	if got := resp.Choices[0].Message["content"]; got != "先读文件" {
		t.Fatalf("content = %#v", got)
	}
	toolCalls, ok := resp.Choices[0].Message["tool_calls"].([]OpenAIChatToolCall)
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %#v", resp.Choices[0].Message["tool_calls"])
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %#v", resp.Choices[0].FinishReason)
	}
	if resp.Usage["total_tokens"] != 15 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

func TestOpenAIChatStreamTranscoder_EmitsToolCallsAndDone(t *testing.T) {
	rr := httptest.NewRecorder()
	transcoder := newOpenAIChatStreamTranscoder(rr, rr, "chatcmpl_test", "gpt-5.4", 123, true)
	frames := []anthropicSSEFrame{
		{Event: "message_start", Data: json.RawMessage(`{"message":{"usage":{"input_tokens":11}}}`)},
		{Event: "content_block_start", Data: json.RawMessage(`{"index":0,"content_block":{"type":"tool_use","id":"call_1","name":"Read","input":{}}}`)},
		{Event: "content_block_delta", Data: json.RawMessage(`{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"README.md\"}"}}`)},
		{Event: "message_delta", Data: json.RawMessage(`{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`)},
		{Event: "message_stop", Data: json.RawMessage(`{"type":"message_stop"}`)},
	}
	for _, frame := range frames {
		if err := transcoder.HandleFrame(frame); err != nil {
			t.Fatalf("HandleFrame(%s) error = %v", frame.Event, err)
		}
	}
	body := rr.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("body missing chat.completion.chunk: %s", body)
	}
	if !strings.Contains(body, `"tool_calls"`) || !strings.Contains(body, `README.md`) {
		t.Fatalf("body missing tool call data: %s", body)
	}
	if !strings.Contains(body, `"usage":{`) || !strings.Contains(body, `"prompt_tokens":11`) || !strings.Contains(body, `"completion_tokens":7`) || !strings.Contains(body, `"total_tokens":18`) {
		t.Fatalf("body missing usage chunk: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body missing DONE: %s", body)
	}
}

func TestOpenAIResponsesStreamTranscoder_EmitsCompletedResponse(t *testing.T) {
	rr := httptest.NewRecorder()
	transcoder := newOpenAIResponsesStreamTranscoder(rr, rr, "resp_test", "gpt-5.4", 456)
	frames := []anthropicSSEFrame{
		{Event: "message_start", Data: json.RawMessage(`{"message":{"usage":{"input_tokens":9}}}`)},
		{Event: "content_block_delta", Data: json.RawMessage(`{"index":0,"delta":{"type":"text_delta","text":"你好"}}`)},
		{Event: "content_block_start", Data: json.RawMessage(`{"index":1,"content_block":{"type":"tool_use","id":"call_2","name":"Read","input":{}}}`)},
		{Event: "content_block_delta", Data: json.RawMessage(`{"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.txt\"}"}}`)},
		{Event: "content_block_stop", Data: json.RawMessage(`{"index":1}`)},
		{Event: "message_delta", Data: json.RawMessage(`{"delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":6}}`)},
	}
	for _, frame := range frames {
		if err := transcoder.HandleFrame(frame); err != nil {
			t.Fatalf("HandleFrame(%s) error = %v", frame.Event, err)
		}
	}
	body := rr.Body.String()
	if !strings.Contains(body, "event: response.created") {
		t.Fatalf("missing response.created: %s", body)
	}
	if !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, "你好") {
		t.Fatalf("missing text delta: %s", body)
	}
	if !strings.Contains(body, "event: response.function_call_arguments.delta") || !strings.Contains(body, `a.txt`) {
		t.Fatalf("missing function_call_arguments.delta: %s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("missing response.completed: %s", body)
	}
}

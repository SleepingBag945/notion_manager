package proxy

import (
	"strings"
	"testing"
)

func TestExtractAnthropicSessionSalt(t *testing.T) {
	metadata := map[string]interface{}{
		"user_id": `{"device_id":"dev-1","session_id":"sess-123","account_uuid":""}`,
	}

	if got := extractAnthropicSessionSalt(metadata); got != "sess-123" {
		t.Fatalf("extractAnthropicSessionSalt() = %q, want %q", got, "sess-123")
	}
}

func TestExtractAnthropicSessionSaltFromPlainStrings(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]interface{}
		want     string
	}{
		{name: "prompt cache key", metadata: map[string]interface{}{"prompt_cache_key": " task-123 "}, want: "task-123"},
		{name: "session id", metadata: map[string]interface{}{"session_id": "session-123"}, want: "session-123"},
		{name: "conversation id", metadata: map[string]interface{}{"conversation_id": "conversation-123"}, want: "conversation-123"},
		{name: "user id", metadata: map[string]interface{}{"user_id": "user-123"}, want: "user-123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractAnthropicSessionSalt(tt.metadata); got != tt.want {
				t.Fatalf("extractAnthropicSessionSalt() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestComputeSessionFingerprintForRequestSeparatesPromptCacheKeysAndModels(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "You are a coding assistant."},
		{Role: "user", Content: "Fix the failing test."},
	}

	base := computeSessionFingerprintForRequest(messages, "task-a", "fireworks-kimi-k3")
	otherTask := computeSessionFingerprintForRequest(messages, "task-b", "fireworks-kimi-k3")
	otherModel := computeSessionFingerprintForRequest(messages, "task-a", "anthropic-opus-4-6")
	continued := computeSessionFingerprintForRequest(append(cloneChatMessages(messages),
		ChatMessage{Role: "assistant", Content: "I will inspect it."},
		ChatMessage{Role: "user", Content: "Continue."},
	), "task-a", "fireworks-kimi-k3")
	if base == otherTask {
		t.Fatal("different prompt_cache_key values must not reuse a Notion thread")
	}
	if base == otherModel {
		t.Fatal("different resolved models must not reuse a Notion thread")
	}
	if base != continued {
		t.Fatal("the same prompt_cache_key and model must keep the Notion thread across turns")
	}
}

func TestComputeSessionFingerprintForRequestUsesStableSaltIdentity(t *testing.T) {
	first := []ChatMessage{
		{Role: "system", Content: "System instructions v1"},
		{Role: "user", Content: "First user message"},
	}
	drifted := []ChatMessage{
		{Role: "system", Content: "Completely different system instructions"},
		{Role: "user", Content: "A different user message"},
		{Role: "assistant", Content: "Prior answer"},
		{Role: "tool", Name: "Read", ToolCallID: "call_1", Content: "tool history"},
	}

	fp1 := computeSessionFingerprintForRequest(first, "task-stable", "fireworks-kimi-k3")
	fp2 := computeSessionFingerprintForRequest(drifted, "task-stable", "fireworks-kimi-k3")
	if fp1 != fp2 {
		t.Fatalf("same prompt_cache_key and resolved model must remain stable across message drift: %s != %s", fp1, fp2)
	}

	fallback1 := computeSessionFingerprintForRequest(first, "", "fireworks-kimi-k3")
	fallback2 := computeSessionFingerprintForRequest(drifted, "", "fireworks-kimi-k3")
	if fallback1 == fallback2 {
		t.Fatal("empty session salt must fall back to the model and message fingerprint")
	}
}

func TestComputeSessionFingerprintWithSalt_IgnoresBillingHeaderDrift(t *testing.T) {
	turn1 := []ChatMessage{
		{Role: "system", Content: "x-anthropic-billing-header: cc_version=2.1.81.a; cch=aaaa;\nYou are Claude Code, Anthropic's official CLI for Claude.\nSystem body"},
		{Role: "user", Content: "<available-deferred-tools>\nGrep\nRead\n</available-deferred-tools>"},
	}
	turn2 := []ChatMessage{
		{Role: "system", Content: "x-anthropic-billing-header: cc_version=2.1.81.b; cch=bbbb;\nYou are Claude Code, Anthropic's official CLI for Claude.\nSystem body"},
		{Role: "user", Content: "<available-deferred-tools>\nGrep\nRead\n</available-deferred-tools>"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"copy"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "Grep", Content: "Found 1 file\nsrc/content.js"},
	}

	fp1 := computeSessionFingerprintWithSalt(turn1, "sess-123")
	fp2 := computeSessionFingerprintWithSalt(turn2, "sess-123")
	if fp1 != fp2 {
		t.Fatalf("fingerprint drifted across billing-header changes: %s vs %s", fp1, fp2)
	}
}

func TestApplyStructuredOutputBridge_JSONSchema(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "x-anthropic-billing-header: cc_version=2.1.81; cch=aaaa;"},
		{Role: "system", Content: "You are Claude Code, Anthropic's official CLI for Claude."},
		{Role: "system", Content: "Generate a concise title.\nReturn JSON with a single \"title\" field."},
		{Role: "user", Content: "检查为什么右侧预览栏的md copy按钮出不来"},
	}
	cfg := &AnthropicOutputConfig{
		Format: &AnthropicOutputFormat{
			Type: "json_schema",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title": map[string]interface{}{"type": "string"},
				},
				"required":             []string{"title"},
				"additionalProperties": false,
			},
		},
	}

	bridged := applyStructuredOutputBridge(messages, cfg)
	if len(bridged) != 1 {
		t.Fatalf("expected 1 bridged message, got %d", len(bridged))
	}
	if bridged[0].Role != "user" {
		t.Fatalf("expected bridged role=user, got %s", bridged[0].Role)
	}

	content := bridged[0].Content
	if strings.Contains(content, "x-anthropic-billing-header") {
		t.Fatalf("structured output bridge leaked billing header: %s", content)
	}
	if strings.Contains(content, "You are Claude Code") {
		t.Fatalf("structured output bridge leaked Claude identity line: %s", content)
	}
	if !strings.Contains(content, `Return JSON with a single "title" field.`) {
		t.Fatalf("structured output bridge dropped system instruction: %s", content)
	}
	if !strings.Contains(content, "检查为什么右侧预览栏的md copy按钮出不来") {
		t.Fatalf("structured output bridge dropped user content: %s", content)
	}
	if !strings.Contains(content, `"title": {`) || !strings.Contains(content, `"required": [`) {
		t.Fatalf("structured output bridge did not embed schema JSON: %s", content)
	}
}

func TestInjectToolsIntoMessages_DropsWrapperOnlyUserMessage(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "Bash", Description: "Execute shell command", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Read", Description: "Read a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Write", Description: "Write a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Edit", Description: "Edit a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Glob", Description: "Find files", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Grep", Description: "Search files", Parameters: map[string]interface{}{"type": "object"}}},
	}
	messages := []ChatMessage{
		{Role: "system", Content: "You are Claude Code."},
		{Role: "user", Content: "<available-deferred-tools>\nRead\nEdit\n</available-deferred-tools>"},
		{Role: "user", Content: "修复登录校验"},
	}

	got := injectToolsIntoMessages(messages, tools, "claude-opus-4-6", nil)
	if len(got) != 1 {
		t.Fatalf("expected 1 bridged message, got %d", len(got))
	}

	content := got[0].Content
	if strings.Contains(content, "User: Hello") || strings.Contains(content, "\nHello\n") {
		t.Fatalf("wrapper-only message should not turn into synthetic Hello: %q", content)
	}
	if strings.Contains(content, "<available-deferred-tools>") {
		t.Fatalf("wrapper-only message leaked into bridged content: %q", content)
	}
	if !strings.Contains(content, `Input: "修复登录校验"`) {
		t.Fatalf("expected actual user query in bridged content, got %q", content)
	}
}

func TestNormalizeStructuredOutputText_StripsLangTagAndMarkdownFence(t *testing.T) {
	raw := "<lang primary=\"zh-CN\"/>\n\n```json\n{\"title\":\"Fix digest error\"}\n```"
	got := normalizeStructuredOutputText(raw)
	want := "{\"title\":\"Fix digest error\"}"
	if got != want {
		t.Fatalf("normalizeStructuredOutputText() = %q, want %q", got, want)
	}
}

func TestNormalizeStructuredOutputText_ExtractsJSONObjectFromPrefixedText(t *testing.T) {
	raw := "Here is the JSON output you requested:\n{\"title\":\"Fix invalid password\"}"
	got := normalizeStructuredOutputText(raw)
	want := "{\"title\":\"Fix invalid password\"}"
	if got != want {
		t.Fatalf("normalizeStructuredOutputText() = %q, want %q", got, want)
	}
}

func TestDetectToolBridgeNoToolResponse_MatchesIdentityDriftHandOff(t *testing.T) {
	raw := `<lang primary="zh-CN"/>

抱歉，我理解你希望我直接帮你修改文件，但**我是 Notion AI，无法访问你的本地文件系统**。我没有 Read、Edit、Bash 这些工具的能力。

把下面这段话直接发给你的编码助手（Cursor / Claude Code），它就能帮你操作。`

	if !detectToolBridgeNoToolResponse(raw) {
		t.Fatalf("expected no-tool identity drift text to be detected")
	}
}

func TestDetectToolBridgeNoToolResponse_DoesNotMatchNormalAnswer(t *testing.T) {
	raw := "我已经根据上面的 grep 结果定位到文件，下一步建议缩小 Read 范围后继续编辑。"

	if detectToolBridgeNoToolResponse(raw) {
		t.Fatalf("normal answer should not be classified as no-tool identity drift")
	}
}

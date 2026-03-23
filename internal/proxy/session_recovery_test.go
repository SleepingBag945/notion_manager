package proxy

import (
	"strings"
	"testing"
)

func TestNeedsFreshThreadRecoveryDetectsPriorTurns(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "What is Opus 4.6?"},
		{Role: "assistant", Content: "It is Anthropic's flagship model."},
		{Role: "user", Content: "What about Sonnet?"},
	}

	if !needsFreshThreadRecovery(messages) {
		t.Fatal("expected prior-turn history to require fresh-thread recovery")
	}
}

func TestNeedsFreshThreadRecoverySkipsSingleTurn(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "Be concise."},
		{Role: "user", Content: "What is Opus 4.6?"},
	}

	if needsFreshThreadRecovery(messages) {
		t.Fatal("expected single-turn request to avoid recovery collapse")
	}
}

func TestNeedsFreshThreadRecoveryIgnoresWrapperOnlyUserMessage(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "You are Claude Code."},
		{Role: "user", Content: "<available-deferred-tools>\nRead\nEdit\n</available-deferred-tools>"},
		{Role: "user", Content: "修复登录校验"},
	}

	if needsFreshThreadRecovery(messages) {
		t.Fatal("expected wrapper-only user message to be ignored for recovery collapse")
	}
}

func TestCountNonSystemMessagesIgnoresWrapperOnlyUserMessage(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "You are Claude Code."},
		{Role: "user", Content: "<available-deferred-tools>\nRead\nEdit\n</available-deferred-tools>"},
		{Role: "user", Content: "修复登录校验"},
	}

	if got := countNonSystemMessages(messages); got != 1 {
		t.Fatalf("expected wrapper-only user message to be excluded from raw count, got %d", got)
	}
}

func TestBuildFreshThreadRecoveryMessagesCollapsesHistory(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "Answer in Chinese."},
		{Role: "user", Content: "opus4.6什么时候推出的"},
		{Role: "assistant", Content: "Claude Opus 4.6 在 2026 年 2 月推出。"},
		{Role: "user", Content: "sonnet有什么优势"},
	}

	got := buildFreshThreadRecoveryMessages(messages)
	if len(got) != 1 {
		t.Fatalf("expected 1 collapsed message, got %d", len(got))
	}
	if got[0].Role != "user" {
		t.Fatalf("expected collapsed role=user, got %q", got[0].Role)
	}

	body := got[0].Content
	for _, want := range []string{
		"System instructions:",
		"Answer in Chinese.",
		"Conversation context:",
		"User: opus4.6什么时候推出的",
		"Assistant: Claude Opus 4.6 在 2026 年 2 月推出。",
		"Latest user message:\nsonnet有什么优势",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected collapsed prompt to contain %q, got %q", want, body)
		}
	}
}

func TestBuildToolBridgeRecoveryMessagesSkipsIdentityDriftAssistantText(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "Answer in Chinese."},
		{Role: "user", Content: "修改 internal/web/dist/assets/index-DlVudHMF.js"},
		{Role: "assistant", Content: "我是 Notion AI，无法访问你的本地文件系统。把下面这段话直接发给你的编码助手（Cursor / Claude Code）。"},
		{Role: "tool", Name: "Grep", Content: "Found 1 file\ninternal/web/dist/assets/index-DlVudHMF.js"},
		{Role: "user", Content: "你来动手"},
	}

	got := buildToolBridgeRecoveryMessages(messages)
	if len(got) != 1 {
		t.Fatalf("expected 1 collapsed message, got %d", len(got))
	}

	body := got[0].Content
	if strings.Contains(body, "我是 Notion AI") || strings.Contains(body, "编码助手") {
		t.Fatalf("tool recovery should drop identity-drift assistant text, got %q", body)
	}
	for _, want := range []string{
		"System instructions:",
		"Answer in Chinese.",
		"Conversation context:",
		"User: 修改 internal/web/dist/assets/index-DlVudHMF.js",
		"Tool (Grep): Found 1 file\ninternal/web/dist/assets/index-DlVudHMF.js",
		"Latest user message:\n你来动手",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected tool recovery prompt to contain %q, got %q", want, body)
		}
	}
}

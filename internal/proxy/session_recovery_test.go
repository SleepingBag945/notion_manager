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

func TestBuildToolBridgeRecoveryMessagesSkipsDroidRoleRefusalAssistantText(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "Answer in Chinese."},
		{Role: "user", Content: "生成一个简短标题"},
		{Role: "assistant", Content: "我是 Notion AI，不是 Droid，也无法扮演其他 AI 角色。不过我可以帮你处理你描述的实际需求。"},
		{Role: "tool", Name: "view", Content: "Loaded user context"},
		{Role: "user", Content: "重新试一次"},
	}

	got := buildToolBridgeRecoveryMessages(messages)
	if len(got) != 1 {
		t.Fatalf("expected 1 collapsed message, got %d", len(got))
	}

	body := got[0].Content
	if strings.Contains(body, "我是 Notion AI") || strings.Contains(body, "不是 Droid") {
		t.Fatalf("tool recovery should drop Droid role-refusal assistant text, got %q", body)
	}
	for _, want := range []string{
		"System instructions:",
		"Answer in Chinese.",
		"Conversation context:",
		"User: 生成一个简短标题",
		"Tool (view): Loaded user context",
		"Latest user message:\n重新试一次",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected tool recovery prompt to contain %q, got %q", want, body)
		}
	}
}

func TestNormalizeSessionUserContent_PreservesInlineTagsForSyntheticFollowUp(t *testing.T) {
	content := "Results from executed function(s):\n[Read]: lines.append(f\"结果文件：<code>{html.escape(output_file)}</code>\")\n\nEdit recovery hint: keep inline tags.\nAvailable functions:\n- Read\nOutput format: {\"name\": \"function_name\", \"arguments\": {...}}"
	got := normalizeSessionUserContent(content)
	if !strings.Contains(got, `<code>{html.escape(output_file)}</code>`) {
		t.Fatalf("expected synthetic follow-up to preserve inline tags, got %q", got)
	}
}

func TestNormalizeSessionUserContent_StripsInlineTagsForRegularUserMessage(t *testing.T) {
	content := "<system-reminder>noise</system-reminder><command-name>Read</command-name> 修复这个问题"
	got := normalizeSessionUserContent(content)
	if strings.Contains(got, "<command-name>") || strings.Contains(got, "noise") {
		t.Fatalf("expected regular user message to keep existing stripping behavior, got %q", got)
	}
	if got != "修复这个问题" {
		t.Fatalf("unexpected normalized content: %q", got)
	}
}

func TestExtractLastUserMessage_PreservesInlineTagsForSyntheticFollowUp(t *testing.T) {
	messages := []ChatMessage{
		{Role: "assistant", Content: "previous"},
		{Role: "user", Content: "Results from executed function(s):\n[Read]: lines.append(f\"结果文件：<code>{html.escape(output_file)}</code>\")\n\nEdit recovery hint: keep inline tags.\nAvailable functions:\n- Read\nOutput format: {\"name\": \"function_name\", \"arguments\": {...}}"},
	}

	got := extractLastUserMessage(messages)
	if !strings.Contains(got, `<code>{html.escape(output_file)}</code>`) {
		t.Fatalf("expected extractLastUserMessage to preserve inline tags for synthetic follow-up, got %q", got)
	}
}

func TestBuildFreshThreadRecoveryMessages_PreservesInlineTagsInSyntheticHistory(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "Answer in Chinese."},
		{Role: "user", Content: "Results from executed function(s):\n[Read]: lines.append(f\"结果文件：<code>{html.escape(output_file)}</code>\")\n\nPath recovery hint: use Read(abs_path).\nAvailable functions:\n- Read\nOutput format: {\"name\": \"function_name\", \"arguments\": {...}}"},
		{Role: "assistant", Content: "继续"},
		{Role: "user", Content: "重新试一次"},
	}

	got := buildFreshThreadRecoveryMessages(messages)
	if len(got) != 1 {
		t.Fatalf("expected 1 collapsed message, got %d", len(got))
	}
	body := got[0].Content
	if !strings.Contains(body, `<code>{html.escape(output_file)}</code>`) {
		t.Fatalf("expected recovery history to preserve inline tags for synthetic prompt, got %q", body)
	}
}

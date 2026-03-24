package proxy

import (
	"strings"
	"testing"
)

func TestExtractWorkingDirectoryFromMessages_PwdSystemReminder(t *testing.T) {
	messages := []ChatMessage{
		{
			Role:    "user",
			Content: "<system-reminder>\nUser system info\n\n% pwd\n/mnt/d/code/check_modoles\n\n% ls\ncheck_models.py\n</system-reminder>",
		},
	}

	got := extractWorkingDirectoryFromMessages(messages)
	if got != "/mnt/d/code/check_modoles" {
		t.Fatalf("expected cwd from %% pwd block, got %q", got)
	}
}

func TestInjectToolsIntoMessages_UsesPwdBlockForWorkingDirectory(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "Bash", Description: "Execute shell command", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Read", Description: "Read a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Edit", Description: "Edit a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Write", Description: "Write a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Glob", Description: "Find paths", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Grep", Description: "Search file content", Parameters: map[string]interface{}{"type": "object"}}},
	}
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "<system-reminder>\n% pwd\n/mnt/d/code/check_modoles\n\n% ls\ncheck_models.py\n</system-reminder>",
		},
		{Role: "user", Content: "检查 check_models.py 有没有问题"},
	}

	got := injectToolsIntoMessages(messages, tools, "sonnet-4.6", nil)
	if len(got) == 0 {
		t.Fatal("expected injected messages")
	}
	last := got[len(got)-1].Content
	if !strings.Contains(last, "Working directory: /mnt/d/code/check_modoles") {
		t.Fatalf("expected working directory hint in injected prompt, got: %s", last)
	}
}

func TestBuildSessionChainFollowUp_ReadAbsolutePathRecoveryHint(t *testing.T) {
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "<system-reminder>\n% pwd\n/mnt/d/code/check_modoles\n\n% ls\ncheck_models.py\n</system-reminder>",
		},
		{Role: "user", Content: "检查 @check_models.py"},
		{
			Role:    "assistant",
			Content: "I'll read the file.",
			ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"check_models.py"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Name: "Read", Content: "Error: file_path must be an absolute path"},
		{
			Role:    "assistant",
			Content: "I'll resolve the path.",
			ToolCalls: []ToolCall{
				{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"\\S","path":"./check_models.py"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_2", Name: "Grep", Content: "check_models.py"},
	}

	got := buildSessionChainFollowUp(messages, "- Read(file_path: str, offset?: num, limit?: num)\n- Grep(pattern: str, path?: str)\n", "")
	if len(got) != 1 {
		t.Fatalf("expected single follow-up message, got %d", len(got))
	}
	body := got[0].Content
	for _, want := range []string{
		"Working directory: /mnt/d/code/check_modoles",
		"Path recovery hint:",
		"Do NOT use __done__ yet",
		`"/mnt/d/code/check_modoles/check_models.py"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected follow-up to contain %q, got: %s", want, body)
		}
	}
}

func TestBuildSessionChainFollowUp_NoPathRecoveryHintWithoutReadFailure(t *testing.T) {
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "<system-reminder>\n% pwd\n/mnt/d/code/check_modoles\n</system-reminder>",
		},
		{Role: "user", Content: "看看这个文件"},
		{
			Role:    "assistant",
			Content: "Searching.",
			ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"TODO","path":"check_models.py"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Name: "Grep", Content: "check_models.py:12:TODO"},
	}

	got := buildSessionChainFollowUp(messages, "- Read(file_path: str)\n- Grep(pattern: str)\n", "")
	if strings.Contains(got[0].Content, "Path recovery hint:") {
		t.Fatalf("did not expect path recovery hint without prior Read absolute-path failure, got: %s", got[0].Content)
	}
}

func TestIsSuspiciousReadOutput_EnvBlock(t *testing.T) {
	content := "SHELL=/bin/bash\x00PWD=/mnt/d/code/check_alive.py\x00PATH=/usr/bin\x00HOME=/home/major"
	if !isSuspiciousReadOutput(content) {
		t.Fatalf("expected env-style Read output to be classified as suspicious")
	}
}

func TestBuildSessionChainFollowUp_SanitizesSuspiciousReadOutput(t *testing.T) {
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "<system-reminder>\n% pwd\n/mnt/d/code/check_alive.py\n</system-reminder>",
		},
		{Role: "user", Content: "检查 @chatgpt_register.py"},
		{
			Role:    "assistant",
			Content: "I'll inspect the file.",
			ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"foo","path":"."}`}},
				{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"./chatgpt_register.py"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Name: "Grep", Content: "./chatgpt_register.py"},
		{Role: "tool", ToolCallID: "call_2", Name: "Read", Content: "SHELL=/bin/bash\x00PWD=/mnt/d/code/check_alive.py\x00PATH=/usr/bin\x00HOME=/home/major"},
	}

	got := buildSessionChainFollowUp(messages, "- Read(file_path: str)\n- Grep(pattern: str)\n", "")
	body := got[0].Content
	for _, want := range []string{
		"Working directory: /mnt/d/code/check_alive.py",
		"Path recovery hint:",
		"environment data instead of file contents",
		`"/mnt/d/code/check_alive.py/chatgpt_register.py"`,
		"Read output omitted because it looks like environment or binary data",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected follow-up to contain %q, got: %s", want, body)
		}
	}
	if strings.Contains(body, "SHELL=/bin/bash") {
		t.Fatalf("suspicious Read output should be redacted, got: %s", body)
	}
}

func TestInjectToolsIntoMessages_SanitizesSuspiciousReadOutputInCollapsedChain(t *testing.T) {
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "Bash", Description: "Execute shell command", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Read", Description: "Read a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Edit", Description: "Edit a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Write", Description: "Write a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Glob", Description: "Find paths", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Grep", Description: "Search file content", Parameters: map[string]interface{}{"type": "object"}}},
	}
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "<system-reminder>\n% pwd\n/mnt/d/code/check_alive.py\n</system-reminder>",
		},
		{Role: "user", Content: "@chatgpt_register.py 中，汇报给 tg 的内容里不要带结果文件"},
		{
			Role:    "assistant",
			Content: "I'll inspect the file.",
			ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Grep", Arguments: `{"pattern":"foo","path":"."}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Name: "Grep", Content: "./chatgpt_register.py"},
		{
			Role:    "assistant",
			Content: "I'll read the file.",
			ToolCalls: []ToolCall{
				{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"./chatgpt_register.py"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_2", Name: "Read", Content: "SHELL=/bin/bash\x00PWD=/mnt/d/code/check_alive.py\x00PATH=/usr/bin\x00HOME=/home/major"},
	}

	got := injectToolsIntoMessages(messages, tools, "opus-4.6", nil)
	if len(got) != 1 {
		t.Fatalf("expected collapsed single follow-up message, got %d", len(got))
	}
	body := got[0].Content
	for _, want := range []string{
		"Working directory: /mnt/d/code/check_alive.py",
		"Path recovery hint:",
		`"/mnt/d/code/check_alive.py/chatgpt_register.py"`,
		"Read output omitted because it looks like environment or binary data",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected collapsed prompt to contain %q, got: %s", want, body)
		}
	}
	if strings.Contains(body, "SHELL=/bin/bash") {
		t.Fatalf("collapsed prompt should redact suspicious Read output, got: %s", body)
	}
}

func TestExtractTaggedFilePathsFromMessages(t *testing.T) {
	messages := []ChatMessage{
		{
			Role:    "user",
			Content: "<system-reminder>\nUser tagged file: /mnt/d/code/check_alive.py/chatgpt_register.py\n</system-reminder>",
		},
		{
			Role:    "user",
			Content: "<system-reminder>\nContents of /mnt/d/code/check_alive.py/other.py (lines 1–20 of 20):\nprint('ok')\n</system-reminder>",
		},
	}

	got := extractTaggedFilePathsFromMessages(messages)
	want := []string{
		"/mnt/d/code/check_alive.py/chatgpt_register.py",
		"/mnt/d/code/check_alive.py/other.py",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected tagged paths: got %v want %v", got, want)
	}
}

func TestNormalizeToolCallPaths_UsesTaggedAbsolutePath(t *testing.T) {
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "<system-reminder>\n% pwd\n/mnt/d/code/check_alive.py\n</system-reminder>",
		},
		{
			Role:    "user",
			Content: "<system-reminder>\nUser tagged file: /mnt/d/code/check_alive.py/chatgpt_register.py\n</system-reminder>",
		},
	}
	toolCalls := []ToolCall{
		{
			ID:   "call_read",
			Type: "function",
			Function: ToolCallFunction{
				Name:      "Read",
				Arguments: `{"file_path":"./chatgpt_register.py"}`,
			},
		},
		{
			ID:   "call_edit",
			Type: "function",
			Function: ToolCallFunction{
				Name:      "Edit",
				Arguments: `{"file_path":"chatgpt_register.py","old_str":"x","new_str":"y"}`,
			},
		},
		{
			ID:   "call_grep",
			Type: "function",
			Function: ToolCallFunction{
				Name:      "Grep",
				Arguments: `{"pattern":"结果文件","path":"./chatgpt_register.py"}`,
			},
		},
	}

	got := normalizeToolCallPaths(messages, toolCalls)
	for _, tc := range got {
		if !strings.Contains(tc.Function.Arguments, `/mnt/d/code/check_alive.py/chatgpt_register.py`) {
			t.Fatalf("expected normalized absolute path in %s args, got %s", tc.Function.Name, tc.Function.Arguments)
		}
	}
}

func TestNormalizeToolCallPaths_DoesNotRewriteDotPath(t *testing.T) {
	messages := []ChatMessage{
		{
			Role:    "system",
			Content: "<system-reminder>\n% pwd\n/mnt/d/code/check_alive.py\n</system-reminder>",
		},
	}
	toolCalls := []ToolCall{
		{
			ID:   "call_grep",
			Type: "function",
			Function: ToolCallFunction{
				Name:      "Grep",
				Arguments: `{"pattern":"foo","path":"."}`,
			},
		},
	}

	got := normalizeToolCallPaths(messages, toolCalls)
	if got[0].Function.Arguments != toolCalls[0].Function.Arguments {
		t.Fatalf("dot-path grep should not be rewritten, got %s", got[0].Function.Arguments)
	}
}

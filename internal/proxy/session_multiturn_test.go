package proxy

import (
	"strings"
	"testing"
	"time"
)

// TestBuildSessionChainFollowUp verifies that the session-based chain follow-up
// builds a concise message with only the latest tool results.
func TestBuildSessionChainFollowUp(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "list files in the current directory"},
		{Role: "assistant", Content: "I'll help with that.", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Bash", Arguments: `{"command":"ls"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "Bash", Content: "file1.txt\nfile2.txt\nREADME.md"},
	}

	compactList := "- Bash(command: str) — Execute shell command\n- Read(file_path: str) — Read a file\n"
	result := buildSessionChainFollowUp(messages, compactList, "/home/user/project")

	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result))
	}
	if result[0].Role != "user" {
		t.Fatalf("expected user role, got %s", result[0].Role)
	}

	content := result[0].Content
	// Should contain tool results
	if !strings.Contains(content, "[Bash]: file1.txt") {
		t.Errorf("expected tool results in follow-up, got: %s", content)
	}
	// Should contain CWD
	if !strings.Contains(content, "Working directory: /home/user/project") {
		t.Errorf("expected CWD in follow-up, got: %s", content)
	}
	// Should contain available functions
	if !strings.Contains(content, "Available functions:") {
		t.Errorf("expected function list in follow-up, got: %s", content)
	}
	// Should contain __done__
	if !strings.Contains(content, "__done__") {
		t.Errorf("expected __done__ in follow-up, got: %s", content)
	}
	// Should NOT contain the original query (context is in the Notion thread)
	if strings.Contains(content, "list files in the current directory") {
		t.Errorf("follow-up should not repeat original query (thread has context)")
	}
}

// TestBuildSessionChainFollowUp_MultipleToolResults verifies handling of parallel tool calls.
func TestBuildSessionChainFollowUp_MultipleToolResults(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "check both files"},
		{Role: "assistant", Content: "I'll read both.", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"a.txt"}`}},
			{ID: "call_2", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"b.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "Read", Content: "content of a"},
		{Role: "tool", ToolCallID: "call_2", Name: "Read", Content: "content of b"},
	}

	result := buildSessionChainFollowUp(messages, "- Read(file_path: str)\n", "")

	content := result[0].Content
	if !strings.Contains(content, "[Read]: content of a") {
		t.Errorf("expected first tool result, got: %s", content)
	}
	if !strings.Contains(content, "[Read]: content of b") {
		t.Errorf("expected second tool result, got: %s", content)
	}
}

// TestBuildSessionChainFollowUp_TruncatesLargeOutput verifies truncation of large tool output.
func TestBuildSessionChainFollowUp_TruncatesLargeOutput(t *testing.T) {
	largeOutput := strings.Repeat("x", 5000)
	messages := []ChatMessage{
		{Role: "user", Content: "read large file"},
		{Role: "assistant", Content: "Reading.", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"big.txt"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "Read", Content: largeOutput},
	}

	result := buildSessionChainFollowUp(messages, "- Read(file_path: str)\n", "")

	content := result[0].Content
	if !strings.Contains(content, "... (truncated)") {
		t.Errorf("expected truncation marker in large output")
	}
	// Should be well under the original 5000 chars
	if len(content) > 4500 {
		t.Errorf("follow-up too large: %d chars (expected < 4500)", len(content))
	}
}

func TestBuildSessionChainFollowUp_ReadOversizeGuard(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "检查为什么 copy 按钮不显示"},
		{Role: "assistant", Content: "I'll inspect the file.", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Read", Arguments: `{"file_path":"src/content.js"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "Read", Content: "File content (31582 tokens) exceeds maximum allowed tokens (10000). Use offset and limit parameters to read specific portions of the file."},
	}

	result := buildSessionChainFollowUp(messages, "- Read(file_path: str, offset?: num, limit?: num)\n- Grep(pattern: str)\n", "")
	content := result[0].Content
	if !strings.Contains(content, "Do NOT repeat the same full-file Read") {
		t.Fatalf("expected oversize read guard in follow-up, got: %s", content)
	}
}

// TestCountNonSystemMessages verifies the new helper function.
func TestCountNonSystemMessages(t *testing.T) {
	tests := []struct {
		name     string
		messages []ChatMessage
		want     int
	}{
		{
			name:     "empty",
			messages: nil,
			want:     0,
		},
		{
			name: "system only",
			messages: []ChatMessage{
				{Role: "system", Content: "you are helpful"},
			},
			want: 0,
		},
		{
			name: "first turn",
			messages: []ChatMessage{
				{Role: "system", Content: "system prompt"},
				{Role: "user", Content: "hello"},
			},
			want: 1,
		},
		{
			name: "chain continuation",
			messages: []ChatMessage{
				{Role: "system", Content: "system prompt"},
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "tool call"},
				{Role: "tool", Content: "result"},
			},
			want: 3,
		},
		{
			name: "multi-round chain",
			messages: []ChatMessage{
				{Role: "system", Content: "system prompt"},
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "tool call 1"},
				{Role: "tool", Content: "result 1"},
				{Role: "assistant", Content: "tool call 2"},
				{Role: "tool", Content: "result 2"},
			},
			want: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countNonSystemMessages(tt.messages)
			if got != tt.want {
				t.Errorf("countNonSystemMessages() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestSessionFingerprintStability verifies that the session fingerprint is stable
// across turns when computed on raw (pre-injection) messages.
func TestSessionFingerprintStability(t *testing.T) {
	systemPrompt := "You are Claude Code, a CLI assistant..."

	// Turn 1: just system + user
	turn1 := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "list files here"},
	}

	// Turn 2: system + user + assistant + tool (chain continuation)
	turn2 := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "list files here"},
		{Role: "assistant", Content: `{"name":"Bash","arguments":{"command":"ls"}}`},
		{Role: "tool", Content: "file1.txt\nfile2.txt"},
	}

	// Turn 3: system + user + assistant + tool + assistant + tool
	turn3 := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "list files here"},
		{Role: "assistant", Content: `{"name":"Bash","arguments":{"command":"ls"}}`},
		{Role: "tool", Content: "file1.txt\nfile2.txt"},
		{Role: "assistant", Content: `{"name":"Read","arguments":{"file_path":"file1.txt"}}`},
		{Role: "tool", Content: "content of file1"},
	}

	fp1 := computeSessionFingerprint(turn1)
	fp2 := computeSessionFingerprint(turn2)
	fp3 := computeSessionFingerprint(turn3)

	if fp1 != fp2 {
		t.Errorf("fingerprint changed between turn 1 and 2: %s vs %s", fp1, fp2)
	}
	if fp2 != fp3 {
		t.Errorf("fingerprint changed between turn 2 and 3: %s vs %s", fp2, fp3)
	}
}

// TestSessionContinuationDetection verifies that rawMsgCount correctly
// distinguishes first turn, continuation, and repeat.
func TestSessionContinuationDetection(t *testing.T) {
	sm := NewSessionManager(5 * time.Minute)

	systemPrompt := "You are Claude Code..."
	fingerprint := "test-fingerprint-123456789012"

	// Turn 1: first turn
	turn1Msgs := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "hello"},
	}
	rawMsgCount1 := countNonSystemMessages(turn1Msgs)
	if rawMsgCount1 != 1 {
		t.Fatalf("expected 1, got %d", rawMsgCount1)
	}

	session := sm.Get(fingerprint)
	if session != nil {
		t.Fatal("expected nil session for first turn")
	}

	// Save session after turn 1
	sm.Set(fingerprint, &Session{
		ThreadID:        "thread-1",
		TurnCount:       1,
		RawMessageCount: rawMsgCount1,
		AccountEmail:    "test@example.com",
		CreatedAt:       time.Now(),
		LastUsedAt:      time.Now(),
	})

	// Turn 2: chain continuation (rawMsgCount increases)
	turn2Msgs := []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "tool call"},
		{Role: "tool", Content: "result"},
	}
	rawMsgCount2 := countNonSystemMessages(turn2Msgs)
	if rawMsgCount2 != 3 {
		t.Fatalf("expected 3, got %d", rawMsgCount2)
	}

	session = sm.Get(fingerprint)
	if session == nil {
		t.Fatal("expected existing session")
	}
	if rawMsgCount2 <= session.RawMessageCount {
		t.Error("expected continuation detection (rawMsgCount > session.RawMessageCount)")
	}

	// Simulate saving after turn 2
	session.TurnCount++
	session.RawMessageCount = rawMsgCount2

	// Retry of turn 2 (same messages): repeat detection
	rawMsgCountRetry := countNonSystemMessages(turn2Msgs)
	if rawMsgCountRetry != session.RawMessageCount {
		t.Errorf("expected repeat detection: rawMsgCount=%d, session.RawMessageCount=%d",
			rawMsgCountRetry, session.RawMessageCount)
	}
}

// TestInjectToolsSessionVsLegacy verifies that injectToolsIntoMessages takes
// the session-based path when a session is provided, and the legacy collapse
// path when no session exists.
func TestInjectToolsSessionVsLegacy(t *testing.T) {
	// Build a chain continuation scenario with >5 tools (triggers useLargeToolSet)
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "Bash", Description: "Execute shell command", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Read", Description: "Read a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Write", Description: "Write a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Edit", Description: "Edit a file", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Glob", Description: "Find files", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Grep", Description: "Search files", Parameters: map[string]interface{}{"type": "object"}}},
	}

	messages := []ChatMessage{
		{Role: "user", Content: "list all go files"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "Bash", Arguments: `{"command":"find . -name '*.go'"}`}},
		}},
		{Role: "tool", ToolCallID: "call_1", Name: "Bash", Content: "main.go\ntools.go\nserver.go"},
	}

	// With session: should use session-based follow-up (shorter, no original query)
	session := &Session{TurnCount: 1, RawMessageCount: 1}
	resultWithSession := injectToolsIntoMessages(messages, tools, "claude-sonnet-4-20250514", session)

	if len(resultWithSession) != 1 {
		t.Fatalf("session path: expected 1 message, got %d", len(resultWithSession))
	}
	if !strings.Contains(resultWithSession[0].Content, "Results from executed function") {
		t.Error("session path: expected 'Results from executed function' prefix")
	}
	if strings.Contains(resultWithSession[0].Content, "I'm writing a unit test") {
		t.Error("session path: should NOT contain unit test framing (context is in thread)")
	}

	// Without session: should use legacy collapse (includes original query + unit test framing)
	resultNoSession := injectToolsIntoMessages(messages, tools, "claude-sonnet-4-20250514", nil)

	if len(resultNoSession) != 1 {
		t.Fatalf("legacy path: expected 1 message, got %d", len(resultNoSession))
	}
	if !strings.Contains(resultNoSession[0].Content, "I'm writing a unit test") {
		t.Error("legacy path: expected 'unit test' framing")
	}
	if !strings.Contains(resultNoSession[0].Content, "list all go files") {
		t.Error("legacy path: expected original query in collapsed message")
	}
}

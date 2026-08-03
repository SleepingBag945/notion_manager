package proxy

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestOffloadLargeSystemMessagesPreservesOriginalInstructionsAndUser(t *testing.T) {
	firstSystem := "  first system line\n中文指令\t" + strings.Repeat("A", 9000) + "\n"
	developer := "developer bytes stay exact\r\n" + strings.Repeat("B", 8000) + "  "
	lastSystem := "last system block"
	user := ChatMessage{
		Role:    "user",
		Content: "你是否能运行 Python 代码？\nDo not alter this user text.",
	}
	assistant := ChatMessage{Role: "assistant", Content: "prior reply"}
	messages := []ChatMessage{
		{Role: "system", Content: firstSystem},
		user,
		{Role: "developer", Content: developer},
		assistant,
		{Role: "system", Content: lastSystem},
	}
	original := cloneChatMessages(messages)
	fingerprintBefore := computeSessionFingerprintWithSalt(messages, "stable-session")

	attachment := buildLargeSystemAttachment(messages, true)
	if attachment == nil {
		t.Fatal("expected large system instructions to be offloaded")
	}
	got := replaceSystemMessagesWithAttachmentBootstrap(messages)

	wantAttachment := firstSystem + "\n\n" + developer + "\n\n" + lastSystem
	if string(attachment.Data) != wantAttachment {
		t.Fatalf("attachment changed original instruction bytes\ngot length:  %d\nwant length: %d", len(attachment.Data), len(wantAttachment))
	}
	if !utf8.Valid(attachment.Data) {
		t.Fatal("attachment must contain valid UTF-8")
	}
	if attachment.FileName != largeSystemAttachmentFileName {
		t.Fatalf("attachment filename = %q, want %q", attachment.FileName, largeSystemAttachmentFileName)
	}
	if attachment.ContentType != "text/plain" {
		t.Fatalf("attachment content type = %q, want text/plain", attachment.ContentType)
	}

	if len(got) != 3 {
		t.Fatalf("offloaded messages count = %d, want 3", len(got))
	}
	if got[0].Role != "system" || got[0].Content != largeSystemAttachmentBootstrap {
		t.Fatalf("bootstrap message = %#v", got[0])
	}
	if !reflect.DeepEqual(got[1], user) || !reflect.DeepEqual(got[2], assistant) {
		t.Fatalf("non-system messages changed: %#v", got)
	}
	if !reflect.DeepEqual(messages, original) {
		t.Fatal("offload mutated the original messages used for fingerprinting")
	}
	if fingerprintAfter := computeSessionFingerprintWithSalt(messages, "stable-session"); fingerprintAfter != fingerprintBefore {
		t.Fatalf("original-message fingerprint changed: %s != %s", fingerprintAfter, fingerprintBefore)
	}
}

func TestOffloadLargeSystemMessagesThresholdAndFirstTurnOnly(t *testing.T) {
	tests := []struct {
		name        string
		systemBytes int
		isFirstTurn bool
		wantOffload bool
	}{
		{name: "below threshold", systemBytes: largeSystemAttachmentThresholdBytes - 1, isFirstTurn: true, wantOffload: false},
		{name: "at threshold", systemBytes: largeSystemAttachmentThresholdBytes, isFirstTurn: true, wantOffload: true},
		{name: "known Codex size", systemBytes: 30198, isFirstTurn: true, wantOffload: true},
		{name: "subsequent turn", systemBytes: 30198, isFirstTurn: false, wantOffload: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := []ChatMessage{
				{Role: "system", Content: strings.Repeat("x", tt.systemBytes)},
				{Role: "user", Content: "user stays in the request body"},
			}
			attachment := buildLargeSystemAttachment(messages, tt.isFirstTurn)
			if (attachment != nil) != tt.wantOffload {
				t.Fatalf("attachment present = %v, want %v", attachment != nil, tt.wantOffload)
			}
			if !reflect.DeepEqual(messages[1], ChatMessage{Role: "user", Content: "user stays in the request body"}) {
				t.Fatalf("attachment collection changed user message: %#v", messages[1])
			}
		})
	}
}

func TestLargeSystemAttachmentKeepsCWDThroughLargeClaudeToolBridge(t *testing.T) {
	system := "<cwd>/tmp/project</cwd>\n" + strings.Repeat("system instruction\n", 1200)
	user := ChatMessage{Role: "user", Content: "inspect the repository"}
	messages := []ChatMessage{
		{Role: "system", Content: system},
		user,
	}
	tools := []Tool{
		{Type: "function", Function: ToolFunction{Name: "Bash", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Read", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Write", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Edit", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Glob", Parameters: map[string]interface{}{"type": "object"}}},
		{Type: "function", Function: ToolFunction{Name: "Grep", Parameters: map[string]interface{}{"type": "object"}}},
	}

	attachment := buildLargeSystemAttachment(messages, true)
	if attachment == nil {
		t.Fatal("expected system instructions to exceed attachment threshold")
	}
	if string(attachment.Data) != system {
		t.Fatal("attachment did not preserve the original system instructions")
	}

	injected := injectToolsIntoMessages(cloneChatMessages(messages), tools, "claude-opus-4-6", nil)
	got := replaceSystemMessagesWithAttachmentBootstrap(injected)
	if len(got) != 2 || got[0].Role != "system" || got[0].Content != largeSystemAttachmentBootstrap {
		t.Fatalf("final messages missing attachment bootstrap: %#v", got)
	}
	if got[1].Role != "user" {
		t.Fatalf("final user framing role = %q, want user", got[1].Role)
	}
	if !strings.Contains(got[1].Content, "Working directory: /tmp/project") {
		t.Fatalf("large-tool framing lost CWD: %q", got[1].Content)
	}
	if !strings.Contains(got[1].Content, `Input: "inspect the repository"`) {
		t.Fatalf("large-tool framing lost user request: %q", got[1].Content)
	}
}

func TestEnsureLargeSystemAttachmentBootstrapKeepsUserUnchanged(t *testing.T) {
	user := ChatMessage{Role: "user", Content: "exact user bytes\n\t中文"}
	got := ensureLargeSystemAttachmentBootstrap([]ChatMessage{user})
	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2", len(got))
	}
	if got[0].Role != "system" || got[0].Content != largeSystemAttachmentBootstrap {
		t.Fatalf("missing bootstrap: %#v", got[0])
	}
	if !reflect.DeepEqual(got[1], user) {
		t.Fatalf("user message changed: %#v", got[1])
	}

	again := ensureLargeSystemAttachmentBootstrap(got)
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("bootstrap was duplicated: %#v", again)
	}
}

func TestBuildAttachmentUploadPlanSharesThread(t *testing.T) {
	plan := buildAttachmentUploadPlan("thread-shared", 3, true)
	if len(plan) != 3 {
		t.Fatalf("plan length = %d, want 3", len(plan))
	}
	for i, target := range plan {
		if target.ThreadID != "thread-shared" {
			t.Fatalf("plan[%d] thread = %q, want thread-shared", i, target.ThreadID)
		}
		wantCreateThread := i == 0
		if target.CreateThread != wantCreateThread {
			t.Fatalf("plan[%d] createThread = %v, want %v", i, target.CreateThread, wantCreateThread)
		}
	}

	existingThreadPlan := buildAttachmentUploadPlan("thread-shared", 2, false)
	for i, target := range existingThreadPlan {
		if target.ThreadID != "thread-shared" || target.CreateThread {
			t.Fatalf("existing plan[%d] = %#v, want shared thread without creation", i, target)
		}
	}
}

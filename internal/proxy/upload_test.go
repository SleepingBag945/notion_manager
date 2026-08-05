package proxy

import (
	"encoding/json"
	"regexp"
	"testing"
)

func TestBuildAttachmentTranscriptMatchesNativeProtocol(t *testing.T) {
	metadata := &AttachmentMetadata{
		TruncatedContent: "",
		FileSizeBytes:    68,
		WasTruncated:     false,
		EstimatedTokens:  map[string]interface{}{"openai": 0, "anthropic": 0},
		AttachmentSource: "user_upload",
		AiTraceId:        "trace-id",
	}
	uploaded := &UploadedAttachment{
		AttachmentURL: "attachment:6d3e95c8-10c8-4d70-b005-10d9938cf19d:42e90274-7c53-4f5f-b7f5-4bb523f243c6.png",
		FileName:      "original.png",
		ContentType:   "image/png",
		FileSizeBytes: 68,
		Metadata:      metadata,
	}

	got := BuildAttachmentTranscript(uploaded)
	if matched, err := regexp.MatchString(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, got.ID); err != nil || !matched {
		t.Fatalf("attachment id = %q, want independent UUIDv4 (match=%v, err=%v)", got.ID, matched, err)
	}
	if metadata.Guardrail != nil {
		t.Fatal("BuildAttachmentTranscript mutated uploaded metadata")
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	want := `{"type":"attachment","id":"` + got.ID + `","fileUrl":"attachment:6d3e95c8-10c8-4d70-b005-10d9938cf19d:42e90274-7c53-4f5f-b7f5-4bb523f243c6.png","fileName":"original.png","contentType":"image/png","metadata":{"truncatedContent":"","fileSizeBytes":68,"wasTruncated":false,"estimatedTokens":{"anthropic":0,"openai":0},"attachmentSource":"user_upload","aiTraceId":"trace-id","guardrail":{"attachmentRisk":"skipped"}}}`
	if string(body) != want {
		t.Fatalf("attachment JSON mismatch\n got: %s\nwant: %s", body, want)
	}
}

func TestBuildAttachmentTranscriptPreservesProcessedGuardrail(t *testing.T) {
	uploaded := &UploadedAttachment{
		AttachmentURL: "attachment:redacted",
		FileName:      "report.pdf",
		ContentType:   "application/pdf",
		Metadata: &AttachmentMetadata{
			Guardrail: &AttachmentGuardrail{
				AttachmentRisk: "safe",
				InferenceId:    "inference-id",
			},
		},
	}

	got := BuildAttachmentTranscript(uploaded)
	if got.Metadata.Guardrail.AttachmentRisk != "safe" || got.Metadata.Guardrail.InferenceId != "inference-id" {
		t.Fatalf("processed guardrail = %#v, want preserved", got.Metadata.Guardrail)
	}
}

func TestBuildAttachmentUploadPlanSharesThread(t *testing.T) {
	firstTurn := buildAttachmentUploadPlan("thread-shared", 3, true)
	if len(firstTurn) != 3 {
		t.Fatalf("first-turn plan length = %d, want 3", len(firstTurn))
	}
	for i, target := range firstTurn {
		if target.ThreadID != "thread-shared" {
			t.Fatalf("first-turn plan[%d] thread = %q, want thread-shared", i, target.ThreadID)
		}
		wantCreateThread := i == 0
		if target.CreateThread != wantCreateThread {
			t.Fatalf("first-turn plan[%d] createThread = %v, want %v", i, target.CreateThread, wantCreateThread)
		}
	}

	subsequentTurn := buildAttachmentUploadPlan("thread-shared", 2, false)
	for i, target := range subsequentTurn {
		if target.ThreadID != "thread-shared" || target.CreateThread {
			t.Fatalf("subsequent-turn plan[%d] = %#v, want existing shared thread", i, target)
		}
	}
	if got := buildAttachmentUploadPlan("thread-shared", 0, true); got != nil {
		t.Fatalf("empty attachment plan = %#v, want nil", got)
	}
}

func TestUploadedAttachmentThreadIDRequiresOneSharedThread(t *testing.T) {
	attachments := []UploadedAttachment{
		{SessionID: "thread-shared"},
		{SessionID: "thread-shared"},
		{SessionID: "thread-shared"},
	}
	got, err := uploadedAttachmentThreadID(attachments)
	if err != nil || got != "thread-shared" {
		t.Fatalf("uploadedAttachmentThreadID() = %q, %v; want thread-shared, nil", got, err)
	}

	attachments[1].SessionID = "thread-other"
	if _, err := uploadedAttachmentThreadID(attachments); err == nil {
		t.Fatal("uploadedAttachmentThreadID() accepted attachments from different threads")
	}
	if _, err := uploadedAttachmentThreadID([]UploadedAttachment{{}}); err == nil {
		t.Fatal("uploadedAttachmentThreadID() accepted an attachment without a thread ID")
	}
}

func TestResolveFirstTurnInferenceThreadReusesUploadThread(t *testing.T) {
	session := &Session{ThreadID: "thread-shared"}
	threadID, createThread, err := resolveFirstTurnInferenceThread(session, "thread-shared")
	if err != nil || threadID != session.ThreadID || createThread {
		t.Fatalf("resolved first-turn attachment thread = %q, create=%v, err=%v", threadID, createThread, err)
	}
	if inferenceCreatedSource(true) != "workflows" {
		t.Fatalf("attachment createdSource = %q, want workflows", inferenceCreatedSource(true))
	}

	threadID, createThread, err = resolveFirstTurnInferenceThread(session, "")
	if err != nil || threadID != session.ThreadID || !createThread {
		t.Fatalf("resolved attachment-free session thread = %q, create=%v, err=%v", threadID, createThread, err)
	}
	if inferenceCreatedSource(false) != "ai_module" {
		t.Fatalf("attachment-free createdSource = %q, want ai_module", inferenceCreatedSource(false))
	}

	if _, _, err := resolveFirstTurnInferenceThread(session, "thread-other"); err == nil {
		t.Fatal("resolveFirstTurnInferenceThread() accepted a mismatched upload thread")
	}
}

func TestBuildPartialTranscriptIncludesAttachmentBeforeUser(t *testing.T) {
	previous := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = previous }()

	session := &Session{
		ThreadID:         "thread-existing",
		TurnCount:        1,
		ConfigID:         "config-id",
		ContextID:        "context-id",
		ContextPageID:    "context-page-id",
		OriginalDatetime: "2026-08-05T01:02:03Z",
		UpdatedConfigIDs: []string{"updated-id"},
	}
	attachments := []UploadedAttachment{{
		AttachmentURL: "attachment:file-id:stored-name.png",
		FileName:      "original-name.png",
		ContentType:   "image/png",
		FileSizeBytes: 68,
		SessionID:     session.ThreadID,
	}}

	transcript := buildPartialTranscript(
		&Account{},
		"what is in this image?",
		"strawberry-whoopiepie",
		"high",
		false,
		false,
		nil,
		false,
		attachments,
		session,
	)
	if len(transcript) != 6 {
		t.Fatalf("partial transcript length = %d, want config + 2 contexts + updated-config + attachment + user", len(transcript))
	}
	config, ok := transcript[0].(ResearcherTranscriptMsg)
	if !ok {
		t.Fatalf("partial config = %#v", transcript[0])
	}
	configValue, ok := config.Value.(map[string]interface{})
	if !ok || configValue["enableCsvAttachmentSupport"] != true {
		t.Fatalf("partial attachment config = %#v, want attachment support enabled", config.Value)
	}
	attachment, ok := transcript[4].(AttachmentTranscriptMsg)
	if !ok || attachment.FileName != "original-name.png" || attachment.ID == "" || attachment.Metadata.Guardrail == nil {
		t.Fatalf("partial attachment step = %#v", transcript[4])
	}
	user, ok := transcript[5].(ResearcherTranscriptMsg)
	if !ok || user.Type != "user" {
		t.Fatalf("partial final step = %#v, want user", transcript[5])
	}
}

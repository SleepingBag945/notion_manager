package proxy

import (
	"encoding/json"
	"sync"
	"time"
)

// ========== Account ==========

type Account struct {
	mu            sync.RWMutex
	TokenV2       string       `json:"token_v2"`
	UserID        string       `json:"user_id"`
	UserName      string       `json:"user_name"`
	UserEmail     string       `json:"user_email"`
	SpaceID       string       `json:"space_id"`
	SpaceName     string       `json:"space_name"`
	AccountID     string       `json:"account_id,omitempty"` // SHA-256(user_id + "\0" + space_id)
	SpaceViewID   string       `json:"space_view_id"`
	PlanType      string       `json:"plan_type"`
	Timezone      string       `json:"timezone"`
	ClientVersion string       `json:"client_version"`
	BrowserID     string       `json:"browser_id,omitempty"`
	DeviceID      string       `json:"device_id,omitempty"`
	FullCookie    string       `json:"full_cookie,omitempty"`
	Models        []ModelEntry `json:"available_models"`
	// RegisteredVia tags which Provider.ID() created this account (e.g.
	// "microsoft"). Empty for accounts onboarded before the provider
	// registry existed; the dashboard treats those as legacy Microsoft.
	RegisteredVia string `json:"registered_via,omitempty"`
	// Runtime-only fields (not serialized)
	QuotaExhaustedAt     *time.Time `json:"-"`
	QuotaInfo            *QuotaInfo `json:"-"`
	QuotaCheckedAt       *time.Time `json:"-"`
	PermanentlyExhausted bool       `json:"-"`
	// Workspace probe state. SpaceCount is the number of `space_views`
	// returned by /api/v3/loadUserContent for this account's user_root.
	// 0 with WorkspaceCheckedAt != nil means the Notion onboarding
	// never completed (or the workspace was deleted) — the SPA renders
	// a perpetual skeleton on /ai for such accounts. The pool refuses to
	// route traffic to these accounts and the dashboard surfaces them as
	// "无工作区". WorkspaceCheckedAt == nil means the account hasn't been
	// probed yet; we treat it as unknown / usable until the next refresh.
	SpaceCount         int        `json:"-"`
	WorkspaceCheckedAt *time.Time `json:"-"`
}

// QuotaInfo holds AI usage quota information from V1 + V2 APIs
type QuotaInfo struct {
	IsEligible    bool  `json:"isEligible"`
	SpaceUsage    int   `json:"spaceUsage"`
	SpaceLimit    int   `json:"spaceLimit"`
	UserUsage     int   `json:"userUsage"`
	UserLimit     int   `json:"userLimit"`
	LastUsageAtMs int64 `json:"lastSpaceUsageAtMs"`
	// Research mode (from V1 API: getAIUsageEligibility)
	ResearchModeUsage int `json:"researchModeUsage"` // usage when "Share data to improve AI" is enabled
	// Premium credits (from V2 API: getAIUsageEligibilityV2)
	HasPremium     bool `json:"hasPremium"`
	PremiumBalance int  `json:"premiumBalance"` // remaining premium credits
	PremiumUsage   int  `json:"premiumUsage"`   // used premium credits (monthlyAllocated)
	PremiumLimit   int  `json:"premiumLimit"`   // total premium credit limit
}

// quotaV1Response is the raw response from getAIUsageEligibility
type quotaV1Response struct {
	IsEligible         bool  `json:"isEligible"`
	ResearchModeUsage  int   `json:"researchModeUsage"`
	SpaceUsage         int   `json:"spaceUsage"`
	SpaceLimit         int   `json:"spaceLimit"`
	UserUsage          int   `json:"userUsage"`
	UserLimit          int   `json:"userLimit"`
	LastSpaceUsageAtMs int64 `json:"lastSpaceUsageAtMs"`
}

// quotaV2Response is the raw response from getAIUsageEligibilityV2 (premium credits)
type quotaV2Response struct {
	BasicCredits struct {
		SpaceUsage         int   `json:"spaceUsage"`
		SpaceLimit         int   `json:"spaceLimit"`
		UserUsage          int   `json:"userUsage"`
		UserLimit          int   `json:"userLimit"`
		LastSpaceUsageAtMs int64 `json:"lastSpaceUsageAtMs"`
	} `json:"basicCredits"`
	PremiumCredits struct {
		TotalCreditBalance int `json:"totalCreditBalance"`
		CreditsInOverage   int `json:"creditsInOverage"`
		PerSource          struct {
			MonthlyAllocated struct {
				UsageTotal int `json:"usageTotal"`
				Limit      int `json:"limit"`
			} `json:"monthlyAllocated"`
			MonthlyCommitted struct {
				UsageTotal int `json:"usageTotal"`
				Limit      int `json:"limit"`
			} `json:"monthlyCommitted"`
			YearlyElastic struct {
				UsageTotal int `json:"usageTotal"`
				Limit      int `json:"limit"`
			} `json:"yearlyElastic"`
		} `json:"perSource"`
	} `json:"premiumCredits"`
}

type ModelEntry struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// ========== Shared Internal Types ==========

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type ToolCall struct {
	Index_   int              `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ========== Notion API Types ==========

type NotionInferenceRequest struct {
	TraceID                                string               `json:"traceId"`
	SpaceID                                string               `json:"spaceId"`
	ThreadID                               string               `json:"threadId,omitempty"`
	Transcript                             []interface{}        `json:"transcript"`
	CreateThread                           bool                 `json:"createThread"`
	GenerateTitle                          bool                 `json:"generateTitle"`
	SaveAllThreadOperations                bool                 `json:"saveAllThreadOperations"`
	SetUnreadState                         bool                 `json:"setUnreadState"`
	ThreadType                             string               `json:"threadType"`
	CreatedSource                          string               `json:"createdSource,omitempty"`
	AsPatchResponse                        bool                 `json:"asPatchResponse"`
	PatchResponseVersion                   int                  `json:"patchResponseVersion,omitempty"`
	IsPartialTranscript                    bool                 `json:"isPartialTranscript"`
	IsUserInAnySalesAssistedSpace          *bool                `json:"isUserInAnySalesAssistedSpace,omitempty"`
	IsSpaceSalesAssisted                   *bool                `json:"isSpaceSalesAssisted,omitempty"`
	SupportsCustomAgentNudgeTranscriptStep *bool                `json:"supportsCustomAgentNudgeTranscriptStep,omitempty"`
	ThreadParentPointer                    *ThreadParentPointer `json:"threadParentPointer,omitempty"`
	DebugOverrides                         DebugOverrides       `json:"debugOverrides"`
}

// ThreadParentPointer identifies the parent of a thread (used only on first turn)
type ThreadParentPointer struct {
	Table   string `json:"table"`
	ID      string `json:"id"`
	SpaceID string `json:"spaceId"`
}

// UpdatedConfigMsg is a placeholder transcript entry for previous assistant turns.
// Notion server uses these to locate stored assistant responses in the thread history.
type UpdatedConfigMsg struct {
	ID   string `json:"id"`
	Type string `json:"type"` // always "updated-config"
}

type TranscriptMsg struct {
	Type  string      `json:"type"`
	Value interface{} `json:"value"`
}

// ResearcherTranscriptMsg extends TranscriptMsg with fields required by researcher mode
type ResearcherTranscriptMsg struct {
	ID        string      `json:"id"`
	Type      string      `json:"type"`
	Value     interface{} `json:"value"`
	UserID    string      `json:"userId,omitempty"`
	CreatedAt string      `json:"createdAt,omitempty"`
}

type DebugOverrides struct {
	EmitAgentSearchExtractedResults bool      `json:"emitAgentSearchExtractedResults,omitempty"`
	CachedInferences                *struct{} `json:"cachedInferences,omitempty"`
	AnnotationInferences            *struct{} `json:"annotationInferences,omitempty"`
	EmitInferences                  *bool     `json:"emitInferences,omitempty"`
}

type NDJSONEvent struct {
	Type string `json:"type"`
}

type AgentInferenceEvent struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Value        []AgentValueEntry `json:"value"`
	FinishedAt   *int64            `json:"finishedAt"`
	InputTokens  *int              `json:"inputTokens"`
	OutputTokens *int              `json:"outputTokens"`
	Model        *string           `json:"model"`
}

type AgentValueEntry struct {
	Type      string `json:"type"`
	Content   string `json:"content"`
	Signature string `json:"signature,omitempty"` // thinking blocks
	// tool_use fields (Anthropic Claude native format)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ThinkingBlock represents a captured thinking entry from Notion AI
type ThinkingBlock struct {
	Content   string
	Signature string
}

// ThinkingDeltaCallback is called for each incremental thinking/process delta.
// delta is the new text to append; done=true signals the thinking phase is complete.
// signature is non-empty only when done=true if a real Anthropic signature was captured.
type ThinkingDeltaCallback func(delta string, done bool, signature string)

// CitationCandidate stores known search result metadata that can be used to
// recover citations when Notion rewrites them to internal view:// URIs.
type CitationCandidate struct {
	URL   string
	Title string
	Text  string
}

// CallOptions holds optional parameters for CallInference
type CallOptions struct {
	NativeToolUses        *[]AgentValueEntry
	ThinkingBlocks        *[]ThinkingBlock
	EnableWebSearch       bool                  // force useWebSearch=true in Notion config
	EnableWorkspaceSearch *bool                 // override workspace search (nil = use config default)
	UseReadOnlyMode       bool                  // ASK mode — Notion's workflow useReadOnlyMode=true (model answers but skips edits)
	ReasoningEffort       string                // Notion workflow reasoningEffort (for example low, medium, or high)
	Attachments           []UploadedAttachment  // uploaded file attachments to include in transcript
	IsResearcher          bool                  // researcher mode (deep research)
	ThinkingCallback      ThinkingDeltaCallback // incremental thinking/process callback for streaming
	KnownCitationURLs     *[]string             // known web result URLs for repairing truncated citations
	KnownCitationDocs     *[]CitationCandidate  // known search result metadata for context-based citation recovery
	KnownToolCallURLs     *map[string][]string  // tool call id -> ordered web result URLs for resolving tool citations
	Session               *Session              // multi-turn session (nil = first turn)
	RequestID             string                // top-level API request ID for log correlation
}

// ========== Researcher Mode NDJSON Event Types ==========

// ResearcherNextStepsEvent represents a researcher-next-steps NDJSON event.
// Contains both planning steps and thinking blocks (output/rawOutput).
type ResearcherNextStepsEvent struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Done  bool   `json:"done"`
	Value struct {
		NextSteps []struct {
			Agent       string `json:"agent"`
			Question    string `json:"question"`
			Key         string `json:"key"`
			SearchType  string `json:"searchType"`
			DisplayName string `json:"displayName"`
		} `json:"nextSteps"`
		UserQuestion string `json:"userQuestion"`
	} `json:"value"`
	// Output contains condensed thinking for UI display
	Output []ResearcherThinkingEntry `json:"output,omitempty"`
	// RawOutput contains full Extended Thinking with signature from the model
	RawOutput []ResearcherRawOutputEntry `json:"rawOutput,omitempty"`
}

// ResearcherThinkingEntry is a condensed thinking entry in researcher-next-steps output
type ResearcherThinkingEntry struct {
	Type    string `json:"type"`    // "thinking"
	Content string `json:"content"` // condensed thinking text
}

// ResearcherRawOutputEntry is a full model output entry in researcher-next-steps rawOutput
type ResearcherRawOutputEntry struct {
	Type            string `json:"type"`                      // "thinking" or "text"
	Content         string `json:"content"`                   // full thinking or JSON decision
	Signature       string `json:"signature,omitempty"`       // Anthropic Extended Thinking signature
	ModelProvider   string `json:"modelProvider,omitempty"`   // "anthropic"
	NotionModelName string `json:"notionModelName,omitempty"` // "anthropic-sonnet-4"
}

// ResearcherAgentEvent represents a researcher-agent NDJSON event
type ResearcherAgentEvent struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Value struct {
		Key              string          `json:"key"`
		Agent            string          `json:"agent"`
		Input            json.RawMessage `json:"input,omitempty"`
		TimestampStartMs int64           `json:"timestampStartMs"`
		TimestampEndMs   int64           `json:"timestampEndMs,omitempty"`
		Status           string          `json:"status"`
		Output           json.RawMessage `json:"output,omitempty"`
	} `json:"value"`
}

// ResearcherReportEvent represents a researcher-report NDJSON event
// Value is a cumulative text string, not an object
type ResearcherReportEvent struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

type PatchEvent struct {
	Type string    `json:"type"`
	V    []PatchOp `json:"v"`
}

type PatchOp struct {
	O string          `json:"o"`
	P string          `json:"p"`
	V json.RawMessage `json:"v"`
}

// ========== File Attachment Types ==========

// FileAttachment represents a file extracted from Anthropic content blocks (image/document)
// that needs to be uploaded to Notion before inference.
type FileAttachment struct {
	Data        []byte // raw file bytes (decoded from base64)
	FileName    string // original or generated filename
	ContentType string // MIME type (image/png, application/pdf, text/csv, etc.)
}

// AttachmentTranscriptMsg is the transcript entry for uploaded files.
// It uses a flat structure (not Type+Value like other transcript entries)
// because Notion's attachment format differs from config/context/user entries.
type AttachmentTranscriptMsg struct {
	Type        string             `json:"type"`        // "attachment"
	ID          string             `json:"id"`          // independent transcript record ID
	FileUrl     string             `json:"fileUrl"`     // attachment:UUID:filename
	FileName    string             `json:"fileName"`    // original filename
	ContentType string             `json:"contentType"` // MIME type
	Metadata    AttachmentMetadata `json:"metadata"`
}

type AttachmentMetadata struct {
	NumRows          int                    `json:"numRows,omitempty"`
	NumFields        int                    `json:"numFields,omitempty"`
	TruncatedContent string                 `json:"truncatedContent"`
	FileSizeBytes    int64                  `json:"fileSizeBytes"`
	WasTruncated     bool                   `json:"wasTruncated"`
	EstimatedTokens  map[string]interface{} `json:"estimatedTokens"`
	AttachmentSource string                 `json:"attachmentSource"`
	AiTraceId        string                 `json:"aiTraceId,omitempty"`
	Guardrail        *AttachmentGuardrail   `json:"guardrail,omitempty"`
	Width            int                    `json:"width,omitempty"`
	Height           int                    `json:"height,omitempty"`
	ContentType      string                 `json:"contentType,omitempty"`
	Moderation       map[string]interface{} `json:"moderation,omitempty"`
}

type AttachmentGuardrail struct {
	AttachmentRisk string `json:"attachmentRisk"`
	InferenceId    string `json:"inferenceId,omitempty"`
}

// NotionUploadURLRequest is the request body for getUploadFileUrlForAssistantChatTranscriptUpload
type NotionUploadURLRequest struct {
	Name         string                     `json:"name"`
	ContentType  string                     `json:"contentType"`
	ContentLen   int                        `json:"contentLength"`
	CreateThread bool                       `json:"createThread"`
	Pointer      NotionAssistantChatPointer `json:"assistantChatTranscriptSessionPointer"`
}

type NotionAssistantChatPointer struct {
	SpaceID string `json:"spaceId"`
	Table   string `json:"table"`
	ID      string `json:"id"`
}

// NotionUploadURLResponse is the response from getUploadFileUrlForAssistantChatTranscriptUpload
type NotionUploadURLResponse struct {
	URL                 string            `json:"url"`
	SignedGetURL        string            `json:"signedGetUrl"`
	SignedUploadPostURL string            `json:"signedUploadPostUrl"`
	Fields              map[string]string `json:"fields"`
}

// NotionEnqueueTaskRequest wraps the enqueueTask call for processAgentAttachment
type NotionEnqueueTaskRequest struct {
	Task NotionTask `json:"task"`
}

type NotionTask struct {
	EventName   string            `json:"eventName"`
	Request     NotionTaskRequest `json:"request"`
	CellRouting NotionCellRouting `json:"cellRouting"`
}

type NotionTaskRequest struct {
	URL              string                     `json:"url"`
	SpaceID          string                     `json:"spaceId"`
	AISessionPointer NotionAssistantChatPointer `json:"aiSessionPointer"`
	Source           string                     `json:"source"`
	ClientVersion    string                     `json:"clientVersion"`
}

type NotionCellRouting struct {
	SpaceIDs []string `json:"spaceIds"`
}

// NotionGetTasksRequest is the request body for getTasks
type NotionGetTasksRequest struct {
	TaskIDs []string `json:"taskIds"`
}

// NotionGetTasksResponse represents the getTasks response
type NotionGetTasksResponse struct {
	Results []NotionTaskResult `json:"results"`
}

type NotionTaskResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// UploadedAttachment holds the result of a successful Notion file upload
type UploadedAttachment struct {
	AttachmentURL string // attachment:UUID:filename
	FileName      string
	ContentType   string
	FileSizeBytes int64
	SessionID     string              // thread/session ID used during upload
	Metadata      *AttachmentMetadata // enriched metadata from processAgentAttachment task (nil if not available)
}

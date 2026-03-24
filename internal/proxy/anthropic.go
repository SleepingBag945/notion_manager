package proxy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var ErrToolBridgeNoTool = errors.New("tool bridge produced no usable tool action")

// citationReplacer is a streaming state machine that replaces Notion's
// [^{{URL}}] and [^URL] citation markers with numbered references [N]
// as text deltas arrive. It buffers only when inside a potential citation.
type citationReplacer struct {
	urlToNum     map[string]int
	urls         []string
	buf          strings.Builder
	state        int // 0=normal, 1=saw [, 2=inside [^...
	knownURLs    *[]string
	knownDocs    *[]CitationCandidate
	toolCallURLs *map[string][]string
	context      string
}

func newCitationReplacer(knownURLs *[]string, knownDocs *[]CitationCandidate, toolCallURLs *map[string][]string) *citationReplacer {
	return &citationReplacer{
		urlToNum:     make(map[string]int),
		knownURLs:    knownURLs,
		knownDocs:    knownDocs,
		toolCallURLs: toolCallURLs,
	}
}

func trimCitationContext(text string) string {
	const maxRunes = 320
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[len(runes)-maxRunes:])
}

// Process takes a delta and returns text with citations replaced by [N].
// Partial citations are buffered across calls.
func (cr *citationReplacer) Process(delta string) string {
	var out strings.Builder
	for _, ch := range delta {
		switch cr.state {
		case 0: // normal text
			if ch == '[' {
				cr.state = 1
				cr.buf.Reset()
				cr.buf.WriteRune(ch)
			} else {
				out.WriteRune(ch)
			}
		case 1: // saw [, expecting ^
			if ch == '^' {
				cr.state = 2
				cr.buf.WriteRune(ch)
			} else {
				// Not a citation — flush buffered [ and continue
				out.WriteString(cr.buf.String())
				cr.buf.Reset()
				cr.state = 0
				if ch == '[' {
					cr.state = 1
					cr.buf.WriteRune(ch)
				} else {
					out.WriteRune(ch)
				}
			}
		case 2: // inside [^..., waiting for ]
			cr.buf.WriteRune(ch)
			if ch == ']' {
				// Complete citation — replace with [N]
				raw := cr.buf.String()
				matches := citationRe.FindStringSubmatch(raw)
				if len(matches) >= 2 {
					context := trimCitationContext(cr.context + out.String())
					rawURL, ok := normalizeCitationTargetWithContext(
						matches[1],
						cr.toolCallURLCandidates(),
						cr.knownURLCandidates(),
						cr.knownDocCandidates(),
						context,
					)
					if !ok {
						// Drop unresolved/non-URL citation tokens (e.g. toolu_* ids).
						cr.buf.Reset()
						cr.state = 0
						continue
					}
					num, exists := cr.urlToNum[rawURL]
					if !exists {
						num = len(cr.urls) + 1
						cr.urlToNum[rawURL] = num
						cr.urls = append(cr.urls, rawURL)
					}
					out.WriteString(fmt.Sprintf(" [%d]", num))
				} else {
					out.WriteString(raw) // not a valid citation, flush raw
				}
				cr.buf.Reset()
				cr.state = 0
			} else if cr.buf.Len() > 2000 {
				// Too long — incomplete citation, drop the markup
				cr.buf.Reset()
				cr.state = 0
			}
		}
	}
	produced := out.String()
	if produced != "" {
		cr.context = trimCitationContext(cr.context + produced)
	}
	return produced
}

// Flush returns any remaining buffered content.
// Incomplete citations (state 2: inside [^...) are dropped rather than
// flushing raw markup like [^{{URL... to the user.
func (cr *citationReplacer) Flush() string {
	var s string
	if cr.state < 2 {
		// State 0 or 1: flush buffered [ if any
		s = cr.buf.String()
	}
	// State 2: inside incomplete citation — drop it
	cr.buf.Reset()
	cr.state = 0
	if s != "" {
		cr.context = trimCitationContext(cr.context + s)
	}
	return s
}

// URLs returns the collected unique citation URLs in order of first appearance.
func (cr *citationReplacer) URLs() []string { return cr.urls }

func (cr *citationReplacer) knownURLCandidates() []string {
	if cr == nil || cr.knownURLs == nil {
		return nil
	}
	return *cr.knownURLs
}

func (cr *citationReplacer) knownDocCandidates() []CitationCandidate {
	if cr == nil || cr.knownDocs == nil {
		return nil
	}
	return *cr.knownDocs
}

func (cr *citationReplacer) toolCallURLCandidates() map[string][]string {
	if cr == nil || cr.toolCallURLs == nil {
		return nil
	}
	return *cr.toolCallURLs
}

func formatCitationSources(urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	var sources strings.Builder
	sources.WriteString("\n---\nSources:\n")
	for i, u := range urls {
		sources.WriteString(fmt.Sprintf("[%d] %s\n", i+1, u))
	}
	return sources.String()
}

func renderAnthropicCitationText(rawText string, knownURLs []string, knownDocs []CitationCandidate, toolCallURLs map[string][]string) string {
	if rawText == "" {
		return ""
	}
	cr := newCitationReplacer(&knownURLs, &knownDocs, &toolCallURLs)
	var rendered strings.Builder
	rendered.WriteString(cr.Process(rawText))
	rendered.WriteString(cr.Flush())
	rendered.WriteString(formatCitationSources(cr.URLs()))
	return rendered.String()
}

// streamWebSearch streams web search results directly as SSE events.
// It replaces inline citations [^{{URL}}] with [N] in real-time using
// a buffered state machine, emits thinking blocks as they arrive,
// then appends a Sources section.
func streamWebSearch(w http.ResponseWriter, flusher http.Flusher, acc *Account, query string, model string, requestID string, blockIndex *int, hasThinking bool) (*UsageInfo, error) {
	var finalUsage *UsageInfo
	var thinkingBlocks []ThinkingBlock
	var streamedText strings.Builder
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)
	cr := newCitationReplacer(&knownCitationURLs, &knownCitationDocs, &knownToolCallURLs)
	textBlockStarted := false
	thinkingEmitted := 0

	messages := []ChatMessage{
		{Role: "user", Content: query},
	}
	callOpts := CallOptions{
		EnableWebSearch:   true,
		ThinkingBlocks:    &thinkingBlocks,
		KnownCitationURLs: &knownCitationURLs,
		KnownCitationDocs: &knownCitationDocs,
		KnownToolCallURLs: &knownToolCallURLs,
		RequestID:         requestID,
	}

	// emitPendingThinking emits any thinking blocks that have been collected
	// since the last check. Called before first text delta to ensure thinking
	// appears before text in the SSE stream.
	emitPendingThinking := func() {
		if !hasThinking {
			return
		}
		for thinkingEmitted < len(thinkingBlocks) {
			tb := thinkingBlocks[thinkingEmitted]
			sig := tb.Signature
			if sig == "" {
				sig = generateFakeSignature()
			}
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         *blockIndex,
				"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": *blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": tb.Content},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": *blockIndex,
				"delta": map[string]interface{}{"type": "signature_delta", "signature": sig},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": *blockIndex,
			})
			*blockIndex++
			thinkingEmitted++
			log.Printf("[search-thinking] emitted thinking block %d (%d chars)", thinkingEmitted, len(tb.Content))
		}
	}

	// emitTextDelta starts text block lazily and sends text delta
	emitTextDelta := func(text string) {
		if text == "" {
			return
		}
		streamedText.WriteString(text)
		if !textBlockStarted {
			// Emit any pending thinking blocks before starting text
			emitPendingThinking()
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         *blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			textBlockStarted = true
		}
		sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": *blockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": text},
		})
	}

	err := CallInference(acc, messages, model, false, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			// Check for new thinking blocks on each text delta
			emitPendingThinking()
			emitTextDelta(cr.Process(delta))
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	// Emit any remaining thinking blocks (e.g., if no text was produced)
	emitPendingThinking()

	// Flush any remaining buffered citation content
	emitTextDelta(cr.Flush())

	// Close text block if started
	if textBlockStarted {
		sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": *blockIndex,
		})
		*blockIndex++
	}

	// Append Sources section with numbered URLs
	if err == nil {
		urls := cr.URLs()
		if sourcesText := formatCitationSources(urls); sourcesText != "" {
			streamedText.WriteString(sourcesText)
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         *blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": *blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": sourcesText},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": *blockIndex,
			})
			*blockIndex++
		}
	}

	var contentBlocks []AnthropicContentBlock
	if hasThinking {
		for _, tb := range thinkingBlocks {
			sig := tb.Signature
			if sig == "" {
				sig = generateFakeSignature()
			}
			contentBlocks = append(contentBlocks, AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  tb.Content,
				Signature: sig,
			})
		}
	}
	if streamedText.Len() > 0 {
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type: "text",
			Text: streamedText.String(),
		})
	}
	if len(contentBlocks) > 0 {
		LogAPIOutputJSON(requestID, "anthropic stream web-search summary", map[string]interface{}{
			"model":   model,
			"query":   query,
			"content": contentBlocks,
		})
	}

	return finalUsage, err
}

// ========== Anthropic Messages API Types ==========

// AnthropicRequest represents an Anthropic Messages API request
type AnthropicRequest struct {
	Model             string                 `json:"model"`
	MaxTokens         int                    `json:"max_tokens"`
	System            interface{}            `json:"system,omitempty"`
	Messages          []AnthropicMessage     `json:"messages"`
	Stream            bool                   `json:"stream"`
	Temperature       *float64               `json:"temperature,omitempty"`
	TopP              *float64               `json:"top_p,omitempty"`
	TopK              *int                   `json:"top_k,omitempty"`
	StopSequences     []string               `json:"stop_sequences,omitempty"`
	Tools             []AnthropicTool        `json:"tools,omitempty"`
	ToolChoice        interface{}            `json:"tool_choice,omitempty"`
	Thinking          interface{}            `json:"thinking,omitempty"`
	OutputConfig      *AnthropicOutputConfig `json:"output_config,omitempty"`
	Metadata          map[string]interface{} `json:"metadata,omitempty"`
	ContextManagement interface{}            `json:"context_management,omitempty"`
}

// AnthropicMessage represents a message in Anthropic format
type AnthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []ContentBlock
}

// AnthropicContentBlock represents a content block in Anthropic format
type AnthropicContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      interface{}     `json:"content,omitempty"`
	CacheControl interface{}     `json:"cache_control,omitempty"`
}

// AnthropicTool represents a tool definition in Anthropic format
type AnthropicTool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema interface{} `json:"input_schema,omitempty"`
}

type AnthropicOutputConfig struct {
	Format *AnthropicOutputFormat `json:"format,omitempty"`
	Effort string                 `json:"effort,omitempty"`
}

type AnthropicOutputFormat struct {
	Type   string      `json:"type,omitempty"`
	Schema interface{} `json:"schema,omitempty"`
}

// AnthropicResponse represents a non-streaming Anthropic Messages API response
type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        *AnthropicUsage         `json:"usage"`
}

// AnthropicUsage represents token usage in Anthropic format
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

var structuredOutputLeadingTagRegex = regexp.MustCompile(`(?s)^\s*(?:<[A-Za-z][^>\n]*/>\s*)+`)

func isJSONSchemaOutput(outputConfig *AnthropicOutputConfig) bool {
	return outputConfig != nil && outputConfig.Format != nil && outputConfig.Format.Type == "json_schema" && outputConfig.Format.Schema != nil
}

func extractStructuredJSONObject(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(trimmed, ""))
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return trimmed
	}
	for _, match := range mdFenceRegex.FindAllStringSubmatch(trimmed, -1) {
		candidate := strings.TrimSpace(match[1])
		candidate = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(candidate, ""))
		if strings.HasPrefix(candidate, "{") && json.Valid([]byte(candidate)) {
			return candidate
		}
	}
	for i, r := range trimmed {
		if r != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(trimmed[i:]))
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || len(raw) == 0 || raw[0] != '{' || !json.Valid(raw) {
			continue
		}
		rest := strings.TrimSpace(trimmed[i+int(decoder.InputOffset()):])
		rest = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(rest, ""))
		if rest == "" {
			return string(raw)
		}
	}
	return ""
}

func normalizeStructuredOutputText(content string) string {
	if extracted := extractStructuredJSONObject(content); extracted != "" {
		return extracted
	}
	trimmed := strings.TrimSpace(content)
	if trimmed != "" {
		log.Printf("[bridge] structured output JSON-only normalization fallback (%d chars)", len(trimmed))
	}
	return trimmed
}

type preparedToolBridgeResponse struct {
	ToolCalls      []ToolCall
	Remaining      string
	DoneText       string
	WebSearchQuery string
	HasCalls       bool
}

func prepareToolBridgeResponse(content string, nativeToolUses []AgentValueEntry) preparedToolBridgeResponse {
	prepared := preparedToolBridgeResponse{}

	if len(nativeToolUses) > 0 {
		prepared.ToolCalls = nativeToolUseToOpenAI(nativeToolUses)
		prepared.HasCalls = len(prepared.ToolCalls) > 0
		prepared.Remaining = content
	}
	if !prepared.HasCalls {
		prepared.ToolCalls, prepared.Remaining, prepared.HasCalls = parseToolCalls(content)
	}

	if prepared.HasCalls {
		var realCalls []ToolCall
		for _, tc := range prepared.ToolCalls {
			if tc.Function.Name == "__done__" {
				var args map[string]interface{}
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
					if r, ok := args["result"].(string); ok {
						prepared.DoneText = r
					}
				}
				if prepared.DoneText == "" {
					prepared.DoneText = tc.Function.Arguments
				}
				log.Printf("[bridge] __done__ intercepted: %s", prepared.DoneText)
			} else {
				realCalls = append(realCalls, tc)
			}
		}
		prepared.ToolCalls = realCalls
		prepared.HasCalls = len(prepared.ToolCalls) > 0
	}

	if prepared.HasCalls {
		var keptCalls []ToolCall
		for _, tc := range prepared.ToolCalls {
			if tc.Function.Name == "WebSearch" {
				var args map[string]interface{}
				var query string
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err == nil {
					if q, ok := args["query"].(string); ok {
						query = q
					}
				}
				if query == "" {
					query = tc.Function.Arguments
				}
				if prepared.WebSearchQuery != "" {
					prepared.WebSearchQuery = prepared.WebSearchQuery + "\n" + query
				} else {
					prepared.WebSearchQuery = query
				}
			} else {
				keptCalls = append(keptCalls, tc)
			}
		}
		prepared.ToolCalls = keptCalls
		prepared.HasCalls = len(prepared.ToolCalls) > 0
	}

	return prepared
}

func normalizeToolBridgeResidualText(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.TrimSpace(structuredOutputLeadingTagRegex.ReplaceAllString(trimmed, ""))
	return trimmed
}

func detectToolBridgeNoToolResponse(text string) bool {
	normalized := normalizeToolBridgeResidualText(text)
	if normalized == "" {
		return false
	}

	lower := strings.ToLower(normalized)
	mentionsNotionIdentity := strings.Contains(normalized, "我是 Notion AI") ||
		strings.Contains(lower, "i am notion ai")
	mentionsDroidRoleRefusal := strings.Contains(normalized, "不是 Droid") ||
		strings.Contains(normalized, "无法扮演其他 AI 角色") ||
		strings.Contains(normalized, "无法扮演其他AI角色") ||
		strings.Contains(lower, "not droid") ||
		strings.Contains(lower, "cannot act as another ai") ||
		strings.Contains(lower, "cannot pretend to be another ai")
	mentionsLocalFS := strings.Contains(normalized, "本地文件系统") ||
		strings.Contains(lower, "local file system")
	mentionsCodingAssistant := strings.Contains(normalized, "编码助手") ||
		strings.Contains(normalized, "Claude Code") ||
		strings.Contains(normalized, "Cursor") ||
		strings.Contains(lower, "coding assistant")
	mentionsManualHandOff := strings.Contains(normalized, "复制粘贴") ||
		strings.Contains(normalized, "手动添加") ||
		strings.Contains(normalized, "你可以这样做") ||
		strings.Contains(lower, "copy and paste") ||
		strings.Contains(lower, "manually add")
	mentionsMissingLocalTools := strings.Contains(lower, "read") &&
		strings.Contains(lower, "edit") &&
		strings.Contains(lower, "bash")

	switch {
	case mentionsNotionIdentity && mentionsDroidRoleRefusal:
		return true
	case mentionsNotionIdentity && mentionsLocalFS:
		return true
	case mentionsLocalFS && mentionsCodingAssistant:
		return true
	case mentionsNotionIdentity && mentionsCodingAssistant:
		return true
	case mentionsMissingLocalTools && mentionsCodingAssistant && mentionsManualHandOff:
		return true
	default:
		return false
	}
}

func extractAnthropicSessionSalt(metadata map[string]interface{}) string {
	if len(metadata) == 0 {
		return ""
	}

	extractFromValue := func(v interface{}) string {
		switch tv := v.(type) {
		case string:
			trimmed := strings.TrimSpace(tv)
			if trimmed == "" {
				return ""
			}
			var parsed map[string]interface{}
			if json.Unmarshal([]byte(trimmed), &parsed) == nil {
				if sid, ok := parsed["session_id"].(string); ok && sid != "" {
					return sid
				}
			}
			return ""
		case map[string]interface{}:
			if sid, ok := tv["session_id"].(string); ok && sid != "" {
				return sid
			}
		}
		return ""
	}

	for _, key := range []string{"session_id", "conversation_id", "user_id"} {
		if sid := extractFromValue(metadata[key]); sid != "" {
			return sid
		}
	}
	return ""
}

func stripStructuredOutputSystemNoise(content string) string {
	content = normalizeSessionSystemContent(content)
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			filtered = append(filtered, "")
			continue
		}
		if strings.HasPrefix(trimmed, "You are Claude Code") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func applyStructuredOutputBridge(messages []ChatMessage, outputConfig *AnthropicOutputConfig) []ChatMessage {
	if outputConfig == nil || outputConfig.Format == nil || outputConfig.Format.Type != "json_schema" || outputConfig.Format.Schema == nil {
		return messages
	}

	schemaJSON, err := json.MarshalIndent(outputConfig.Format.Schema, "", "  ")
	if err != nil {
		schemaJSON, _ = json.Marshal(outputConfig.Format.Schema)
	}

	var instructionParts []string
	var conversationParts []string
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		switch msg.Role {
		case "system":
			cleaned := stripStructuredOutputSystemNoise(content)
			if cleaned != "" {
				instructionParts = append(instructionParts, cleaned)
			}
		case "user":
			cleaned := stripSystemReminders(content)
			if cleaned == "" {
				cleaned = content
			}
			conversationParts = append(conversationParts, "User:\n"+cleaned)
		case "assistant":
			conversationParts = append(conversationParts, "Assistant:\n"+content)
		case "tool":
			conversationParts = append(conversationParts, "Tool result:\n"+content)
		}
	}

	if len(conversationParts) == 0 {
		return messages
	}

	var prompt strings.Builder
	prompt.WriteString("Return exactly one JSON object that matches this schema.\n")
	prompt.WriteString("Do not output markdown fences, explanations, or extra text.\n\n")
	prompt.WriteString("Schema:\n")
	prompt.Write(schemaJSON)
	if len(instructionParts) > 0 {
		prompt.WriteString("\n\nInstructions:\n")
		prompt.WriteString(strings.Join(instructionParts, "\n\n"))
	}
	prompt.WriteString("\n\nConversation:\n")
	prompt.WriteString(strings.Join(conversationParts, "\n\n"))
	prompt.WriteString("\n\nJSON only.")

	log.Printf("[bridge] structured output bridge applied (json_schema, %d chars)", prompt.Len())
	return []ChatMessage{{
		Role:    "user",
		Content: prompt.String(),
	}}
}

// SSE events are constructed using maps for precise JSON field control
// (avoiding Go's omitempty dropping required empty-string fields)

// ========== Handler ==========

// HandleAnthropicMessages returns an HTTP handler for the /v1/messages endpoint (Anthropic Messages API)
func HandleAnthropicMessages(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := "msg_" + generateUUIDv4()

		if r.Method != http.MethodPost {
			writeAnthropicError(w, requestID, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "failed to read request body: "+err.Error(), "invalid_request_error")
			return
		}
		if len(bodyBytes) == 0 {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "request body is required", "invalid_request_error")
			return
		}
		if json.Valid(bodyBytes) {
			LogAPIInputJSONBytes(requestID, "incoming /v1/messages request", bodyBytes)
		} else {
			LogAPIInputText(requestID, "incoming /v1/messages request (raw)", string(bodyBytes))
		}

		var req AnthropicRequest
		if err := json.Unmarshal(bodyBytes, &req); err != nil {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "invalid request body: "+err.Error(), "invalid_request_error")
			return
		}

		if len(req.Messages) == 0 {
			writeAnthropicError(w, requestID, http.StatusBadRequest, "messages is required", "invalid_request_error")
			return
		}

		model := req.Model
		if model == "" {
			model = AppConfig.Proxy.DefaultModel
		}

		// ── Detailed request logging ──
		logAnthropicRequest(req, model)

		// Convert Anthropic messages to internal ChatMessage format
		messages, fileAttachments := convertAnthropicMessages(req.System, req.Messages)
		if len(fileAttachments) > 0 {
			log.Printf("[upload-debug] extracted %d file attachment(s) from request", len(fileAttachments))
		}
		sessionSalt := extractAnthropicSessionSalt(req.Metadata)
		if sessionSalt != "" {
			log.Printf("[session] extracted metadata session salt %s", truncateForLog(sessionSalt, 8))
		}

		// Log converted internal messages
		logConvertedMessages(messages)
		rawMessages := append([]ChatMessage(nil), messages...)

		// Detect researcher mode — skip tools and file uploads
		isResearcher := IsResearcherModel(model)
		if isResearcher {
			if len(fileAttachments) > 0 {
				log.Printf("[researcher] ignoring %d file attachment(s) — not supported in researcher mode", len(fileAttachments))
				fileAttachments = nil
			}
			if len(req.Tools) > 0 {
				log.Printf("[researcher] ignoring %d tool(s) — not supported in researcher mode", len(req.Tools))
			}
		}

		// ── Resolve search settings: header override > config default ──
		// Web search: default from config, overridable via X-Web-Search header
		effectiveWebSearch := AppConfig.WebSearchEnabled()
		if hdr := r.Header.Get("X-Web-Search"); hdr != "" {
			effectiveWebSearch = strings.EqualFold(hdr, "true") || hdr == "1"
			log.Printf("[search] X-Web-Search header override: %v", effectiveWebSearch)
		}
		// Workspace search: default from config, overridable via X-Workspace-Search header
		var enableWorkspaceSearch *bool
		if hdr := r.Header.Get("X-Workspace-Search"); hdr != "" {
			b := strings.EqualFold(hdr, "true") || hdr == "1"
			enableWorkspaceSearch = &b
			log.Printf("[search] X-Workspace-Search header override: %v", b)
		}

		// Convert Anthropic tools to OpenAI tools format for tool injection
		hasTools := !isResearcher && len(req.Tools) > 0
		enableWebSearch := effectiveWebSearch

		// ── Multi-turn session management ──
		// Compute fingerprint BEFORE tool injection so it's stable across turns.
		// Tool injection may collapse/modify messages, breaking fingerprint stability.
		var fingerprint string
		var session *Session
		isFirstTurn := true
		isRepeatTurn := false
		rawMsgCount := 0

		if !isResearcher {
			fingerprint = computeSessionFingerprintWithSalt(messages, sessionSalt)
			session = globalSessionManager.Get(fingerprint)
			rawMsgCount = countNonSystemMessages(messages)

			if session != nil {
				if rawMsgCount < session.RawMessageCount {
					// Message count decreased (edit/rollback) — invalidate session
					log.Printf("[session] message rollback detected (rawMsgs=%d < prev=%d), clearing session",
						rawMsgCount, session.RawMessageCount)
					globalSessionManager.Delete(fingerprint)
					session = nil
				} else if rawMsgCount == session.RawMessageCount {
					// Same message count — not a new turn (e.g. retry or tool call loop)
					isRepeatTurn = true
					log.Printf("[session] repeat turn detected (rawMsgs=%d, turnCount=%d), reusing session",
						rawMsgCount, session.TurnCount)
				}
			}

			if session != nil {
				isFirstTurn = false
				log.Printf("[session] found existing session: thread=%s turns=%d rawMsgs=%d account=%s",
					session.ThreadID, session.TurnCount, session.RawMessageCount, session.AccountEmail)
			} else {
				isFirstTurn = true
				log.Printf("[session] no existing session, will create new thread (fingerprint=%s)", fingerprint[:8])
			}
		}

		if hasTools {
			var tools []Tool
			for _, t := range req.Tools {
				tools = append(tools, Tool{
					Type: "function",
					Function: ToolFunction{
						Name:        t.Name,
						Description: t.Description,
						Parameters:  t.InputSchema,
					},
				})
			}
			// Filter out WebSearch/WebFetch — these are handled by Notion's native search.
			// Injecting them as custom tools causes the model to generate JSON tool calls
			// instead of using Notion's server-side search which actually executes.
			var toolDetectedWebSearch bool
			tools, toolDetectedWebSearch = filterNativeSearchTools(tools)
			if toolDetectedWebSearch {
				enableWebSearch = true
				log.Printf("[bridge] WebSearch/WebFetch detected — enabling Notion native search, stripping history")
				messages = stripWebSearchHistory(messages)
			}
			messages = injectToolsIntoMessages(messages, tools, model, session, req.ToolChoice)
			// Log messages after tool injection only when verbose debugging is enabled.
			if DebugLoggingEnabled() {
				log.Printf("[debug] === After tool injection (%d messages) ===", len(messages))
				for i, m := range messages {
					preview := truncateForLog(m.Content, 300)
					log.Printf("[debug]   [%d] role=%s toolcalls=%d content_len=%d: %s", i, m.Role, len(m.ToolCalls), len(m.Content), preview)
				}
			}
		} else if !isResearcher && req.OutputConfig != nil && req.OutputConfig.Format != nil && req.OutputConfig.Format.Type == "json_schema" {
			messages = applyStructuredOutputBridge(messages, req.OutputConfig)
			if DebugLoggingEnabled() {
				log.Printf("[debug] === After structured output bridge (%d messages) ===", len(messages))
				for i, m := range messages {
					preview := truncateForLog(m.Content, 300)
					log.Printf("[debug]   [%d] role=%s toolcalls=%d content_len=%d: %s", i, m.Role, len(m.ToolCalls), len(m.Content), preview)
				}
			}
		}

		// Try accounts with automatic failover
		tried := make(map[*Account]bool)
		maxAttempts := pool.Count()
		var lastNonQuotaErr error
		var sawEmptyResponse bool
		var recoveryMessages []ChatMessage
		var toolRecoveryMessages []ChatMessage
		toolBridgeRetried := false

		for attempt := 0; attempt < maxAttempts; attempt++ {
			var acc *Account

			// Account binding: for subsequent turns, try the bound account first
			if !isFirstTurn && session != nil && attempt == 0 {
				acc = pool.GetByEmail(session.AccountEmail)
				if acc == nil {
					log.Printf("[session] bound account %s unavailable, clearing session", session.AccountEmail)
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
				}
			}

			if acc == nil {
				if attempt == 0 {
					if isResearcher {
						acc = pool.NextForResearch()
					} else {
						acc = pool.Next()
					}
				} else {
					acc = pool.NextExcluding(tried)
				}
			}
			if acc == nil {
				break
			}
			tried[acc] = true

			requestMessages := messages
			if !isResearcher && isFirstTurn {
				switch {
				case len(toolRecoveryMessages) > 0:
					requestMessages = toolRecoveryMessages
				case needsFreshThreadRecovery(messages):
					if recoveryMessages == nil {
						recoveryMessages = buildFreshThreadRecoveryMessages(messages)
						if len(recoveryMessages) == 1 {
							log.Printf("[session] collapsed history to self-contained fresh-thread prompt (%d→%d chars)",
								len(messages), len(recoveryMessages[0].Content))
						}
					}
					requestMessages = recoveryMessages
				}
			}

			// For first turn, pre-create session with generated IDs
			var currentSession *Session
			if !isResearcher {
				if isFirstTurn {
					now := time.Now().Format(time.RFC3339Nano)
					currentSession = &Session{
						ThreadID:         generateUUIDv4(),
						TurnCount:        0,
						AccountEmail:     acc.UserEmail,
						CreatedAt:        time.Now(),
						LastUsedAt:       time.Now(),
						ConfigID:         generateUUIDv4(),
						ContextID:        generateUUIDv4(),
						OriginalDatetime: now,
					}
				} else {
					currentSession = session
				}
			}

			log.Printf("[req] %s model=%s messages=%d stream=%v tools=%d attachments=%d account=%s session=%v (attempt %d/%d) [anthropic]",
				requestID, model, len(req.Messages), req.Stream, len(req.Tools), len(fileAttachments), acc.UserEmail, !isFirstTurn, attempt+1, maxAttempts)

			// Upload file attachments to Notion (if any) — skip for researcher mode
			var uploadedAttachments []UploadedAttachment
			if !isResearcher && len(fileAttachments) > 0 {
				for i, fa := range fileAttachments {
					log.Printf("[upload-debug] %s: uploading attachment %d/%d: %s (%s, %d bytes)",
						requestID, i+1, len(fileAttachments), fa.FileName, fa.ContentType, len(fa.Data))
					uploaded, err := UploadFileToNotion(acc, &fa)
					if err != nil {
						log.Printf("[upload] %s: attachment %d upload failed: %v", requestID, i+1, err)
						writeAnthropicError(w, requestID, http.StatusBadGateway, "file upload failed: "+err.Error(), "api_error")
						return
					}
					uploadedAttachments = append(uploadedAttachments, *uploaded)
					log.Printf("[upload-debug] %s: attachment %d uploaded: %s", requestID, i+1, uploaded.AttachmentURL)
				}
			}

			// For streaming responses, default to emitting thinking blocks even when
			// the client did not explicitly request Anthropic thinking.
			hasThinking := req.Thinking != nil || req.Stream
			var reqErr error
			if isResearcher {
				// Researcher mode — always use thinking blocks for research progress
				hasThinking = true
				if req.Stream {
					reqErr = handleResearcherStream(w, acc, requestMessages, model, requestID, hasThinking)
				} else {
					reqErr = handleResearcherNonStream(w, acc, requestMessages, model, requestID, hasThinking)
				}
			} else if req.Stream {
				reqErr = handleAnthropicStream(w, acc, requestMessages, rawMessages, model, requestID, hasTools, hasThinking, enableWebSearch, enableWorkspaceSearch, uploadedAttachments, req.OutputConfig, currentSession)
			} else {
				reqErr = handleAnthropicNonStream(w, acc, requestMessages, rawMessages, model, requestID, hasTools, hasThinking, enableWebSearch, enableWorkspaceSearch, uploadedAttachments, req.OutputConfig, currentSession)
			}

			if reqErr != nil && errors.Is(reqErr, ErrResearchQuotaExhausted) {
				// Research mode quota exhausted — account can still serve normal chat
				log.Printf("[research-quota] %s research quota exhausted (research_usage=%d), trying next account",
					acc.UserEmail, func() int {
						if acc.QuotaInfo != nil {
							return acc.QuotaInfo.ResearchModeUsage
						}
						return -1
					}())
				continue
			}
			if reqErr != nil && errors.Is(reqErr, ErrQuotaExhausted) {
				if isFreePlan(acc) {
					pool.MarkPermanentlyExhausted(acc)
				} else {
					pool.MarkQuotaExhausted(acc)
				}
				log.Printf("[quota] %s quota exhausted, trying next account (%d/%d available)",
					acc.UserEmail, pool.AvailableCount(), pool.Count())
				// Clear session if the bound account was exhausted
				if !isFirstTurn && session != nil {
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
				}
				continue
			}

			if reqErr != nil && errors.Is(reqErr, ErrEmptyResponse) {
				// Empty response — account/thread in bad state, clear session and try next account
				log.Printf("[empty] %s returned empty response, trying next account", acc.UserEmail)
				sawEmptyResponse = true
				if currentSession != nil {
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
				}
				continue
			}

			if reqErr != nil && errors.Is(reqErr, ErrToolBridgeNoTool) {
				if !toolBridgeRetried {
					log.Printf("[bridge] %s returned no-tool identity-drift text, clearing session and retrying once with sanitized recovery prompt", acc.UserEmail)
					toolBridgeRetried = true
					if fingerprint != "" {
						globalSessionManager.Delete(fingerprint)
					}
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
					toolRecoveryMessages = buildToolBridgeRecoveryMessages(messages)
					recoveryMessages = nil
					tried = make(map[*Account]bool)
					attempt = -1
					continue
				}
				lastNonQuotaErr = reqErr
			}

			if reqErr != nil {
				// Non-quota error — if this was a subsequent turn, try clearing session and retrying as first turn
				if !isFirstTurn && session != nil {
					log.Printf("[session] subsequent turn failed (%v), clearing session and falling back to first turn", reqErr)
					globalSessionManager.Delete(fingerprint)
					session = nil
					isFirstTurn = true
					isRepeatTurn = false
					continue
				}
				lastNonQuotaErr = reqErr
			}

			// ── Success: save/update session ──
			if !isResearcher && currentSession != nil && reqErr == nil {
				if isFirstTurn {
					currentSession.TurnCount = 1
					currentSession.RawMessageCount = rawMsgCount
					currentSession.UpdatedConfigIDs = []string{generateUUIDv4()}
					currentSession.ModelUsed = ResolveModel(model)
					globalSessionManager.Set(fingerprint, currentSession)
					log.Printf("[session] saved new session: thread=%s fingerprint=%s rawMsgs=%d",
						currentSession.ThreadID, fingerprint[:8], rawMsgCount)
				} else if isRepeatTurn {
					currentSession.LastUsedAt = time.Now()
					log.Printf("[session] kept session unchanged on repeat turn: thread=%s turns=%d",
						currentSession.ThreadID, currentSession.TurnCount)
				} else {
					currentSession.TurnCount++
					currentSession.RawMessageCount = rawMsgCount
					currentSession.UpdatedConfigIDs = append(currentSession.UpdatedConfigIDs, generateUUIDv4())
					currentSession.LastUsedAt = time.Now()
					log.Printf("[session] updated session: thread=%s turns=%d rawMsgs=%d",
						currentSession.ThreadID, currentSession.TurnCount, rawMsgCount)
				}
			}

			return
		}

		if lastNonQuotaErr != nil {
			writeAnthropicError(w, requestID, http.StatusBadGateway,
				"notion API error: "+lastNonQuotaErr.Error(), "api_error")
			return
		}
		if sawEmptyResponse {
			writeAnthropicError(w, requestID, http.StatusBadGateway,
				"notion returned empty response after retries", "api_error")
			return
		}
		writeAnthropicError(w, requestID, http.StatusServiceUnavailable,
			"all accounts exhausted after retries", "overloaded_error")
	}
}

// convertAnthropicMessages converts Anthropic system + messages to internal ChatMessage format.
// It also extracts file attachments (image/document content blocks) for Notion upload.
func convertAnthropicMessages(system interface{}, msgs []AnthropicMessage) ([]ChatMessage, []FileAttachment) {
	var attachments []FileAttachment
	var result []ChatMessage

	// Handle system prompt
	if system != nil {
		switch s := system.(type) {
		case string:
			if s != "" {
				result = append(result, ChatMessage{Role: "system", Content: s})
			}
		case []interface{}:
			// System can be array of content blocks
			var parts []string
			for _, block := range s {
				if m, ok := block.(map[string]interface{}); ok {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			if len(parts) > 0 {
				result = append(result, ChatMessage{Role: "system", Content: strings.Join(parts, "\n")})
			}
		}
	}

	// Build tool_use_id → name map by scanning all messages first
	toolIDToName := map[string]string{}
	for _, msg := range msgs {
		if blocks, ok := msg.Content.([]interface{}); ok {
			for _, block := range blocks {
				if m, ok := block.(map[string]interface{}); ok {
					if t, _ := m["type"].(string); t == "tool_use" {
						id, _ := m["id"].(string)
						name, _ := m["name"].(string)
						if id != "" && name != "" {
							toolIDToName[id] = name
						}
					}
				}
			}
		}
	}

	for _, msg := range msgs {
		cm := ChatMessage{Role: msg.Role}

		switch content := msg.Content.(type) {
		case string:
			cm.Content = content
		case []interface{}:
			// Array of content blocks
			var textParts []string
			for _, block := range content {
				if m, ok := block.(map[string]interface{}); ok {
					blockType, _ := m["type"].(string)
					switch blockType {
					case "text":
						if text, ok := m["text"].(string); ok {
							textParts = append(textParts, text)
						}
					case "thinking", "redacted_thinking":
						// Silently drop — CC echoes back our synthetic thinking blocks;
						// we don't need them in internal format since chain collapse
						// rebuilds context from scratch each turn.
					case "tool_use":
						// Convert to proper ToolCall for multi-turn strategy detection
						name, _ := m["name"].(string)
						id, _ := m["id"].(string)
						inputRaw, _ := json.Marshal(m["input"])
						cm.ToolCalls = append(cm.ToolCalls, ToolCall{
							ID:   id,
							Type: "function",
							Function: ToolCallFunction{
								Name:      name,
								Arguments: string(inputRaw),
							},
						})
					case "image":
						// Extract image from base64 or URL source
						if source, ok := m["source"].(map[string]interface{}); ok {
							fa := extractFileFromSource(source, "image")
							if fa != nil {
								attachments = append(attachments, *fa)
							}
						}
					case "document":
						// Extract PDF/text document from base64, URL, or file source
						if source, ok := m["source"].(map[string]interface{}); ok {
							fa := extractFileFromSource(source, "document")
							if fa != nil {
								attachments = append(attachments, *fa)
							}
						}
					case "tool_result":
						// Convert tool_result to tool role message
						toolUseID, _ := m["tool_use_id"].(string)
						var resultText string
						if c, ok := m["content"].(string); ok {
							resultText = c
						} else if c, ok := m["content"].([]interface{}); ok {
							for _, cb := range c {
								if cbm, ok := cb.(map[string]interface{}); ok {
									if t, ok := cbm["text"].(string); ok {
										resultText += t
									}
								}
							}
						}
						result = append(result, ChatMessage{
							Role:       "tool",
							Content:    resultText,
							ToolCallID: toolUseID,
							Name:       toolIDToName[toolUseID],
						})
						continue
					}
				}
			}
			if len(textParts) > 0 {
				cm.Content = strings.Join(textParts, "")
			}
		}

		if cm.Content != "" || cm.Role == "assistant" || len(cm.ToolCalls) > 0 {
			result = append(result, cm)
		}
	}

	return result, attachments
}

// extractFileFromSource extracts file data from an Anthropic content block source.
// Supports base64 and URL source types. blockKind is "image" or "document".
func extractFileFromSource(source map[string]interface{}, blockKind string) *FileAttachment {
	srcType, _ := source["type"].(string)

	switch srcType {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data64, _ := source["data"].(string)
		if data64 == "" {
			return nil
		}
		decoded, err := base64.StdEncoding.DecodeString(data64)
		if err != nil {
			log.Printf("[upload] failed to decode base64 %s: %v", blockKind, err)
			return nil
		}
		if mediaType == "" {
			if blockKind == "image" {
				mediaType = "image/png"
			} else {
				mediaType = "application/pdf"
			}
		}
		ext := mimeToExt(mediaType)
		return &FileAttachment{
			Data:        decoded,
			FileName:    generateUUIDv4() + ext,
			ContentType: mediaType,
		}

	case "url":
		urlStr, _ := source["url"].(string)
		if urlStr == "" {
			return nil
		}
		// Download the file from URL
		resp, err := http.Get(urlStr)
		if err != nil {
			log.Printf("[upload] failed to download %s from URL: %v", blockKind, err)
			return nil
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[upload] failed to read %s URL response: %v", blockKind, err)
			return nil
		}
		mediaType := resp.Header.Get("Content-Type")
		if mediaType == "" {
			if blockKind == "image" {
				mediaType = "image/png"
			} else {
				mediaType = "application/pdf"
			}
		}
		// Strip charset suffix if present (e.g. "image/png; charset=utf-8")
		if idx := strings.Index(mediaType, ";"); idx > 0 {
			mediaType = strings.TrimSpace(mediaType[:idx])
		}
		ext := mimeToExt(mediaType)
		return &FileAttachment{
			Data:        data,
			FileName:    generateUUIDv4() + ext,
			ContentType: mediaType,
		}

	default:
		log.Printf("[upload] unsupported source type %q for %s block", srcType, blockKind)
		return nil
	}
}

func streamAnthropicTextResponse(w http.ResponseWriter, acc *Account, messages []ChatMessage, model, requestID string, hasThinking bool, disableBuiltin bool, outputConfig *AnthropicOutputConfig, callOpts CallOptions) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, requestID, http.StatusInternalServerError, "streaming not supported", "api_error")
		return nil
	}

	var fullContent strings.Builder
	var thinkingForLog strings.Builder
	var finalUsage *UsageInfo
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)
	cr := newCitationReplacer(&knownCitationURLs, &knownCitationDocs, &knownToolCallURLs)
	headersSent := false
	thinkingBlockOpen := false
	textBlockOpen := false
	blockIndex := 0
	sawContent := false
	callOpts.KnownCitationURLs = &knownCitationURLs
	callOpts.KnownCitationDocs = &knownCitationDocs
	callOpts.KnownToolCallURLs = &knownToolCallURLs
	jsonOnlyOutput := isJSONSchemaOutput(outputConfig)

	ensureHeaders := func() {
		if headersSent {
			return
		}
		headersSent = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		inputTokens := 0
		if finalUsage != nil {
			inputTokens = finalUsage.PromptTokens
		}
		sendAnthropicSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            requestID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]interface{}{"input_tokens": inputTokens, "output_tokens": 0},
			},
		})
		sendAnthropicSSE(w, flusher, "ping", map[string]string{"type": "ping"})
	}

	closeThinkingBlock := func(signature string) {
		if !thinkingBlockOpen {
			return
		}
		if signature == "" {
			signature = generateFakeSignature()
		}
		sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{"type": "signature_delta", "signature": signature},
		})
		sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": blockIndex,
		})
		blockIndex++
		thinkingBlockOpen = false
	}

	ensureTextBlock := func() {
		ensureHeaders()
		if !textBlockOpen {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			textBlockOpen = true
		}
	}

	if hasThinking {
		callOpts.ThinkingCallback = func(delta string, done bool, signature string) {
			if done {
				closeThinkingBlock(signature)
				return
			}
			if delta == "" {
				return
			}
			thinkingForLog.WriteString(delta)
			sawContent = true
			ensureHeaders()
			if !thinkingBlockOpen {
				sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
				})
				thinkingBlockOpen = true
			}
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": delta},
			})
		}
	}

	cbErr := CallInference(acc, messages, model, disableBuiltin, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			fullContent.WriteString(delta)
			if !jsonOnlyOutput {
				cleaned := cr.Process(delta)
				if cleaned != "" {
					sawContent = true
					ensureTextBlock()
					sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": blockIndex,
						"delta": map[string]interface{}{"type": "text_delta", "text": cleaned},
					})
				}
			}
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	if cbErr != nil {
		if !headersSent {
			if errors.Is(cbErr, ErrQuotaExhausted) {
				return cbErr
			}
			log.Printf("[err] %s: %v", requestID, cbErr)
			writeAnthropicError(w, requestID, http.StatusBadGateway, "notion API error: "+cbErr.Error(), "api_error")
			return nil
		}
		log.Printf("[err] %s: streaming completed with partial data before error: %v", requestID, cbErr)
	}

	if fullContent.Len() == 0 && !sawContent {
		log.Printf("[warn] %s: empty response from Notion, will retry", requestID)
		return ErrEmptyResponse
	}

	if thinkingBlockOpen {
		closeThinkingBlock("")
	}

	if !jsonOnlyOutput {
		if flushed := cr.Flush(); flushed != "" {
			sawContent = true
			ensureTextBlock()
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": flushed},
			})
		}
	} else {
		normalized := normalizeStructuredOutputText(fullContent.String())
		if normalized != "" {
			sawContent = true
			ensureTextBlock()
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": normalized},
			})
		}
	}

	if textBlockOpen {
		sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": blockIndex,
		})
		blockIndex++
	}

	urls := cr.URLs()
	if !jsonOnlyOutput {
		if sourcesText := formatCitationSources(urls); sourcesText != "" {
			ensureHeaders()
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": sourcesText},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			blockIndex++
		}
	}

	outputTokens := 0
	inputTokens := 0
	if finalUsage != nil {
		inputTokens = finalUsage.PromptTokens
		outputTokens = finalUsage.CompletionTokens
	}
	ensureHeaders()
	sendAnthropicSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"input_tokens": inputTokens, "output_tokens": outputTokens},
	})
	sendAnthropicSSE(w, flusher, "message_stop", map[string]string{"type": "message_stop"})

	var contentBlocks []AnthropicContentBlock
	if hasThinking && thinkingForLog.Len() > 0 {
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type:      "thinking",
			Thinking:  thinkingForLog.String(),
			Signature: generateFakeSignature(),
		})
	}
	if fullContent.Len() > 0 {
		text := renderAnthropicCitationText(fullContent.String(), knownCitationURLs, knownCitationDocs, knownToolCallURLs)
		if jsonOnlyOutput {
			text = normalizeStructuredOutputText(fullContent.String())
		}
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type: "text",
			Text: text,
		})
	}
	LogAPIOutputJSON(requestID, "anthropic stream summary", AnthropicResponse{
		ID:         requestID,
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      model,
		StopReason: strPtr("end_turn"),
		Usage: &AnthropicUsage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
		},
	})

	log.Printf("[thinking] %s streamed text=%d chars sources=%d thinking=%v", requestID, fullContent.Len(), len(urls), hasThinking)
	return nil
}

// handleAnthropicStream handles streaming Anthropic response
func handleAnthropicStream(w http.ResponseWriter, acc *Account, messages []ChatMessage, rawMessages []ChatMessage, model, requestID string, hasTools bool, hasThinking bool, enableWebSearch bool, enableWorkspaceSearch *bool, attachments []UploadedAttachment, outputConfig *AnthropicOutputConfig, session *Session) error {
	var fullContent strings.Builder
	var finalUsage *UsageInfo
	var nativeToolUses []AgentValueEntry
	var thinkingBlocks []ThinkingBlock
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)

	disableBuiltin := AppConfig.Proxy.DisableNotionPrompt

	callOpts := CallOptions{
		ThinkingBlocks:        &thinkingBlocks,
		EnableWebSearch:       enableWebSearch,
		EnableWorkspaceSearch: enableWorkspaceSearch,
		Attachments:           attachments,
		KnownCitationURLs:     &knownCitationURLs,
		KnownCitationDocs:     &knownCitationDocs,
		KnownToolCallURLs:     &knownToolCallURLs,
		Session:               session,
		RequestID:             requestID,
	}

	if !hasTools {
		return streamAnthropicTextResponse(w, acc, messages, model, requestID, hasThinking, disableBuiltin, outputConfig, callOpts)
	}

	callOpts.NativeToolUses = &nativeToolUses
	// Format-based injection: tools embedded in user messages, use normal chat path
	cbErr := CallInference(acc, messages, model, disableBuiltin, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			fullContent.WriteString(delta)
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	if cbErr != nil {
		if errors.Is(cbErr, ErrQuotaExhausted) {
			return cbErr
		}
		log.Printf("[err] %s: %v", requestID, cbErr)
		writeAnthropicError(w, requestID, http.StatusBadGateway, "notion API error: "+cbErr.Error(), "api_error")
		return nil
	}

	contentStr := fullContent.String()
	log.Printf("[debug] %s: fullContent=%d bytes, hasTools=%v", requestID, len(contentStr), hasTools)
	if len(contentStr) > 0 {
		contentHead := truncateForLog(contentStr, 200)
		log.Printf("[debug] %s HEAD: %s", requestID, contentHead)
	}

	// Empty response: Notion returned 200 but produced no text — retry on next account
	if len(contentStr) == 0 && len(nativeToolUses) == 0 {
		log.Printf("[warn] %s: empty response from Notion, will retry", requestID)
		return ErrEmptyResponse
	}

	var prepared preparedToolBridgeResponse
	if hasTools {
		prepared = prepareToolBridgeResponse(contentStr, nativeToolUses)
		prepared.ToolCalls = normalizeToolCallPaths(rawMessages, prepared.ToolCalls)
		actionDetected := prepared.HasCalls || prepared.WebSearchQuery != "" || prepared.DoneText != ""
		if !actionDetected && detectToolBridgeNoToolResponse(prepared.Remaining) {
			log.Printf("[bridge] %s detected no-tool identity-drift text (%d chars), requesting clean retry", requestID, len(prepared.Remaining))
			return ErrToolBridgeNoTool
		}
	}

	// Build usage
	aUsage := &AnthropicUsage{}
	if finalUsage != nil {
		aUsage.InputTokens = finalUsage.PromptTokens
		aUsage.OutputTokens = finalUsage.CompletionTokens
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, requestID, http.StatusInternalServerError, "streaming not supported", "api_error")
		return nil
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// message_start
	sendAnthropicSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            requestID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": aUsage.InputTokens, "output_tokens": 0},
		},
	})

	// ping
	sendAnthropicSSE(w, flusher, "ping", map[string]string{"type": "ping"})

	// Emit thinking blocks from Notion (real thinking from Sonnet 4.6 etc.)
	blockIndex := 0
	loggedContentBlocks := make([]AnthropicContentBlock, 0, len(thinkingBlocks)+4)
	if hasThinking && len(thinkingBlocks) > 0 {
		log.Printf("[thinking] emitting %d thinking block(s)", len(thinkingBlocks))
		for _, tb := range thinkingBlocks {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": tb.Content},
			})
			sig := tb.Signature
			if sig == "" {
				sig = generateFakeSignature()
			}
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "signature_delta", "signature": sig},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			loggedContentBlocks = append(loggedContentBlocks, AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  tb.Content,
				Signature: sig,
			})
			blockIndex++
		}
	}

	if hasTools {
		toolCalls := prepared.ToolCalls
		remaining := prepared.Remaining
		hasCalls := prepared.HasCalls
		doneText := prepared.DoneText
		webSearchQuery := prepared.WebSearchQuery

		// When any tool action is detected (tool calls, __done__, or WebSearch),
		// remaining is usually framing residue or Notion-identity leakage.
		// Real thinking blocks are already captured separately, so suppress this
		// text instead of echoing it back to Claude Code.
		if remaining != "" && (hasCalls || webSearchQuery != "" || doneText != "") {
			log.Printf("[bridge] suppressed %d chars of residual tool framing text", len(remaining))
			remaining = ""
		} else if remaining != "" {
			// No thinking requested or no tool calls — send remaining as plain text
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": remaining},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			loggedContentBlocks = append(loggedContentBlocks, AnthropicContentBlock{Type: "text", Text: remaining})
			blockIndex++
		}

		// Send __done__ result as text block
		if doneText != "" {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "text_delta", "text": doneText},
			})
			sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type": "content_block_stop", "index": blockIndex,
			})
			loggedContentBlocks = append(loggedContentBlocks, AnthropicContentBlock{Type: "text", Text: doneText})
			blockIndex++
		}

		// Stream WebSearch results in real-time (after text blocks, before tool_use)
		if webSearchQuery != "" {
			log.Printf("[bridge] WebSearch intercepted — streaming via Notion native search: %q", webSearchQuery)
			searchUsage, searchErr := streamWebSearch(w, flusher, acc, webSearchQuery, model, requestID, &blockIndex, hasThinking)
			if searchErr != nil {
				log.Printf("[bridge] WebSearch streaming failed: %v", searchErr)
			}
			if searchUsage != nil && finalUsage != nil {
				finalUsage.PromptTokens += searchUsage.PromptTokens
				finalUsage.CompletionTokens += searchUsage.CompletionTokens
				finalUsage.TotalTokens = finalUsage.PromptTokens + finalUsage.CompletionTokens
			}
		}
		if finalUsage != nil {
			aUsage.InputTokens = finalUsage.PromptTokens
			aUsage.OutputTokens = finalUsage.CompletionTokens
		}

		// Send tool_use blocks
		if hasCalls {
			for _, tc := range toolCalls {
				sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": blockIndex,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tc.ID,
						"name":  tc.Function.Name,
						"input": map[string]interface{}{},
					},
				})
				sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": blockIndex,
					"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
				sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type": "content_block_stop", "index": blockIndex,
				})
				loggedContentBlocks = append(loggedContentBlocks, AnthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(tc.Function.Arguments),
				})
				blockIndex++
			}
		}

		stopReason := "end_turn"
		if hasCalls {
			stopReason = "tool_use"
		}
		sendAnthropicSSE(w, flusher, "message_delta", map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]interface{}{"output_tokens": aUsage.OutputTokens},
		})
		LogAPIOutputJSON(requestID, "anthropic stream tool response summary", AnthropicResponse{
			ID:         requestID,
			Type:       "message",
			Role:       "assistant",
			Content:    loggedContentBlocks,
			Model:      model,
			StopReason: strPtr(stopReason),
			Usage:      aUsage,
		})
	}

	// message_stop
	sendAnthropicSSE(w, flusher, "message_stop", map[string]string{"type": "message_stop"})

	return nil
}

// handleAnthropicNonStream handles non-streaming Anthropic response
func handleAnthropicNonStream(w http.ResponseWriter, acc *Account, messages []ChatMessage, rawMessages []ChatMessage, model, requestID string, hasTools bool, hasThinking bool, enableWebSearch bool, enableWorkspaceSearch *bool, attachments []UploadedAttachment, outputConfig *AnthropicOutputConfig, session *Session) error {
	var fullContent strings.Builder
	var finalUsage *UsageInfo
	var nativeToolUses []AgentValueEntry
	var thinkingBlocks []ThinkingBlock
	var thinkingProcess strings.Builder
	var thinkingSignature string
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)

	callOpts := CallOptions{
		ThinkingBlocks:        &thinkingBlocks,
		EnableWebSearch:       enableWebSearch,
		EnableWorkspaceSearch: enableWorkspaceSearch,
		Attachments:           attachments,
		KnownCitationURLs:     &knownCitationURLs,
		KnownCitationDocs:     &knownCitationDocs,
		KnownToolCallURLs:     &knownToolCallURLs,
		Session:               session,
		RequestID:             requestID,
	}
	if hasThinking {
		callOpts.ThinkingCallback = func(delta string, done bool, signature string) {
			if delta != "" {
				thinkingProcess.WriteString(delta)
			}
			if done && signature != "" {
				thinkingSignature = signature
			}
		}
	}

	var err error
	if hasTools {
		callOpts.NativeToolUses = &nativeToolUses
		// Format-based injection: tools embedded in user messages, use normal chat path
		err = CallInference(acc, messages, model, AppConfig.Proxy.DisableNotionPrompt, func(delta string, done bool, usage *UsageInfo) {
			if delta != "" {
				fullContent.WriteString(delta)
			}
			if usage != nil {
				finalUsage = usage
			}
		}, callOpts)
	} else {
		err = CallInference(acc, messages, model, AppConfig.Proxy.DisableNotionPrompt, func(delta string, done bool, usage *UsageInfo) {
			if delta != "" {
				fullContent.WriteString(delta)
			}
			if usage != nil {
				finalUsage = usage
			}
		}, callOpts)
	}

	if err != nil {
		if errors.Is(err, ErrQuotaExhausted) {
			return err
		}
		log.Printf("[err] %s: %v", requestID, err)
		writeAnthropicError(w, requestID, http.StatusBadGateway, "notion API error: "+err.Error(), "api_error")
		return nil
	}

	content := fullContent.String()

	// Empty response: Notion returned 200 but produced no text — retry on next account
	if len(content) == 0 && len(nativeToolUses) == 0 {
		log.Printf("[warn] %s: empty non-stream response from Notion, will retry", requestID)
		return ErrEmptyResponse
	}

	var prepared preparedToolBridgeResponse
	if hasTools {
		prepared = prepareToolBridgeResponse(content, nativeToolUses)
		prepared.ToolCalls = normalizeToolCallPaths(rawMessages, prepared.ToolCalls)
		actionDetected := prepared.HasCalls || prepared.WebSearchQuery != "" || prepared.DoneText != ""
		if !actionDetected && detectToolBridgeNoToolResponse(prepared.Remaining) {
			log.Printf("[bridge] %s detected no-tool identity-drift text (%d chars), requesting clean retry", requestID, len(prepared.Remaining))
			return ErrToolBridgeNoTool
		}
	}

	aUsage := &AnthropicUsage{}
	if finalUsage != nil {
		aUsage.InputTokens = finalUsage.PromptTokens
		aUsage.OutputTokens = finalUsage.CompletionTokens
	}

	var contentBlocks []AnthropicContentBlock
	stopReason := "end_turn"

	// Prepend process-oriented thinking when available, otherwise fall back to raw blocks.
	if hasThinking && thinkingProcess.Len() > 0 {
		sig := thinkingSignature
		if sig == "" {
			sig = generateFakeSignature()
		}
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type:      "thinking",
			Thinking:  thinkingProcess.String(),
			Signature: sig,
		})
	} else if hasThinking && len(thinkingBlocks) > 0 {
		for _, tb := range thinkingBlocks {
			sig := tb.Signature
			if sig == "" {
				sig = generateFakeSignature()
			}
			contentBlocks = append(contentBlocks, AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  tb.Content,
				Signature: sig,
			})
		}
	}

	if hasTools {
		toolCalls := prepared.ToolCalls
		remaining := prepared.Remaining
		hasCalls := prepared.HasCalls
		doneText := prepared.DoneText

		// Intercept WebSearch tool calls → execute via Notion's native search
		if prepared.WebSearchQuery != "" {
			log.Printf("[bridge] WebSearch intercepted — executing via Notion native search: %q", prepared.WebSearchQuery)
			searchResult, searchUsage, searchErr := executeWebSearch(acc, prepared.WebSearchQuery, model, requestID)
			if searchErr == nil && searchResult != "" {
				if doneText != "" {
					doneText = doneText + "\n\n" + searchResult
				} else {
					doneText = searchResult
				}
				if searchUsage != nil && finalUsage != nil {
					finalUsage.PromptTokens += searchUsage.PromptTokens
					finalUsage.CompletionTokens += searchUsage.CompletionTokens
					finalUsage.TotalTokens = finalUsage.PromptTokens + finalUsage.CompletionTokens
				}
			} else if searchErr != nil {
				log.Printf("[bridge] WebSearch execution failed: %v", searchErr)
				if doneText != "" {
					doneText = doneText + "\n\nWeb search failed: " + searchErr.Error()
				} else {
					doneText = "Web search failed: " + searchErr.Error()
				}
			}
		}

		if hasCalls {
			var keptCalls []ToolCall
			for _, tc := range toolCalls {
				keptCalls = append(keptCalls, tc)
			}
			toolCalls = keptCalls
			hasCalls = len(toolCalls) > 0
		}

		// When tool actions were detected, suppress residual framing / identity text.
		if remaining != "" && hasCalls {
			log.Printf("[bridge] suppressed %d chars of residual tool framing text", len(remaining))
		} else if remaining != "" {
			contentBlocks = append(contentBlocks, AnthropicContentBlock{Type: "text", Text: remaining})
		}
		if doneText != "" {
			contentBlocks = append(contentBlocks, AnthropicContentBlock{Type: "text", Text: doneText})
		}
		if hasCalls {
			stopReason = "tool_use"
			for _, tc := range toolCalls {
				contentBlocks = append(contentBlocks, AnthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(tc.Function.Arguments),
				})
			}
		}
	} else {
		if isJSONSchemaOutput(outputConfig) {
			content = normalizeStructuredOutputText(content)
		}
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type: "text",
			Text: cleanCitationsWithContext(content, knownToolCallURLs, knownCitationURLs, knownCitationDocs),
		})
	}

	resp := AnthropicResponse{
		ID:         requestID,
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      model,
		StopReason: &stopReason,
		Usage:      aUsage,
	}

	LogAPIOutputJSON(requestID, "anthropic non-stream response", resp)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	return nil
}

func sendAnthropicSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, data interface{}) {
	raw, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw)
	flusher.Flush()
}

// truncateForLog truncates by rune count to avoid splitting UTF-8 sequences.
func truncateForLog(s string, maxRunes int) string {
	if s == "" || maxRunes <= 0 {
		return ""
	}
	runes := 0
	for i := range s {
		if runes == maxRunes {
			return s[:i] + "..."
		}
		runes++
	}
	return s
}

func strPtr(s string) *string {
	return &s
}

// logAnthropicRequest logs the raw Anthropic request details
func logAnthropicRequest(req AnthropicRequest, model string) {
	if !DebugLoggingEnabled() {
		return
	}

	log.Printf("[debug] ╔══ Anthropic Request ══╗")
	log.Printf("[debug] ║ model=%s max_tokens=%d stream=%v", model, req.MaxTokens, req.Stream)

	// Log system prompt
	if req.System != nil {
		switch s := req.System.(type) {
		case string:
			preview := truncateForLog(s, 200)
			log.Printf("[debug] ║ system(%d chars): %s", len(s), preview)
			if AppConfig.Server.DumpAPIInput {
				os.WriteFile("claude_code_system_dump.txt", []byte(s), 0644)
				log.Printf("[debug] ║ system dumped to claude_code_system_dump.txt")
			}
		case []interface{}:
			log.Printf("[debug] ║ system: %d content blocks", len(s))
			if AppConfig.Server.DumpAPIInput {
				sysDump, _ := json.MarshalIndent(s, "", "  ")
				os.WriteFile("claude_code_system_dump.json", sysDump, 0644)
				log.Printf("[debug] ║ system dumped to claude_code_system_dump.json")
			}
		}
	}

	// Log tools
	if len(req.Tools) > 0 {
		log.Printf("[debug] ║ tools(%d):", len(req.Tools))
		for _, t := range req.Tools {
			log.Printf("[debug] ║   - %s: %s", t.Name, t.Description)
		}
		if AppConfig.Server.DumpAPIInput {
			toolDump, _ := json.MarshalIndent(req.Tools, "", "  ")
			os.WriteFile("claude_code_tools_dump.json", toolDump, 0644)
			log.Printf("[debug] ║ tools dumped to claude_code_tools_dump.json (%d bytes)", len(toolDump))
		}
	}

	// Log tool_choice
	if req.ToolChoice != nil {
		tcRaw, _ := json.Marshal(req.ToolChoice)
		log.Printf("[debug] ║ tool_choice: %s", string(tcRaw))
	}

	// Log messages
	log.Printf("[debug] ║ messages(%d):", len(req.Messages))
	for i, msg := range req.Messages {
		switch content := msg.Content.(type) {
		case string:
			preview := truncateForLog(content, 200)
			log.Printf("[debug] ║   [%d] role=%s text(%d): %s", i, msg.Role, len(content), preview)
		case []interface{}:
			var blockTypes []string
			for _, block := range content {
				if m, ok := block.(map[string]interface{}); ok {
					bt, _ := m["type"].(string)
					switch bt {
					case "tool_use":
						name, _ := m["name"].(string)
						blockTypes = append(blockTypes, fmt.Sprintf("tool_use(%s)", name))
					case "tool_result":
						tuID, _ := m["tool_use_id"].(string)
						blockTypes = append(blockTypes, fmt.Sprintf("tool_result(%s)", tuID))
					case "text":
						text, _ := m["text"].(string)
						text = truncateForLog(text, 80)
						blockTypes = append(blockTypes, fmt.Sprintf("text(%d chars)", len(text)))
					default:
						blockTypes = append(blockTypes, bt)
					}
				}
			}
			log.Printf("[debug] ║   [%d] role=%s blocks=[%s]", i, msg.Role, strings.Join(blockTypes, ", "))
		}
	}
	log.Printf("[debug] ╚═══════════════════════╝")
}

// logConvertedMessages logs the internal ChatMessage format after conversion
func logConvertedMessages(messages []ChatMessage) {
	if !DebugLoggingEnabled() {
		return
	}

	log.Printf("[debug] === Converted to %d internal messages ===", len(messages))
	for i, m := range messages {
		preview := truncateForLog(m.Content, 200)
		extra := ""
		if len(m.ToolCalls) > 0 {
			var names []string
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			extra = fmt.Sprintf(" tool_calls=[%s]", strings.Join(names, ","))
		}
		if m.ToolCallID != "" {
			extra += fmt.Sprintf(" tool_call_id=%s name=%s", m.ToolCallID, m.Name)
		}
		log.Printf("[debug]   [%d] role=%-9s len=%-5d%s: %s", i, m.Role, len(m.Content), extra, preview)
	}
}

// generateFakeSignature creates a synthetic base64 signature for thinking blocks.
// Real Claude API uses cryptographic signatures for integrity verification.
// CC may or may not validate these — if it does, this will need adjustment.
func generateFakeSignature() string {
	b := make([]byte, 96)
	for i := range b {
		b[i] = byte(i*37 + 13) // deterministic pseudo-random fill
	}
	return "EqQB" + base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}

// handleResearcherStream handles streaming Anthropic response for researcher mode.
//
// Two modes depending on whether the client enabled thinking:
//   - hasThinking=true:  thinking block (research steps) → text block (report)
//   - hasThinking=false: single text block (research steps + separator + report)
//
// SSE headers are deferred until the first actual data arrives, so that quota-
// exhaustion retries don't produce duplicate headers.
func handleResearcherStream(w http.ResponseWriter, acc *Account, messages []ChatMessage, model, requestID string, hasThinking bool) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, requestID, http.StatusInternalServerError, "streaming not supported", "api_error")
		return nil
	}

	var finalUsage *UsageInfo
	var thinkingForLog strings.Builder
	var textForLog strings.Builder
	blockIndex := 0
	thinkingBlockOpen := false
	textBlockStarted := false
	headersSent := false
	// When !hasThinking, research steps are streamed as text; this tracks whether
	// a separator has been emitted between steps and the report.
	researchTextEmitted := false

	// ensureHeaders writes SSE headers + message_start on the first data event.
	ensureHeaders := func() {
		if headersSent {
			return
		}
		headersSent = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		sendAnthropicSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            requestID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]interface{}{"input_tokens": 500, "output_tokens": 0},
			},
		})
		sendAnthropicSSE(w, flusher, "ping", map[string]string{"type": "ping"})
	}

	// ensureTextBlock opens a text block if not already open
	ensureTextBlock := func() {
		ensureHeaders()
		if !textBlockStarted {
			sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         blockIndex,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			textBlockStarted = true
		}
	}

	// sendTextDelta sends a text delta SSE event
	sendTextDelta := func(text string) {
		textForLog.WriteString(text)
		sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{"type": "text_delta", "text": text},
		})
	}

	callOpts := CallOptions{
		IsResearcher: true,
		RequestID:    requestID,
	}

	// Set up ThinkingCallback — always needed for researcher mode
	callOpts.ThinkingCallback = func(delta string, done bool, signature string) {
		if hasThinking {
			// Client supports thinking: emit as thinking block
			if done {
				if thinkingBlockOpen {
					sig := signature
					if sig == "" {
						sig = generateFakeSignature()
					}
					sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": blockIndex,
						"delta": map[string]interface{}{"type": "signature_delta", "signature": sig},
					})
					sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
						"type": "content_block_stop", "index": blockIndex,
					})
					blockIndex++
					thinkingBlockOpen = false
					log.Printf("[researcher] closed thinking block (real_sig=%v)", signature != "")
				}
				return
			}
			if delta == "" {
				return
			}
			thinkingForLog.WriteString(delta)
			ensureHeaders()
			if !thinkingBlockOpen {
				sendAnthropicSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":          "content_block_start",
					"index":         blockIndex,
					"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
				})
				thinkingBlockOpen = true
				log.Printf("[researcher] opened thinking block")
			}
			sendAnthropicSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{"type": "thinking_delta", "thinking": delta},
			})
		} else {
			// Client doesn't support thinking: stream research steps as text
			if done {
				// Add separator between research steps and report
				if researchTextEmitted {
					ensureTextBlock()
					sendTextDelta("\n\n---\n\n")
				}
				return
			}
			if delta == "" {
				return
			}
			ensureTextBlock()
			sendTextDelta(delta)
			researchTextEmitted = true
		}
	}

	cbErr := CallInference(acc, messages, model, false, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			ensureTextBlock()
			sendTextDelta(delta)
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	if cbErr != nil {
		if errors.Is(cbErr, ErrQuotaExhausted) || errors.Is(cbErr, ErrResearchQuotaExhausted) {
			return cbErr
		}
		log.Printf("[err] %s researcher: %v", requestID, cbErr)
		if !headersSent {
			writeAnthropicError(w, requestID, http.StatusBadGateway, "notion researcher error: "+cbErr.Error(), "api_error")
		}
		return nil
	}

	// Close text block if started
	if textBlockStarted {
		sendAnthropicSSE(w, flusher, "content_block_stop", map[string]interface{}{
			"type": "content_block_stop", "index": blockIndex,
		})
		blockIndex++
	}

	// message_delta + message_stop
	ensureHeaders()
	outputTokens := 0
	if finalUsage != nil {
		outputTokens = finalUsage.CompletionTokens
	}
	sendAnthropicSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": outputTokens},
	})
	sendAnthropicSSE(w, flusher, "message_stop", map[string]string{"type": "message_stop"})

	var contentBlocks []AnthropicContentBlock
	if hasThinking && thinkingForLog.Len() > 0 {
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type:      "thinking",
			Thinking:  thinkingForLog.String(),
			Signature: generateFakeSignature(),
		})
	}
	if textForLog.Len() > 0 {
		contentBlocks = append(contentBlocks, AnthropicContentBlock{
			Type: "text",
			Text: textForLog.String(),
		})
	}
	LogAPIOutputJSON(requestID, "anthropic researcher stream summary", AnthropicResponse{
		ID:         requestID,
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      model,
		StopReason: strPtr("end_turn"),
		Usage: &AnthropicUsage{
			InputTokens: func() int {
				if finalUsage != nil {
					return finalUsage.PromptTokens
				}
				return 0
			}(),
			OutputTokens: outputTokens,
		},
	})

	log.Printf("[researcher] %s complete: thinking_mode=%v, text_streamed=%v", requestID, hasThinking, textBlockStarted)
	return nil
}

// handleResearcherNonStream handles non-streaming Anthropic response for researcher mode.
// Collects all content first, then returns a complete JSON response.
func handleResearcherNonStream(w http.ResponseWriter, acc *Account, messages []ChatMessage, model, requestID string, hasThinking bool) error {
	var fullContent strings.Builder
	var finalUsage *UsageInfo
	var thinkingBlocks []ThinkingBlock

	callOpts := CallOptions{
		IsResearcher:   true,
		ThinkingBlocks: &thinkingBlocks,
		RequestID:      requestID,
	}

	cbErr := CallInference(acc, messages, model, false, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			fullContent.WriteString(delta)
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	if cbErr != nil {
		if errors.Is(cbErr, ErrQuotaExhausted) || errors.Is(cbErr, ErrResearchQuotaExhausted) {
			return cbErr
		}
		log.Printf("[err] %s researcher non-stream: %v", requestID, cbErr)
		writeAnthropicError(w, requestID, http.StatusBadGateway, "notion researcher error: "+cbErr.Error(), "api_error")
		return nil
	}

	aUsage := &AnthropicUsage{}
	if finalUsage != nil {
		aUsage.InputTokens = finalUsage.PromptTokens
		aUsage.OutputTokens = finalUsage.CompletionTokens
	}

	var contentBlocks []AnthropicContentBlock
	if hasThinking && len(thinkingBlocks) > 0 {
		for _, tb := range thinkingBlocks {
			sig := tb.Signature
			if sig == "" {
				sig = generateFakeSignature()
			}
			contentBlocks = append(contentBlocks, AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  tb.Content,
				Signature: sig,
			})
		}
	}
	contentBlocks = append(contentBlocks, AnthropicContentBlock{
		Type: "text",
		Text: fullContent.String(),
	})

	stopReason := "end_turn"
	resp := AnthropicResponse{
		ID:         requestID,
		Type:       "message",
		Role:       "assistant",
		Content:    contentBlocks,
		Model:      model,
		StopReason: &stopReason,
		Usage:      aUsage,
	}

	LogAPIOutputJSON(requestID, "anthropic researcher non-stream response", resp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)

	log.Printf("[researcher] %s non-stream complete: %d thinking blocks, %d chars text", requestID, len(thinkingBlocks), fullContent.Len())
	return nil
}

func writeAnthropicError(w http.ResponseWriter, requestID string, status int, message, errType string) {
	payload := map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	}
	LogAPIOutputJSON(requestID, fmt.Sprintf("anthropic error status=%d", status), payload)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Session represents an active multi-turn conversation mapped to a Notion thread.
// A thread is bound to the account that created it — subsequent turns must use the same account.
type Session struct {
	ThreadID     string // Notion threadId (generated on first turn, reused)
	TurnCount    int    // completed conversation turns (user+assistant pairs)
	AccountEmail string // bound account (thread is tied to the creating account)
	CreatedAt    time.Time
	LastUsedAt   time.Time

	// Reused transcript entry IDs (generated on first turn, reused on subsequent turns)
	ConfigID  string
	ContextID string

	// Each completed turn produces one updated-config placeholder ID
	UpdatedConfigIDs []string

	// First turn's context.currentDatetime (reused on subsequent turns — NOT updated!)
	OriginalDatetime string

	// Model resolved on first turn (added to config on subsequent turns)
	ModelUsed string

	// Total non-system messages in the Anthropic request at this turn.
	// Used to distinguish chain continuation (count increased) from retry (count unchanged).
	RawMessageCount int
}

// SessionManager manages the mapping from Anthropic API conversation fingerprints to Notion threads.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	ttl      time.Duration
}

// globalSessionManager is the package-level session manager instance
var globalSessionManager *SessionManager

func init() {
	globalSessionManager = NewSessionManager(30 * time.Minute)
}

// NewSessionManager creates a new SessionManager with the given TTL and starts cleanup.
func NewSessionManager(ttl time.Duration) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*Session),
		ttl:      ttl,
	}
	go sm.cleanupLoop()
	return sm
}

// Get retrieves a session by fingerprint, optionally filtering by account email.
// Returns nil if no matching session exists or if the session has expired.
func (sm *SessionManager) Get(fingerprint string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	s, ok := sm.sessions[fingerprint]
	if !ok {
		return nil
	}
	if time.Since(s.LastUsedAt) > sm.ttl {
		return nil
	}
	return s
}

// Set stores a session for the given fingerprint.
func (sm *SessionManager) Set(fingerprint string, session *Session) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.sessions[fingerprint] = session
}

// Delete removes a session by fingerprint.
func (sm *SessionManager) Delete(fingerprint string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, fingerprint)
}

// DeleteByAccount removes all sessions bound to a specific account email.
func (sm *SessionManager) DeleteByAccount(email string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	for fp, s := range sm.sessions {
		if s.AccountEmail == email {
			delete(sm.sessions, fp)
		}
	}
}

// Count returns the number of active sessions.
func (sm *SessionManager) Count() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// cleanupLoop periodically removes expired sessions.
func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sm.mu.Lock()
		now := time.Now()
		removed := 0
		for fp, s := range sm.sessions {
			if now.Sub(s.LastUsedAt) > sm.ttl {
				delete(sm.sessions, fp)
				removed++
			}
		}
		sm.mu.Unlock()
		if removed > 0 {
			log.Printf("[session] cleaned up %d expired sessions, %d remaining", removed, sm.Count())
		}
	}
}

func normalizeSessionSystemContent(content string) string {
	if content == "" {
		return ""
	}
	lines := strings.Split(content, "\n")
	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "x-anthropic-billing-header:") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func normalizeSessionUserContent(content string) string {
	if content == "" {
		return ""
	}
	if isProxySyntheticUserPrompt(content) {
		return strings.TrimSpace(stripSystemReminderBlocks(content))
	}
	return strings.TrimSpace(stripSystemReminders(content))
}

func isProxySyntheticUserPrompt(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "Continue this conversation on a fresh thread.") {
		return true
	}
	if strings.Contains(trimmed, "Output format: {\"name\": \"function_name\", \"arguments\": {...}}") {
		if strings.HasPrefix(trimmed, "Results from executed function(s):") ||
			strings.HasPrefix(trimmed, "I'm writing a unit test") {
			return true
		}
	}
	if strings.Contains(trimmed, "Edit recovery hint:") || strings.Contains(trimmed, "Path recovery hint:") {
		return true
	}
	return false
}

func isMeaningfulUserMessage(msg ChatMessage) bool {
	return msg.Role == "user" && msg.ToolCallID == "" && normalizeSessionUserContent(msg.Content) != ""
}

func shouldCountNonSystemMessage(msg ChatMessage) bool {
	switch msg.Role {
	case "system":
		return false
	case "user":
		return isMeaningfulUserMessage(msg)
	case "assistant":
		return strings.TrimSpace(msg.Content) != "" || len(msg.ToolCalls) > 0
	case "tool":
		return strings.TrimSpace(msg.Content) != "" || msg.ToolCallID != "" || msg.Name != ""
	default:
		return strings.TrimSpace(msg.Content) != ""
	}
}

// computeSessionFingerprintWithSalt generates a fingerprint from the message history
// to identify the same conversation across Anthropic API requests.
// Strategy: hash(optional stable salt + normalized system prompt prefix + first user message prefix).
func computeSessionFingerprintWithSalt(messages []ChatMessage, stableSalt string) string {
	h := sha256.New()
	if stableSalt != "" {
		h.Write([]byte("salt:"))
		h.Write([]byte(stableSalt))
		h.Write([]byte{'\n'})
	}
	// Include system prompt
	for _, m := range messages {
		if m.Role == "system" {
			content := normalizeSessionSystemContent(m.Content)
			if len(content) > 200 {
				content = content[:200]
			}
			h.Write([]byte(content))
			break
		}
	}
	// Include first user message
	for _, m := range messages {
		if isMeaningfulUserMessage(m) {
			content := normalizeSessionUserContent(m.Content)
			if len(content) > 200 {
				content = content[:200]
			}
			h.Write([]byte(content))
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// computeSessionFingerprint keeps the legacy signature for tests/callers that
// do not have an explicit stable salt available.
func computeSessionFingerprint(messages []ChatMessage) string {
	return computeSessionFingerprintWithSalt(messages, "")
}

// countUserMessages counts the number of user-role messages in the list.
func countUserMessages(messages []ChatMessage) int {
	count := 0
	for _, m := range messages {
		if isMeaningfulUserMessage(m) {
			count++
		}
	}
	return count
}

// countNonSystemMessages counts all messages except system-role messages.
// Used for session continuation detection: tool chains add assistant+tool messages
// each turn, while user message count stays constant.
func countNonSystemMessages(messages []ChatMessage) int {
	count := 0
	for _, m := range messages {
		if shouldCountNonSystemMessage(m) {
			count++
		}
	}
	return count
}

// extractLastUserMessage returns the content of the last user message.
func extractLastUserMessage(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if isMeaningfulUserMessage(messages[i]) {
			return normalizeSessionUserContent(messages[i].Content)
		}
	}
	return ""
}

// needsFreshThreadRecovery returns true when the incoming message list carries
// prior conversation state that should be collapsed before starting a new
// Notion thread. Replaying assistant history as a fresh transcript is brittle
// and can lead to empty responses from Notion.
func needsFreshThreadRecovery(messages []ChatMessage) bool {
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if isMeaningfulUserMessage(messages[i]) {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx <= 0 {
		return false
	}
	for i := 0; i < lastUserIdx; i++ {
		if shouldCountNonSystemMessage(messages[i]) {
			return true
		}
	}
	return false
}

// buildFreshThreadRecoveryMessages collapses prior conversation state into a
// single self-contained user prompt for use when we must recover onto a brand
// new Notion thread (for example after session loss or account failover).
func buildRecoveryMessages(messages []ChatMessage, skipEntry func(ChatMessage, string) bool) []ChatMessage {
	if !needsFreshThreadRecovery(messages) {
		return messages
	}

	const (
		maxSystemChars  = 1200
		maxHistoryChars = 4000
		maxEntryChars   = 900
	)

	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if isMeaningfulUserMessage(messages[i]) {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return messages
	}

	clip := func(s string, limit int) string {
		if limit <= 0 || len(s) <= limit {
			return s
		}
		return s[:limit] + "..."
	}

	var systemParts []string
	for _, m := range messages {
		if m.Role == "system" && strings.TrimSpace(m.Content) != "" {
			systemParts = append(systemParts, strings.TrimSpace(m.Content))
		}
	}

	type historyEntry struct {
		label   string
		content string
	}

	var reversed []historyEntry
	usedChars := 0
	for i := lastUserIdx - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == "system" {
			continue
		}

		content := strings.TrimSpace(m.Content)
		if m.Role == "user" {
			content = normalizeSessionUserContent(m.Content)
		}
		if content == "" {
			continue
		}
		if skipEntry != nil && skipEntry(m, content) {
			continue
		}

		label := ""
		switch m.Role {
		case "user":
			label = "User"
		case "assistant":
			label = "Assistant"
		case "tool":
			name := m.Name
			if name == "" {
				name = "tool"
			}
			label = fmt.Sprintf("Tool (%s)", name)
		default:
			continue
		}

		content = clip(content, maxEntryChars)
		entryCost := len(label) + len(content) + 4
		if usedChars > 0 && usedChars+entryCost > maxHistoryChars {
			break
		}
		usedChars += entryCost
		reversed = append(reversed, historyEntry{label: label, content: content})
	}

	var history strings.Builder
	for i := len(reversed) - 1; i >= 0; i-- {
		if history.Len() > 0 {
			history.WriteString("\n\n")
		}
		history.WriteString(reversed[i].label)
		history.WriteString(": ")
		history.WriteString(reversed[i].content)
	}

	latest := normalizeSessionUserContent(messages[lastUserIdx].Content)

	var prompt strings.Builder
	prompt.WriteString("Continue this conversation on a fresh thread.\n")
	prompt.WriteString("Use the context below and answer the latest user message directly.\n")
	prompt.WriteString("Do not mention missing context, prior thread state, or recovery.\n")

	if len(systemParts) > 0 {
		prompt.WriteString("\n\nSystem instructions:\n")
		prompt.WriteString(clip(strings.Join(systemParts, "\n\n"), maxSystemChars))
	}

	if history.Len() > 0 {
		prompt.WriteString("\n\nConversation context:\n")
		prompt.WriteString(history.String())
	}

	prompt.WriteString("\n\nLatest user message:\n")
	prompt.WriteString(latest)

	return []ChatMessage{{
		Role:    "user",
		Content: prompt.String(),
	}}
}

func buildFreshThreadRecoveryMessages(messages []ChatMessage) []ChatMessage {
	return buildRecoveryMessages(messages, nil)
}

func buildToolBridgeRecoveryMessages(messages []ChatMessage) []ChatMessage {
	return buildRecoveryMessages(messages, func(msg ChatMessage, content string) bool {
		return msg.Role == "assistant" && detectToolBridgeNoToolResponse(content)
	})
}

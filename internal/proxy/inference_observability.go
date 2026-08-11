package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"
)

const notionObservationPrefix = "[notion-observe]"

const (
	maxAffectedObservationModels    = 16
	maxAffectedObservationAccounts  = 64
	maxTrackedObservationWorkspaces = 4096
	observationWorkspaceRetention   = 7 * 24 * time.Hour
)

type inferenceDiagnosticState string

type inferenceObservationSourceContextKey struct{}

var inferenceObservationSourceKey inferenceObservationSourceContextKey

type inferenceRequestObservationMetadata struct {
	SourceAPI     string
	CorrelationID string
}

var inferenceRequestObservationRegistry sync.Map // request ID -> inferenceRequestObservationMetadata

const (
	inferenceDiagnosticClosed   inferenceDiagnosticState = "closed"
	inferenceDiagnosticOpen     inferenceDiagnosticState = "open"
	inferenceDiagnosticHalfOpen inferenceDiagnosticState = "half_open"
)

// inferenceWorkspaceObservation is deliberately diagnostic-only. None of the
// fields below are consulted by account selection or failover code.
type inferenceWorkspaceObservation struct {
	state            inferenceDiagnosticState
	emptyStreak      int
	firstEmptyAt     time.Time
	lastEmptyAt      time.Time
	affectedAccounts map[string]struct{}
	accountOverflow  bool
	affectedModels   map[string]struct{}
	modelOverflow    bool
	pendingEmpty     map[string]struct{}
}

type inferenceObservationTracker struct {
	mu         sync.Mutex
	workspaces map[string]*inferenceWorkspaceObservation // keyed by the complete, raw SpaceID
}

func newInferenceObservationTracker() *inferenceObservationTracker {
	return &inferenceObservationTracker{
		workspaces: make(map[string]*inferenceWorkspaceObservation),
	}
}

var globalInferenceObservationTracker = newInferenceObservationTracker()

// notionObservationLogSink is a small test seam which also prevents a slow
// log writer from holding the workspace-state lock.
var notionObservationLogSink = struct {
	sync.RWMutex
	write func(string)
}{
	write: func(line string) { log.Print(line) },
}

type quotaObservationSnapshot struct {
	Available             bool  `json:"available"`
	IsEligible            bool  `json:"is_eligible"`
	SpaceUsage            int   `json:"space_usage"`
	SpaceLimit            int   `json:"space_limit"`
	UserUsage             int   `json:"user_usage"`
	UserLimit             int   `json:"user_limit"`
	LastUsageAtMs         int64 `json:"last_usage_at_ms"`
	ResearchModeUsage     int   `json:"research_mode_usage"`
	HasPremium            bool  `json:"has_premium"`
	PremiumBalance        int   `json:"premium_balance"`
	PremiumUsage          int   `json:"premium_usage"`
	PremiumLimit          int   `json:"premium_limit"`
	TotalCreditBalance    int   `json:"total_credit_balance"`
	CreditsInOverage      int   `json:"credits_in_overage"`
	MonthlyAllocatedUsage int   `json:"monthly_allocated_usage"`
	MonthlyAllocatedLimit int   `json:"monthly_allocated_limit"`
	MonthlyCommittedUsage int   `json:"monthly_committed_usage"`
	MonthlyCommittedLimit int   `json:"monthly_committed_limit"`
	YearlyElasticUsage    int   `json:"yearly_elastic_usage"`
	YearlyElasticLimit    int   `json:"yearly_elastic_limit"`
	V2SpaceUsage          int   `json:"v2_space_usage"`
	V2SpaceLimit          int   `json:"v2_space_limit"`
	V2UserUsage           int   `json:"v2_user_usage"`
	V2UserLimit           int   `json:"v2_user_limit"`
	V2LastUsageAtMs       int64 `json:"v2_last_usage_at_ms"`
}

type inferenceObservationEvent struct {
	ObservedAt              string                   `json:"observed_at"`
	Event                   string                   `json:"event"`
	DiagnosticOnly          bool                     `json:"diagnostic_only"`
	DiagnosticState         inferenceDiagnosticState `json:"diagnostic_state"`
	PreviousState           inferenceDiagnosticState `json:"previous_state"`
	RequestID               string                   `json:"request_id"`
	SourceAPI               string                   `json:"source_api"`
	CorrelationID           string                   `json:"correlation_id"`
	Model                   string                   `json:"model"`
	WorkspaceSHA256         string                   `json:"workspace_sha256"`
	AccountSHA256           string                   `json:"account_sha256"`
	Attempt                 int                      `json:"attempt"`
	Total                   int                      `json:"total"`
	PayloadBytes            int                      `json:"payload_bytes"`
	DurationMS              int64                    `json:"duration_ms"`
	EmptyStreak             int                      `json:"empty_streak"`
	AffectedAccountCount    int                      `json:"affected_account_count"`
	AffectedAccountOverflow bool                     `json:"affected_account_overflow"`
	AffectedModelCount      int                      `json:"affected_model_count"`
	AffectedModelOverflow   bool                     `json:"affected_model_overflow"`
	PendingRequestCount     int                      `json:"pending_empty_request_count"`
	FirstEmptyAt            string                   `json:"first_empty_at,omitempty"`
	LastEmptyAt             string                   `json:"last_empty_at,omitempty"`
	OutageDurationMS        int64                    `json:"outage_duration_ms"`
	Quota                   quotaObservationSnapshot `json:"quota"`
}

type quotaObservationDelta struct {
	SpaceUsage            int   `json:"space_usage"`
	SpaceLimit            int   `json:"space_limit"`
	UserUsage             int   `json:"user_usage"`
	UserLimit             int   `json:"user_limit"`
	LastUsageAtMs         int64 `json:"last_usage_at_ms"`
	ResearchModeUsage     int   `json:"research_mode_usage"`
	PremiumBalance        int   `json:"premium_balance"`
	PremiumUsage          int   `json:"premium_usage"`
	PremiumLimit          int   `json:"premium_limit"`
	TotalCreditBalance    int   `json:"total_credit_balance"`
	CreditsInOverage      int   `json:"credits_in_overage"`
	MonthlyAllocatedUsage int   `json:"monthly_allocated_usage"`
	MonthlyAllocatedLimit int   `json:"monthly_allocated_limit"`
	MonthlyCommittedUsage int   `json:"monthly_committed_usage"`
	MonthlyCommittedLimit int   `json:"monthly_committed_limit"`
	YearlyElasticUsage    int   `json:"yearly_elastic_usage"`
	YearlyElasticLimit    int   `json:"yearly_elastic_limit"`
	V2SpaceUsage          int   `json:"v2_space_usage"`
	V2SpaceLimit          int   `json:"v2_space_limit"`
	V2UserUsage           int   `json:"v2_user_usage"`
	V2UserLimit           int   `json:"v2_user_limit"`
	V2LastUsageAtMs       int64 `json:"v2_last_usage_at_ms"`
	IsEligibleChanged     bool  `json:"is_eligible_changed"`
	HasPremiumChanged     bool  `json:"has_premium_changed"`
}

type quotaObservationEvent struct {
	ObservedAt      string                   `json:"observed_at"`
	Event           string                   `json:"event"`
	DiagnosticOnly  bool                     `json:"diagnostic_only"`
	WorkspaceSHA256 string                   `json:"workspace_sha256"`
	AccountSHA256   string                   `json:"account_sha256"`
	Initial         bool                     `json:"initial"`
	Current         quotaObservationSnapshot `json:"current"`
	Delta           *quotaObservationDelta   `json:"delta,omitempty"`
}

func quotaSnapshot(info *QuotaInfo) quotaObservationSnapshot {
	if info == nil {
		return quotaObservationSnapshot{}
	}
	return quotaObservationSnapshot{
		Available:             true,
		IsEligible:            info.IsEligible,
		SpaceUsage:            info.SpaceUsage,
		SpaceLimit:            info.SpaceLimit,
		UserUsage:             info.UserUsage,
		UserLimit:             info.UserLimit,
		LastUsageAtMs:         info.LastUsageAtMs,
		ResearchModeUsage:     info.ResearchModeUsage,
		HasPremium:            info.HasPremium,
		PremiumBalance:        info.PremiumBalance,
		PremiumUsage:          info.PremiumUsage,
		PremiumLimit:          info.PremiumLimit,
		TotalCreditBalance:    info.TotalCreditBalance,
		CreditsInOverage:      info.CreditsInOverage,
		MonthlyAllocatedUsage: info.MonthlyAllocatedUsage,
		MonthlyAllocatedLimit: info.MonthlyAllocatedLimit,
		MonthlyCommittedUsage: info.MonthlyCommittedUsage,
		MonthlyCommittedLimit: info.MonthlyCommittedLimit,
		YearlyElasticUsage:    info.YearlyElasticUsage,
		YearlyElasticLimit:    info.YearlyElasticLimit,
		V2SpaceUsage:          info.V2SpaceUsage,
		V2SpaceLimit:          info.V2SpaceLimit,
		V2UserUsage:           info.V2UserUsage,
		V2UserLimit:           info.V2UserLimit,
		V2LastUsageAtMs:       info.V2LastUsageAtMs,
	}
}

func quotaDelta(previous, current *QuotaInfo) *quotaObservationDelta {
	if previous == nil || current == nil {
		return nil
	}
	prev := quotaSnapshot(previous)
	curr := quotaSnapshot(current)
	return &quotaObservationDelta{
		SpaceUsage:            curr.SpaceUsage - prev.SpaceUsage,
		SpaceLimit:            curr.SpaceLimit - prev.SpaceLimit,
		UserUsage:             curr.UserUsage - prev.UserUsage,
		UserLimit:             curr.UserLimit - prev.UserLimit,
		LastUsageAtMs:         curr.LastUsageAtMs - prev.LastUsageAtMs,
		ResearchModeUsage:     curr.ResearchModeUsage - prev.ResearchModeUsage,
		PremiumBalance:        curr.PremiumBalance - prev.PremiumBalance,
		PremiumUsage:          curr.PremiumUsage - prev.PremiumUsage,
		PremiumLimit:          curr.PremiumLimit - prev.PremiumLimit,
		TotalCreditBalance:    curr.TotalCreditBalance - prev.TotalCreditBalance,
		CreditsInOverage:      curr.CreditsInOverage - prev.CreditsInOverage,
		MonthlyAllocatedUsage: curr.MonthlyAllocatedUsage - prev.MonthlyAllocatedUsage,
		MonthlyAllocatedLimit: curr.MonthlyAllocatedLimit - prev.MonthlyAllocatedLimit,
		MonthlyCommittedUsage: curr.MonthlyCommittedUsage - prev.MonthlyCommittedUsage,
		MonthlyCommittedLimit: curr.MonthlyCommittedLimit - prev.MonthlyCommittedLimit,
		YearlyElasticUsage:    curr.YearlyElasticUsage - prev.YearlyElasticUsage,
		YearlyElasticLimit:    curr.YearlyElasticLimit - prev.YearlyElasticLimit,
		V2SpaceUsage:          curr.V2SpaceUsage - prev.V2SpaceUsage,
		V2SpaceLimit:          curr.V2SpaceLimit - prev.V2SpaceLimit,
		V2UserUsage:           curr.V2UserUsage - prev.V2UserUsage,
		V2UserLimit:           curr.V2UserLimit - prev.V2UserLimit,
		V2LastUsageAtMs:       curr.V2LastUsageAtMs - prev.V2LastUsageAtMs,
		IsEligibleChanged:     curr.IsEligible != prev.IsEligible,
		HasPremiumChanged:     curr.HasPremium != prev.HasPremium,
	}
}

func quotaDeltaChanged(delta *quotaObservationDelta) bool {
	return delta != nil && *delta != (quotaObservationDelta{})
}

func shortSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func normalizeInferenceSourceAPI(value string) string {
	switch value {
	case "messages", "chat_completions", "responses":
		return value
	default:
		return "messages"
	}
}

func registerInferenceRequestObservation(requestID string, metadata inferenceRequestObservationMetadata) {
	metadata.SourceAPI = normalizeInferenceSourceAPI(metadata.SourceAPI)
	if metadata.CorrelationID == "" {
		metadata.CorrelationID = requestID
	}
	inferenceRequestObservationRegistry.Store(requestID, metadata)
}

func unregisterInferenceRequestObservation(requestID string) {
	inferenceRequestObservationRegistry.Delete(requestID)
}

func inferenceRequestMetadata(requestID string) (inferenceRequestObservationMetadata, bool) {
	value, ok := inferenceRequestObservationRegistry.Load(requestID)
	if !ok {
		return inferenceRequestObservationMetadata{}, false
	}
	metadata, ok := value.(inferenceRequestObservationMetadata)
	return metadata, ok
}

func observationModelLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Only trusted, configured model labels are useful in clear text. A
	// syntactically valid client-supplied model can still be a token or other
	// secret, so unknown values are always reduced to a bounded digest.
	if _, known := safeObservationModelLabels[value]; known {
		return value
	}
	if _, known := anthropicModelAliases[value]; known {
		return value
	}
	for name, notionID := range SnapshotModelMap() {
		if value == name || value == notionID {
			return value
		}
	}
	return "sha256:" + shortSHA256(value)
}

var safeObservationModelLabels = map[string]struct{}{
	"grok-4.5":     {},
	"kimi-k3":      {},
	"gpt-5.6-luna": {},
	"researcher":   {},
}

func isSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func observationIdentity(acc *Account) (spaceID, workspaceHash, accountHash string) {
	if acc == nil {
		return "", shortSHA256(""), shortSHA256("")
	}
	spaceID = acc.SpaceID
	if acc.UserID != "" {
		// AccountID is SHA-256(user_id + NUL + space_id). Deriving it from
		// the canonical tuple keeps the hash stable before and after AccountID
		// is populated.
		accountHash = shortSHA256(acc.UserID + "\x00" + spaceID)
	} else if isSHA256Hex(acc.AccountID) {
		// Persisted AccountID is already a SHA-256 digest.
		accountHash = acc.AccountID[:12]
	} else if acc.AccountID != "" {
		accountHash = shortSHA256(acc.AccountID)
	} else {
		// Accounts constructed in focused tests may not have identity
		// metadata. Email is only a last-resort hash input and is never
		// emitted in plaintext.
		accountHash = shortSHA256(acc.UserEmail + "\x00" + spaceID)
	}
	return spaceID, shortSHA256(spaceID), accountHash
}

func durationMilliseconds(duration time.Duration) int64 {
	if duration <= 0 {
		return 0
	}
	return duration.Milliseconds()
}

func newWorkspaceObservation() *inferenceWorkspaceObservation {
	return &inferenceWorkspaceObservation{
		state:            inferenceDiagnosticClosed,
		affectedAccounts: make(map[string]struct{}),
		affectedModels:   make(map[string]struct{}),
		pendingEmpty:     make(map[string]struct{}),
	}
}

func recordBoundedObservationValue(values map[string]struct{}, value string, limit int) bool {
	if _, exists := values[value]; exists {
		return false
	}
	if len(values) >= limit {
		return true
	}
	values[value] = struct{}{}
	return false
}

func (t *inferenceObservationTracker) pruneLocked(now time.Time, currentSpaceID, event string) {
	for spaceID, state := range t.workspaces {
		if spaceID == currentSpaceID || state.lastEmptyAt.IsZero() || len(state.pendingEmpty) > 0 {
			continue
		}
		if now.Sub(state.lastEmptyAt) > observationWorkspaceRetention {
			delete(t.workspaces, spaceID)
		}
	}
	if event != "attempt_empty" && event != "request_empty" {
		return
	}
	if _, exists := t.workspaces[currentSpaceID]; exists || len(t.workspaces) < maxTrackedObservationWorkspaces {
		return
	}

	// Account configuration can be hot-reloaded. If removed workspaces never
	// recover, evict the oldest diagnostic episode before admitting another so
	// observation state remains bounded and cannot affect serving memory.
	oldestSpaceID := ""
	var oldestAt time.Time
	for spaceID, state := range t.workspaces {
		candidate := state.lastEmptyAt
		if candidate.IsZero() {
			candidate = state.firstEmptyAt
		}
		if oldestSpaceID == "" || candidate.Before(oldestAt) {
			oldestSpaceID = spaceID
			oldestAt = candidate
		}
	}
	if oldestSpaceID != "" {
		delete(t.workspaces, oldestSpaceID)
	}
}

func (t *inferenceObservationTracker) observe(
	event string,
	acc *Account,
	requestID, model string,
	attempt, total, payloadBytes int,
	duration time.Duration,
) inferenceObservationEvent {
	model = observationModelLabel(model)
	spaceID, workspaceHash, accountHash := observationIdentity(acc)
	now := time.Now().UTC()
	quota := quotaObservationSnapshot{}
	if acc != nil {
		quota = quotaSnapshot(acc.quotaInfoSnapshot())
	}
	requestMetadata, _ := inferenceRequestMetadata(requestID)

	t.mu.Lock()
	t.pruneLocked(now, spaceID, event)
	state, exists := t.workspaces[spaceID]
	if !exists {
		state = newWorkspaceObservation()
	}
	previousState := state.state
	logEvent := event

	switch event {
	case "attempt_start":
		if state.state == inferenceDiagnosticOpen {
			state.state = inferenceDiagnosticHalfOpen
		}
	case "attempt_empty":
		if state.firstEmptyAt.IsZero() {
			state.firstEmptyAt = now
		}
		state.lastEmptyAt = now
		state.accountOverflow = recordBoundedObservationValue(state.affectedAccounts, accountHash, maxAffectedObservationAccounts) || state.accountOverflow
		state.modelOverflow = recordBoundedObservationValue(state.affectedModels, model, maxAffectedObservationModels) || state.modelOverflow
		state.pendingEmpty[requestID] = struct{}{}
		t.workspaces[spaceID] = state
	case "request_empty":
		if state.firstEmptyAt.IsZero() {
			state.firstEmptyAt = now
		}
		state.lastEmptyAt = now
		state.accountOverflow = recordBoundedObservationValue(state.affectedAccounts, accountHash, maxAffectedObservationAccounts) || state.accountOverflow
		state.modelOverflow = recordBoundedObservationValue(state.affectedModels, model, maxAffectedObservationModels) || state.modelOverflow
		delete(state.pendingEmpty, requestID)
		state.emptyStreak++
		state.state = inferenceDiagnosticOpen
		t.workspaces[spaceID] = state
	case "success":
		delete(state.pendingEmpty, requestID)
		if len(state.pendingEmpty) > 0 {
			// Another concurrent request has already observed an empty attempt
			// but has not reached its terminal outcome. Do not let this success
			// erase that in-flight episode or manufacture an early recovery.
			t.workspaces[spaceID] = state
			break
		}
		if state.state != inferenceDiagnosticClosed || state.emptyStreak > 0 {
			logEvent = "recovery"
		}
		state.state = inferenceDiagnosticClosed
	case "attempt_aborted":
		if state.state == inferenceDiagnosticHalfOpen && state.emptyStreak > 0 {
			state.state = inferenceDiagnosticOpen
			t.workspaces[spaceID] = state
		}
	case "request_aborted":
		delete(state.pendingEmpty, requestID)
		if state.state == inferenceDiagnosticHalfOpen && state.emptyStreak > 0 {
			state.state = inferenceDiagnosticOpen
			t.workspaces[spaceID] = state
		}
		if state.emptyStreak == 0 && len(state.pendingEmpty) == 0 {
			delete(t.workspaces, spaceID)
		}
	}

	firstEmptyAt := ""
	lastEmptyAt := ""
	var outageDurationMS int64
	if !state.firstEmptyAt.IsZero() {
		firstEmptyAt = state.firstEmptyAt.Format(time.RFC3339Nano)
		lastEmptyAt = state.lastEmptyAt.Format(time.RFC3339Nano)
		outageDurationMS = now.Sub(state.firstEmptyAt).Milliseconds()
	}
	result := inferenceObservationEvent{
		ObservedAt:              now.Format(time.RFC3339Nano),
		Event:                   logEvent,
		DiagnosticOnly:          true,
		DiagnosticState:         state.state,
		PreviousState:           previousState,
		RequestID:               requestID,
		SourceAPI:               requestMetadata.SourceAPI,
		CorrelationID:           requestMetadata.CorrelationID,
		Model:                   model,
		WorkspaceSHA256:         workspaceHash,
		AccountSHA256:           accountHash,
		Attempt:                 attempt,
		Total:                   total,
		PayloadBytes:            payloadBytes,
		DurationMS:              durationMilliseconds(duration),
		EmptyStreak:             state.emptyStreak,
		AffectedAccountCount:    len(state.affectedAccounts),
		AffectedAccountOverflow: state.accountOverflow,
		AffectedModelCount:      len(state.affectedModels),
		AffectedModelOverflow:   state.modelOverflow,
		PendingRequestCount:     len(state.pendingEmpty),
		FirstEmptyAt:            firstEmptyAt,
		LastEmptyAt:             lastEmptyAt,
		OutageDurationMS:        outageDurationMS,
		Quota:                   quota,
	}
	if event == "success" && len(state.pendingEmpty) == 0 {
		// A successful request closes the diagnostic episode. Deleting the
		// workspace bounds memory without losing the recovery event counts.
		delete(t.workspaces, spaceID)
	}
	t.mu.Unlock()

	return result
}

func emitNotionObservation(event any) {
	raw, err := json.Marshal(event)
	if err != nil {
		return
	}
	line := notionObservationPrefix + " " + string(raw)
	notionObservationLogSink.RLock()
	write := notionObservationLogSink.write
	notionObservationLogSink.RUnlock()
	write(line)
}

// logInferenceObservation is shared by parser/HTTP summaries which already
// consist exclusively of safe counters and classifications. Callers must not
// pass request or response bodies; the helper guarantees compact, single-line
// JSON and reserves the common envelope fields.
func logInferenceObservation(event string, fields map[string]interface{}) {
	payload := make(map[string]interface{}, len(fields)+3)
	for key, value := range fields {
		if label, ok := value.(string); ok {
			switch key {
			case "model", "notion_model":
				value = observationModelLabel(label)
			case "content_type":
				value = observationContentType(label)
			case "content_encoding":
				value = observationContentEncoding(label)
			case "retry_after":
				value = observationRetryAfter(label)
			}
		}
		payload[key] = value
	}
	if requestID, ok := payload["request_id"].(string); ok {
		if metadata, exists := inferenceRequestMetadata(requestID); exists {
			payload["source_api"] = metadata.SourceAPI
			payload["correlation_id"] = metadata.CorrelationID
		}
	}
	payload["observed_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	payload["event"] = event
	payload["diagnostic_only"] = true
	emitNotionObservation(payload)
}

// ObserveInferenceAttemptStart records an account attempt. When a prior
// logical request for the workspace was empty, this is labelled half_open for
// diagnostics only; it never gates the attempt.
func (p *AccountPool) ObserveInferenceAttemptStart(acc *Account, requestID, model string, attempt, total, payloadBytes int) {
	event := globalInferenceObservationTracker.observe("attempt_start", acc, requestID, model, attempt, total, payloadBytes, 0)
	if event.DiagnosticState == inferenceDiagnosticHalfOpen || event.PreviousState != event.DiagnosticState {
		emitNotionObservation(event)
	}
}

// ObserveInferenceAttemptEmpty records one account returning an empty semantic
// result. The logical empty streak is incremented only by RequestEmpty.
func (p *AccountPool) ObserveInferenceAttemptEmpty(acc *Account, requestID, model string, attempt, total, payloadBytes int, duration time.Duration) {
	event := globalInferenceObservationTracker.observe("attempt_empty", acc, requestID, model, attempt, total, payloadBytes, duration)
	emitNotionObservation(event)
}

// ObserveInferenceRequestEmpty records that every account attempt for one
// logical request was empty.
func (p *AccountPool) ObserveInferenceRequestEmpty(acc *Account, requestID, model string, attempt, total, payloadBytes int, duration time.Duration) {
	event := globalInferenceObservationTracker.observe("request_empty", acc, requestID, model, attempt, total, payloadBytes, duration)
	emitNotionObservation(event)
}

// ObserveInferenceSuccess records a successful semantic result. It emits a
// recovery event when the workspace was open/half_open, then discards the
// episode state so the tracker cannot influence or grow with normal routing.
func (p *AccountPool) ObserveInferenceSuccess(acc *Account, requestID, model string, attempt, total, payloadBytes int, duration time.Duration) {
	event := globalInferenceObservationTracker.observe("success", acc, requestID, model, attempt, total, payloadBytes, duration)
	if event.Event == "recovery" {
		emitNotionObservation(event)
	}
}

// ObserveInferenceAttemptAborted returns a diagnostic half-open probe to open
// when an attempt ends without either a semantic success or an empty result.
// It does not clear logical-request pending markers and never affects routing.
func (p *AccountPool) ObserveInferenceAttemptAborted(acc *Account, requestID, model string) {
	globalInferenceObservationTracker.observe("attempt_aborted", acc, requestID, model, 0, 0, 0, 0)
}

// ObserveInferenceRequestAborted removes an in-flight empty marker when the
// logical request terminates for a different error. It emits nothing and never
// changes routing.
func (p *AccountPool) ObserveInferenceRequestAborted(acc *Account, requestID, model string) {
	globalInferenceObservationTracker.observe("request_aborted", acc, requestID, model, 0, 0, 0, 0)
}

// logQuotaObservation emits every safe numeric field from the V1/V2 quota
// snapshot plus deltas. It intentionally accepts no request content and never
// logs account email, tokens, or raw SpaceID.
func logQuotaObservation(acc *Account, _ *QuotaInfo, current *QuotaInfo) {
	if acc == nil {
		return
	}
	acc.quotaObservationMu.Lock()
	defer acc.quotaObservationMu.Unlock()
	logQuotaObservationLocked(acc, current)
}

// logQuotaObservationLocked requires acc.quotaObservationMu. Keeping quota
// apply and emission under this per-account sequence lock prevents concurrent
// refreshes from publishing reversed snapshots or deltas without holding the
// main account mutex during log I/O.
func logQuotaObservationLocked(acc *Account, current *QuotaInfo) {
	_, workspaceHash, accountHash := observationIdentity(acc)
	acc.mu.Lock()
	initial := !acc.quotaObservationSeen
	previousObserved := cloneQuotaInfo(acc.quotaObservationInfo)
	currentObserved := cloneQuotaInfo(current)
	if !initial && quotaSnapshot(previousObserved) == quotaSnapshot(currentObserved) {
		acc.mu.Unlock()
		return
	}
	acc.quotaObservationSeen = true
	acc.quotaObservationInfo = currentObserved
	acc.mu.Unlock()

	var delta *quotaObservationDelta
	if !initial {
		delta = quotaDelta(previousObserved, currentObserved)
	}
	emitNotionObservation(quotaObservationEvent{
		ObservedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		Event:           "quota_snapshot",
		DiagnosticOnly:  true,
		WorkspaceSHA256: workspaceHash,
		AccountSHA256:   accountHash,
		Initial:         initial,
		Current:         quotaSnapshot(currentObserved),
		Delta:           delta,
	})
}

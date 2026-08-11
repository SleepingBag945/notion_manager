package proxy

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInferenceObservationStateTransitions(t *testing.T) {
	tracker := newInferenceObservationTracker()
	first := &Account{
		UserID:    "user-a",
		AccountID: "account-a",
		SpaceID:   "complete-workspace-id",
		QuotaInfo: &QuotaInfo{SpaceUsage: 11, TotalCreditBalance: 22},
	}
	second := &Account{
		UserID:    "user-b",
		AccountID: "account-b",
		SpaceID:   first.SpaceID,
		QuotaInfo: &QuotaInfo{SpaceUsage: 11, TotalCreditBalance: 22},
	}

	start := tracker.observe("attempt_start", first, "req-1", "grok-4.5", 1, 2, 123, 0)
	if start.DiagnosticState != inferenceDiagnosticClosed || start.EmptyStreak != 0 {
		t.Fatalf("initial attempt state = %s streak=%d, want closed/0", start.DiagnosticState, start.EmptyStreak)
	}
	firstEmpty := tracker.observe("attempt_empty", first, "req-1", "grok-4.5", 1, 2, 123, 25*time.Millisecond)
	secondEmpty := tracker.observe("attempt_empty", second, "req-1", "grok-4.5", 2, 2, 123, 30*time.Millisecond)
	if firstEmpty.EmptyStreak != 0 || secondEmpty.EmptyStreak != 0 {
		t.Fatalf("account attempts must not increment logical streak: %d, %d", firstEmpty.EmptyStreak, secondEmpty.EmptyStreak)
	}
	if secondEmpty.AffectedAccountCount != 2 || secondEmpty.AffectedModelCount != 1 {
		t.Fatalf("affected counts = accounts:%d models:%d, want 2/1", secondEmpty.AffectedAccountCount, secondEmpty.AffectedModelCount)
	}

	requestEmpty := tracker.observe("request_empty", second, "req-1", "grok-4.5", 2, 2, 123, 55*time.Millisecond)
	if requestEmpty.DiagnosticState != inferenceDiagnosticOpen || requestEmpty.PreviousState != inferenceDiagnosticClosed {
		t.Fatalf("request-empty transition = %s -> %s, want closed -> open", requestEmpty.PreviousState, requestEmpty.DiagnosticState)
	}
	if requestEmpty.EmptyStreak != 1 {
		t.Fatalf("logical empty streak = %d, want 1", requestEmpty.EmptyStreak)
	}

	probe := tracker.observe("attempt_start", first, "req-2", "kimi-k3", 1, 2, 456, 0)
	if probe.PreviousState != inferenceDiagnosticOpen || probe.DiagnosticState != inferenceDiagnosticHalfOpen {
		t.Fatalf("probe transition = %s -> %s, want open -> half_open", probe.PreviousState, probe.DiagnosticState)
	}
	if probe.EmptyStreak != 1 {
		t.Fatalf("probe changed logical streak to %d", probe.EmptyStreak)
	}

	recovery := tracker.observe("success", first, "req-2", "kimi-k3", 1, 2, 456, 40*time.Millisecond)
	if recovery.Event != "recovery" || recovery.PreviousState != inferenceDiagnosticHalfOpen || recovery.DiagnosticState != inferenceDiagnosticClosed {
		t.Fatalf("recovery event/transition = %q %s -> %s", recovery.Event, recovery.PreviousState, recovery.DiagnosticState)
	}
	if recovery.EmptyStreak != 1 || recovery.AffectedAccountCount != 2 || recovery.AffectedModelCount != 1 {
		t.Fatalf("recovery lost episode context: %+v", recovery)
	}
	if len(tracker.workspaces) != 0 {
		t.Fatalf("successful recovery retained %d workspace states", len(tracker.workspaces))
	}

	normalSuccess := tracker.observe("success", first, "req-3", "kimi-k3", 1, 2, 12, time.Millisecond)
	if normalSuccess.Event != "success" || normalSuccess.EmptyStreak != 0 {
		t.Fatalf("normal success = %q streak=%d, want success/0", normalSuccess.Event, normalSuccess.EmptyStreak)
	}
}

func TestInferenceObservationConcurrentAggregation(t *testing.T) {
	tracker := newInferenceObservationTracker()
	const (
		accountCount = 8
		requestCount = 240
	)
	accounts := make([]*Account, accountCount)
	for i := range accounts {
		accounts[i] = &Account{
			AccountID: "account-" + string(rune('a'+i)),
			SpaceID:   "shared-full-space-id",
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			acc := accounts[i%len(accounts)]
			model := []string{"grok-4.5", "kimi-k3", "gpt-5.6-luna"}[i%3]
			tracker.observe("attempt_empty", acc, "request", model, 1, 1, i, time.Millisecond)
			tracker.observe("request_empty", acc, "request", model, 1, 1, i, 2*time.Millisecond)
		}(i)
	}
	wg.Wait()

	tracker.mu.Lock()
	state := tracker.workspaces[accounts[0].SpaceID]
	tracker.mu.Unlock()
	if state == nil {
		t.Fatal("shared workspace aggregation missing")
	}
	if state.state != inferenceDiagnosticOpen || state.emptyStreak != requestCount {
		t.Fatalf("concurrent state = %s streak=%d, want open/%d", state.state, state.emptyStreak, requestCount)
	}
	if len(state.affectedAccounts) != accountCount || len(state.affectedModels) != 3 {
		t.Fatalf("concurrent affected counts = accounts:%d models:%d, want %d/3", len(state.affectedAccounts), len(state.affectedModels), accountCount)
	}
}

func TestInferenceObservationBoundsAffectedModels(t *testing.T) {
	tracker := newInferenceObservationTracker()
	acc := &Account{UserID: "user", SpaceID: "bounded-model-workspace"}
	for i := 0; i < maxAffectedObservationModels+100; i++ {
		tracker.observe("attempt_empty", acc, "request", "untrusted-model-"+string(rune(i+256)), 1, 1, 1, time.Millisecond)
	}
	state := tracker.workspaces[acc.SpaceID]
	if state == nil || len(state.affectedModels) != maxAffectedObservationModels || !state.modelOverflow {
		t.Fatalf("bounded model state = %+v", state)
	}
}

func TestInferenceObservationBoundsAffectedAccounts(t *testing.T) {
	tracker := newInferenceObservationTracker()
	for i := 0; i < maxAffectedObservationAccounts+100; i++ {
		acc := &Account{UserID: "user-" + string(rune(i+256)), SpaceID: "bounded-account-workspace"}
		tracker.observe("attempt_empty", acc, "request", "grok-4.5", 1, 1, 1, time.Millisecond)
	}
	state := tracker.workspaces["bounded-account-workspace"]
	if state == nil || len(state.affectedAccounts) != maxAffectedObservationAccounts || !state.accountOverflow {
		t.Fatalf("bounded account state = %+v", state)
	}
}

func TestInferenceObservationPrunesExpiredAndBoundsWorkspaces(t *testing.T) {
	tracker := newInferenceObservationTracker()
	now := time.Now().UTC()
	tracker.workspaces["expired"] = &inferenceWorkspaceObservation{
		state: inferenceDiagnosticOpen, lastEmptyAt: now.Add(-observationWorkspaceRetention - time.Minute),
		affectedAccounts: map[string]struct{}{}, affectedModels: map[string]struct{}{}, pendingEmpty: map[string]struct{}{},
	}
	for i := 0; i < maxTrackedObservationWorkspaces; i++ {
		spaceID := "workspace-" + string(rune(i+256))
		tracker.workspaces[spaceID] = &inferenceWorkspaceObservation{
			state: inferenceDiagnosticOpen, lastEmptyAt: now,
			affectedAccounts: map[string]struct{}{}, affectedModels: map[string]struct{}{}, pendingEmpty: map[string]struct{}{},
		}
	}
	acc := &Account{UserID: "new-user", SpaceID: "new-workspace"}
	tracker.observe("attempt_empty", acc, "request", "grok-4.5", 1, 1, 1, time.Millisecond)
	if _, exists := tracker.workspaces["expired"]; exists {
		t.Fatal("expired workspace was not pruned")
	}
	if _, exists := tracker.workspaces[acc.SpaceID]; !exists {
		t.Fatal("new workspace was not admitted")
	}
	if len(tracker.workspaces) > maxTrackedObservationWorkspaces {
		t.Fatalf("workspace state grew to %d", len(tracker.workspaces))
	}
}

func TestObservationModelLabelHashesUnknownTokenLikeValue(t *testing.T) {
	raw := "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"
	if got, want := observationModelLabel(raw), "sha256:"+shortSHA256(raw); got != want {
		t.Fatalf("observationModelLabel() = %q, want %q", got, want)
	}
	if got := observationModelLabel("grok-4.5"); got != "grok-4.5" {
		t.Fatalf("known route label was hidden: %q", got)
	}
}

func TestInferenceObservationAbortedHalfOpenAttemptReturnsToOpen(t *testing.T) {
	tracker := newInferenceObservationTracker()
	acc := &Account{UserID: "user", SpaceID: "workspace"}
	tracker.observe("request_empty", acc, "failed", "grok-4.5", 1, 1, 1, time.Millisecond)
	probe := tracker.observe("attempt_start", acc, "probe", "grok-4.5", 1, 1, 1, 0)
	if probe.DiagnosticState != inferenceDiagnosticHalfOpen {
		t.Fatalf("probe state = %s", probe.DiagnosticState)
	}
	aborted := tracker.observe("attempt_aborted", acc, "probe", "grok-4.5", 1, 1, 1, time.Millisecond)
	if aborted.DiagnosticState != inferenceDiagnosticOpen || tracker.workspaces[acc.SpaceID].state != inferenceDiagnosticOpen {
		t.Fatalf("aborted probe state = %+v", aborted)
	}
}

func TestInferenceObservationEventCarriesSourceCorrelation(t *testing.T) {
	tracker := newInferenceObservationTracker()
	acc := &Account{UserID: "user", SpaceID: "workspace"}
	requestID := "msg_source"
	registerInferenceRequestObservation(requestID, inferenceRequestObservationMetadata{
		SourceAPI: "chat_completions", CorrelationID: "chatcmpl_source",
	})
	t.Cleanup(func() { unregisterInferenceRequestObservation(requestID) })
	event := tracker.observe("attempt_empty", acc, requestID, "grok-4.5", 1, 1, 1, time.Millisecond)
	if event.SourceAPI != "chat_completions" || event.CorrelationID != "chatcmpl_source" {
		t.Fatalf("source correlation missing: %+v", event)
	}
}

func TestInferenceObservationKeepsWorkspacesIsolatedAndDoesNotRoute(t *testing.T) {
	tracker := newInferenceObservationTracker()
	firstWorkspace := &Account{UserID: "first-user", SpaceID: "first-workspace"}
	secondWorkspace := &Account{UserID: "second-user", SpaceID: "second-workspace"}
	tracker.observe("request_empty", firstWorkspace, "request-1", "grok-4.5", 1, 1, 1, time.Millisecond)
	tracker.observe("request_empty", firstWorkspace, "request-2", "grok-4.5", 1, 1, 1, time.Millisecond)
	tracker.observe("request_empty", secondWorkspace, "request-3", "kimi-k3", 1, 1, 1, time.Millisecond)
	if len(tracker.workspaces) != 2 {
		t.Fatalf("workspace states = %d, want 2", len(tracker.workspaces))
	}
	if tracker.workspaces[firstWorkspace.SpaceID].emptyStreak != 2 || tracker.workspaces[secondWorkspace.SpaceID].emptyStreak != 1 {
		t.Fatalf("workspace streaks were not isolated: first=%d second=%d",
			tracker.workspaces[firstWorkspace.SpaceID].emptyStreak,
			tracker.workspaces[secondWorkspace.SpaceID].emptyStreak)
	}

	low := &Account{
		UserID:    "low-user",
		SpaceID:   "routing-workspace-low",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 100, SpaceUsage: 90, UserLimit: 100, UserUsage: 90},
	}
	high := &Account{
		UserID:    "high-user",
		SpaceID:   "routing-workspace-high",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 100, SpaceUsage: 10, UserLimit: 100, UserUsage: 10},
	}
	pool := &AccountPool{accounts: []*Account{low, high}}
	if got := pool.NextBest(); got != high {
		t.Fatalf("routing baseline selected %p, want high-quota account %p", got, high)
	}
	pool.ObserveInferenceAttemptStart(low, "request", "grok-4.5", 1, 2, 10)
	pool.ObserveInferenceAttemptEmpty(low, "request", "grok-4.5", 1, 2, 10, time.Millisecond)
	pool.ObserveInferenceRequestEmpty(low, "request", "grok-4.5", 2, 2, 10, 2*time.Millisecond)
	if got := pool.NextBest(); got != high {
		t.Fatalf("diagnostic observation changed routing: got %p, want %p", got, high)
	}
}

func TestObservationIdentityHashIsStableAcrossAccountIDPopulation(t *testing.T) {
	const (
		userID  = "stable-user"
		spaceID = "stable-workspace"
	)
	withoutPersistedID := &Account{UserID: userID, SpaceID: spaceID}
	withPersistedID := &Account{UserID: userID, SpaceID: spaceID, AccountID: ComputeAccountID(userID, spaceID)}
	_, firstWorkspaceHash, firstAccountHash := observationIdentity(withoutPersistedID)
	_, secondWorkspaceHash, secondAccountHash := observationIdentity(withPersistedID)
	if firstWorkspaceHash != secondWorkspaceHash || firstAccountHash != secondAccountHash {
		t.Fatalf("identity hashes changed after AccountID population: workspace %q/%q account %q/%q",
			firstWorkspaceHash, secondWorkspaceHash, firstAccountHash, secondAccountHash)
	}
	if firstAccountHash != ComputeAccountID(userID, spaceID)[:12] {
		t.Fatalf("account hash = %q, want canonical AccountID prefix %q", firstAccountHash, ComputeAccountID(userID, spaceID)[:12])
	}
}

func TestInferenceObservationLogsAreSingleLineAndPrivate(t *testing.T) {
	var (
		logsMu sync.Mutex
		lines  []string
	)
	notionObservationLogSink.Lock()
	previousWriter := notionObservationLogSink.write
	notionObservationLogSink.write = func(line string) {
		logsMu.Lock()
		lines = append(lines, line)
		logsMu.Unlock()
	}
	notionObservationLogSink.Unlock()
	t.Cleanup(func() {
		notionObservationLogSink.Lock()
		notionObservationLogSink.write = previousWriter
		notionObservationLogSink.Unlock()
	})

	const (
		rawSpace = "176faced-secret-complete-space-id"
		rawEmail = "private-user@example.com"
		rawToken = "token_v2=do-not-log-this"
		rawUser  = "secret-user-id"
		rawName  = "prompt text must never be logged"
	)
	previousQuota := &QuotaInfo{
		IsEligible:            true,
		SpaceUsage:            100,
		SpaceLimit:            300,
		UserUsage:             20,
		UserLimit:             75,
		LastUsageAtMs:         1_000,
		ResearchModeUsage:     4,
		HasPremium:            true,
		PremiumBalance:        40,
		PremiumUsage:          50,
		PremiumLimit:          300,
		TotalCreditBalance:    40,
		CreditsInOverage:      2,
		MonthlyAllocatedUsage: 50,
		MonthlyAllocatedLimit: 300,
		MonthlyCommittedUsage: 60,
		MonthlyCommittedLimit: 400,
		YearlyElasticUsage:    70,
		YearlyElasticLimit:    500,
		V2SpaceUsage:          101,
		V2SpaceLimit:          301,
		V2UserUsage:           21,
		V2UserLimit:           76,
		V2LastUsageAtMs:       1_100,
	}
	currentQuota := cloneQuotaInfo(previousQuota)
	currentQuota.SpaceUsage = 104
	currentQuota.LastUsageAtMs = 2_000
	currentQuota.TotalCreditBalance = 36
	currentQuota.MonthlyCommittedUsage = 66
	currentQuota.YearlyElasticUsage = 77
	currentQuota.V2SpaceUsage = 105
	currentQuota.V2UserUsage = 25
	currentQuota.V2LastUsageAtMs = 2_100

	acc := &Account{
		TokenV2:              rawToken,
		FullCookie:           rawToken + "; cookie=secret",
		UserID:               rawUser,
		UserName:             rawName,
		UserEmail:            rawEmail,
		SpaceID:              rawSpace,
		AccountID:            "raw-account-id",
		QuotaInfo:            currentQuota,
		quotaObservationSeen: true,
		quotaObservationInfo: cloneQuotaInfo(previousQuota),
	}
	pool := NewAccountPool()
	pool.ObserveInferenceAttemptStart(acc, "req-safe", "grok-4.5", 1, 2, 4096)
	pool.ObserveInferenceAttemptEmpty(acc, "req-safe", "grok-4.5", 1, 2, 4096, 1500*time.Microsecond)
	pool.ObserveInferenceRequestEmpty(acc, "req-safe", "grok-4.5", 2, 2, 4096, 12*time.Millisecond)
	pool.ObserveInferenceSuccess(acc, "req-recovery", "grok-4.5", 1, 2, 128, 20*time.Millisecond)
	logQuotaObservation(acc, previousQuota, currentQuota)

	logsMu.Lock()
	captured := append([]string(nil), lines...)
	logsMu.Unlock()
	if len(captured) != 4 {
		t.Fatalf("captured %d observation lines, want 4", len(captured))
	}
	for _, line := range captured {
		if !strings.HasPrefix(line, notionObservationPrefix+" {") {
			t.Fatalf("line lacks observation prefix: %q", line)
		}
		if strings.ContainsAny(line, "\r\n") {
			t.Fatalf("observation is not single-line: %q", line)
		}
		for _, secret := range []string{rawSpace, rawEmail, rawToken, rawUser, rawName, "raw-account-id"} {
			if strings.Contains(line, secret) {
				t.Fatalf("observation leaked %q: %s", secret, line)
			}
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, notionObservationPrefix+" ")), &decoded); err != nil {
			t.Fatalf("invalid observation JSON: %v: %s", err, line)
		}
		if decoded["workspace_sha256"] != shortSHA256(rawSpace) {
			t.Fatalf("workspace hash = %v, want %s", decoded["workspace_sha256"], shortSHA256(rawSpace))
		}
		wantAccountHash := shortSHA256(rawUser + "\x00" + rawSpace)
		if decoded["account_sha256"] != wantAccountHash {
			t.Fatalf("account hash = %v, want %s", decoded["account_sha256"], wantAccountHash)
		}
	}

	var quotaLog map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(captured[len(captured)-1], notionObservationPrefix+" ")), &quotaLog); err != nil {
		t.Fatal(err)
	}
	current := quotaLog["current"].(map[string]any)
	delta := quotaLog["delta"].(map[string]any)
	if current["monthly_committed_usage"] != float64(66) || current["yearly_elastic_limit"] != float64(500) ||
		current["v2_space_usage"] != float64(105) || current["v2_user_limit"] != float64(76) || current["v2_last_usage_at_ms"] != float64(2_100) {
		t.Fatalf("complete V2 quota fields missing: %v", current)
	}
	if delta["space_usage"] != float64(4) || delta["total_credit_balance"] != float64(-4) || delta["yearly_elastic_usage"] != float64(7) ||
		delta["v2_space_usage"] != float64(4) || delta["v2_user_usage"] != float64(4) || delta["v2_last_usage_at_ms"] != float64(1_000) {
		t.Fatalf("quota deltas incorrect: %v", delta)
	}
}

func TestInferenceObservationConcurrentSuccessDoesNotErasePendingEmpty(t *testing.T) {
	tracker := newInferenceObservationTracker()
	acc := &Account{UserID: "user", SpaceID: "shared-space"}

	tracker.observe("attempt_empty", acc, "req-empty", "grok-4.5", 1, 2, 10, time.Millisecond)
	success := tracker.observe("success", acc, "req-success", "grok-4.5", 1, 2, 10, time.Millisecond)
	if success.Event == "recovery" || success.PendingRequestCount != 1 {
		t.Fatalf("concurrent success cleared pending empty request: %+v", success)
	}
	requestEmpty := tracker.observe("request_empty", acc, "req-empty", "grok-4.5", 2, 2, 10, 2*time.Millisecond)
	if requestEmpty.DiagnosticState != inferenceDiagnosticOpen || requestEmpty.EmptyStreak != 1 {
		t.Fatalf("pending empty request did not open episode: %+v", requestEmpty)
	}
	if requestEmpty.FirstEmptyAt == "" || requestEmpty.FirstEmptyAt != success.FirstEmptyAt {
		t.Fatalf("failure start changed across interleaving: success=%+v empty=%+v", success, requestEmpty)
	}
}

func TestInferenceObservationSameRequestFallbackWithinWorkspaceIsNotRecovery(t *testing.T) {
	tracker := newInferenceObservationTracker()
	first := &Account{UserID: "first-user", SpaceID: "shared-workspace"}
	second := &Account{UserID: "second-user", SpaceID: first.SpaceID}

	tracker.observe("attempt_empty", first, "same-request", "grok-4.5", 1, 2, 10, time.Millisecond)
	success := tracker.observe("success", second, "same-request", "grok-4.5", 2, 2, 10, time.Millisecond)
	if success.Event != "success" || success.DiagnosticState != inferenceDiagnosticClosed || success.PendingRequestCount != 0 {
		t.Fatalf("same-request fallback was reported as recovery or retained pending state: %+v", success)
	}
	if len(tracker.workspaces) != 0 {
		t.Fatalf("same-request fallback retained %d workspace states", len(tracker.workspaces))
	}
}

func useIsolatedGlobalInferenceObservationTracker(t *testing.T) *inferenceObservationTracker {
	t.Helper()
	previous := globalInferenceObservationTracker
	tracker := newInferenceObservationTracker()
	globalInferenceObservationTracker = tracker
	t.Cleanup(func() { globalInferenceObservationTracker = previous })
	return tracker
}

func TestEmptyWorkspaceAttemptsAbortOtherWorkspaceOnSuccess(t *testing.T) {
	tracker := useIsolatedGlobalInferenceObservationTracker(t)
	pool := NewAccountPool()
	failed := &Account{UserID: "failed-user", SpaceID: "workspace-a"}
	succeeded := &Account{UserID: "succeeded-user", SpaceID: "workspace-b"}
	attempts := make(emptyWorkspaceAttempts)

	pool.ObserveInferenceAttemptEmpty(failed, "cross-workspace-request", "grok-4.5", 1, 2, 10, time.Millisecond)
	attempts.record(failed, 1)
	attempts.observeSuccess(pool, succeeded, "cross-workspace-request", "grok-4.5", 2, 2, 10, time.Millisecond)

	tracker.mu.Lock()
	remaining := len(tracker.workspaces)
	tracker.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("cross-workspace success retained %d workspace states, want 0", remaining)
	}
}

func TestEmptyWorkspaceAttemptsOpenEveryWorkspaceWhenAllEmpty(t *testing.T) {
	tracker := useIsolatedGlobalInferenceObservationTracker(t)
	pool := NewAccountPool()
	first := &Account{UserID: "first-user", SpaceID: "workspace-a"}
	second := &Account{UserID: "second-user", SpaceID: "workspace-b"}
	attempts := make(emptyWorkspaceAttempts)

	pool.ObserveInferenceAttemptEmpty(first, "all-empty-request", "grok-4.5", 1, 2, 10, time.Millisecond)
	attempts.record(first, 1)
	pool.ObserveInferenceAttemptEmpty(second, "all-empty-request", "grok-4.5", 2, 2, 10, 2*time.Millisecond)
	attempts.record(second, 2)
	attempts.observeRequestEmpty(pool, "all-empty-request", "grok-4.5", 2, 10, 3*time.Millisecond)

	tracker.mu.Lock()
	firstState := tracker.workspaces[first.SpaceID]
	secondState := tracker.workspaces[second.SpaceID]
	tracker.mu.Unlock()
	for spaceID, state := range map[string]*inferenceWorkspaceObservation{
		first.SpaceID:  firstState,
		second.SpaceID: secondState,
	} {
		if state == nil || state.state != inferenceDiagnosticOpen || state.emptyStreak != 1 || len(state.pendingEmpty) != 0 {
			t.Fatalf("workspace %s terminal state = %+v, want open/streak=1/no pending", spaceID, state)
		}
	}
}

func TestQuotaObservationInitialSnapshot(t *testing.T) {
	current := &QuotaInfo{SpaceUsage: 9, MonthlyAllocatedLimit: 300}
	event := quotaObservationEvent{
		Initial: true,
		Current: quotaSnapshot(current),
		Delta:   quotaDelta(nil, current),
	}
	if !event.Initial || event.Delta != nil || !event.Current.Available {
		t.Fatalf("initial quota event = %+v", event)
	}
}

func TestQuotaObservationEmitsInitialSnapshotWithPreloadedQuota(t *testing.T) {
	events := captureNotionObservations(t)
	quota := &QuotaInfo{IsEligible: true, SpaceUsage: 9, MonthlyAllocatedLimit: 300}
	acc := &Account{UserID: "user", SpaceID: "workspace", QuotaInfo: cloneQuotaInfo(quota)}
	logQuotaObservation(acc, quota, quota)
	event := lastObservationEvent(t, *events, "quota_snapshot")
	if event["initial"] != true {
		t.Fatalf("preloaded quota was not emitted as initial: %#v", event)
	}
	if _, exists := event["delta"]; exists {
		t.Fatalf("initial quota unexpectedly included delta: %#v", event)
	}
}

func TestAddAccountEmitsInitialQuotaOnlyOnce(t *testing.T) {
	events := captureNotionObservations(t)
	acc := &Account{UserID: "user", SpaceID: "workspace", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceUsage: 9}}
	pool := NewAccountPool()
	pool.AddAccount(acc)
	pool.AddAccount(acc)
	count := 0
	for _, event := range *events {
		if event["event"] == "quota_snapshot" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("quota initial events = %d, want 1: %#v", count, *events)
	}
}

func TestConcurrentQuotaObservationsEndAtAppliedSnapshot(t *testing.T) {
	events := captureNotionObservations(t)
	acc := &Account{UserID: "user", SpaceID: "workspace"}
	pool := NewAccountPool()
	const updates = 128
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 1; i <= updates; i++ {
		wg.Add(1)
		go func(usage int) {
			defer wg.Done()
			<-start
			pool.applyQuotaInfo(acc, &QuotaInfo{IsEligible: true, SpaceUsage: usage, SpaceLimit: 300})
		}(i)
	}
	close(start)
	wg.Wait()

	finalQuota := acc.quotaInfoSnapshot()
	if finalQuota == nil {
		t.Fatal("final quota is nil")
	}
	last := lastObservationEvent(t, *events, "quota_snapshot")
	current, ok := last["current"].(map[string]interface{})
	if !ok || current["space_usage"] != float64(finalQuota.SpaceUsage) {
		t.Fatalf("last observation = %#v, final quota = %+v", last, finalQuota)
	}
}

func TestNormalSuccessAndUnchangedQuotaStayQuiet(t *testing.T) {
	var (
		mu    sync.Mutex
		lines []string
	)
	notionObservationLogSink.Lock()
	previousWriter := notionObservationLogSink.write
	notionObservationLogSink.write = func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	}
	notionObservationLogSink.Unlock()
	t.Cleanup(func() {
		notionObservationLogSink.Lock()
		notionObservationLogSink.write = previousWriter
		notionObservationLogSink.Unlock()
	})

	acc := &Account{UserID: "quiet-user", SpaceID: "quiet-space", QuotaInfo: &QuotaInfo{IsEligible: true, SpaceUsage: 1}}
	acc.quotaObservationSeen = true
	acc.quotaObservationInfo = cloneQuotaInfo(acc.QuotaInfo)
	pool := NewAccountPool()
	pool.ObserveInferenceAttemptStart(acc, "quiet-request", "grok-4.5", 1, 1, 20)
	pool.ObserveInferenceSuccess(acc, "quiet-request", "grok-4.5", 1, 1, 20, time.Millisecond)
	unchanged := cloneQuotaInfo(acc.QuotaInfo)
	logQuotaObservation(acc, unchanged, cloneQuotaInfo(unchanged))

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 0 {
		t.Fatalf("normal success/unchanged quota emitted %d observation lines: %v", len(lines), lines)
	}
}

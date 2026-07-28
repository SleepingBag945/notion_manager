package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

// TestHasNoWorkspaceOnlyAfterProbe verifies that an unprobed account is
// treated as usable until a probe records a real result. Otherwise a
// fresh-from-disk pool would refuse to serve traffic on the first boot
// before RefreshAll ever runs.
func TestHasNoWorkspaceOnlyAfterProbe(t *testing.T) {
	pool := NewAccountPool()
	acc := &Account{UserEmail: "u@example.com"}

	if pool.hasNoWorkspace(acc) {
		t.Fatalf("unprobed account should NOT be flagged as no_workspace")
	}

	now := time.Now()
	acc.WorkspaceCheckedAt = &now
	acc.SpaceCount = 0
	if !pool.hasNoWorkspace(acc) {
		t.Fatalf("probed account with SpaceCount=0 should be flagged")
	}

	acc.SpaceCount = 2
	if pool.hasNoWorkspace(acc) {
		t.Fatalf("account with SpaceCount>0 must not be flagged")
	}
}

// TestPickBestSkipsNoWorkspace ensures the dashboard "best" selector
// never returns an account that has been confirmed to lack a workspace,
// even when its quota is healthy.
func TestPickBestSkipsNoWorkspace(t *testing.T) {
	pool := NewAccountPool()
	now := time.Now()
	bad := &Account{
		UserEmail:          "bad@example.com",
		QuotaInfo:          &QuotaInfo{IsEligible: true, SpaceLimit: 100, SpaceUsage: 0, UserLimit: 100, UserUsage: 0},
		WorkspaceCheckedAt: &now,
		SpaceCount:         0,
	}
	good := &Account{
		UserEmail:          "good@example.com",
		QuotaInfo:          &QuotaInfo{IsEligible: true, SpaceLimit: 100, SpaceUsage: 50, UserLimit: 100, UserUsage: 50},
		WorkspaceCheckedAt: &now,
		SpaceCount:         3,
	}
	pool.accounts = []*Account{bad, good}

	for i := 0; i < 5; i++ {
		got := pool.GetBestAccount()
		if got == nil {
			t.Fatalf("iteration %d: GetBestAccount returned nil", i)
		}
		if got.UserEmail == bad.UserEmail {
			t.Fatalf("iteration %d: GetBestAccount returned no-workspace account", i)
		}
	}
}

// TestPickBestAllNoWorkspaceReturnsNil double-checks the edge case where
// every account is bad — we must return nil rather than fall back to a
// known-broken account just to have *something* to redirect to.
func TestPickBestAllNoWorkspaceReturnsNil(t *testing.T) {
	pool := NewAccountPool()
	now := time.Now()
	pool.accounts = []*Account{
		{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 0},
		{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 0},
	}
	if got := pool.GetBestAccount(); got != nil {
		t.Fatalf("expected nil when every account is workspace-less, got %s", got.UserEmail)
	}
}

// TestAvailableCountExcludesNoWorkspace ensures the dashboard summary
// number reflects accounts that can actually serve traffic.
func TestAvailableCountExcludesNoWorkspace(t *testing.T) {
	pool := NewAccountPool()
	now := time.Now()
	pool.accounts = []*Account{
		{UserEmail: "a@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 1},
		{UserEmail: "b@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}, WorkspaceCheckedAt: &now, SpaceCount: 0},
		{UserEmail: "c@example.com", QuotaInfo: &QuotaInfo{IsEligible: false}, WorkspaceCheckedAt: &now, SpaceCount: 1},
	}
	if got := pool.AvailableCount(); got != 1 {
		t.Fatalf("AvailableCount = %d, want 1", got)
	}
}

// TestApplyWorkspaceCountStampsAndSignalsChange covers the first-probe
// vs. transition vs. no-op cases of applyWorkspaceCount so callers know
// when to log.
func TestApplyWorkspaceCountStampsAndSignalsChange(t *testing.T) {
	pool := NewAccountPool()
	acc := &Account{UserEmail: "u@example.com"}

	prev, changed := pool.applyWorkspaceCount(acc, 0)
	if !changed || prev != 0 || acc.SpaceCount != 0 || acc.WorkspaceCheckedAt == nil {
		t.Fatalf("first probe should set checked_at and report changed=true; got prev=%d changed=%v acc=%+v", prev, changed, acc)
	}

	prev, changed = pool.applyWorkspaceCount(acc, 0)
	if changed {
		t.Fatalf("re-probing the same value must not report changed=true (would spam logs)")
	}
	if prev != 0 {
		t.Fatalf("prev = %d, want 0", prev)
	}

	prev, changed = pool.applyWorkspaceCount(acc, 3)
	if !changed || prev != 0 || acc.SpaceCount != 3 {
		t.Fatalf("0→3 transition not recorded: prev=%d changed=%v space_count=%d", prev, changed, acc.SpaceCount)
	}
}

// TestRefreshAndPersistAccountSavesWorkspace ensures the post-register
// flow persists `space_count` / `workspace_checked_at` so a server
// restart immediately remembers which fresh accounts are bad.
func TestRefreshAndPersistAccountSavesWorkspace(t *testing.T) {
	dir := t.TempDir()
	email := "fresh@example.com"
	path := seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{UserEmail: email, SpaceID: "s-" + email, UserID: "u-" + email}}

	restoreFetch := withFetchers(t,
		func(*Account) (*QuotaInfo, error) {
			return &QuotaInfo{IsEligible: true, SpaceUsage: 1, SpaceLimit: 100}, nil
		},
		func(*Account) ([]ModelEntry, error) {
			return []ModelEntry{{ID: "m"}}, nil
		},
	)
	defer restoreFetch()
	restoreProbe := withWorkspaceProbe(t, func(*Account) (int, error) { return 0, nil })
	defer restoreProbe()

	if err := pool.RefreshAndPersistAccount(context.Background(), dir, email); err != nil {
		t.Fatalf("RefreshAndPersistAccount: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sc, ok := got["space_count"].(float64)
	if !ok || int(sc) != 0 {
		t.Fatalf("space_count missing or wrong: %v (raw=%s)", got["space_count"], string(raw))
	}
	if got["workspace_checked_at"].(string) == "" {
		t.Fatalf("workspace_checked_at not persisted")
	}

	// In-memory state must agree.
	if !pool.hasNoWorkspace(pool.accounts[0]) {
		t.Fatalf("hasNoWorkspace should be true after probe")
	}
}

// TestRefreshAndPersistAccountSurvivesProbeError ensures a transient
// loadUserContent failure doesn't poison the persisted snapshot — we
// still want to commit the quota/models we successfully fetched.
func TestRefreshAndPersistAccountSurvivesProbeError(t *testing.T) {
	dir := t.TempDir()
	email := "probefail@example.com"
	path := seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{UserEmail: email, UserID: "u-" + email, SpaceID: "s-" + email}}

	restoreFetch := withFetchers(t,
		func(*Account) (*QuotaInfo, error) {
			return &QuotaInfo{IsEligible: true, SpaceUsage: 0, SpaceLimit: 100}, nil
		},
		func(*Account) ([]ModelEntry, error) { return nil, nil },
	)
	defer restoreFetch()
	restoreProbe := withWorkspaceProbe(t, func(*Account) (int, error) {
		return 0, errors.New("notion 503")
	})
	defer restoreProbe()

	if err := pool.RefreshAndPersistAccount(context.Background(), dir, email); err != nil {
		t.Fatalf("RefreshAndPersistAccount: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]interface{}
	_ = json.Unmarshal(raw, &got)
	if _, ok := got["quota_info"]; !ok {
		t.Fatalf("quota_info should still be persisted on probe failure: %s", string(raw))
	}
	if _, ok := got["space_count"]; ok {
		t.Fatalf("space_count must NOT be persisted on probe failure (would falsely flag account)")
	}
}

// TestLoadFromDirRehydratesWorkspaceProbe verifies the disk → runtime
// round-trip so a server restart remembers which accounts were bad.
func TestLoadFromDirRehydratesWorkspaceProbe(t *testing.T) {
	dir := t.TempDir()
	email := "persist@example.com"
	path := seedAccountFile(t, dir, email)

	// Rewrite the seed with persisted workspace fields.
	raw, _ := os.ReadFile(path)
	var body map[string]interface{}
	_ = json.Unmarshal(raw, &body)
	body["space_count"] = 0
	body["workspace_checked_at"] = time.Now().UTC().Format(time.RFC3339)
	out, _ := json.MarshalIndent(body, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("rewrite seed: %v", err)
	}

	pool := NewAccountPool()
	if err := pool.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if len(pool.accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(pool.accounts))
	}
	acc := pool.accounts[0]
	if acc.WorkspaceCheckedAt == nil || acc.SpaceCount != 0 {
		t.Fatalf("workspace probe not rehydrated: checked_at=%v count=%d", acc.WorkspaceCheckedAt, acc.SpaceCount)
	}
	if !pool.hasNoWorkspace(acc) {
		t.Fatalf("hasNoWorkspace should be true after rehydration")
	}
}

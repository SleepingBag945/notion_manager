package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withFetchers swaps the package-level quota/models/workspace fetchers
// for the duration of a test. It returns a restore func the caller defers.
//
// The seam exists so we can drive RefreshAndPersistAccount without spinning
// up an httptest server (the real Notion path uses a TLS-fingerprinted
// transport that does not target arbitrary hosts).
//
// Tests that don't care about the workspace probe leave it untouched —
// the helper installs a default stub that reports a single accessible
// workspace, matching the "happy path" the existing fixtures expect.
func withFetchers(
	t *testing.T,
	q func(*Account) (*QuotaInfo, error),
	m func(*Account) ([]ModelEntry, error),
) func() {
	t.Helper()
	origQ, origM, origW := quotaFetcher, modelsFetcher, workspaceProbe
	if q != nil {
		quotaFetcher = q
	}
	if m != nil {
		modelsFetcher = m
	}
	workspaceProbe = func(*Account) (int, error) { return 1, nil }
	return func() {
		quotaFetcher = origQ
		modelsFetcher = origM
		workspaceProbe = origW
	}
}

// withWorkspaceProbe overrides only the workspace probe stub for tests
// that exercise the new "no workspace" code paths.
func withWorkspaceProbe(t *testing.T, w func(*Account) (int, error)) func() {
	t.Helper()
	orig := workspaceProbe
	if w != nil {
		workspaceProbe = w
	}
	return func() { workspaceProbe = orig }
}

// seedAccountFile writes a minimal account JSON shaped like what the
// regjob runner produces immediately after a successful login. No
// quota_info / available_models yet — those are added by
// RefreshAndPersistAccount.
func seedAccountFile(t *testing.T, dir, email string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := map[string]interface{}{
		"token_v2":       "tk-" + email,
		"user_id":        "u-" + email,
		"user_name":      "Tester",
		"user_email":     email,
		"space_id":       "s-" + email,
		"space_name":     "Workspace",
		"space_view_id":  "sv-" + email,
		"plan_type":      "free",
		"timezone":       "UTC",
		"client_version": DefaultClientVersion,
		"browser_id":     "bid-" + email,
		"device_id":      "did-" + email,
		"full_cookie":    "cookie-" + email,
		"registered_via": "microsoft",
	}
	data, _ := json.MarshalIndent(body, "", "  ")
	path := filepath.Join(dir, "acct.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return path
}

func TestSaveAccountFilePreservesSessionFieldsAndMergesQuota(t *testing.T) {
	dir := t.TempDir()
	email := "alice@example.com"
	path := seedAccountFile(t, dir, email)

	now := time.Now().UTC().Truncate(time.Second)
	acc := &Account{
		UserEmail: email,
		UserID:    "u-" + email,
		SpaceID:   "s-" + email,
		QuotaInfo: &QuotaInfo{
			IsEligible: true, SpaceUsage: 12, SpaceLimit: 100,
			UserUsage: 5, UserLimit: 50, HasPremium: true,
			PremiumBalance: 80, PremiumUsage: 20, PremiumLimit: 100,
		},
		QuotaCheckedAt: &now,
		Models:         []ModelEntry{{ID: "gpt-5", Name: "GPT-5"}, {ID: "claude-4", Name: "Claude 4"}},
	}

	if err := saveAccountFile(dir, acc); err != nil {
		t.Fatalf("saveAccountFile: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Original session fields must still be there.
	for _, k := range []string{"token_v2", "user_id", "space_id", "space_name", "browser_id", "registered_via"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("session field missing after save: %q", k)
		}
	}
	if got["registered_via"].(string) != "microsoft" {
		t.Fatalf("registered_via clobbered: %v", got["registered_via"])
	}

	// Quota fields must be merged in.
	q, ok := got["quota_info"].(map[string]interface{})
	if !ok {
		t.Fatalf("quota_info missing: %v", got["quota_info"])
	}
	if v, _ := q["is_eligible"].(bool); !v {
		t.Fatalf("quota_info.is_eligible = %v, want true", q["is_eligible"])
	}
	if v, _ := q["space_usage"].(float64); int(v) != 12 {
		t.Fatalf("space_usage: %v", q["space_usage"])
	}
	if v, _ := q["premium_balance"].(float64); int(v) != 80 {
		t.Fatalf("premium_balance: %v", q["premium_balance"])
	}

	models, ok := got["available_models"].([]interface{})
	if !ok || len(models) != 2 {
		t.Fatalf("available_models: %v", got["available_models"])
	}

	if got["quota_checked_at"].(string) == "" {
		t.Fatalf("quota_checked_at should be set")
	}

	// Atomic: no leftover .tmp.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover tmp file: %s", e.Name())
		}
	}
}

func TestSaveAccountFileNoMatchingFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	acc := &Account{UserEmail: "ghost@example.com", QuotaInfo: &QuotaInfo{IsEligible: true}}
	if err := saveAccountFile(dir, acc); err == nil {
		t.Fatalf("expected error when no matching file, got nil")
	}
}

func TestRefreshAndPersistAccountUsesFetchersAndPersists(t *testing.T) {
	dir := t.TempDir()
	email := "bob@example.com"
	path := seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{
		TokenV2:   "tk-" + email,
		UserID:    "u-" + email,
		UserEmail: email,
		SpaceID:   "s-" + email,
	}}

	var fetched atomic.Int32
	restore := withFetchers(t,
		func(acc *Account) (*QuotaInfo, error) {
			fetched.Add(1)
			return &QuotaInfo{
				IsEligible: true, SpaceUsage: 7, SpaceLimit: 100,
				UserUsage: 2, UserLimit: 50,
			}, nil
		},
		func(acc *Account) ([]ModelEntry, error) {
			return []ModelEntry{{ID: "gpt-5", Name: "GPT-5"}}, nil
		},
	)
	defer restore()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := pool.RefreshAndPersistAccount(ctx, dir, email); err != nil {
		t.Fatalf("RefreshAndPersistAccount: %v", err)
	}
	if fetched.Load() == 0 {
		t.Fatalf("quotaFetcher was not invoked")
	}

	// In-memory account got the quota.
	acc := pool.accounts[0]
	if acc.QuotaInfo == nil || !acc.QuotaInfo.IsEligible || acc.QuotaInfo.SpaceUsage != 7 {
		t.Fatalf("in-memory quota not applied: %+v", acc.QuotaInfo)
	}
	if acc.QuotaCheckedAt == nil {
		t.Fatalf("QuotaCheckedAt not set")
	}
	if len(acc.Models) != 1 || acc.Models[0].ID != "gpt-5" {
		t.Fatalf("models not applied: %+v", acc.Models)
	}

	// Disk got the quota too.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["quota_info"]; !ok {
		t.Fatalf("quota_info missing on disk: %s", string(raw))
	}
	if _, ok := got["quota_checked_at"]; !ok {
		t.Fatalf("quota_checked_at missing on disk: %s", string(raw))
	}
	if models, ok := got["available_models"].([]interface{}); !ok || len(models) != 1 {
		t.Fatalf("available_models missing on disk: %v", got["available_models"])
	}
}

func TestRefreshAndPersistAccountUnknownEmailReturnsError(t *testing.T) {
	dir := t.TempDir()
	pool := NewAccountPool()

	restore := withFetchers(t,
		func(acc *Account) (*QuotaInfo, error) { return &QuotaInfo{IsEligible: true}, nil },
		func(acc *Account) ([]ModelEntry, error) { return nil, nil },
	)
	defer restore()

	err := pool.RefreshAndPersistAccount(context.Background(), dir, "nobody@example.com")
	if err == nil {
		t.Fatalf("expected error for unknown email")
	}
}

func TestRefreshAndPersistAccountQuotaFailureDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	email := "carol@example.com"
	path := seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{UserEmail: email, UserID: "u-" + email, SpaceID: "s-" + email}}

	restore := withFetchers(t,
		func(acc *Account) (*QuotaInfo, error) {
			return nil, errors.New("notion 503")
		},
		func(acc *Account) ([]ModelEntry, error) {
			return []ModelEntry{{ID: "should-not-be-saved"}}, nil
		},
	)
	defer restore()

	err := pool.RefreshAndPersistAccount(context.Background(), dir, email)
	if err == nil {
		t.Fatalf("expected error from quota fetcher")
	}

	// File should NOT have quota_info or available_models.
	raw, _ := os.ReadFile(path)
	var got map[string]interface{}
	_ = json.Unmarshal(raw, &got)
	if _, ok := got["quota_info"]; ok {
		t.Fatalf("quota_info should not be persisted on quota-fetch failure: %s", string(raw))
	}
	if _, ok := got["available_models"]; ok {
		t.Fatalf("available_models should not be persisted on quota-fetch failure: %s", string(raw))
	}
}

func TestRefreshAndPersistAccountModelsFailureKeepsQuota(t *testing.T) {
	dir := t.TempDir()
	email := "dan@example.com"
	path := seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{UserEmail: email, UserID: "u-" + email, SpaceID: "s-" + email}}

	restore := withFetchers(t,
		func(acc *Account) (*QuotaInfo, error) {
			return &QuotaInfo{IsEligible: true, SpaceUsage: 1, SpaceLimit: 10}, nil
		},
		func(acc *Account) ([]ModelEntry, error) {
			return nil, errors.New("models 500")
		},
	)
	defer restore()

	if err := pool.RefreshAndPersistAccount(context.Background(), dir, email); err != nil {
		t.Fatalf("RefreshAndPersistAccount: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]interface{}
	_ = json.Unmarshal(raw, &got)
	if _, ok := got["quota_info"]; !ok {
		t.Fatalf("quota_info should be persisted even when models fetch fails: %s", string(raw))
	}
}

func TestRefreshAndPersistAccountIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	email := "eve@example.com"
	_ = seedAccountFile(t, dir, email)

	pool := NewAccountPool()
	pool.accounts = []*Account{{UserEmail: email, UserID: "u-" + email, SpaceID: "s-" + email}}

	restore := withFetchers(t,
		func(acc *Account) (*QuotaInfo, error) {
			return &QuotaInfo{IsEligible: true, SpaceUsage: 1, SpaceLimit: 10}, nil
		},
		func(acc *Account) ([]ModelEntry, error) {
			return []ModelEntry{{ID: "m"}}, nil
		},
	)
	defer restore()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pool.RefreshAndPersistAccount(context.Background(), dir, email)
		}()
	}
	wg.Wait()
}

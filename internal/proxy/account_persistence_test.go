package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helpers

func makeTestAccount(userID, email, spaceID, spaceName string, spaceUsage, spaceLimit int) *Account {
	acc := &Account{
		TokenV2:       "tok-" + spaceID,
		UserID:        userID,
		UserEmail:     email,
		UserName:      "TestUser",
		SpaceID:       spaceID,
		SpaceName:     spaceName,
		PlanType:      "plus",
		BrowserID:     "b-" + spaceID,
		DeviceID:      "d-" + spaceID,
		ClientVersion: DefaultClientVersion,
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceUsage: spaceUsage,
			SpaceLimit: spaceLimit,
		},
	}
	now := time.Now()
	acc.QuotaCheckedAt = &now
	acc.EnsureAccountID()
	return acc
}

func readJSONFile(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func countJSONFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			n++
		}
	}
	return n
}

func jsonFilePaths(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

func findFileByAccountID(t *testing.T, dir, accountID string) string {
	t.Helper()
	for _, p := range jsonFilePaths(t, dir) {
		m := readJSONFile(t, p)
		aid, _ := m["account_id"].(string)
		if aid == "" {
			uid, _ := m["user_id"].(string)
			sid, _ := m["space_id"].(string)
			if uid != "" && sid != "" {
				aid = ComputeAccountID(uid, sid)
			}
		}
		if aid == accountID {
			return p
		}
	}
	return ""
}

func getQuotaUsage(m map[string]interface{}) int {
	qi, ok := m["quota_info"].(map[string]interface{})
	if !ok {
		return -1
	}
	v, _ := qi["space_usage"].(float64)
	return int(v)
}

// ── Test: same email + different space_id → two JSON files ──

func TestSaveAccountToFile_TwoWorkspaces_SameEmail(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "Workspace A", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "Workspace B", 200, 600)

	if accA.AccountID == accB.AccountID {
		t.Fatal("account_ids must differ for different space_ids")
	}

	// Save via dashboard path
	fnA, err := SaveAccountToFile(accA, dir)
	if err != nil {
		t.Fatalf("save A: %v", err)
	}
	fnB, err := SaveAccountToFile(accB, dir)
	if err != nil {
		t.Fatalf("save B: %v", err)
	}

	// Filenames must differ
	if fnA == fnB {
		t.Fatalf("filenames must differ: both are %q", fnA)
	}

	// Must be exactly 2 files on disk
	if n := countJSONFiles(t, dir); n != 2 {
		t.Fatalf("want 2 JSON files, got %d", n)
	}

	// Each file contains correct account_id
	pathA := findFileByAccountID(t, dir, accA.AccountID)
	pathB := findFileByAccountID(t, dir, accB.AccountID)
	if pathA == "" {
		t.Fatal("no file matches account_id A")
	}
	if pathB == "" {
		t.Fatal("no file matches account_id B")
	}
	if pathA == pathB {
		t.Fatal("both account_ids resolve to same file")
	}

	// Quota values are distinct and correct
	mA := readJSONFile(t, pathA)
	mB := readJSONFile(t, pathB)
	if getQuotaUsage(mA) != 100 {
		t.Errorf("file A space_usage: want 100, got %d", getQuotaUsage(mA))
	}
	if getQuotaUsage(mB) != 200 {
		t.Errorf("file B space_usage: want 200, got %d", getQuotaUsage(mB))
	}

	// Filenames must not contain secrets
	for _, fn := range []string{fnA, fnB} {
		for _, secret := range []string{accA.TokenV2, accB.TokenV2, accA.UserID, accA.SpaceID, accB.SpaceID} {
			if strings.Contains(fn, secret) {
				t.Errorf("filename %q leaks secret %q", fn, secret)
			}
		}
	}
}

// ── Test: SaveAccounts preserves both files after periodic refresh ──

func TestSaveAccounts_TwoWorkspaces_SameEmail(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "Workspace A", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "Workspace B", 200, 600)

	// Save both via dashboard path
	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)

	if n := countJSONFiles(t, dir); n != 2 {
		t.Fatalf("precondition: want 2 files, got %d", n)
	}

	// Simulate quota change, then periodic save
	accA.QuotaInfo.SpaceUsage = 150
	accB.QuotaInfo.SpaceUsage = 250

	pool := &AccountPool{}
	pool.AddAccount(accA)
	pool.AddAccount(accB)

	pool.SaveAccounts(dir)

	// Must still have exactly 2 files
	if n := countJSONFiles(t, dir); n != 2 {
		t.Fatalf("after SaveAccounts: want 2 files, got %d", n)
	}

	// Each file must have updated (not cross-contaminated) quota
	pathA := findFileByAccountID(t, dir, accA.AccountID)
	pathB := findFileByAccountID(t, dir, accB.AccountID)
	if pathA == "" || pathB == "" {
		t.Fatalf("missing file: A=%q B=%q", pathA, pathB)
	}
	mA := readJSONFile(t, pathA)
	mB := readJSONFile(t, pathB)
	if getQuotaUsage(mA) != 150 {
		t.Errorf("file A space_usage after SaveAccounts: want 150, got %d", getQuotaUsage(mA))
	}
	if getQuotaUsage(mB) != 250 {
		t.Errorf("file B space_usage after SaveAccounts: want 250, got %d", getQuotaUsage(mB))
	}
}

// ── Test: update A leaves B byte-for-byte unchanged ──

func TestSaveAccountFile_UpdateA_LeavesB_Unchanged(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "WA", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "WB", 200, 600)

	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)

	// Snapshot file B bytes
	pathB := findFileByAccountID(t, dir, accB.AccountID)
	origB, _ := os.ReadFile(pathB)

	// Update A only
	accA.QuotaInfo.SpaceUsage = 999
	if err := saveAccountFile(dir, accA); err != nil {
		t.Fatalf("saveAccountFile A: %v", err)
	}

	// B must be byte-for-byte unchanged
	afterB, _ := os.ReadFile(pathB)
	if string(origB) != string(afterB) {
		t.Error("file B was modified by updating A")
	}

	// A must be updated
	pathA := findFileByAccountID(t, dir, accA.AccountID)
	mA := readJSONFile(t, pathA)
	if getQuotaUsage(mA) != 999 {
		t.Errorf("file A space_usage: want 999, got %d", getQuotaUsage(mA))
	}
}

// ── Test: update B leaves A byte-for-byte unchanged ──

func TestSaveAccountFile_UpdateB_LeavesA_Unchanged(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "WA", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "WB", 200, 600)

	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)

	// Snapshot file A bytes
	pathA := findFileByAccountID(t, dir, accA.AccountID)
	origA, _ := os.ReadFile(pathA)

	// Update B only
	accB.QuotaInfo.SpaceUsage = 888
	if err := saveAccountFile(dir, accB); err != nil {
		t.Fatalf("saveAccountFile B: %v", err)
	}

	// A must be byte-for-byte unchanged
	afterA, _ := os.ReadFile(pathA)
	if string(origA) != string(afterA) {
		t.Error("file A was modified by updating B")
	}
}

// ── Test: delete A removes only A ──

func TestDeleteByAccountID_RemovesOnlyTarget(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "WA", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "WB", 200, 600)

	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)

	pool := &AccountPool{}
	pool.AddAccount(accA)
	pool.AddAccount(accB)

	// Delete A
	if err := DeleteAccountFile(accA.AccountID, dir); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	pool.RemoveAccountByAccountID(accA.AccountID)

	// Only 1 file remains
	if n := countJSONFiles(t, dir); n != 1 {
		t.Fatalf("want 1 file, got %d", n)
	}

	// Remaining file is B
	pathB := findFileByAccountID(t, dir, accB.AccountID)
	if pathB == "" {
		t.Fatal("file B should still exist")
	}

	// Pool should have 1
	if pool.Count() != 1 {
		t.Errorf("pool count: want 1, got %d", pool.Count())
	}
}

// ── Test: delete B preserves A ──

func TestDeleteB_PreservesA(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "WA", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "WB", 200, 600)

	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)

	pathA := findFileByAccountID(t, dir, accA.AccountID)
	origA, _ := os.ReadFile(pathA)

	if err := DeleteAccountFile(accB.AccountID, dir); err != nil {
		t.Fatalf("delete B: %v", err)
	}

	if n := countJSONFiles(t, dir); n != 1 {
		t.Fatalf("want 1 file, got %d", n)
	}
	afterA, _ := os.ReadFile(pathA)
	if string(origA) != string(afterA) {
		t.Error("file A was altered by deleting B")
	}
}

// ── Test: LoadFromDir restores Count()==2 after restart ──

func TestLoadFromDir_RestoresTwoWorkspaces(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "WA", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "WB", 200, 600)

	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)

	// Simulate restart
	pool := &AccountPool{}
	if err := pool.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if pool.Count() != 2 {
		t.Fatalf("want Count()==2, got %d", pool.Count())
	}

	// Verify quota values are distinct and correct after reload
	var usages []int
	for _, acc := range pool.accounts {
		if acc.QuotaInfo != nil {
			usages = append(usages, acc.QuotaInfo.SpaceUsage)
		}
	}
	hasA, hasB := false, false
	for _, u := range usages {
		if u == 100 {
			hasA = true
		}
		if u == 200 {
			hasB = true
		}
	}
	if !hasA || !hasB {
		t.Errorf("quota values after reload: want [100,200], got %v", usages)
	}
}

// ── Test: repeated save does not create duplicates ──

func TestSaveAccountToFile_RepeatedSave_NoDuplicates(t *testing.T) {
	dir := t.TempDir()

	acc := makeTestAccount("user1", "user@example.com", "space-x", "WX", 50, 500)

	SaveAccountToFile(acc, dir)
	SaveAccountToFile(acc, dir) // same account_id
	SaveAccountToFile(acc, dir) // same account_id again

	if n := countJSONFiles(t, dir); n != 1 {
		t.Fatalf("repeated save: want 1 file, got %d", n)
	}
}

// ── Test: identical account_id updates existing file ──

func TestSaveAccountToFile_IdenticalAccountID_Updates(t *testing.T) {
	dir := t.TempDir()

	acc := makeTestAccount("user1", "user@example.com", "space-x", "WX", 50, 500)

	SaveAccountToFile(acc, dir)
	acc.QuotaInfo.SpaceUsage = 999
	SaveAccountToFile(acc, dir)

	if n := countJSONFiles(t, dir); n != 1 {
		t.Fatalf("want 1 file, got %d", n)
	}
	path := findFileByAccountID(t, dir, acc.AccountID)
	m := readJSONFile(t, path)
	if getQuotaUsage(m) != 999 {
		t.Errorf("want updated usage 999, got %d", getQuotaUsage(m))
	}
}

// ── Test: SaveAccounts creates file for in-memory-only profile ──

func TestSaveAccounts_CreatesFilesForMemoryOnlyProfiles(t *testing.T) {
	dir := t.TempDir()

	accA := makeTestAccount("same-user", "same@example.com", "space-a", "WA", 100, 500)
	accB := makeTestAccount("same-user", "same@example.com", "space-b", "WB", 200, 600)

	// Only save A to disk; B exists only in memory
	SaveAccountToFile(accA, dir)

	pool := &AccountPool{}
	pool.AddAccount(accA)
	pool.AddAccount(accB)

	// SaveAccounts should create a file for B
	pool.SaveAccounts(dir)

	if n := countJSONFiles(t, dir); n != 2 {
		t.Fatalf("want 2 files after SaveAccounts, got %d", n)
	}

	pathB := findFileByAccountID(t, dir, accB.AccountID)
	if pathB == "" {
		t.Fatal("SaveAccounts did not create file for in-memory-only profile B")
	}
}

// ── Test: legacy email.json migrates safely ──

func TestLegacyEmailJSON_MigratesSafely(t *testing.T) {
	dir := t.TempDir()

	// Write a legacy file named by email (no account_id field)
	legacy := map[string]interface{}{
		"token_v2":       "tok-legacy",
		"user_id":        "user-legacy",
		"user_email":     "legacy@example.com",
		"user_name":      "Legacy",
		"space_id":       "space-legacy",
		"space_name":     "Legacy Workspace",
		"plan_type":      "plus",
		"browser_id":     "b-legacy",
		"device_id":      "d-legacy",
		"client_version": DefaultClientVersion,
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	legacyPath := filepath.Join(dir, "legacy@example.com.json")
	os.WriteFile(legacyPath, data, 0644)

	// LoadFromDir should load it and compute account_id
	pool := &AccountPool{}
	if err := pool.LoadFromDir(dir); err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if pool.Count() != 1 {
		t.Fatalf("want 1 account, got %d", pool.Count())
	}

	acc := pool.accounts[0]
	expectedAID := ComputeAccountID("user-legacy", "space-legacy")
	if acc.AccountID != expectedAID {
		t.Errorf("account_id: want %s, got %s", expectedAID, acc.AccountID)
	}

	// saveAccountFile should update the legacy file (matched by computed account_id)
	acc.QuotaInfo = &QuotaInfo{IsEligible: true, SpaceUsage: 42, SpaceLimit: 100}
	if err := saveAccountFile(dir, acc); err != nil {
		t.Fatalf("saveAccountFile on legacy: %v", err)
	}

	// Legacy file should now contain account_id
	m := readJSONFile(t, legacyPath)
	if aid, _ := m["account_id"].(string); aid != expectedAID {
		t.Errorf("migrated account_id: want %s, got %s", expectedAID, aid)
	}
}

// ── Test: filenames do not contain secrets ──

func TestSaveAccountToFile_FilenameNoSecrets(t *testing.T) {
	dir := t.TempDir()

	acc := makeTestAccount("uid-secret-123", "user@example.com", "sid-secret-456", "WS", 10, 100)
	acc.TokenV2 = "super-secret-cookie-value"

	fn, err := SaveAccountToFile(acc, dir)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	for _, secret := range []string{acc.TokenV2, acc.UserID, acc.SpaceID} {
		if strings.Contains(fn, secret) {
			t.Errorf("filename %q contains secret %q", fn, secret)
		}
	}
}

func persistenceTestAccount(email string) *Account {
	return &Account{
		TokenV2:   "test-token",
		UserID:    "test-user-" + email,
		UserName:  "Test User",
		UserEmail: email,
		SpaceID:   "test-space-" + email,
		SpaceName: "Test Space",
		PlanType:  "free",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceLimit: 200,
			UserLimit:  200,
		},
	}
}

func TestMarkInferenceUnavailableRetainsPoolAndAccountFile(t *testing.T) {
	tests := []struct {
		name                       string
		prepare                    func(*Account)
		wantPermanent              bool
		wantAvailableAfterEligible int
	}{
		{name: "free", wantPermanent: true},
		{
			name:                       "paid",
			wantAvailableAfterEligible: 1,
			prepare: func(acc *Account) {
				acc.PlanType = "plus"
				acc.QuotaInfo.HasPremium = true
				acc.QuotaInfo.PremiumBalance = 100
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			acc := persistenceTestAccount(tt.name + "@example.com")
			if tt.prepare != nil {
				tt.prepare(acc)
			}
			filename, err := SaveAccountToFile(acc, dir)
			if err != nil {
				t.Fatalf("SaveAccountToFile() error = %v", err)
			}

			pool := NewAccountPool()
			pool.AddAccount(acc)
			if got := pool.AvailableCount(); got != 1 {
				t.Fatalf("AvailableCount() before disable = %d, want 1", got)
			}

			pool.MarkInferenceUnavailable(acc)

			if got := pool.Count(); got != 1 {
				t.Errorf("Count() after automatic disable = %d, want retained total 1", got)
			}
			if got := pool.AvailableCount(); got != 0 {
				t.Errorf("AvailableCount() after automatic disable = %d, want 0", got)
			}
			if pool.FindByAccountID(acc.AccountID) != acc {
				t.Error("automatically disabled account was removed from pool")
			}
			if _, err := os.Stat(filepath.Join(dir, filename)); err != nil {
				t.Errorf("account file was removed after automatic disable: %v", err)
			}
			quota := acc.quotaSnapshot()
			if quota.ExhaustedAt == nil || quota.PermanentlyExhausted != tt.wantPermanent {
				t.Errorf("automatic disable state = %+v, want permanent=%v", quota, tt.wantPermanent)
			}

			pool.applyQuotaInfo(acc, quota.Info)
			if got := pool.AvailableCount(); got != tt.wantAvailableAfterEligible {
				t.Errorf("AvailableCount() after eligible quota refresh = %d, want %d", got, tt.wantAvailableAfterEligible)
			}
		})
	}
}

func TestAccountFilesUseOwnerOnlyPermissions(t *testing.T) {
	t.Run("save and atomic rewrite", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "accounts")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("Chmod(dir) error = %v", err)
		}

		acc := persistenceTestAccount("save@example.com")
		filename, err := SaveAccountToFile(acc, dir)
		if err != nil {
			t.Fatalf("SaveAccountToFile() error = %v", err)
		}
		path := filepath.Join(dir, filename)
		assertMode(t, dir, accountsDirMode)
		assertMode(t, path, accountFileMode)

		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("Chmod(dir) error = %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("Chmod(file) error = %v", err)
		}
		acc.QuotaInfo.SpaceUsage = 25
		if err := saveAccountFile(dir, acc); err != nil {
			t.Fatalf("saveAccountFile() error = %v", err)
		}
		assertMode(t, dir, accountsDirMode)
		assertMode(t, path, accountFileMode)
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("atomic temporary file remains after rewrite: %v", err)
		}
	})

	t.Run("load tightens existing permissions", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "accounts")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("Mkdir() error = %v", err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("Chmod(dir) error = %v", err)
		}

		acc := persistenceTestAccount("load@example.com")
		data, err := json.Marshal(acc)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		path := filepath.Join(dir, "existing.json")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("Chmod(file) error = %v", err)
		}

		pool := NewAccountPool()
		if err := pool.LoadFromDir(dir); err != nil {
			t.Fatalf("LoadFromDir() error = %v", err)
		}
		assertMode(t, dir, accountsDirMode)
		assertMode(t, path, accountFileMode)
	})
}

func TestAccountLoadsFailClosedWhenChmodFails(t *testing.T) {
	errChmod := errors.New("chmod denied")
	secret := "secret-token-must-not-appear"

	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		stubAccountChmod(t, func(path string, mode os.FileMode) error {
			if path == dir {
				return errChmod
			}
			return os.Chmod(path, mode)
		})
		pool := NewAccountPool()
		err := pool.LoadFromDir(dir)
		assertSecureLoadError(t, err, errChmod, secret)
		if got := pool.Count(); got != 0 {
			t.Errorf("Count() = %d, want 0 after directory chmod failure", got)
		}
	})

	t.Run("credential", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "blocked.json")
		if err := os.WriteFile(path, []byte(secret), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		stubAccountChmod(t, func(chmodPath string, mode os.FileMode) error {
			if chmodPath == path {
				return errChmod
			}
			return os.Chmod(chmodPath, mode)
		})
		pool := NewAccountPool()
		err := pool.LoadFromDir(dir)
		assertSecureLoadError(t, err, errChmod, secret)
		if got := pool.Count(); got != 0 {
			t.Errorf("Count() = %d, want 0 after credential chmod failure", got)
		}
	})

	t.Run("single token", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "token.json")
		if err := os.WriteFile(path, []byte(secret), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		stubAccountChmod(t, func(chmodPath string, mode os.FileMode) error {
			if chmodPath == path {
				return errChmod
			}
			return os.Chmod(chmodPath, mode)
		})
		pool := NewAccountPool()
		err := pool.LoadSingle(path)
		assertSecureLoadError(t, err, errChmod, secret)
		if got := pool.Count(); got != 0 {
			t.Errorf("Count() = %d, want 0 after token chmod failure", got)
		}
	})
}

func TestReloadFromDirSkipsCredentialsWhoseChmodFails(t *testing.T) {
	dir := t.TempDir()
	blockedPath := filepath.Join(dir, "blocked.json")
	if err := os.WriteFile(blockedPath, []byte("blocked-secret-token"), 0o644); err != nil {
		t.Fatalf("WriteFile(blocked) error = %v", err)
	}
	goodData, err := json.Marshal(persistenceTestAccount("good@example.com"))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.json"), goodData, 0o644); err != nil {
		t.Fatalf("WriteFile(good) error = %v", err)
	}
	errChmod := errors.New("chmod denied")
	stubAccountChmod(t, func(path string, mode os.FileMode) error {
		if path == blockedPath {
			return errChmod
		}
		return os.Chmod(path, mode)
	})

	pool := NewAccountPool()
	pool.ReloadFromDir(dir)
	if got := pool.Count(); got != 1 {
		t.Fatalf("Count() = %d, want only the securely loaded credential", got)
	}
	if _, err := pool.FindByEmail("good@example.com"); err != nil {
		t.Errorf("secure credential was not loaded: %v", err)
	}
}

func TestPremiumFeatureUnavailableOnlyDisablesFreeAccounts(t *testing.T) {
	tests := []struct {
		name          string
		prepare       func(*Account)
		wantAvailable int
	}{
		{name: "free", wantAvailable: 0},
		{
			name:          "paid",
			wantAvailable: 1,
			prepare: func(account *Account) {
				account.PlanType = "plus"
				account.QuotaInfo.HasPremium = true
				account.QuotaInfo.PremiumBalance = 100
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := persistenceTestAccount(tt.name + "@example.com")
			if tt.prepare != nil {
				tt.prepare(account)
			}
			pool := NewAccountPool()
			pool.AddAccount(account)

			handlePremiumFeatureUnavailable(pool, account)

			if got := pool.AvailableCount(); got != tt.wantAvailable {
				t.Fatalf("AvailableCount() = %d, want %d", got, tt.wantAvailable)
			}
		})
	}
}

func TestAccountCredentialScansChmodBeforeRead(t *testing.T) {
	dir := t.TempDir()
	account := persistenceTestAccount("scan@example.com")
	account.EnsureAccountID()
	data, err := json.Marshal(account)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	path := filepath.Join(dir, "existing.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	errChmod := errors.New("chmod denied")

	t.Run("directory before save scan", func(t *testing.T) {
		readCalls := 0
		stubAccountReadFile(t, func(path string) ([]byte, error) {
			readCalls++
			return os.ReadFile(path)
		})
		stubAccountChmod(t, func(chmodPath string, mode os.FileMode) error {
			if chmodPath == dir {
				return errChmod
			}
			return os.Chmod(chmodPath, mode)
		})

		_, err := SaveAccountToFile(account, dir)
		if !errors.Is(err, errChmod) {
			t.Fatalf("SaveAccountToFile() error = %v, want wrapped %v", err, errChmod)
		}
		if readCalls != 0 {
			t.Fatalf("credential reads = %d, want 0 before directory chmod succeeds", readCalls)
		}
	})

	operations := []struct {
		name string
		run  func() error
	}{
		{
			name: "save account",
			run: func() error {
				_, err := SaveAccountToFile(account, dir)
				return err
			},
		},
		{name: "rewrite account", run: func() error { return saveAccountFile(dir, account) }},
		{name: "delete by id", run: func() error { return DeleteAccountFile(account.AccountID, dir) }},
		{name: "delete by email", run: func() error { return DeleteAccountFileByEmail(account.UserEmail, dir) }},
		{name: "admin delete by id", run: func() error { return deleteAccountByID(nil, dir, account.AccountID) }},
		{name: "admin delete by email", run: func() error { return deleteAccountByEmail(nil, dir, account.UserEmail) }},
	}
	for _, operation := range operations {
		t.Run(operation.name+" secures file", func(t *testing.T) {
			readCalls := 0
			stubAccountReadFile(t, func(path string) ([]byte, error) {
				readCalls++
				return os.ReadFile(path)
			})
			stubAccountChmod(t, func(chmodPath string, mode os.FileMode) error {
				if chmodPath == path {
					return errChmod
				}
				return os.Chmod(chmodPath, mode)
			})

			err := operation.run()
			if !errors.Is(err, errChmod) {
				t.Fatalf("operation error = %v, want wrapped %v", err, errChmod)
			}
			if readCalls != 0 {
				t.Fatalf("credential reads = %d, want 0 before file chmod succeeds", readCalls)
			}
		})
	}
}

func stubAccountChmod(t *testing.T, stub func(string, os.FileMode) error) {
	t.Helper()
	original := accountChmod
	accountChmod = stub
	t.Cleanup(func() { accountChmod = original })
}

func stubAccountReadFile(t *testing.T, stub func(string) ([]byte, error)) {
	t.Helper()
	original := accountReadFile
	accountReadFile = stub
	t.Cleanup(func() { accountReadFile = original })
}

func assertSecureLoadError(t *testing.T, got, want error, secret string) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want wrapped %v", got, want)
	}
	if strings.Contains(got.Error(), secret) {
		t.Errorf("permission error leaked credential content: %v", got)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode(%q) = %04o, want %04o", path, got, want)
	}
}

package proxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// helper: create a temp dir with JSON account files and return the dir path.
func writeTempAccounts(t *testing.T, accounts ...map[string]interface{}) string {
	t.Helper()
	dir := t.TempDir()
	for i, acc := range accounts {
		data, err := json.MarshalIndent(acc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		name := "acc" + string(rune('A'+i)) + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func baseAccount(userID, spaceID, email, name string) map[string]interface{} {
	return map[string]interface{}{
		"token_v2":       "tok_" + userID + "_" + spaceID,
		"user_id":        userID,
		"space_id":       spaceID,
		"user_email":     email,
		"user_name":      name,
		"space_name":     "Space-" + spaceID[:8],
		"client_version": DefaultClientVersion,
		"browser_id":     "00000000-0000-0000-0000-000000000000",
	}
}

// Test 1: same user_id + different space_id loads two profiles
func TestLoadFromDir_SameUserDifferentSpace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 2 {
		t.Fatalf("expected 2 accounts, got %d", p.Count())
	}
}

// Test 2: same email + different space_id loads two profiles
func TestLoadFromDir_SameEmailDifferentSpace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-A", "space-111", "shared@test.com", "Alice"),
		baseAccount("user-B", "space-222", "shared@test.com", "Bob"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 2 {
		t.Fatalf("expected 2 accounts, got %d", p.Count())
	}
}

// Test 3: same user_id + same space_id is deduplicated
func TestLoadFromDir_SameUserSameSpace_Dedup(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-aaa", "u@test.com", "User1-dup"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 1 {
		t.Fatalf("expected 1 account (deduped), got %d", p.Count())
	}
}

// Test 4: hot reload adds second workspace
func TestReloadFromDir_AddsSecondWorkspace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 1 {
		t.Fatalf("expected 1 account, got %d", p.Count())
	}
	// Add a second workspace file
	acc2 := baseAccount("user-1", "space-bbb", "u@test.com", "User1")
	data, _ := json.MarshalIndent(acc2, "", "  ")
	os.WriteFile(filepath.Join(dir, "accB.json"), data, 0644)
	p.ReloadFromDir(dir)
	if p.Count() != 2 {
		t.Fatalf("expected 2 accounts after reload, got %d", p.Count())
	}
}

// Test 5: SaveAccountToFile updates only matching workspace
func TestSaveAccountToFile_MatchingWorkspace(t *testing.T) {
	dir := t.TempDir()
	acc := &Account{
		TokenV2:       "tok_test",
		UserID:        "user-1",
		SpaceID:       "space-aaa",
		UserEmail:     "u@test.com",
		UserName:      "User1",
		SpaceName:     "SpaceA",
		ClientVersion: DefaultClientVersion,
		BrowserID:     "00000000-0000-0000-0000-000000000000",
	}
	acc.EnsureAccountID()
	_, err := SaveAccountToFile(acc, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Verify the saved file contains account_id
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var saved map[string]interface{}
	json.Unmarshal(data, &saved)
	if saved["account_id"] == nil || saved["account_id"] == "" {
		t.Fatal("saved file missing account_id")
	}
	if saved["account_id"] != acc.AccountID {
		t.Fatalf("account_id mismatch: got %v, want %s", saved["account_id"], acc.AccountID)
	}
}

// Test 6: deleting one workspace preserves another
func TestRemoveAccount_PreservesOtherWorkspace(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 2 {
		t.Fatalf("expected 2, got %d", p.Count())
	}
	// Find the space-aaa account and remove it by AccountID
	var target *Account
	for _, acc := range p.accounts {
		if acc.SpaceID == "space-aaa" {
			target = acc
			break
		}
	}
	if target == nil {
		t.Fatal("space-aaa account not found")
	}
	p.RemoveAccountByAccountID(target.AccountID)
	if p.Count() != 1 {
		t.Fatalf("expected 1 after removal, got %d", p.Count())
	}
	if p.accounts[0].SpaceID != "space-bbb" {
		t.Fatalf("wrong account preserved: got space_id=%s", p.accounts[0].SpaceID)
	}
}

// Test 7: legacy JSON without account_id remains loadable
func TestLoadFromDir_LegacyJSON(t *testing.T) {
	// Legacy JSON has no account_id field
	legacy := map[string]interface{}{
		"token_v2":       "tok_legacy",
		"user_id":        "user-legacy",
		"space_id":       "space-legacy",
		"user_email":     "legacy@test.com",
		"user_name":      "Legacy",
		"space_name":     "OldSpace",
		"client_version": DefaultClientVersion,
		"browser_id":     "00000000-0000-0000-0000-000000000000",
	}
	dir := writeTempAccounts(t, legacy)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	if p.Count() != 1 {
		t.Fatalf("expected 1 account, got %d", p.Count())
	}
	acc := p.accounts[0]
	if acc.AccountID == "" {
		t.Fatal("account_id should be computed on load for legacy JSON")
	}
	expected := ComputeAccountID("user-legacy", "space-legacy")
	if acc.AccountID != expected {
		t.Fatalf("account_id mismatch: got %s, want %s", acc.AccountID, expected)
	}
}

// Test 8: legacy email lookup returns ambiguity error
func TestFindByEmail_AmbiguityError(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "shared@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "shared@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	_, err := p.FindByEmail("shared@test.com")
	if err == nil {
		t.Fatal("expected ambiguity error, got nil")
	}
	var ambErr *AmbiguousEmailError
	if !errors.As(err, &ambErr) {
		t.Fatalf("expected AmbiguousEmailError, got %T: %v", err, err)
	}
	if ambErr.Count != 2 {
		t.Fatalf("expected 2 matches, got %d", ambErr.Count)
	}
}

// Test 9: failover selects another workspace
func TestNextExcluding_FailoverSelectsOther(t *testing.T) {
	dir := writeTempAccounts(t,
		baseAccount("user-1", "space-aaa", "u@test.com", "User1"),
		baseAccount("user-1", "space-bbb", "u@test.com", "User1"),
	)
	p := NewAccountPool()
	if err := p.LoadFromDir(dir); err != nil {
		t.Fatal(err)
	}
	first := p.Next()
	if first == nil {
		t.Fatal("Next() returned nil")
	}
	exclude := map[*Account]bool{first: true}
	second := p.NextExcluding(exclude)
	if second == nil {
		t.Fatal("NextExcluding() returned nil — failover failed")
	}
	if second.AccountID == first.AccountID {
		t.Fatal("failover returned the same account")
	}
}

// Test 10: dashboard DTO contains account_id
func TestAccountDTO_ContainsAccountID(t *testing.T) {
	acc := &Account{
		TokenV2:   "tok_secret",
		UserID:    "user-1",
		SpaceID:   "space-aaa-1234-5678-9abc",
		UserEmail: "u@test.com",
		UserName:  "User1",
		SpaceName: "MySpace",
		PlanType:  "business",
	}
	acc.EnsureAccountID()
	dto := NewAccountDTO(acc)
	if dto.AccountID == "" {
		t.Fatal("DTO missing account_id")
	}
	if dto.AccountID != acc.AccountID {
		t.Fatalf("DTO account_id mismatch: got %s, want %s", dto.AccountID, acc.AccountID)
	}
	if dto.SpaceIDShort != "space-aa" {
		t.Fatalf("DTO space_id_short wrong: got %s", dto.SpaceIDShort)
	}
	if dto.UserEmail != "u@test.com" {
		t.Fatal("DTO missing user_email")
	}
	if dto.SpaceName != "MySpace" {
		t.Fatal("DTO missing space_name")
	}
}

// Test 11: dashboard DTO contains no token_v2 or cookie
func TestAccountDTO_NoSecrets(t *testing.T) {
	acc := &Account{
		TokenV2:    "super_secret_token",
		FullCookie: "session=abc123; token_v2=secret",
		UserID:     "user-1",
		SpaceID:    "space-aaa",
		UserEmail:  "u@test.com",
		UserName:   "User1",
		SpaceName:  "MySpace",
	}
	dto := NewAccountDTO(acc)
	// Marshal to JSON and check no secrets leak
	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if contains(s, "super_secret_token") {
		t.Fatal("DTO JSON contains token_v2")
	}
	if contains(s, "session=abc123") {
		t.Fatal("DTO JSON contains cookie")
	}
	if contains(s, "token_v2") {
		t.Fatal("DTO JSON contains token_v2 key")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Test: NUL separator prevents ambiguity between ("ab","c") and ("a","bc")
func TestComputeAccountID_NULSeparator(t *testing.T) {
	id1 := ComputeAccountID("ab", "c")
	id2 := ComputeAccountID("a", "bc")
	if id1 == id2 {
		t.Fatal("(ab,c) and (a,bc) must produce different account_ids")
	}
}

// Test: EnsureAccountID is fail-closed on empty inputs
func TestEnsureAccountID_EmptyInputs(t *testing.T) {
	// Empty user_id
	acc1 := &Account{UserID: "", SpaceID: "space-aaa"}
	acc1.EnsureAccountID()
	if acc1.AccountID != "" {
		t.Fatal("empty user_id should not produce an account_id")
	}
	// Empty space_id
	acc2 := &Account{UserID: "user-1", SpaceID: ""}
	acc2.EnsureAccountID()
	if acc2.AccountID != "" {
		t.Fatal("empty space_id should not produce an account_id")
	}
	// Both empty
	acc3 := &Account{}
	acc3.EnsureAccountID()
	if acc3.AccountID != "" {
		t.Fatal("both empty should not produce an account_id")
	}
}

// Test: DTO does not contain full user_id or full space_id
func TestAccountDTO_NoFullIDs(t *testing.T) {
	acc := &Account{
		TokenV2:   "secret",
		UserID:    "user-1-full-uuid-value",
		SpaceID:   "space-aaa-full-uuid-value",
		UserEmail: "u@test.com",
		UserName:  "User1",
		SpaceName: "MySpace",
	}
	dto := NewAccountDTO(acc)
	data, _ := json.Marshal(dto)
	s := string(data)
	if contains(s, "user-1-full-uuid-value") {
		t.Fatal("DTO contains full user_id")
	}
	if contains(s, "space-aaa-full-uuid-value") {
		t.Fatal("DTO contains full space_id")
	}
	if !contains(s, "space-aa") {
		t.Fatal("DTO should contain shortened space_id")
	}
}

// Test: Two SaveAccountToFile calls with same email produce different files
func TestSaveAccountToFile_TwoWorkspacesDifferentFiles(t *testing.T) {
	dir := t.TempDir()
	accA := &Account{
		TokenV2: "tokA", UserID: "user-1", SpaceID: "space-aaa",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceA",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	accB := &Account{
		TokenV2: "tokB", UserID: "user-1", SpaceID: "space-bbb",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceB",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	fA, err := SaveAccountToFile(accA, dir)
	if err != nil {
		t.Fatal(err)
	}
	fB, err := SaveAccountToFile(accB, dir)
	if err != nil {
		t.Fatal(err)
	}
	if fA == fB {
		t.Fatalf("two workspaces with same email produced same filename: %s", fA)
	}
	// Verify both files exist
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files, got %d", len(entries))
	}
}

// Test: DeleteAccountFileByEmail returns ambiguity error with two matches
func TestDeleteAccountFileByEmail_AmbiguityError(t *testing.T) {
	dir := t.TempDir()
	accA := &Account{
		TokenV2: "tokA", UserID: "user-1", SpaceID: "space-aaa",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceA",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	accB := &Account{
		TokenV2: "tokB", UserID: "user-1", SpaceID: "space-bbb",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceB",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)
	err := DeleteAccountFileByEmail("same@test.com", dir)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	var ambErr *AmbiguousEmailError
	if !errors.As(err, &ambErr) {
		t.Fatalf("expected AmbiguousEmailError, got: %v", err)
	}
	// Verify nothing was deleted
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files preserved, got %d", len(entries))
	}
}

// Test: DeleteAccountFile by account_id deletes only target
func TestDeleteAccountFile_ByAccountID_DeletesOnlyTarget(t *testing.T) {
	dir := t.TempDir()
	accA := &Account{
		TokenV2: "tokA", UserID: "user-1", SpaceID: "space-aaa",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceA",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	accB := &Account{
		TokenV2: "tokB", UserID: "user-1", SpaceID: "space-bbb",
		UserEmail: "same@test.com", UserName: "User1", SpaceName: "SpaceB",
		ClientVersion: DefaultClientVersion, BrowserID: "00000000-0000-0000-0000-000000000000",
	}
	SaveAccountToFile(accA, dir)
	SaveAccountToFile(accB, dir)
	accA.EnsureAccountID()
	err := DeleteAccountFile(accA.AccountID, dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file remaining, got %d", len(entries))
	}
	// Verify the remaining file is accB
	data, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	var saved map[string]interface{}
	json.Unmarshal(data, &saved)
	if saved["space_id"] != "space-bbb" {
		t.Fatalf("wrong file preserved: space_id=%v", saved["space_id"])
	}
}

// Test: Profile with usage > limit, remaining = 0, eligible = true should show as available
func TestAccountDTO_UsageExceedsLimit_StillAvailable(t *testing.T) {
	acc := &Account{
		UserID:    "user-overquota",
		SpaceID:   "space-overquota",
		UserEmail: "over@test.com",
		UserName:  "OverUser",
		SpaceName: "OverSpace",
		PlanType:  "business",
		QuotaInfo: &QuotaInfo{
			IsEligible: true,
			SpaceUsage: 604,
			SpaceLimit: 75,
			UserUsage:  604,
			UserLimit:  75,
		},
	}
	acc.EnsureAccountID()

	// The DTO should show the account as available (not exhausted)
	dto := NewAccountDTO(acc)
	if dto.IsExhausted {
		t.Fatal("account with eligible=true should NOT be marked exhausted even when usage > limit")
	}
	if dto.IsPermanent {
		t.Fatal("account with eligible=true should NOT be permanently exhausted")
	}

	// Verify the pool also treats it as usable
	pool := NewAccountPool()
	pool.accounts = []*Account{acc}
	if pool.isQuotaExhausted(acc) {
		t.Fatal("pool should NOT mark eligible=true account as exhausted")
	}
	next := pool.Next()
	if next == nil {
		t.Fatal("pool.Next() should return the account (it is eligible)")
	}

	// Verify quota details are exposed correctly for UI
	details := pool.GetAccountDetails()
	if len(details) != 1 {
		t.Fatalf("expected 1 detail entry, got %d", len(details))
	}
	d := details[0]
	// Basic usage fields
	if d["space_usage"] != 604 {
		t.Fatalf("space_usage wrong: got %v", d["space_usage"])
	}
	if d["space_limit"] != 75 {
		t.Fatalf("space_limit wrong: got %v", d["space_limit"])
	}
	// Remaining should be 0 (clamped, not negative)
	if d["remaining"] != 0 {
		t.Fatalf("remaining should be 0 when usage > limit, got %v", d["remaining"])
	}
	// Eligible should be true
	if d["eligible"] != true {
		t.Fatalf("eligible should be true, got %v", d["eligible"])
	}
	// Exhausted should be false
	if d["exhausted"] != false {
		t.Fatalf("exhausted should be false for eligible account, got %v", d["exhausted"])
	}
}

// Test: ComputeAccountID determinism and format
func TestComputeAccountID(t *testing.T) {
	id1 := ComputeAccountID("user-1", "space-aaa")
	id2 := ComputeAccountID("user-1", "space-aaa")
	if id1 != id2 {
		t.Fatal("ComputeAccountID is not deterministic")
	}
	if len(id1) != 64 {
		t.Fatalf("expected 64-char hex, got %d chars", len(id1))
	}
	// Different space_id must produce different account_id
	id3 := ComputeAccountID("user-1", "space-bbb")
	if id1 == id3 {
		t.Fatal("different space_id should produce different account_id")
	}
}

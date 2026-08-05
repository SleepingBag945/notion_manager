package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// quotaFetcher / modelsFetcher / workspaceProbe are package-level seams
// so unit tests can substitute deterministic stubs without spinning up a
// fake Notion server (the real code path uses a TLS-fingerprinted
// transport pinned to www.notion.so, which can't be repointed at httptest
// URLs).
var (
	quotaFetcher    = CheckQuota
	modelsFetcher   = FetchModels
	workspaceProbe  = CheckUserWorkspace
	accountChmod    = os.Chmod
	accountReadFile = os.ReadFile
)

const (
	accountsDirMode = 0o700
	accountFileMode = 0o600
)

func ensurePrivateAccountsDir(dir string) error {
	if err := os.MkdirAll(dir, accountsDirMode); err != nil {
		return err
	}
	return accountChmod(dir, accountsDirMode)
}

func chmodAccountDir(dir string) error {
	return accountChmod(dir, accountsDirMode)
}

func chmodAccountFile(path string) error {
	return accountChmod(path, accountFileMode)
}

func readPrivateAccountsDir(dir string) ([]os.DirEntry, error) {
	if err := chmodAccountDir(dir); err != nil {
		return nil, fmt.Errorf("secure accounts dir permissions: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read accounts dir: %w", err)
	}
	return entries, nil
}

func readPrivateAccountFile(path string) ([]byte, error) {
	if err := chmodAccountFile(path); err != nil {
		return nil, fmt.Errorf("secure account file %s permissions: %w", filepath.Base(path), err)
	}
	data, err := accountReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read account file %s: %w", filepath.Base(path), err)
	}
	return data, nil
}

func writePrivateAccountFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, accountFileMode)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
	}()

	if err := f.Chmod(accountFileMode); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if n, err := f.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

type AccountPool struct {
	mu       sync.RWMutex
	accounts []*Account
	index    atomic.Uint64

	// Refresh state (protected by refreshMu)
	refreshMu      sync.RWMutex
	refreshing     bool
	refreshDone    int
	refreshTotal   int
	lastRefreshAt  *time.Time
	lastRefreshErr string

	// Per-account live quota refresh state (key: account pointer)
	// Used to deduplicate concurrent live quota checks for the same account.
	liveQuotaMu       sync.Mutex
	liveQuotaInflight map[*Account]bool
}

func NewAccountPool() *AccountPool {
	return &AccountPool{
		liveQuotaInflight: make(map[*Account]bool),
	}
}

// AccountsSnapshot returns a stable copy of the pool membership for startup
// activation. Mutating the returned slice does not change the source pool.
func (p *AccountPool) AccountsSnapshot() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	accounts := make([]*Account, len(p.accounts))
	copy(accounts, p.accounts)
	return accounts
}

type accountQuotaSnapshot struct {
	Info                 *QuotaInfo
	CheckedAt            *time.Time
	ExhaustedAt          *time.Time
	PermanentlyExhausted bool
}

func cloneModelEntries(src []ModelEntry) []ModelEntry {
	if len(src) == 0 {
		return nil
	}
	dst := make([]ModelEntry, len(src))
	copy(dst, src)
	return dst
}

func cloneQuotaInfo(src *QuotaInfo) *QuotaInfo {
	if src == nil {
		return nil
	}
	dst := *src
	return &dst
}

func cloneTimePtr(src *time.Time) *time.Time {
	if src == nil {
		return nil
	}
	dst := *src
	return &dst
}

func (acc *Account) modelsSnapshot() []ModelEntry {
	if acc == nil {
		return nil
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return cloneModelEntries(acc.Models)
}

func (acc *Account) quotaSnapshot() accountQuotaSnapshot {
	if acc == nil {
		return accountQuotaSnapshot{}
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return accountQuotaSnapshot{
		Info:                 cloneQuotaInfo(acc.QuotaInfo),
		CheckedAt:            cloneTimePtr(acc.QuotaCheckedAt),
		ExhaustedAt:          cloneTimePtr(acc.QuotaExhaustedAt),
		PermanentlyExhausted: acc.PermanentlyExhausted,
	}
}

func (acc *Account) quotaInfoSnapshot() *QuotaInfo {
	return acc.quotaSnapshot().Info
}

func (acc *Account) setModels(models []ModelEntry) {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.Models = cloneModelEntries(models)
}

func (acc *Account) setQuotaInfo(info *QuotaInfo, checkedAt *time.Time) {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.QuotaInfo = cloneQuotaInfo(info)
	acc.QuotaCheckedAt = cloneTimePtr(checkedAt)
}

func (acc *Account) markQuotaExhausted(now time.Time, permanent bool) bool {
	if acc == nil {
		return false
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.QuotaExhaustedAt != nil {
		if permanent {
			acc.PermanentlyExhausted = true
		}
		return false
	}
	ts := now
	acc.QuotaExhaustedAt = &ts
	acc.PermanentlyExhausted = permanent
	return true
}

func (acc *Account) clearQuotaExhausted() {
	if acc == nil {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	acc.QuotaExhaustedAt = nil
	acc.PermanentlyExhausted = false
}

func (p *AccountPool) LoadFromDir(dir string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if err := chmodAccountFile(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("secure account file %s permissions: %w", entry.Name(), err)
		}
	}

	seen := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var acc Account
		if err := json.Unmarshal(data, &acc); err != nil {
			log.Printf("[account] skip %s: %v", entry.Name(), err)
			continue
		}
		if acc.TokenV2 == "" || acc.UserID == "" || acc.SpaceID == "" {
			log.Printf("[account] skip %s: missing required fields", entry.Name())
			continue
		}
		if acc.TokenV2 == "YOUR_TOKEN_V2_HERE" || strings.HasPrefix(acc.UserID, "xxxxxxxx") {
			log.Printf("[account] skip %s: placeholder/example config", entry.Name())
			continue
		}
		acc.EnsureAccountID()
		if acc.AccountID != "" && seen[acc.AccountID] {
			log.Printf("[account] skip %s: duplicate account_id (same user_id+space_id)", entry.Name())
			continue
		}
		if acc.AccountID != "" {
			seen[acc.AccountID] = true
		}
		if acc.BrowserID == "" {
			acc.BrowserID = generateUUIDv4()
		}
		if acc.ClientVersion == "" || acc.ClientVersion == "unknown" {
			acc.ClientVersion = DefaultClientVersion
		}
		// Load persisted quota info (snake_case keys) into runtime QuotaInfo
		acc.QuotaInfo = loadPersistedQuotaInfo(data)
		// Load persisted workspace probe (`space_count` /
		// `workspace_checked_at`) so a server restart doesn't have to
		// re-probe every account before the pool can refuse known-bad
		// ones.
		loadPersistedWorkspace(data, &acc)
		p.accounts = append(p.accounts, &acc)
		log.Printf("[account] loaded: %s (%s) [%s] aid=%s", acc.UserName, acc.UserEmail, acc.PlanType, acc.ShortSpaceID())
	}

	if len(p.accounts) == 0 {
		return fmt.Errorf("no valid accounts found in %s", dir)
	}
	log.Printf("[account] total: %d accounts loaded", len(p.accounts))
	return nil
}

// ReloadFromDir scans the accounts directory and adds any account whose
// user_id is not already present in the pool. Existing entries (and their
// runtime quota state) are preserved. This is invoked after bulk
// registration so newly-created files become live without a server
// restart.
func (p *AccountPool) ReloadFromDir(dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	known := make(map[string]bool, len(p.accounts))
	for _, acc := range p.accounts {
		acc.EnsureAccountID()
		if acc.AccountID != "" {
			known[acc.AccountID] = true
		}
	}

	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		log.Printf("[account] reload %s: %v", dir, err)
		return
	}
	added := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			log.Printf("[account] reload skip %s: %v", entry.Name(), err)
			continue
		}
		var acc Account
		if err := json.Unmarshal(data, &acc); err != nil {
			continue
		}
		if acc.TokenV2 == "" || acc.UserID == "" || acc.SpaceID == "" {
			continue
		}
		acc.EnsureAccountID()
		if acc.AccountID != "" && known[acc.AccountID] {
			continue
		}
		if acc.BrowserID == "" {
			acc.BrowserID = generateUUIDv4()
		}
		if acc.ClientVersion == "" || acc.ClientVersion == "unknown" {
			acc.ClientVersion = DefaultClientVersion
		}
		acc.QuotaInfo = loadPersistedQuotaInfo(data)
		loadPersistedWorkspace(data, &acc)
		p.accounts = append(p.accounts, &acc)
		if acc.AccountID != "" {
			known[acc.AccountID] = true
		}
		added++
		log.Printf("[account] reload added: %s (%s)", acc.UserName, acc.UserEmail)
	}
	if added > 0 {
		log.Printf("[account] reload: %d new account(s); pool now has %d", added, len(p.accounts))
	}
}

func (p *AccountPool) LoadSingle(tokenFile string) error {
	data, err := readPrivateAccountFile(tokenFile)
	if err != nil {
		return fmt.Errorf("secure token file: %w", err)
	}

	var acc Account
	if err := json.Unmarshal(data, &acc); err != nil {
		// Treat as plain token file
		acc = Account{
			TokenV2:       string(data),
			UserID:        "322d872b-594c-816e-b8ce-00022c725bb3",
			SpaceID:       "176faced-55bd-8161-bbbf-000339934d27",
			UserName:      "default",
			SpaceName:     "default",
			Timezone:      "UTC",
			ClientVersion: DefaultClientVersion,
			BrowserID:     generateUUIDv4(),
		}
	}
	if acc.BrowserID == "" {
		acc.BrowserID = generateUUIDv4()
	}
	if acc.ClientVersion == "" || acc.ClientVersion == "unknown" {
		acc.ClientVersion = DefaultClientVersion
	}
	acc.EnsureAccountID()
	p.accounts = append(p.accounts, &acc)
	log.Printf("[account] loaded single account: %s (aid=%s)", acc.UserName, acc.ShortSpaceID())
	return nil
}

func (p *AccountPool) Next() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pickNextRoundRobin(nil)
}

// NextForResearch returns the next available account suitable for research mode.
// Premium accounts are treated as research-capable regardless of research_usage.
// For non-premium accounts, it prefers research_usage < 3 and falls back if needed.
func (p *AccountPool) NextForResearch() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	start := p.index.Add(1) - 1
	// First pass: prefer premium accounts and accounts with lower research usage.
	var fallback *Account
	bestUsage := int(^uint(0) >> 1)
	for i := 0; i < n; i++ {
		acc := p.accounts[(start+uint64(i))%uint64(n)]
		if p.isUnusable(acc) {
			continue
		}
		quota := acc.quotaInfoSnapshot()
		if quota == nil {
			if fallback == nil {
				fallback = acc
			}
			continue
		}
		if quota.HasPremium {
			return acc
		}
		if quota.ResearchModeUsage < 3 {
			if fallback == nil || quota.ResearchModeUsage < bestUsage {
				fallback = acc
				bestUsage = quota.ResearchModeUsage
			}
			continue
		}
		if fallback == nil {
			fallback = acc // keep a usable fallback if no fresher account exists
		}
	}
	return fallback // nil if all exhausted
}

// NextExcluding returns the next available account excluding the given ones (for retry)
func (p *AccountPool) NextExcluding(exclude map[*Account]bool) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pickNextRoundRobin(exclude)
}

// pickNextRoundRobin returns the first available (non-exhausted, non-excluded)
// account starting from the rotating index. This gives true round-robin
// distribution across accounts.
func (p *AccountPool) pickNextRoundRobin(exclude map[*Account]bool) *Account {
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	start := p.index.Add(1) - 1
	for i := 0; i < n; i++ {
		acc := p.accounts[(start+uint64(i))%uint64(n)]
		if exclude != nil && exclude[acc] {
			continue
		}
		if p.isUnusable(acc) {
			continue
		}
		return acc
	}
	return nil
}

// pickBestAccountLocked returns the available account with the highest
// effective remaining quota. Used by GetBestAccount (dashboard) and
// NextBest/NextBestExcluding (request routing).
//
// Selection rules:
//  1. Skip exhausted/excluded accounts.
//  2. Among accounts with known quota (QuotaInfo != nil), pick the one with
//     the most basic remaining (space ⊓ user). Premium accounts are treated
//     as effectively unlimited (priority overrides basic remaining).
//  3. If no scored account is available (e.g. all accounts are unrefreshed),
//     fall back to the first usable account in rotation order so freshly
//     loaded accounts can still serve traffic before the first refresh
//     completes.
func (p *AccountPool) pickBestAccountLocked(exclude map[*Account]bool) *Account {
	n := len(p.accounts)
	if n == 0 {
		return nil
	}
	start := p.index.Add(1) - 1
	var best *Account
	var fallback *Account
	bestScore := -1
	for i := 0; i < n; i++ {
		acc := p.accounts[(start+uint64(i))%uint64(n)]
		if exclude != nil && exclude[acc] {
			continue
		}
		if p.isUnusable(acc) {
			continue
		}
		score := accountQuotaPriority(acc)
		if score < 0 {
			// Unknown quota — keep as fallback if no scored account exists
			if fallback == nil {
				fallback = acc
			}
			continue
		}
		if best == nil || score > bestScore {
			best = acc
			bestScore = score
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

// NextBest returns the next available account, preferring accounts with the
// highest remaining basic quota. Used for new conversations (no existing
// session) so high-quota accounts get used first instead of round-robin.
func (p *AccountPool) NextBest() *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pickBestAccountLocked(nil)
}

// NextBestExcluding returns the next best available account, excluding the
// given accounts (used for retries / failover).
func (p *AccountPool) NextBestExcluding(exclude map[*Account]bool) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pickBestAccountLocked(exclude)
}

// MarkQuotaExhausted marks an account as quota-exhausted with a timestamp.
// Recovery only happens when RefreshAll confirms isEligible=true via API.
// Free plan accounts (200 lifetime credits) will stay exhausted permanently.
// Paid plan accounts recover when monthly credits reset at billing cycle boundary.
func (p *AccountPool) MarkQuotaExhausted(acc *Account) {
	if !acc.markQuotaExhausted(time.Now(), false) {
		return // already marked
	}
	log.Printf("[quota] marked %s (%s) as exhausted (recovery via API re-check only)", acc.UserName, acc.UserEmail)
}

// ClearQuotaExhausted removes the exhausted mark (called when API confirms recovery)
func (p *AccountPool) ClearQuotaExhausted(acc *Account) {
	acc.clearQuotaExhausted()
}

func (p *AccountPool) isQuotaExhausted(acc *Account) bool {
	quota := acc.quotaSnapshot()
	if quota.PermanentlyExhausted {
		return true
	}
	if quota.ExhaustedAt != nil {
		return true
	}
	if quota.Info != nil {
		return !quota.Info.IsEligible
	}
	return false
}

// hasNoWorkspace returns true only after a probe has confirmed the
// account has zero accessible workspaces. Unprobed accounts (fresh
// registrations, accounts loaded before the probe ever ran) are treated
// as unknown / usable so the pool can still serve traffic on the first
// boot and the next refresh tick promotes/demotes them.
func (p *AccountPool) hasNoWorkspace(acc *Account) bool {
	return acc != nil && acc.WorkspaceCheckedAt != nil && acc.SpaceCount == 0
}

// isUnusable folds quota-exhausted and no-workspace accounts into a
// single "do not select" predicate used by every picker.
func (p *AccountPool) isUnusable(acc *Account) bool {
	return p.isQuotaExhausted(acc) || p.hasNoWorkspace(acc)
}

// applyWorkspaceCount records the latest probe result. Caller must NOT
// hold p.mu. Returns the previous SpaceCount and whether the snapshot
// changed so callers can decide to log only on transitions.
func (p *AccountPool) applyWorkspaceCount(acc *Account, count int) (prev int, changed bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev = acc.SpaceCount
	hadCheck := acc.WorkspaceCheckedAt != nil
	now := time.Now()
	acc.SpaceCount = count
	acc.WorkspaceCheckedAt = &now
	changed = !hadCheck || prev != count
	return prev, changed
}

// MarkPermanentlyExhausted marks a free-plan account as permanently exhausted (never recovers).
func (p *AccountPool) MarkPermanentlyExhausted(acc *Account) {
	acc.markQuotaExhausted(time.Now(), true)
	log.Printf("[quota] marked %s (%s) as PERMANENTLY exhausted (free plan, no recovery)", acc.UserName, acc.UserEmail)
}

// MarkInferenceUnavailable keeps an automatically-disabled account in the
// pool and on disk while preventing future inference routing. Free accounts
// are permanent because their lifetime credits do not reset; paid accounts
// remain eligible for a later API-confirmed recovery.
func (p *AccountPool) MarkInferenceUnavailable(acc *Account) {
	if acc == nil {
		return
	}
	if isFreePlan(acc) {
		p.MarkPermanentlyExhausted(acc)
		return
	}
	p.MarkQuotaExhausted(acc)
}

// quotaApplyResult describes how applyQuotaInfo changed the account state.
// Caller can use it to emit a human-friendly log line.
type quotaApplyResult struct {
	Recovered    bool // was previously exhausted, now eligible
	NowExhausted bool // is currently not eligible
	NowPermanent bool // free-plan account that is now permanently exhausted
	WasPermanent bool // was already permanently flagged before this update
	BasicLeft    int  // basic remaining after the update (0 if info nil)
	HasPremium   bool
}

// applyQuotaInfo records the latest quota check result for the account and
// updates exhausted/permanent state accordingly. Caller must NOT hold acc.mu.
// The returned snapshot describes what changed so the caller can log the
// transition without re-locking.
func (p *AccountPool) applyQuotaInfo(acc *Account, info *QuotaInfo) quotaApplyResult {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	res := quotaApplyResult{WasPermanent: acc.PermanentlyExhausted}
	now := time.Now()
	acc.QuotaInfo = cloneQuotaInfo(info)
	acc.QuotaCheckedAt = &now
	if info == nil {
		return res
	}
	res.BasicLeft = basicRemaining(info)
	res.HasPremium = info.HasPremium
	if info.IsEligible {
		if acc.PermanentlyExhausted {
			return res
		}
		if acc.QuotaExhaustedAt != nil {
			res.Recovered = true
		}
		acc.QuotaExhaustedAt = nil
		acc.PermanentlyExhausted = false
		return res
	}
	res.NowExhausted = true
	if acc.QuotaExhaustedAt == nil {
		acc.QuotaExhaustedAt = &now
	}
	isFree := false
	if !(info.HasPremium || info.PremiumLimit > 0 || info.PremiumBalance > 0) {
		switch strings.ToLower(strings.TrimSpace(acc.PlanType)) {
		case "personal", "free", "":
			isFree = true
		default:
			isFree = !info.HasPremium
		}
	}
	if isFree {
		acc.PermanentlyExhausted = true
		res.NowPermanent = true
	}
	return res
}

// RefreshAccountQuota performs a live quota check for a single account.
//
//   - When minInterval > 0 and the cached quota was checked recently, returns
//     the cached eligibility without making an HTTP call. This avoids hammering
//     the Notion quota API on retry loops.
//   - Updates the account's QuotaInfo / QuotaExhaustedAt / PermanentlyExhausted
//     fields atomically based on the live result.
//
// Returns true when the account is currently eligible to serve traffic.
func (p *AccountPool) RefreshAccountQuota(acc *Account, minInterval time.Duration) bool {
	if acc == nil {
		return false
	}
	// Cached fast path: avoid hammering Notion on tight retry loops.
	quota := acc.quotaSnapshot()
	if minInterval > 0 && quota.CheckedAt != nil && time.Since(*quota.CheckedAt) < minInterval {
		if quota.Info != nil {
			return quota.Info.IsEligible
		}
		return !p.isQuotaExhaustedRLock(acc)
	}
	// Mark this account as having an in-flight live check so async callers
	// do not pile on. We intentionally still proceed even if another check
	// is running so the synchronous caller gets a fresh decision.
	p.liveQuotaMu.Lock()
	p.liveQuotaInflight[acc] = true
	p.liveQuotaMu.Unlock()
	defer func() {
		p.liveQuotaMu.Lock()
		delete(p.liveQuotaInflight, acc)
		p.liveQuotaMu.Unlock()
	}()

	info, err := quotaFetcher(acc)
	if err != nil {
		log.Printf("[quota-live] %s check failed: %v (using cached state)", acc.UserEmail, err)
		// On error, trust the cached snapshot — return current eligibility.
		quota := acc.quotaSnapshot()
		if quota.Info != nil {
			return quota.Info.IsEligible
		}
		return !p.isQuotaExhaustedRLock(acc)
	}

	res := p.applyQuotaInfo(acc, info)
	switch {
	case res.Recovered:
		log.Printf("[quota-live] %s recovered (basic remaining ~%d, premium=%v)",
			acc.UserEmail, res.BasicLeft, res.HasPremium)
	case res.NowPermanent:
		log.Printf("[quota-live] %s NOT eligible — disabling permanently (free plan)", acc.UserEmail)
	case res.NowExhausted:
		log.Printf("[quota-live] %s NOT eligible — disabled (space %d/%d, user %d/%d)",
			acc.UserEmail, info.SpaceUsage, info.SpaceLimit, info.UserUsage, info.UserLimit)
	default:
		log.Printf("[quota-live] %s eligible (basic remaining ~%d, premium=%v)",
			acc.UserEmail, res.BasicLeft, res.HasPremium)
	}
	return info.IsEligible
}

// RefreshAccountQuotaAsync triggers a live quota check in the background.
// Used to refresh the cached quota after a successful inference call so the
// next selection sees up-to-date numbers without blocking the user request.
// Concurrent calls for the same account are deduplicated.
func (p *AccountPool) RefreshAccountQuotaAsync(acc *Account) {
	if acc == nil {
		return
	}
	p.liveQuotaMu.Lock()
	if p.liveQuotaInflight[acc] {
		p.liveQuotaMu.Unlock()
		return
	}
	p.liveQuotaInflight[acc] = true
	p.liveQuotaMu.Unlock()

	go func() {
		defer func() {
			p.liveQuotaMu.Lock()
			delete(p.liveQuotaInflight, acc)
			p.liveQuotaMu.Unlock()
		}()
		info, err := quotaFetcher(acc)
		if err != nil {
			log.Printf("[quota-live-async] %s check failed: %v", acc.UserEmail, err)
			return
		}
		res := p.applyQuotaInfo(acc, info)
		switch {
		case res.NowPermanent:
			log.Printf("[quota-live-async] %s NOT eligible — disabled permanently (free plan)", acc.UserEmail)
		case res.NowExhausted:
			log.Printf("[quota-live-async] %s NOT eligible — disabled (basic %d left)", acc.UserEmail, res.BasicLeft)
		case res.Recovered:
			log.Printf("[quota-live-async] %s recovered (basic %d left)", acc.UserEmail, res.BasicLeft)
		}
	}()
}

// isQuotaExhaustedRLock is a read-locked variant of isQuotaExhausted for
// callers that hold no lock yet.
func (p *AccountPool) isQuotaExhaustedRLock(acc *Account) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.isQuotaExhausted(acc)
}

// RemoveAccount removes an account from the pool and deletes its JSON file from disk.
// Used for free-plan accounts that are confirmed exhausted (e.g. premium feature unavailable).
func (p *AccountPool) RemoveAccount(acc *Account) {
	p.mu.Lock()
	for i, a := range p.accounts {
		if a == acc {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			break
		}
	}
	p.mu.Unlock()

	// Delete the JSON file from disk
	dir := ""
	if AppConfig != nil {
		dir = AppConfig.Server.AccountsDir
	}
	if dir == "" {
		return
	}
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		log.Printf("[account] remove %s: %v", acc.UserEmail, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			log.Printf("[account] remove %s: %v", acc.UserEmail, err)
			return
		}
		var existing map[string]interface{}
		if err := json.Unmarshal(data, &existing); err != nil {
			continue
		}
		// Match by account_id first, fall back to user_id+space_id
		exUID, _ := existing["user_id"].(string)
		exSID, _ := existing["space_id"].(string)
		exAID, _ := existing["account_id"].(string)
		acc.EnsureAccountID()
		match := false
		if acc.AccountID != "" && exAID != "" {
			match = exAID == acc.AccountID
		} else {
			match = exUID == acc.UserID && exSID == acc.SpaceID
		}
		if match {
			if err := os.Remove(path); err != nil {
				log.Printf("[account] failed to delete %s: %v", path, err)
			} else {
				log.Printf("[account] deleted exhausted free account file: %s (%s)", path, acc.UserEmail)
			}
			break
		}
	}
}

// RemoveAccountByAccountID drops the in-memory pool entry whose AccountID
// matches. Does NOT touch disk.
func (p *AccountPool) RemoveAccountByAccountID(accountID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		a.EnsureAccountID()
		if a.AccountID == accountID {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			return true
		}
	}
	return false
}

// RemoveAccountByEmail drops the in-memory pool entry whose user_email
// matches (case-insensitive). Does NOT touch disk; callers are responsible
// for the file lifecycle (used by the dashboard delete endpoint).
// NOTE: With multi-workspace, this removes only the FIRST match.
func (p *AccountPool) RemoveAccountByEmail(email string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if strings.EqualFold(a.UserEmail, email) {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			return true
		}
	}
	return false
}

// AvailableCount returns the number of accounts the pool can currently
// route traffic to (i.e. quota is healthy AND the workspace probe didn't
// flag the account as having zero accessible workspaces).
func (p *AccountPool) AvailableCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	count := 0
	for _, acc := range p.accounts {
		if !p.isUnusable(acc) {
			count++
		}
	}
	return count
}

func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// GetByEmail returns an available (non-exhausted, has workspace) account
// by email, or nil if not found / exhausted / has no workspace.
func (p *AccountPool) GetByEmail(email string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, acc := range p.accounts {
		if acc.UserEmail == email && !p.isUnusable(acc) {
			return acc
		}
	}
	return nil
}

// FindByEmail returns all accounts matching the given email.
// Returns AmbiguousEmailError if multiple workspaces share the same email.
func (p *AccountPool) FindByEmail(email string) (*Account, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var matches []*Account
	for _, acc := range p.accounts {
		if strings.EqualFold(acc.UserEmail, email) {
			matches = append(matches, acc)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no account found for email %s", email)
	case 1:
		return matches[0], nil
	default:
		return nil, &AmbiguousEmailError{Email: email, Count: len(matches)}
	}
}

// FindByAccountID returns the account with the given AccountID, or nil.
func (p *AccountPool) FindByAccountID(id string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, acc := range p.accounts {
		acc.EnsureAccountID()
		if acc.AccountID == id {
			return acc
		}
	}
	return nil
}

// HasNoWorkspace returns true when the account has been probed and found
// to have zero accessible workspaces. Used by handlers that need to
// distinguish "no such account" from "account exists but is broken".
func (p *AccountPool) HasNoWorkspace(acc *Account) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.hasNoWorkspace(acc)
}

func (p *AccountPool) AllModels() []ModelEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	seen := map[string]bool{}
	var models []ModelEntry
	for _, acc := range p.accounts {
		for _, m := range acc.modelsSnapshot() {
			if !seen[m.ID] {
				seen[m.ID] = true
				models = append(models, m)
			}
		}
	}
	if len(models) == 0 {
		// Return default models if none loaded
		for name, id := range SnapshotModelMap() {
			models = append(models, ModelEntry{Name: name, ID: id})
		}
	}
	return models
}

// GetRefreshStatus returns the current refresh state for the API
func (p *AccountPool) GetRefreshStatus() map[string]interface{} {
	p.refreshMu.RLock()
	defer p.refreshMu.RUnlock()
	status := map[string]interface{}{
		"refreshing": p.refreshing,
		"done":       p.refreshDone,
		"total":      p.refreshTotal,
	}
	if p.lastRefreshAt != nil {
		status["last_refresh_at"] = p.lastRefreshAt.Format(time.RFC3339)
	}
	if p.lastRefreshErr != "" {
		status["error"] = p.lastRefreshErr
	}
	return status
}

// TriggerRefresh starts RefreshAll in a background goroutine if not already running.
// Returns true if a new refresh was started, false if one is already in progress.
func (p *AccountPool) TriggerRefresh(accountsDir string) bool {
	p.refreshMu.Lock()
	if p.refreshing {
		p.refreshMu.Unlock()
		return false
	}
	p.refreshMu.Unlock()
	go p.RefreshAll(accountsDir)
	return true
}

// RefreshAll proactively checks AI quota and fetches models for all accounts via Notion API.
// It also persists updated info back to account JSON files.
func (p *AccountPool) RefreshAll(accountsDir string) {
	p.refreshMu.Lock()
	if p.refreshing {
		p.refreshMu.Unlock()
		return // already running
	}
	p.refreshing = true
	p.refreshDone = 0
	p.lastRefreshErr = ""
	p.refreshMu.Unlock()

	defer func() {
		p.refreshMu.Lock()
		p.refreshing = false
		now := time.Now()
		p.lastRefreshAt = &now
		p.refreshMu.Unlock()
	}()

	p.mu.RLock()
	accs := make([]*Account, len(p.accounts))
	copy(accs, p.accounts)
	p.mu.RUnlock()

	p.refreshMu.Lock()
	p.refreshTotal = len(accs)
	p.refreshMu.Unlock()

	concurrency := 10
	if AppConfig != nil && AppConfig.Refresh.Concurrency > 0 {
		concurrency = AppConfig.Refresh.Concurrency
	}
	log.Printf("[refresh] refreshing %d accounts (quota + models, concurrency=%d)...", len(accs), concurrency)

	var (
		modelsUpdatedFlag atomic.Bool
		disabledNow       atomic.Int64
		recoveredNow      atomic.Int64
		failedChecks      atomic.Int64
		workspaceLost     atomic.Int64
	)
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)

	for _, acc := range accs {
		quota := acc.quotaSnapshot()
		// Skip permanently exhausted accounts (free plan, no recovery possible)
		if quota.PermanentlyExhausted {
			if isFreePlan(acc) {
				log.Printf("[refresh] %s (%s): permanently exhausted, skipped", acc.UserName, acc.UserEmail)
				p.refreshMu.Lock()
				p.refreshDone++
				p.refreshMu.Unlock()
				continue
			}
			log.Printf("[refresh] %s (%s): clearing stale permanent exhaustion flag before re-check", acc.UserName, acc.UserEmail)
			p.ClearQuotaExhausted(acc)
		}

		wg.Add(1)
		sem <- struct{}{} // acquire semaphore slot
		go func(acc *Account, quota accountQuotaSnapshot) {
			defer wg.Done()
			defer func() { <-sem }() // release semaphore slot

			// 1. Check quota — applyQuotaInfo handles state transitions and
			// ensures every write happens under p.mu so concurrent selectors
			// always observe a consistent view.
			info, err := quotaFetcher(acc)
			if err != nil {
				log.Printf("[refresh] %s (%s): quota check failed: %v", acc.UserName, acc.UserEmail, err)
				failedChecks.Add(1)
			} else {
				res := p.applyQuotaInfo(acc, info)
				premiumInfo := ""
				if info.HasPremium {
					premiumInfo = fmt.Sprintf(", premium %d/%d", info.PremiumUsage, info.PremiumLimit)
				}
				if info.ResearchModeUsage > 0 {
					premiumInfo += fmt.Sprintf(", research=%d", info.ResearchModeUsage)
				}
				switch {
				case res.NowPermanent:
					log.Printf("[refresh] %s (%s): NOT eligible — disabled permanently (free plan, space %d/%d, user %d/%d)",
						acc.UserName, acc.UserEmail, info.SpaceUsage, info.SpaceLimit, info.UserUsage, info.UserLimit)
					disabledNow.Add(1)
				case res.NowExhausted:
					log.Printf("[refresh] %s (%s): NOT eligible — disabled (space %d/%d, user %d/%d%s)",
						acc.UserName, acc.UserEmail, info.SpaceUsage, info.SpaceLimit, info.UserUsage, info.UserLimit, premiumInfo)
					disabledNow.Add(1)
				case res.Recovered:
					log.Printf("[refresh] %s (%s): RECOVERED (space %d/%d, user %d/%d, remaining ~%d%s)",
						acc.UserName, acc.UserEmail, info.SpaceUsage, info.SpaceLimit, info.UserUsage, info.UserLimit, res.BasicLeft, premiumInfo)
					recoveredNow.Add(1)
				default:
					log.Printf("[refresh] %s (%s): eligible (space %d/%d, user %d/%d, remaining ~%d%s)",
						acc.UserName, acc.UserEmail, info.SpaceUsage, info.SpaceLimit, info.UserUsage, info.UserLimit, res.BasicLeft, premiumInfo)
				}
			}

			p.refreshMu.Lock()
			p.refreshDone++
			p.refreshMu.Unlock()

			// 2. Fetch models
			models, err := modelsFetcher(acc)
			if err != nil {
				log.Printf("[refresh] %s (%s): model fetch failed: %v", acc.UserName, acc.UserEmail, err)
			} else if len(models) > 0 {
				acc.setModels(models)
				modelsUpdatedFlag.Store(true)
				log.Printf("[refresh] %s (%s): fetched %d models", acc.UserName, acc.UserEmail, len(models))
			}

			// 3. Probe workspace. An account whose user_root has zero
			// space_views is the silent failure mode that makes /ai
			// hang forever — exclude these from selection so the user
			// never opens a dead account from the dashboard.
			count, err := workspaceProbe(acc)
			if err != nil {
				log.Printf("[refresh] %s (%s): workspace probe failed: %v", acc.UserName, acc.UserEmail, err)
			} else {
				prev, changed := p.applyWorkspaceCount(acc, count)
				switch {
				case count == 0 && (changed || prev == 0):
					log.Printf("[refresh] %s (%s): NO WORKSPACE — excluded from pool (loadUserContent.user_root.space_views is empty)", acc.UserName, acc.UserEmail)
					workspaceLost.Add(1)
				case count > 0 && changed:
					log.Printf("[refresh] %s (%s): %d workspace(s) accessible", acc.UserName, acc.UserEmail, count)
				}
			}
		}(acc, quota)
	}
	wg.Wait()

	modelsUpdated := modelsUpdatedFlag.Load()

	if modelsUpdated {
		// Update DefaultModelMap from fetched models
		p.mu.RLock()
		for _, acc := range p.accounts {
			for _, m := range acc.modelsSnapshot() {
				normalizedName := normalizeModelName(m.Name)
				if normalizedName != "" {
					SetModelID(normalizedName, m.ID)
				}
			}
		}
		p.mu.RUnlock()
	}

	// 3. Persist to disk
	if accountsDir != "" {
		p.SaveAccounts(accountsDir)
	}

	available := p.AvailableCount()
	log.Printf("[refresh] complete: %d/%d available, disabled=%d, recovered=%d, no_workspace=%d, check_errors=%d",
		available, len(accs), disabledNow.Load(), recoveredNow.Load(), workspaceLost.Load(), failedChecks.Load())
}

// normalizeModelName converts display name like "GPT-5.2" to a user-friendly alias like "gpt-5.2"
func normalizeModelName(displayName string) string {
	s := strings.ToLower(strings.TrimSpace(displayName))
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// SaveAccounts persists current account state (models, quota) back to JSON files.
// Each account is matched to its disk file by account_id (not email).
// If no file exists for an in-memory account, a new file is created.
func (p *AccountPool) SaveAccounts(dir string) {
	p.mu.RLock()
	accs := make([]*Account, len(p.accounts))
	copy(accs, p.accounts)
	p.mu.RUnlock()

	for _, acc := range accs {
		acc.EnsureAccountID()
		err := saveAccountFile(dir, acc)
		if err != nil {
			// No disk file yet — create one.
			if _, createErr := SaveAccountToFile(acc, dir); createErr != nil {
				log.Printf("[save-accounts] create file for %s (aid=%s): %v",
					acc.UserEmail, acc.ShortSpaceID(), createErr)
			}
		}
	}
}

// saveAccountFile rewrites a single account's JSON file so it carries the
// freshest quota_info / quota_checked_at / available_models, while
// preserving every other field (token, ids, browser/device, registered_via).
//
// The write is atomic (tmp + os.Rename) so a crash mid-save can't leave
// LoadFromDir staring at a half-written JSON. Callers should hold no
// AccountPool lock — we read acc fields directly so concurrent updates by
// applyQuotaInfo are protected by Go's safe-map / pointer guarantees on
// the small primitives we copy.
//
// Returns an error if no on-disk file matches acc.UserEmail; that is a
// real-world signal someone deleted the account file out from under us.
func saveAccountFile(dir string, acc *Account) error {
	if acc == nil {
		return fmt.Errorf("saveAccountFile: nil account")
	}
	if dir == "" {
		return fmt.Errorf("saveAccountFile: empty dir")
	}
	if err := ensurePrivateAccountsDir(dir); err != nil {
		return fmt.Errorf("secure accounts dir: %w", err)
	}
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	var matchPath string
	var existing map[string]interface{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		// Match by account_id first, fall back to user_id+space_id
		acc.EnsureAccountID()
		exAID, _ := raw["account_id"].(string)
		if exAID == "" {
			exUID, _ := raw["user_id"].(string)
			exSID, _ := raw["space_id"].(string)
			if exUID != "" && exSID != "" {
				exAID = ComputeAccountID(exUID, exSID)
			}
		}
		if acc.AccountID == "" || exAID != acc.AccountID {
			continue
		}
		matchPath = path
		existing = raw
		break
	}
	if matchPath == "" {
		return fmt.Errorf("no account file matches account_id for %s", acc.UserEmail)
	}

	// Migrate: persist account_id into legacy files
	if acc.AccountID != "" {
		existing["account_id"] = acc.AccountID
	}

	if len(acc.Models) > 0 {
		modelEntries := make([]map[string]string, 0, len(acc.Models))
		for _, m := range acc.Models {
			modelEntries = append(modelEntries, map[string]string{"id": m.ID, "name": m.Name})
		}
		existing["available_models"] = modelEntries
	}
	if acc.QuotaInfo != nil {
		existing["quota_info"] = map[string]interface{}{
			"is_eligible":         acc.QuotaInfo.IsEligible,
			"space_usage":         acc.QuotaInfo.SpaceUsage,
			"space_limit":         acc.QuotaInfo.SpaceLimit,
			"user_usage":          acc.QuotaInfo.UserUsage,
			"user_limit":          acc.QuotaInfo.UserLimit,
			"last_usage_at":       acc.QuotaInfo.LastUsageAtMs,
			"research_mode_usage": acc.QuotaInfo.ResearchModeUsage,
			"has_premium":         acc.QuotaInfo.HasPremium,
			"premium_balance":     acc.QuotaInfo.PremiumBalance,
			"premium_usage":       acc.QuotaInfo.PremiumUsage,
			"premium_limit":       acc.QuotaInfo.PremiumLimit,
		}
	}
	if acc.QuotaCheckedAt != nil {
		existing["quota_checked_at"] = acc.QuotaCheckedAt.Format(time.RFC3339)
	}
	if acc.WorkspaceCheckedAt != nil {
		existing["space_count"] = acc.SpaceCount
		existing["workspace_checked_at"] = acc.WorkspaceCheckedAt.Format(time.RFC3339)
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	out = append(out, '\n')
	tmp := matchPath + ".tmp"
	if err := writePrivateAccountFile(tmp, out); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, matchPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// RefreshAndPersistAccount runs a live quota + models check for a single
// account and persists the result to disk. Designed for one-off refreshes
// (e.g. immediately after a successful registration) so the dashboard
// sees real numbers without waiting for the next global refresh tick.
//
// Errors:
//   - email is unknown to the pool (no Account loaded yet)
//   - quota fetch failed (we don't persist nothing)
//   - ctx was already cancelled
//
// A models-fetch failure is logged but not returned, since quota is the
// higher-value half of the snapshot.
func (p *AccountPool) RefreshAndPersistAccount(ctx context.Context, accountsDir, email string) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	target := strings.ToLower(strings.TrimSpace(email))
	if target == "" {
		return fmt.Errorf("RefreshAndPersistAccount: empty email")
	}

	p.mu.RLock()
	var acc *Account
	for _, a := range p.accounts {
		if strings.EqualFold(strings.TrimSpace(a.UserEmail), target) {
			acc = a
			break
		}
	}
	p.mu.RUnlock()
	if acc == nil {
		return fmt.Errorf("account not in pool: %s", email)
	}

	info, err := quotaFetcher(acc)
	if err != nil {
		return fmt.Errorf("quota check: %w", err)
	}
	p.applyQuotaInfo(acc, info)

	models, mErr := modelsFetcher(acc)
	if mErr != nil {
		log.Printf("[post-register] %s: models fetch failed: %v (persisting quota only)", acc.UserEmail, mErr)
	} else if len(models) > 0 {
		p.mu.Lock()
		acc.Models = models
		p.mu.Unlock()
	}

	// Workspace probe is best-effort. If it fails we still persist the
	// quota so the dashboard sees real numbers; the next /admin/refresh
	// retry will re-probe.
	if count, wErr := workspaceProbe(acc); wErr != nil {
		log.Printf("[post-register] %s: workspace probe failed: %v", acc.UserEmail, wErr)
	} else {
		p.applyWorkspaceCount(acc, count)
		if count == 0 {
			log.Printf("[post-register] %s: NO WORKSPACE detected immediately after registration — account will be excluded from the pool", acc.UserEmail)
		}
	}

	if accountsDir == "" {
		return nil
	}
	if err := saveAccountFile(accountsDir, acc); err != nil {
		return fmt.Errorf("persist: %w", err)
	}
	log.Printf("[post-register] %s: quota refreshed and persisted (eligible=%v, space %d/%d, workspaces=%d)",
		acc.UserEmail, info.IsEligible, info.SpaceUsage, info.SpaceLimit, acc.SpaceCount)
	return nil
}

// StartRefreshLoop runs a background goroutine that periodically refreshes all accounts
func (p *AccountPool) StartRefreshLoop(interval time.Duration, accountsDir string) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			p.RefreshAll(accountsDir)
		}
	}()
}

// GetAccountDetails returns detailed info for all accounts (for admin dashboard)
func (p *AccountPool) GetAccountDetails() []map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var details []map[string]interface{}
	for _, acc := range p.accounts {
		quota := acc.quotaSnapshot()
		models := acc.modelsSnapshot()
		acc.EnsureAccountID()
		entry := map[string]interface{}{
			"account_id":     acc.AccountID,
			"email":          acc.UserEmail,
			"name":           acc.UserName,
			"plan":           acc.PlanType,
			"space":          acc.SpaceName,
			"space_id_short": acc.ShortSpaceID(),
			"exhausted":      p.isQuotaExhausted(acc),
			"permanent":      quota.PermanentlyExhausted,
			"no_workspace":   p.hasNoWorkspace(acc),
		}
		if acc.WorkspaceCheckedAt != nil {
			entry["space_count"] = acc.SpaceCount
			entry["workspace_checked_at"] = acc.WorkspaceCheckedAt.Format(time.RFC3339)
		}
		if acc.RegisteredVia != "" {
			entry["registered_via"] = acc.RegisteredVia
		}
		if quota.Info != nil {
			entry["eligible"] = quota.Info.IsEligible
			entry["usage"] = quota.Info.SpaceUsage
			entry["limit"] = quota.Info.SpaceLimit
			entry["space_usage"] = quota.Info.SpaceUsage
			entry["space_limit"] = quota.Info.SpaceLimit
			entry["space_remaining"] = quotaRemaining(quota.Info.SpaceLimit, quota.Info.SpaceUsage)
			entry["user_usage"] = quota.Info.UserUsage
			entry["user_limit"] = quota.Info.UserLimit
			entry["user_remaining"] = quotaRemaining(quota.Info.UserLimit, quota.Info.UserUsage)
			entry["remaining"] = basicRemaining(quota.Info)
			entry["last_usage_at"] = quota.Info.LastUsageAtMs
			// Research mode (V1)
			entry["research_usage"] = quota.Info.ResearchModeUsage
			// Premium credit data (V2)
			entry["has_premium"] = quota.Info.HasPremium
			entry["premium_balance"] = quota.Info.PremiumBalance
			entry["premium_usage"] = quota.Info.PremiumUsage
			entry["premium_limit"] = quota.Info.PremiumLimit
		}
		if quota.CheckedAt != nil {
			entry["checked_at"] = quota.CheckedAt.Format(time.RFC3339)
		}
		if p.isQuotaExhausted(acc) && quota.ExhaustedAt != nil {
			entry["exhausted_at"] = quota.ExhaustedAt.Format(time.RFC3339)
		}
		// Models
		var modelEntries []map[string]string
		for _, m := range models {
			modelEntries = append(modelEntries, map[string]string{"id": m.ID, "name": m.Name})
		}
		entry["models"] = modelEntries
		details = append(details, entry)
	}
	return details
}

// GetQuotaSummary returns quota summary for all accounts (for /health endpoint)
func (p *AccountPool) GetQuotaSummary() []map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var summary []map[string]interface{}
	for _, acc := range p.accounts {
		quota := acc.quotaSnapshot()
		entry := map[string]interface{}{
			"email":        acc.UserEmail,
			"name":         acc.UserName,
			"plan":         acc.PlanType,
			"exhausted":    p.isQuotaExhausted(acc),
			"permanent":    quota.PermanentlyExhausted,
			"no_workspace": p.hasNoWorkspace(acc),
		}
		if acc.WorkspaceCheckedAt != nil {
			entry["space_count"] = acc.SpaceCount
			entry["workspace_checked_at"] = acc.WorkspaceCheckedAt.Format(time.RFC3339)
		}
		if quota.Info != nil {
			entry["eligible"] = quota.Info.IsEligible
			entry["usage"] = quota.Info.SpaceUsage
			entry["limit"] = quota.Info.SpaceLimit
			entry["space_usage"] = quota.Info.SpaceUsage
			entry["space_limit"] = quota.Info.SpaceLimit
			entry["space_remaining"] = quotaRemaining(quota.Info.SpaceLimit, quota.Info.SpaceUsage)
			entry["user_usage"] = quota.Info.UserUsage
			entry["user_limit"] = quota.Info.UserLimit
			entry["user_remaining"] = quotaRemaining(quota.Info.UserLimit, quota.Info.UserUsage)
			entry["remaining"] = basicRemaining(quota.Info)
			entry["last_usage_at"] = quota.Info.LastUsageAtMs
			// Research mode (V1)
			entry["research_usage"] = quota.Info.ResearchModeUsage
			// Premium credit data (V2)
			entry["has_premium"] = quota.Info.HasPremium
			entry["premium_balance"] = quota.Info.PremiumBalance
			entry["premium_usage"] = quota.Info.PremiumUsage
			entry["premium_limit"] = quota.Info.PremiumLimit
		}
		if quota.CheckedAt != nil {
			entry["checked_at"] = quota.CheckedAt.Format(time.RFC3339)
		}
		summary = append(summary, entry)
	}
	return summary
}

// loadPersistedWorkspace fills acc.SpaceCount / acc.WorkspaceCheckedAt
// from the persisted JSON. Absent fields leave the runtime values
// untouched (so the next probe still treats the account as unknown).
func loadPersistedWorkspace(data []byte, acc *Account) {
	var raw struct {
		SpaceCount         *int    `json:"space_count"`
		WorkspaceCheckedAt *string `json:"workspace_checked_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	if raw.WorkspaceCheckedAt == nil || *raw.WorkspaceCheckedAt == "" {
		return
	}
	t, err := time.Parse(time.RFC3339, *raw.WorkspaceCheckedAt)
	if err != nil {
		return
	}
	acc.WorkspaceCheckedAt = &t
	if raw.SpaceCount != nil {
		acc.SpaceCount = *raw.SpaceCount
	}
}

// loadPersistedQuotaInfo parses the persisted quota_info (snake_case keys) from raw account JSON.
// Returns nil if quota_info is not present or cannot be parsed.
func loadPersistedQuotaInfo(data []byte) *QuotaInfo {
	var raw struct {
		QuotaInfo *struct {
			IsEligible        bool  `json:"is_eligible"`
			SpaceUsage        int   `json:"space_usage"`
			SpaceLimit        int   `json:"space_limit"`
			UserUsage         int   `json:"user_usage"`
			UserLimit         int   `json:"user_limit"`
			LastUsageAt       int64 `json:"last_usage_at"`
			ResearchModeUsage int   `json:"research_mode_usage"`
			HasPremium        bool  `json:"has_premium"`
			PremiumBalance    int   `json:"premium_balance"`
			PremiumUsage      int   `json:"premium_usage"`
			PremiumLimit      int   `json:"premium_limit"`
		} `json:"quota_info"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || raw.QuotaInfo == nil {
		return nil
	}
	return &QuotaInfo{
		IsEligible:        raw.QuotaInfo.IsEligible,
		SpaceUsage:        raw.QuotaInfo.SpaceUsage,
		SpaceLimit:        raw.QuotaInfo.SpaceLimit,
		UserUsage:         raw.QuotaInfo.UserUsage,
		UserLimit:         raw.QuotaInfo.UserLimit,
		LastUsageAtMs:     raw.QuotaInfo.LastUsageAt,
		ResearchModeUsage: raw.QuotaInfo.ResearchModeUsage,
		HasPremium:        raw.QuotaInfo.HasPremium,
		PremiumBalance:    raw.QuotaInfo.PremiumBalance,
		PremiumUsage:      raw.QuotaInfo.PremiumUsage,
		PremiumLimit:      raw.QuotaInfo.PremiumLimit,
	}
}

func quotaRemaining(limit, usage int) int {
	if limit <= 0 {
		return 0
	}
	remaining := limit - usage
	if remaining < 0 {
		return 0
	}
	return remaining
}

func basicRemaining(info *QuotaInfo) int {
	if info == nil {
		return 0
	}
	remaining := []int{}
	if info.SpaceLimit > 0 {
		remaining = append(remaining, quotaRemaining(info.SpaceLimit, info.SpaceUsage))
	}
	if info.UserLimit > 0 {
		remaining = append(remaining, quotaRemaining(info.UserLimit, info.UserUsage))
	}
	if len(remaining) == 0 {
		return 0
	}
	best := remaining[0]
	for _, value := range remaining[1:] {
		if value < best {
			best = value
		}
	}
	return best
}

// accountQuotaPriority returns a sortable score for an account's remaining
// quota. Higher = more preferred when picking the "best" account.
//
//   - Unknown quota (QuotaInfo == nil): -1 (treated as fallback in pickBest).
//   - Premium account: basicRemaining + premiumRemaining (premium credits add
//     significant headroom so a premium account with low basic credits should
//     still rank above a basic-only account that's nearly drained).
//   - Basic-only account: basicRemaining (space ⊓ user).
func accountQuotaPriority(acc *Account) int {
	quota := acc.quotaInfoSnapshot()
	if quota == nil {
		return -1
	}
	score := basicRemaining(quota)
	if quota.HasPremium {
		// Premium balance often dwarfs basic credits — fold it in so premium
		// accounts win over basic-only accounts when both are eligible.
		score += quota.PremiumBalance
		// If both basic and premium look exhausted but isEligible is still
		// true (rare), keep premium accounts above unknown-quota fallback.
		if score <= 0 {
			score = 1
		}
	}
	return score
}

func generateUUIDv4() string {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		// Fallback
		for i := range b {
			b[i] = byte(mrand.Intn(256))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

package proxy

import (
	"errors"
	"testing"
	"time"
)

func TestQuotaSchedulerAllowPremiumFalseSkipsPremiumOnlyAccounts(t *testing.T) {
	withTestQuotaScheduling(t, quotaStrategyBalanced, false, 0)
	premiumOnly := &Account{
		UserEmail: "premium-only@example.com",
		QuotaInfo: quotaWithBasicAndPremium(0, 500),
	}
	basic := &Account{
		UserEmail: "basic@example.com",
		QuotaInfo: quotaWithBasicAndPremium(20, 0),
	}
	pool := newPool(premiumOnly, basic)

	if got := pool.NextBest(); got == nil || got.UserEmail != "basic@example.com" {
		t.Fatalf("allow_premium=false should skip premium-only account, got %#v", got)
	}

	lease, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("NextBestLease: %v", err)
	}
	if lease == nil || lease.Account() != basic {
		t.Fatalf("lease should select basic account, got %#v", lease)
	}
	lease.Release()
}

func TestQuotaSchedulerAllowPremiumFalseStillUsesPremiumAccountBasicCredits(t *testing.T) {
	withTestQuotaScheduling(t, quotaStrategyBalanced, false, 0)
	premiumWithBasic := &Account{
		UserEmail: "premium-with-basic@example.com",
		QuotaInfo: quotaWithBasicAndPremium(150, 500),
	}
	basic := &Account{
		UserEmail: "basic@example.com",
		QuotaInfo: quotaWithBasicAndPremium(20, 0),
	}
	pool := newPool(basic, premiumWithBasic)

	if got := pool.NextBest(); got == nil || got.UserEmail != "premium-with-basic@example.com" {
		t.Fatalf("allow_premium=false should still use basic credits on premium accounts, got %#v", got)
	}
}

func TestQuotaSchedulerPremiumReserveThreshold(t *testing.T) {
	withTestQuotaScheduling(t, quotaStrategyPremiumFirst, true, 100)
	reserved := &Account{
		UserEmail: "reserved@example.com",
		QuotaInfo: quotaWithBasicAndPremium(0, 100),
	}
	usablePremium := &Account{
		UserEmail: "usable-premium@example.com",
		QuotaInfo: quotaWithBasicAndPremium(0, 101),
	}
	pool := newPool(reserved, usablePremium)

	if got := pool.NextBest(); got == nil || got.UserEmail != "usable-premium@example.com" {
		t.Fatalf("premium balance at threshold must not count as usable premium, got %#v", got)
	}

	withTestQuotaScheduling(t, quotaStrategyBalanced, true, 100)
	pool = newPool(reserved)
	if got := pool.NextBest(); got != nil {
		t.Fatalf("premium-only account at reserve threshold should be skipped, got %#v", got)
	}
}

func TestQuotaSchedulerStrategies(t *testing.T) {
	basicRich := &Account{UserEmail: "basic-rich@example.com", QuotaInfo: quotaWithBasicAndPremium(150, 0)}
	premiumRich := &Account{UserEmail: "premium-rich@example.com", QuotaInfo: quotaWithBasicAndPremium(10, 1000)}

	t.Run("balanced folds basic and premium", func(t *testing.T) {
		withTestQuotaScheduling(t, quotaStrategyBalanced, true, 0)
		pool := newPool(basicRich, premiumRich)
		if got := pool.NextBest(); got == nil || got.UserEmail != "premium-rich@example.com" {
			t.Fatalf("balanced should prefer larger combined balance, got %#v", got)
		}
	})

	t.Run("basic first", func(t *testing.T) {
		withTestQuotaScheduling(t, quotaStrategyBasicFirst, true, 0)
		pool := newPool(premiumRich, basicRich)
		if got := pool.NextBest(); got == nil || got.UserEmail != "basic-rich@example.com" {
			t.Fatalf("basic_first should prefer available basic credits, got %#v", got)
		}
	})

	t.Run("premium first", func(t *testing.T) {
		withTestQuotaScheduling(t, quotaStrategyPremiumFirst, true, 0)
		pool := newPool(basicRich, premiumRich)
		if got := pool.NextBest(); got == nil || got.UserEmail != "premium-rich@example.com" {
			t.Fatalf("premium_first should prefer usable premium credits, got %#v", got)
		}
	})
}

func TestQuotaSchedulerHardFilters(t *testing.T) {
	withTestQuotaScheduling(t, quotaStrategyBalanced, true, 0)
	now := time.Now()
	accounts := []*Account{
		{UserEmail: "disabled@example.com", Disabled: true, QuotaInfo: quotaWithBasicAndPremium(200, 1000)},
		{UserEmail: "no-workspace@example.com", WorkspaceCheckedAt: &now, SpaceCount: 0, QuotaInfo: quotaWithBasicAndPremium(200, 1000)},
		{UserEmail: "ineligible@example.com", QuotaInfo: &QuotaInfo{IsEligible: false, SpaceLimit: 200, SpaceUsage: 0, UserLimit: 200, UserUsage: 0, HasPremium: true, PremiumBalance: 1000}},
		{UserEmail: "eligible@example.com", WorkspaceCheckedAt: &now, SpaceCount: 1, QuotaInfo: quotaWithBasicAndPremium(10, 0)},
	}
	pool := newPool(accounts...)

	if got := pool.NextBest(); got == nil || got.UserEmail != "eligible@example.com" {
		t.Fatalf("hard filters should leave only eligible account, got %#v", got)
	}
}

func TestQuotaSchedulerBalancedConsidersInflight(t *testing.T) {
	withTestMaxConcurrency(t, 3)
	withTestQuotaScheduling(t, quotaStrategyBalanced, true, 0)
	busyHigh := &Account{
		UserEmail: "busy-high@example.com",
		QuotaInfo: quotaWithBasicAndPremium(180, 0),
		InFlight:  2,
	}
	idleLow := &Account{
		UserEmail: "idle-low@example.com",
		QuotaInfo: quotaWithBasicAndPremium(100, 0),
	}
	pool := newPool(busyHigh, idleLow)

	lease, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("NextBestLease: %v", err)
	}
	if lease == nil || lease.Account() != idleLow {
		t.Fatalf("balanced lease should consider inflight and prefer idle lower-balance account, got %#v", lease)
	}
	lease.Release()
}

func TestQuotaSchedulerDoesNotDoubleCountSharedSpaceQuota(t *testing.T) {
	withTestQuotaScheduling(t, quotaStrategyBalanced, true, 0)
	sharedA := &Account{
		UserEmail: "shared-a@example.com",
		SpaceID:   "shared-space",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 1000, SpaceUsage: 100, UserLimit: 200, UserUsage: 190}, // user left 10
	}
	sharedB := &Account{
		UserEmail: "shared-b@example.com",
		SpaceID:   "shared-space",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 1000, SpaceUsage: 100, UserLimit: 200, UserUsage: 20}, // user left 180
	}
	otherSpace := &Account{
		UserEmail: "other-space@example.com",
		SpaceID:   "other-space",
		QuotaInfo: &QuotaInfo{IsEligible: true, SpaceLimit: 400, SpaceUsage: 50, UserLimit: 200, UserUsage: 100}, // user left 100
	}
	pool := newPool(sharedA, sharedB, otherSpace)

	if got := pool.NextBest(); got == nil || got.UserEmail != "shared-b@example.com" {
		t.Fatalf("shared space score must be differentiated by user quota, got %#v", got)
	}

	sharedB.InFlight = 2
	withTestMaxConcurrency(t, 3)
	lease, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("NextBestLease: %v", err)
	}
	if lease == nil || lease.Account().UserEmail != "other-space@example.com" {
		t.Fatalf("inflight should differentiate shared-space members, got %#v", lease)
	}
	lease.Release()
}

func TestQuotaSchedulerAllPremiumOnlyBusyWithPremiumDisabledLooksUnavailable(t *testing.T) {
	withTestMaxConcurrency(t, 1)
	withTestQuotaScheduling(t, quotaStrategyBalanced, false, 0)
	premiumOnlyBusy := &Account{UserEmail: "premium-only@example.com", QuotaInfo: quotaWithBasicAndPremium(0, 500), InFlight: 1}
	pool := newPool(premiumOnlyBusy)

	lease, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("premium-disabled account should be filtered by quota policy, not reported busy: %v", err)
	}
	if lease != nil {
		t.Fatalf("expected no lease, got %#v", lease)
	}
}

func TestQuotaSchedulerBusyEligibleAccountsStillReturnBusy(t *testing.T) {
	withTestMaxConcurrency(t, 1)
	withTestQuotaScheduling(t, quotaStrategyBalanced, true, 0)
	busy := &Account{UserEmail: "busy@example.com", QuotaInfo: quotaWithBasicAndPremium(100, 0), InFlight: 1}
	pool := newPool(busy)

	lease, err := pool.NextBestLease(nil)
	if !errors.Is(err, ErrAllAccountsBusy) {
		t.Fatalf("expected ErrAllAccountsBusy, got lease=%#v err=%v", lease, err)
	}
	if lease != nil {
		t.Fatalf("expected nil lease, got %#v", lease)
	}
}

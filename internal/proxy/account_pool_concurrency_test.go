package proxy

import (
	"errors"
	"sync"
	"testing"
)

func withTestMaxConcurrency(t *testing.T, n int) {
	t.Helper()
	prev := AppConfig
	cfg := DefaultConfig()
	cfg.Proxy.MaxConcurrency = n
	AppConfig = cfg
	t.Cleanup(func() { AppConfig = prev })
}

func quotaWithRemaining(remaining int) *QuotaInfo {
	limit := 200
	usage := limit - remaining
	if usage < 0 {
		usage = 0
	}
	return &QuotaInfo{
		IsEligible: true,
		SpaceLimit: limit,
		SpaceUsage: usage,
		UserLimit:  limit,
		UserUsage:  usage,
	}
}

func TestNextBestLeasePicksHighestRemainingQuota(t *testing.T) {
	withTestMaxConcurrency(t, 1)
	low := &Account{UserEmail: "low@example.com", QuotaInfo: quotaWithRemaining(20)}
	mid := &Account{UserEmail: "mid@example.com", QuotaInfo: quotaWithRemaining(100)}
	high := &Account{UserEmail: "high@example.com", QuotaInfo: quotaWithRemaining(180)}
	pool := newPool(low, mid, high)

	lease, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("NextBestLease returned error: %v", err)
	}
	if lease == nil || lease.Account() != high {
		t.Fatalf("expected highest-remaining account, got %#v", lease)
	}
	if high.InFlight != 1 {
		t.Fatalf("expected selected account InFlight=1, got %d", high.InFlight)
	}
	lease.Release()
}

func TestNextBestLeaseSkipsBusyAccountAtConcurrencyLimit(t *testing.T) {
	withTestMaxConcurrency(t, 1)
	busyHigh := &Account{
		UserEmail: "busy-high@example.com",
		QuotaInfo: quotaWithRemaining(180),
		InFlight:  1,
	}
	availableLow := &Account{
		UserEmail: "available-low@example.com",
		QuotaInfo: quotaWithRemaining(20),
	}
	pool := newPool(busyHigh, availableLow)

	lease, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("NextBestLease returned error: %v", err)
	}
	if lease == nil || lease.Account() != availableLow {
		t.Fatalf("expected busy high-balance account to be skipped, got %#v", lease)
	}
	if busyHigh.InFlight != 1 {
		t.Fatalf("busy account InFlight should be unchanged, got %d", busyHigh.InFlight)
	}
	if availableLow.InFlight != 1 {
		t.Fatalf("selected account should be leased, got InFlight=%d", availableLow.InFlight)
	}
	lease.Release()
}

func TestNextBestLeaseAllBusyReturnsErrAllAccountsBusy(t *testing.T) {
	withTestMaxConcurrency(t, 1)
	a := &Account{UserEmail: "a@example.com", QuotaInfo: quotaWithRemaining(180), InFlight: 1}
	b := &Account{UserEmail: "b@example.com", QuotaInfo: quotaWithRemaining(100), InFlight: 1}
	pool := newPool(a, b)

	lease, err := pool.NextBestLease(nil)
	if !errors.Is(err, ErrAllAccountsBusy) {
		t.Fatalf("expected ErrAllAccountsBusy, got lease=%#v err=%v", lease, err)
	}
	if lease != nil {
		t.Fatalf("busy selection must not return a lease: %#v", lease)
	}
	if a.InFlight != 1 || b.InFlight != 1 {
		t.Fatalf("busy failure must not mutate InFlight: a=%d b=%d", a.InFlight, b.InFlight)
	}
}

func TestNextBestLeaseReleaseAllowsReselection(t *testing.T) {
	withTestMaxConcurrency(t, 1)
	acc := &Account{UserEmail: "solo@example.com", QuotaInfo: quotaWithRemaining(180)}
	pool := newPool(acc)

	first, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("first lease returned error: %v", err)
	}
	if first == nil || first.Account() != acc {
		t.Fatalf("expected first lease on solo account, got %#v", first)
	}
	if acc.InFlight != 1 {
		t.Fatalf("expected InFlight=1 after first lease, got %d", acc.InFlight)
	}

	second, err := pool.NextBestLease(nil)
	if !errors.Is(err, ErrAllAccountsBusy) {
		t.Fatalf("expected ErrAllAccountsBusy while lease is held, got lease=%#v err=%v", second, err)
	}
	if second != nil {
		t.Fatalf("expected no second lease while account is busy, got %#v", second)
	}

	first.Release()
	if acc.InFlight != 0 {
		t.Fatalf("expected InFlight=0 after release, got %d", acc.InFlight)
	}
	first.Release()
	if acc.InFlight != 0 {
		t.Fatalf("double release must be idempotent, got InFlight=%d", acc.InFlight)
	}

	third, err := pool.NextBestLease(nil)
	if err != nil {
		t.Fatalf("third lease after release returned error: %v", err)
	}
	if third == nil || third.Account() != acc {
		t.Fatalf("expected released account to be selectable again, got %#v", third)
	}
	third.Release()
}

func TestNextBestLeaseConcurrentAcquireHonorsLimitAndReleases(t *testing.T) {
	withTestMaxConcurrency(t, 3)
	acc := &Account{UserEmail: "concurrent@example.com", QuotaInfo: quotaWithRemaining(180)}
	pool := newPool(acc)

	const workers = 20
	start := make(chan struct{})
	leases := make(chan *AccountLease, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, err := pool.NextBestLease(nil)
			if err != nil {
				errs <- err
				return
			}
			leases <- lease
		}()
	}
	close(start)
	wg.Wait()
	close(leases)
	close(errs)

	var held []*AccountLease
	for lease := range leases {
		if lease == nil || lease.Account() != acc {
			t.Fatalf("unexpected lease: %#v", lease)
		}
		held = append(held, lease)
	}
	busyCount := 0
	for err := range errs {
		if !errors.Is(err, ErrAllAccountsBusy) {
			t.Fatalf("expected only ErrAllAccountsBusy from rejected workers, got %v", err)
		}
		busyCount++
	}
	if len(held) != 3 || busyCount != workers-3 {
		t.Fatalf("expected 3 leases and %d busy errors, got leases=%d busy=%d", workers-3, len(held), busyCount)
	}
	if acc.InFlight != 3 {
		t.Fatalf("expected InFlight=3 while leases are held, got %d", acc.InFlight)
	}
	for _, lease := range held {
		lease.Release()
	}
	if acc.InFlight != 0 {
		t.Fatalf("expected all releases to clear InFlight, got %d", acc.InFlight)
	}
}

func TestConfigMaxConcurrencyDefaultsAndBounds(t *testing.T) {
	if got := (*Config)(nil).MaxConcurrency(); got != 1 {
		t.Fatalf("nil config default: got %d, want 1", got)
	}
	if got := DefaultConfig().MaxConcurrency(); got != 1 {
		t.Fatalf("DefaultConfig MaxConcurrency: got %d, want 1", got)
	}
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{name: "zero", in: 0, want: 1},
		{name: "negative", in: -5, want: 1},
		{name: "lower bound", in: 1, want: 1},
		{name: "middle", in: 42, want: 42},
		{name: "upper bound", in: 100, want: 100},
		{name: "above upper bound", in: 101, want: 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Proxy: ProxyConfig{MaxConcurrency: tc.in}}
			if got := cfg.MaxConcurrency(); got != tc.want {
				t.Fatalf("MaxConcurrency(%d): got %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

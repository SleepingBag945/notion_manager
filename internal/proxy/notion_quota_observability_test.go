package proxy

import (
	"encoding/json"
	"testing"
)

func TestMergeQuotaResponsesPreservesCompleteV2Snapshot(t *testing.T) {
	var v1 quotaV1Response
	if err := json.Unmarshal([]byte(`{"isEligible":true,"researchModeUsage":7,"spaceUsage":65047,"spaceLimit":300,"userUsage":20933,"userLimit":75,"lastSpaceUsageAtMs":1786430876123}`), &v1); err != nil {
		t.Fatal(err)
	}
	var v2 quotaV2Response
	if err := json.Unmarshal([]byte(`{"basicCredits":{"spaceUsage":101,"spaceLimit":102,"userUsage":103,"userLimit":104,"lastSpaceUsageAtMs":1786430999123},"premiumCredits":{"totalCreditBalance":11,"creditsInOverage":12,"perSource":{"monthlyAllocated":{"usageTotal":554,"limit":300},"monthlyCommitted":{"usageTotal":21,"limit":40},"yearlyElastic":{"usageTotal":31,"limit":50}}}}`), &v2); err != nil {
		t.Fatal(err)
	}

	got := mergeQuotaResponses(v1, v2)
	if got.TotalCreditBalance != 11 || got.CreditsInOverage != 12 {
		t.Fatalf("raw balance fields = (%d, %d), want (11, 12)", got.TotalCreditBalance, got.CreditsInOverage)
	}
	if got.MonthlyAllocatedUsage != 554 || got.MonthlyAllocatedLimit != 300 {
		t.Fatalf("monthlyAllocated = (%d, %d), want (554, 300)", got.MonthlyAllocatedUsage, got.MonthlyAllocatedLimit)
	}
	if got.MonthlyCommittedUsage != 21 || got.MonthlyCommittedLimit != 40 {
		t.Fatalf("monthlyCommitted = (%d, %d), want (21, 40)", got.MonthlyCommittedUsage, got.MonthlyCommittedLimit)
	}
	if got.YearlyElasticUsage != 31 || got.YearlyElasticLimit != 50 {
		t.Fatalf("yearlyElastic = (%d, %d), want (31, 50)", got.YearlyElasticUsage, got.YearlyElasticLimit)
	}
	if got.V2SpaceUsage != 101 || got.V2SpaceLimit != 102 || got.V2UserUsage != 103 || got.V2UserLimit != 104 || got.V2LastUsageAtMs != 1786430999123 {
		t.Fatalf("V2 basicCredits not preserved: %+v", got)
	}
	if got.PremiumBalance != 11 || got.PremiumUsage != 554 || got.PremiumLimit != 300 {
		t.Fatalf("legacy premium aliases = (%d, %d, %d), want (11, 554, 300)", got.PremiumBalance, got.PremiumUsage, got.PremiumLimit)
	}
}

func TestLoadPersistedQuotaInfoPreservesCompleteV2Snapshot(t *testing.T) {
	raw := []byte(`{"quota_info":{"is_eligible":true,"total_credit_balance":11,"credits_in_overage":12,"monthly_allocated_usage":13,"monthly_allocated_limit":14,"monthly_committed_usage":15,"monthly_committed_limit":16,"yearly_elastic_usage":17,"yearly_elastic_limit":18,"v2_space_usage":19,"v2_space_limit":20,"v2_user_usage":21,"v2_user_limit":22,"v2_last_usage_at":23}}`)
	got := loadPersistedQuotaInfo(raw)
	if got == nil {
		t.Fatal("loadPersistedQuotaInfo() = nil")
	}
	if got.TotalCreditBalance != 11 || got.CreditsInOverage != 12 ||
		got.MonthlyAllocatedUsage != 13 || got.MonthlyAllocatedLimit != 14 ||
		got.MonthlyCommittedUsage != 15 || got.MonthlyCommittedLimit != 16 ||
		got.YearlyElasticUsage != 17 || got.YearlyElasticLimit != 18 ||
		got.V2SpaceUsage != 19 || got.V2SpaceLimit != 20 || got.V2UserUsage != 21 || got.V2UserLimit != 22 || got.V2LastUsageAtMs != 23 {
		t.Fatalf("complete V2 snapshot not preserved: %+v", got)
	}
}

func TestCompleteV2QuotaStaysOutOfPublicHealthSummary(t *testing.T) {
	acc := &Account{
		UserID: "user", SpaceID: "space", UserEmail: "private@example.com",
		QuotaInfo: &QuotaInfo{IsEligible: true, TotalCreditBalance: 11, CreditsInOverage: 12, MonthlyCommittedLimit: 40, V2SpaceUsage: 101},
	}
	pool := &AccountPool{accounts: []*Account{acc}}

	health := pool.GetQuotaSummary()[0]
	for _, key := range []string{"total_credit_balance", "credits_in_overage", "monthly_committed_limit", "v2_space_usage"} {
		if _, exists := health[key]; exists {
			t.Fatalf("public health summary unexpectedly exposes %q", key)
		}
	}

	admin := pool.GetAccountDetails()[0]
	if admin["total_credit_balance"] != 11 || admin["credits_in_overage"] != 12 || admin["monthly_committed_limit"] != 40 || admin["v2_space_usage"] != 101 {
		t.Fatalf("admin details missing complete V2 snapshot: %#v", admin)
	}
}

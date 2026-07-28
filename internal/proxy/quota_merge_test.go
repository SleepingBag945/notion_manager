package proxy

import "testing"

func TestBuildQuotaInfoFromResponsesPrefersV2BasicCreditsWhenLimitAvailable(t *testing.T) {
	v1 := quotaV1Response{
		IsEligible:         true,
		ResearchModeUsage:  7,
		SpaceUsage:         10,
		SpaceLimit:         100,
		UserUsage:          20,
		UserLimit:          200,
		LastSpaceUsageAtMs: 1111,
	}

	var v2 quotaV2Response
	v2.BasicCredits.SpaceUsage = 1
	v2.BasicCredits.SpaceLimit = 0
	v2.BasicCredits.UserUsage = 2
	v2.BasicCredits.UserLimit = 50
	v2.BasicCredits.LastSpaceUsageAtMs = 2222

	info := buildQuotaInfoFromResponses(v1, v2)

	if !info.IsEligible {
		t.Fatalf("expected eligibility from V1")
	}
	if info.ResearchModeUsage != 7 {
		t.Fatalf("expected research mode usage from V1, got %d", info.ResearchModeUsage)
	}
	if info.SpaceUsage != 1 || info.SpaceLimit != 0 || info.UserUsage != 2 || info.UserLimit != 50 || info.LastUsageAtMs != 2222 {
		t.Fatalf("expected V2 basic credits to win completely, got %+v", info)
	}
}

func TestBuildQuotaInfoFromResponsesFallsBackToV1BasicCreditsWhenV2HasNoLimit(t *testing.T) {
	v1 := quotaV1Response{
		IsEligible:         true,
		ResearchModeUsage:  3,
		SpaceUsage:         10,
		SpaceLimit:         100,
		UserUsage:          20,
		UserLimit:          200,
		LastSpaceUsageAtMs: 1111,
	}

	var v2 quotaV2Response
	v2.BasicCredits.SpaceUsage = 99
	v2.BasicCredits.SpaceLimit = 0
	v2.BasicCredits.UserUsage = 88
	v2.BasicCredits.UserLimit = 0
	v2.BasicCredits.LastSpaceUsageAtMs = 2222

	info := buildQuotaInfoFromResponses(v1, v2)

	if info.SpaceUsage != 10 || info.SpaceLimit != 100 || info.UserUsage != 20 || info.UserLimit != 200 || info.LastUsageAtMs != 1111 {
		t.Fatalf("expected V1 basic credits fallback, got %+v", info)
	}
}

func TestBuildQuotaInfoFromResponsesPremiumFallbackAndHasPremium(t *testing.T) {
	t.Run("total credit balance has priority over monthly allocated remaining", func(t *testing.T) {
		var v2 quotaV2Response
		v2.PremiumCredits.TotalCreditBalance = 5
		v2.PremiumCredits.PerSource.MonthlyAllocated.UsageTotal = 20
		v2.PremiumCredits.PerSource.MonthlyAllocated.Limit = 100

		info := buildQuotaInfoFromResponses(quotaV1Response{}, v2)

		if info.PremiumBalance != 5 {
			t.Fatalf("expected totalCreditBalance to win, got balance %d", info.PremiumBalance)
		}
		if info.PremiumUsage != 20 || info.PremiumLimit != 100 {
			t.Fatalf("expected monthlyAllocated usage/limit, got %+v", info)
		}
		if !info.HasPremium {
			t.Fatalf("expected HasPremium when monthlyAllocated limit/balance is present")
		}
	})

	t.Run("monthly allocated remaining is fallback balance", func(t *testing.T) {
		var v2 quotaV2Response
		v2.PremiumCredits.TotalCreditBalance = 0
		v2.PremiumCredits.PerSource.MonthlyAllocated.UsageTotal = 30
		v2.PremiumCredits.PerSource.MonthlyAllocated.Limit = 100

		info := buildQuotaInfoFromResponses(quotaV1Response{}, v2)

		if info.PremiumBalance != 70 {
			t.Fatalf("expected monthlyAllocated remaining fallback balance 70, got %d", info.PremiumBalance)
		}
		if !info.HasPremium {
			t.Fatalf("expected HasPremium from monthlyAllocated limit/fallback balance")
		}
	})

	t.Run("monthly committed marks premium", func(t *testing.T) {
		var v2 quotaV2Response
		v2.PremiumCredits.PerSource.MonthlyCommitted.Limit = 42

		info := buildQuotaInfoFromResponses(quotaV1Response{}, v2)

		if !info.HasPremium {
			t.Fatalf("expected HasPremium from monthlyCommitted limit")
		}
	})

	t.Run("yearly elastic marks premium", func(t *testing.T) {
		var v2 quotaV2Response
		v2.PremiumCredits.PerSource.YearlyElastic.Limit = 42

		info := buildQuotaInfoFromResponses(quotaV1Response{}, v2)

		if !info.HasPremium {
			t.Fatalf("expected HasPremium from yearlyElastic limit")
		}
	})
}

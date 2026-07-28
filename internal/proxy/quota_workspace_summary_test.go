package proxy

import "testing"

func TestSummarizeAccountsDeduplicatesSharedWorkspaceQuota(t *testing.T) {
	accounts := []map[string]interface{}{
		account("a", "a@x", map[string]interface{}{
			"space_id": "shared", "space_usage": 90, "space_limit": 100, "space_remaining": 10,
			"user_usage": 5, "user_limit": 20, "user_remaining": 15,
		}),
		account("b", "b@x", map[string]interface{}{
			"space_id": "shared", "space_usage": 90, "space_limit": 100, "space_remaining": 10,
			"user_usage": 10, "user_limit": 20, "user_remaining": 10,
		}),
		account("c", "c@x", map[string]interface{}{
			"space_id": "other", "space_usage": 20, "space_limit": 50, "space_remaining": 30,
			"user_usage": 2, "user_limit": 10, "user_remaining": 8,
		}),
	}
	s := summarizeAccounts(accounts)
	if s.TotalSpaceUsage != 110 || s.TotalSpaceLimit != 150 || s.TotalSpaceRemaining != 40 {
		t.Fatalf("shared workspace counted more than once: %+v", s)
	}
	if s.TotalUserUsage != 17 || s.TotalUserLimit != 50 || s.TotalUserRemaining != 33 {
		t.Fatalf("user quotas should remain per-account: %+v", s)
	}
}

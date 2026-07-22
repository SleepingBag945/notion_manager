package proxy

import (
	"reflect"
	"testing"
)

// account is a tiny constructor that keeps test cases readable. Fields
// not provided default to "available, unlimited remaining" so callers
// only set what matters for the case at hand.
func account(name, email string, override map[string]interface{}) map[string]interface{} {
	a := map[string]interface{}{
		"email":     email,
		"name":      name,
		"plan":      "personal",
		"space":     "",
		"exhausted": false,
		"permanent": false,
	}
	for k, v := range override {
		a[k] = v
	}
	return a
}

func TestSortAccountDetailsRanksHealthyFirst(t *testing.T) {
	accounts := []map[string]interface{}{
		account("alice", "alice@x.com", map[string]interface{}{"remaining": 100}),
		account("bob", "bob@x.com", map[string]interface{}{"exhausted": true, "remaining": 0}),
		account("carol", "carol@x.com", map[string]interface{}{"permanent": true}),
		account("dan", "dan@x.com", map[string]interface{}{"no_workspace": true, "remaining": 50}),
		account("eve", "eve@x.com", map[string]interface{}{"remaining": 200}),
		account("frank", "frank@x.com", map[string]interface{}{"remaining": 50, "research_usage": 5}),
	}

	sortAccountDetails(accounts)

	got := make([]string, len(accounts))
	for i, a := range accounts {
		got[i] = mapString(a, "name")
	}

	// 1. healthy w/ remaining desc (eve 200 > alice 100 > frank 50 with research limit)
	// 2. research-limited but otherwise healthy (frank)
	// 3. exhausted (bob)
	// 4. no_workspace (dan)
	// 5. permanent (carol)
	want := []string{"eve", "alice", "frank", "bob", "dan", "carol"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sort order mismatch:\n got=%v\nwant=%v", got, want)
	}
}

func TestFilterAccountDetailsMatchesEmailNamePlanSpace(t *testing.T) {
	accounts := []map[string]interface{}{
		account("Alice", "alice@notion.so", map[string]interface{}{"plan": "plus", "space": "Acme"}),
		account("Bob", "bob@example.com", map[string]interface{}{"plan": "personal", "space": "Beta"}),
		account("Carol", "carol@example.com", map[string]interface{}{"plan": "business", "space": "Gamma"}),
	}

	cases := []struct {
		query string
		want  []string
	}{
		{"alice", []string{"Alice"}},              // name match
		{"BOB@", []string{"Bob"}},                 // email, case-insensitive
		{"plus", []string{"Alice"}},               // plan match
		{"gamma", []string{"Carol"}},              // space match
		{"example", []string{"Bob", "Carol"}},     // multiple matches preserve order
		{"  ", []string{"Alice", "Bob", "Carol"}}, // whitespace == no filter
		{"", []string{"Alice", "Bob", "Carol"}},   // empty == no filter
		{"missing", nil},                          // zero match
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			out := filterAccountDetails(accounts, tc.query)
			got := make([]string, 0, len(out))
			for _, a := range out {
				got = append(got, mapString(a, "name"))
			}
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("filter %q: got=%v want=%v", tc.query, got, tc.want)
			}
		})
	}
}

func TestPaginateAccountsBoundary(t *testing.T) {
	mk := func(n int) []map[string]interface{} {
		out := make([]map[string]interface{}, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, map[string]interface{}{"name": string(rune('a' + i))})
		}
		return out
	}
	all := mk(7)

	// Page 0, size 3 -> 3 entries.
	got := paginateAccounts(all, 0, 3)
	if len(got) != 3 || mapString(got[0], "name") != "a" {
		t.Fatalf("page 0/3: got %v", got)
	}

	// Last partial page.
	got = paginateAccounts(all, 2, 3)
	if len(got) != 1 || mapString(got[0], "name") != "g" {
		t.Fatalf("page 2/3: got %v", got)
	}

	// Past the end -> empty (non-nil).
	got = paginateAccounts(all, 10, 3)
	if got == nil || len(got) != 0 {
		t.Fatalf("out of range: want empty non-nil slice, got %v", got)
	}

	// pageSize <= 0 returns input unchanged.
	got = paginateAccounts(all, 0, 0)
	if len(got) != 7 {
		t.Fatalf("pageSize=0: want full slice, got %d entries", len(got))
	}

	// Negative page coerces to 0.
	got = paginateAccounts(all, -1, 3)
	if len(got) != 3 || mapString(got[0], "name") != "a" {
		t.Fatalf("negative page: got %v", got)
	}
}

func TestSummarizeAccountsAggregates(t *testing.T) {
	accounts := []map[string]interface{}{
		account("a", "a@x", map[string]interface{}{
			"remaining": 100, "space_usage": 10, "space_limit": 200, "user_usage": 12, "user_limit": 200,
			"space_remaining": 190, "user_remaining": 188,
			"premium_balance": 500, "premium_limit": 500, "has_premium": true,
		}),
		account("b", "b@x", map[string]interface{}{
			"remaining": 0, "exhausted": true,
			"space_usage": 200, "space_limit": 200, "user_usage": 200, "user_limit": 200,
		}),
		account("c", "c@x", map[string]interface{}{
			"no_workspace": true, "remaining": 50,
		}),
		account("d", "d@x", map[string]interface{}{
			"permanent": true,
		}),
		account("e", "e@x", map[string]interface{}{
			"remaining": 80, "research_usage": 4, // research-limited (no premium)
		}),
	}

	s := summarizeAccounts(accounts)

	if s.NoWorkspace != 1 {
		t.Errorf("NoWorkspace: want 1 got %d", s.NoWorkspace)
	}
	// b is exhausted-only; d is permanent-only. c has no_workspace so it's
	// excluded from the bucket per UX spec. -> 2.
	if s.ExhaustedOnly != 2 {
		t.Errorf("ExhaustedOnly: want 2 got %d", s.ExhaustedOnly)
	}
	if s.PremiumAccounts != 1 {
		t.Errorf("PremiumAccounts: want 1 got %d", s.PremiumAccounts)
	}
	// Only e is research-limited (a is premium so excluded; b/c/d are
	// exhausted-or-equivalent).
	if s.ResearchLimited != 1 {
		t.Errorf("ResearchLimited: want 1 got %d", s.ResearchLimited)
	}
	if s.TotalRemaining != 230 { // 100 + 0 + 50 + 0 + 80
		t.Errorf("TotalRemaining: want 230 got %d", s.TotalRemaining)
	}
	if s.TotalSpaceUsage != 210 || s.TotalSpaceLimit != 400 {
		t.Errorf("space totals: usage=%d limit=%d", s.TotalSpaceUsage, s.TotalSpaceLimit)
	}
	if s.TotalUserUsage != 212 || s.TotalUserLimit != 400 {
		t.Errorf("user totals: usage=%d limit=%d", s.TotalUserUsage, s.TotalUserLimit)
	}
	if s.TotalPremiumBalance != 500 || s.TotalPremiumLimit != 500 {
		t.Errorf("premium totals: balance=%d limit=%d", s.TotalPremiumBalance, s.TotalPremiumLimit)
	}
}

func TestIsResearchLimitedMapPremiumExempt(t *testing.T) {
	premium := map[string]interface{}{"research_usage": 9, "has_premium": true}
	if isResearchLimitedMap(premium) {
		t.Errorf("premium account should not be research-limited")
	}
	hot := map[string]interface{}{"research_usage": 3}
	if !isResearchLimitedMap(hot) {
		t.Errorf("non-premium with 3 uses should be limited")
	}
	cool := map[string]interface{}{"research_usage": 2}
	if isResearchLimitedMap(cool) {
		t.Errorf("non-premium with 2 uses should not be limited")
	}
}

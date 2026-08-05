package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"notion-manager/internal/proxy"
)

func TestRequiresAPIKey(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/v1/messages", want: true},
		{path: "/v1/chat/completions", want: true},
		{path: "/v1/responses", want: true},
		{path: "/v1/models", want: true},
		{path: "/models", want: true},
		{path: "/health", want: false},
		{path: "/dashboard/", want: false},
	}

	for _, tc := range tests {
		if got := requiresAPIKey(tc.path); got != tc.want {
			t.Fatalf("requiresAPIKey(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestAPIKeyAuthMiddleware_ProtectsModelsRoutes(t *testing.T) {
	handler := apiKeyAuthMiddleware("sk-test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	tests := []struct {
		name    string
		path    string
		headers map[string]string
		want    int
	}{
		{name: "models missing key", path: "/models", want: http.StatusUnauthorized},
		{name: "models wrong key", path: "/models", headers: map[string]string{"Authorization": "Bearer sk-wrong"}, want: http.StatusUnauthorized},
		{name: "models bearer", path: "/models", headers: map[string]string{"Authorization": "Bearer sk-test"}, want: http.StatusNoContent},
		{name: "v1 models x-api-key", path: "/v1/models", headers: map[string]string{"x-api-key": "sk-test"}, want: http.StatusNoContent},
		{name: "chat missing key", path: "/v1/chat/completions", want: http.StatusUnauthorized},
		{name: "responses x-api-key", path: "/v1/responses", headers: map[string]string{"x-api-key": "sk-test"}, want: http.StatusNoContent},
		{name: "health no auth", path: "/health", want: http.StatusNoContent},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		for key, value := range tc.headers {
			req.Header.Set(key, value)
		}

		handler.ServeHTTP(rec, req)

		if rec.Code != tc.want {
			t.Fatalf("%s: expected %d, got %d body=%s", tc.name, tc.want, rec.Code, rec.Body.String())
		}
	}
}

func TestNewMux_RegistersModelsRoutes(t *testing.T) {
	original := proxy.SnapshotModelMap()
	proxy.ReplaceModelMap(map[string]string{
		"opus-4.6": "avocado-froyo-medium",
	})
	t.Cleanup(func() {
		proxy.ReplaceModelMap(original)
	})

	pool := proxy.NewAccountPool()
	dashAuth := proxy.NewDashboardAuth("", "sk-test")
	usageStats := proxy.InitUsageStats("")
	regDeps := &proxy.RegisterJobsDeps{Pool: pool, AccountsDir: "", Auth: dashAuth}
	mux := newMux(pool, "", "sk-test", dashAuth, usageStats, regDeps)
	handler := apiKeyAuthMiddleware("sk-test", mux)

	for _, path := range []string{"/v1/models", "/models"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer sk-test")
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestNewMux_RegistersOpenAIRoutes(t *testing.T) {
	originalConfig := proxy.AppConfig
	proxy.AppConfig = proxy.DefaultConfig()
	t.Cleanup(func() {
		proxy.AppConfig = originalConfig
	})

	pool := proxy.NewAccountPool()
	dashAuth := proxy.NewDashboardAuth("", "sk-test")
	usageStats := proxy.InitUsageStats("")
	regDeps := &proxy.RegisterJobsDeps{Pool: pool, AccountsDir: "", Auth: dashAuth}
	mux := newMux(pool, "", "sk-test", dashAuth, usageStats, regDeps)
	handler := apiKeyAuthMiddleware("sk-test", mux)

	tests := []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"messages":[]}`},
		{path: "/v1/responses", body: `{"input":"ping"}`},
	}

	for _, tc := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec, req)

		if rec.Code == http.StatusNotFound {
			t.Fatalf("%s: expected registered handler, got 404", tc.path)
		}
	}
}

func clearAccountDiscoveryEnvironment(t *testing.T) {
	t.Helper()
	for index := 1; index <= maxStartupAccounts; index++ {
		for _, base := range []string{
			"NOTION_TOKEN_V2",
			"NOTION_ACTIVE_USER_ID",
			"NOTION_SPACE_ID",
			"NOTION_EXPECTED_EMAIL",
		} {
			t.Setenv(indexedSecretName(base, index), "")
		}
	}
}

func stubStartupAccountIO(
	t *testing.T,
	discover func(string, proxy.AccountDiscoveryOptions) (*proxy.Account, error),
	save func(*proxy.Account, string) (string, error),
) {
	t.Helper()
	previousDiscover := discoverAccountFromToken
	previousSave := saveAccountToFile
	discoverAccountFromToken = discover
	saveAccountToFile = save
	t.Cleanup(func() {
		discoverAccountFromToken = previousDiscover
		saveAccountToFile = previousSave
	})
}

func startupAccount(slot int) *proxy.Account {
	return &proxy.Account{
		TokenV2:   fmt.Sprintf("token-%d", slot),
		UserID:    fmt.Sprintf("user-%d", slot),
		UserEmail: fmt.Sprintf("user-%d@example.test", slot),
		SpaceID:   fmt.Sprintf("space-%d", slot),
	}
}

func TestLoadStartupAccountsImportsTwoAccounts(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	t.Setenv("NOTION_TOKEN_V2", "first-token")
	t.Setenv("NOTION_TOKEN_V2_2", "second-token")

	var saved []string
	stubStartupAccountIO(t,
		func(token string, _ proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			switch token {
			case "first-token":
				return startupAccount(1), nil
			case "second-token":
				return startupAccount(2), nil
			default:
				return nil, fmt.Errorf("unexpected token")
			}
		},
		func(acc *proxy.Account, _ string) (string, error) {
			saved = append(saved, acc.UserID)
			return acc.UserID + ".json", nil
		},
	)

	pool := proxy.NewAccountPool()
	if err := loadStartupAccounts(pool, t.TempDir(), t.TempDir()+"/missing-token.json"); err != nil {
		t.Fatalf("loadStartupAccounts() error = %v", err)
	}
	if got := pool.Count(); got != 2 {
		t.Fatalf("pool.Count() = %d, want 2", got)
	}
	if got := strings.Join(saved, ","); got != "user-1,user-2" {
		t.Fatalf("saved accounts = %q, want %q", got, "user-1,user-2")
	}
}

func TestLoadStartupAccountsKeepsSelectorsIsolatedBySlot(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	t.Setenv("NOTION_TOKEN_V2", "first-token")
	t.Setenv("NOTION_ACTIVE_USER_ID", "first-user")
	t.Setenv("NOTION_SPACE_ID", "first-space")
	t.Setenv("NOTION_EXPECTED_EMAIL", "first@example.test")
	t.Setenv("NOTION_TOKEN_V2_2", "second-token")
	t.Setenv("NOTION_ACTIVE_USER_ID_2", "second-user")
	t.Setenv("NOTION_SPACE_ID_2", "second-space")
	t.Setenv("NOTION_EXPECTED_EMAIL_2", "second@example.test")

	seen := make(map[string]proxy.AccountDiscoveryOptions)
	stubStartupAccountIO(t,
		func(token string, options proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			seen[token] = options
			if token == "first-token" {
				return startupAccount(1), nil
			}
			return startupAccount(2), nil
		},
		func(acc *proxy.Account, _ string) (string, error) {
			return acc.UserID + ".json", nil
		},
	)

	pool := proxy.NewAccountPool()
	if err := loadStartupAccounts(pool, t.TempDir(), t.TempDir()+"/missing-token.json"); err != nil {
		t.Fatalf("loadStartupAccounts() error = %v", err)
	}

	wantFirst := proxy.AccountDiscoveryOptions{
		ActiveUserID:  "first-user",
		SpaceID:       "first-space",
		ExpectedEmail: "first@example.test",
	}
	wantSecond := proxy.AccountDiscoveryOptions{
		ActiveUserID:  "second-user",
		SpaceID:       "second-space",
		ExpectedEmail: "second@example.test",
	}
	if got := seen["first-token"]; got != wantFirst {
		t.Fatalf("first selectors = %#v, want %#v", got, wantFirst)
	}
	if got := seen["second-token"]; got != wantSecond {
		t.Fatalf("second selectors = %#v, want %#v", got, wantSecond)
	}
}

func TestLoadStartupAccountsSecondDiscoveryFailureLeavesPoolEmpty(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	t.Setenv("REQUIRE_SECRETS", "true")
	t.Setenv("NOTION_TOKEN_V2", "first-sensitive-token")
	t.Setenv("NOTION_EXPECTED_EMAIL", "first-sensitive@example.test")
	t.Setenv("NOTION_TOKEN_V2_2", "second-sensitive-token")
	t.Setenv("NOTION_EXPECTED_EMAIL_2", "second-sensitive@example.test")

	saveCalls := 0
	stubStartupAccountIO(t,
		func(token string, _ proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			if token == "first-sensitive-token" {
				return startupAccount(1), nil
			}
			return nil, errors.New("rejected second-sensitive-token for second-sensitive@example.test")
		},
		func(_ *proxy.Account, _ string) (string, error) {
			saveCalls++
			return "unexpected.json", nil
		},
	)

	pool := proxy.NewAccountPool()
	err := loadStartupAccounts(pool, t.TempDir(), t.TempDir()+"/missing-token.json")
	if err == nil {
		t.Fatal("loadStartupAccounts() error = nil, want discovery failure")
	}
	for _, secret := range []string{
		"first-sensitive-token",
		"second-sensitive-token",
		"first-sensitive@example.test",
		"second-sensitive@example.test",
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q leaked secret %q", err, secret)
		}
	}
	if got := pool.Count(); got != 0 {
		t.Fatalf("pool.Count() = %d, want 0", got)
	}
	if saveCalls != 0 {
		t.Fatalf("save calls = %d, want 0", saveCalls)
	}
}

func TestLoadStartupAccountsSecondSaveFailureLeavesPoolEmpty(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	t.Setenv("REQUIRE_SECRETS", "true")
	t.Setenv("NOTION_TOKEN_V2", "first-sensitive-token")
	t.Setenv("NOTION_EXPECTED_EMAIL", "first-sensitive@example.test")
	t.Setenv("NOTION_TOKEN_V2_2", "second-sensitive-token")
	t.Setenv("NOTION_EXPECTED_EMAIL_2", "second-sensitive@example.test")

	saveCalls := 0
	stubStartupAccountIO(t,
		func(token string, _ proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			if token == "first-sensitive-token" {
				return startupAccount(1), nil
			}
			return startupAccount(2), nil
		},
		func(acc *proxy.Account, _ string) (string, error) {
			saveCalls++
			if saveCalls == 2 {
				return "", errors.New("cannot persist second-sensitive-token for second-sensitive@example.test")
			}
			return acc.UserID + ".json", nil
		},
	)

	pool := proxy.NewAccountPool()
	err := loadStartupAccounts(pool, t.TempDir(), t.TempDir()+"/missing-token.json")
	if err == nil {
		t.Fatal("loadStartupAccounts() error = nil, want persistence failure")
	}
	for _, secret := range []string{
		"first-sensitive-token",
		"second-sensitive-token",
		"first-sensitive@example.test",
		"second-sensitive@example.test",
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q leaked secret %q", err, secret)
		}
	}
	if got := pool.Count(); got != 0 {
		t.Fatalf("pool.Count() = %d, want 0", got)
	}
	if saveCalls != 2 {
		t.Fatalf("save calls = %d, want 2", saveCalls)
	}
}

func TestLoadStartupAccountsInvalidIndexedSelectorsFailBeforeDiscovery(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	t.Setenv("REQUIRE_SECRETS", "true")
	t.Setenv("NOTION_TOKEN_V2", "first-sensitive-token")
	t.Setenv("NOTION_EXPECTED_EMAIL_2", "second-sensitive@example.test")

	discoverCalls := 0
	stubStartupAccountIO(t,
		func(_ string, _ proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			discoverCalls++
			return startupAccount(1), nil
		},
		func(_ *proxy.Account, _ string) (string, error) {
			return "unexpected.json", nil
		},
	)

	pool := proxy.NewAccountPool()
	err := loadStartupAccounts(pool, t.TempDir(), t.TempDir()+"/missing-token.json")
	if err == nil {
		t.Fatal("loadStartupAccounts() error = nil, want configuration failure")
	}
	for _, secret := range []string{"first-sensitive-token", "second-sensitive@example.test"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error %q leaked secret %q", err, secret)
		}
	}
	if discoverCalls != 0 {
		t.Fatalf("discovery calls = %d, want 0", discoverCalls)
	}
	if got := pool.Count(); got != 0 {
		t.Fatalf("pool.Count() = %d, want 0", got)
	}
}

func TestLoadStartupAccountsKeepsLegacySecretCompatible(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	t.Setenv("NOTION_TOKEN_V2", "legacy-token")
	t.Setenv("NOTION_ACTIVE_USER_ID", "legacy-user")
	t.Setenv("NOTION_SPACE_ID", "legacy-space")
	t.Setenv("NOTION_EXPECTED_EMAIL", "legacy@example.test")

	var gotToken string
	var gotOptions proxy.AccountDiscoveryOptions
	stubStartupAccountIO(t,
		func(token string, options proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			gotToken = token
			gotOptions = options
			return startupAccount(1), nil
		},
		func(acc *proxy.Account, _ string) (string, error) {
			return acc.UserID + ".json", nil
		},
	)

	pool := proxy.NewAccountPool()
	if err := loadStartupAccounts(pool, t.TempDir(), t.TempDir()+"/missing-token.json"); err != nil {
		t.Fatalf("loadStartupAccounts() error = %v", err)
	}
	if gotToken != "legacy-token" {
		t.Fatalf("discovery token = %q, want legacy token", gotToken)
	}
	wantOptions := proxy.AccountDiscoveryOptions{
		ActiveUserID:  "legacy-user",
		SpaceID:       "legacy-space",
		ExpectedEmail: "legacy@example.test",
	}
	if gotOptions != wantOptions {
		t.Fatalf("legacy selectors = %#v, want %#v", gotOptions, wantOptions)
	}
	if got := pool.Count(); got != 1 {
		t.Fatalf("pool.Count() = %d, want 1", got)
	}
}

func TestLoadStartupAccountsDeduplicatesByAccountID(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	t.Setenv("NOTION_TOKEN_V2", "first-token")
	t.Setenv("NOTION_TOKEN_V2_2", "duplicate-token")

	saveCalls := 0
	stubStartupAccountIO(t,
		func(_ string, _ proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			return startupAccount(1), nil
		},
		func(acc *proxy.Account, _ string) (string, error) {
			saveCalls++
			return acc.UserID + ".json", nil
		},
	)

	pool := proxy.NewAccountPool()
	if err := loadStartupAccounts(pool, t.TempDir(), t.TempDir()+"/missing-token.json"); err != nil {
		t.Fatalf("loadStartupAccounts() error = %v", err)
	}
	if got := pool.Count(); got != 1 {
		t.Fatalf("pool.Count() = %d, want 1", got)
	}
	if saveCalls != 1 {
		t.Fatalf("save calls = %d, want 1", saveCalls)
	}
}

func persistStartupAccount(t *testing.T, dir string, account *proxy.Account) {
	t.Helper()
	if _, err := proxy.SaveAccountToFile(account, dir); err != nil {
		t.Fatalf("SaveAccountToFile() error = %v", err)
	}
}

func TestLoadStartupAccountsCombinesPersistedAndEnvironmentAccounts(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	accountsDir := t.TempDir()
	persisted := startupAccount(1)
	persistStartupAccount(t, accountsDir, persisted)
	t.Setenv("NOTION_TOKEN_V2", "environment-token")

	environment := startupAccount(2)
	stubStartupAccountIO(t,
		func(string, proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			return environment, nil
		},
		proxy.SaveAccountToFile,
	)

	pool := proxy.NewAccountPool()
	if err := loadStartupAccounts(pool, accountsDir, accountsDir+"/missing-token.json"); err != nil {
		t.Fatalf("loadStartupAccounts() error = %v", err)
	}
	if got := pool.Count(); got != 2 {
		t.Fatalf("pool.Count() = %d, want persisted + environment = 2", got)
	}
	for _, account := range []*proxy.Account{persisted, environment} {
		account.EnsureAccountID()
		if pool.FindByAccountID(account.AccountID) == nil {
			t.Errorf("account %s was not activated", account.AccountID)
		}
	}
}

func TestLoadStartupAccountsEnvironmentFailureDoesNotActivatePersistedAccount(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	accountsDir := t.TempDir()
	persistStartupAccount(t, accountsDir, startupAccount(1))
	t.Setenv("NOTION_TOKEN_V2", "failing-token")
	stubStartupAccountIO(t,
		func(string, proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			return nil, errors.New("discovery failed")
		},
		proxy.SaveAccountToFile,
	)

	pool := proxy.NewAccountPool()
	if err := loadStartupAccounts(pool, accountsDir, accountsDir+"/missing-token.json"); err == nil {
		t.Fatal("loadStartupAccounts() error = nil, want discovery failure")
	}
	if got := pool.Count(); got != 0 {
		t.Fatalf("pool.Count() = %d, want 0 after staged startup failure", got)
	}
}

func TestLoadStartupAccountsEnvironmentReplacesPersistedAccountIdentity(t *testing.T) {
	clearAccountDiscoveryEnvironment(t)
	accountsDir := t.TempDir()
	persisted := startupAccount(1)
	persisted.TokenV2 = "persisted-token"
	persistStartupAccount(t, accountsDir, persisted)
	t.Setenv("NOTION_TOKEN_V2", "fresh-token")

	fresh := startupAccount(1)
	fresh.TokenV2 = "fresh-token"
	stubStartupAccountIO(t,
		func(string, proxy.AccountDiscoveryOptions) (*proxy.Account, error) {
			return fresh, nil
		},
		proxy.SaveAccountToFile,
	)

	pool := proxy.NewAccountPool()
	if err := loadStartupAccounts(pool, accountsDir, accountsDir+"/missing-token.json"); err != nil {
		t.Fatalf("loadStartupAccounts() error = %v", err)
	}
	if got := pool.Count(); got != 1 {
		t.Fatalf("pool.Count() = %d, want 1 de-duplicated identity", got)
	}
	fresh.EnsureAccountID()
	if got := pool.FindByAccountID(fresh.AccountID); got == nil || got.TokenV2 != "fresh-token" {
		t.Fatalf("activated account = %#v, want refreshed environment account", got)
	}
}

package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"notion-manager/internal/proxy"
	"notion-manager/internal/regjob"
	"notion-manager/internal/regjob/providers"
	"notion-manager/internal/regjob/providers/microsoft"
)

var (
	discoverAccountFromToken = proxy.DiscoverAccountFromTokenWithOptions
	saveAccountToFile        = proxy.SaveAccountToFile
)

const maxStartupAccounts = 16

type accountDiscoveryConfig struct {
	tokenName string
	tokenV2   string
	options   proxy.AccountDiscoveryOptions
}

type discoveredStartupAccount struct {
	config  accountDiscoveryConfig
	account *proxy.Account
}

func indexedSecretName(base string, index int) string {
	if index == 1 {
		return base
	}
	return fmt.Sprintf("%s_%d", base, index)
}

func accountDiscoveryOptionsFromEnvironmentAt(index int) proxy.AccountDiscoveryOptions {
	return proxy.AccountDiscoveryOptions{
		ActiveUserID:  strings.TrimSpace(os.Getenv(indexedSecretName("NOTION_ACTIVE_USER_ID", index))),
		SpaceID:       strings.TrimSpace(os.Getenv(indexedSecretName("NOTION_SPACE_ID", index))),
		ExpectedEmail: strings.TrimSpace(os.Getenv(indexedSecretName("NOTION_EXPECTED_EMAIL", index))),
	}
}

func selectorsConfigured(options proxy.AccountDiscoveryOptions) bool {
	return options.ActiveUserID != "" || options.SpaceID != "" || options.ExpectedEmail != ""
}

func accountDiscoveryConfigsFromEnvironment() ([]accountDiscoveryConfig, error) {
	configs := make([]accountDiscoveryConfig, 0, maxStartupAccounts)
	for index := 1; index <= maxStartupAccounts; index++ {
		tokenName := indexedSecretName("NOTION_TOKEN_V2", index)
		tokenV2 := strings.TrimSpace(os.Getenv(tokenName))
		options := accountDiscoveryOptionsFromEnvironmentAt(index)
		if tokenV2 == "" {
			if selectorsConfigured(options) {
				return nil, fmt.Errorf("%s is required when account selectors for slot %d are configured", tokenName, index)
			}
			continue
		}
		configs = append(configs, accountDiscoveryConfig{
			tokenName: tokenName,
			tokenV2:   tokenV2,
			options:   options,
		})
	}
	return configs, nil
}

func accountDiscoveryConfigured() bool {
	for index := 1; index <= maxStartupAccounts; index++ {
		if strings.TrimSpace(os.Getenv(indexedSecretName("NOTION_TOKEN_V2", index))) != "" || selectorsConfigured(accountDiscoveryOptionsFromEnvironmentAt(index)) {
			return true
		}
	}
	return false
}

func loadDiscoveredAccounts(pool *proxy.AccountPool, accountsDir string, configs []accountDiscoveryConfig) error {
	discovered := make([]discoveredStartupAccount, 0, len(configs))
	seenAccountIDs := make(map[string]struct{}, len(configs))
	for _, config := range configs {
		acc, err := discoverAccountFromToken(config.tokenV2, config.options)
		if err != nil {
			return fmt.Errorf("discover account from %s failed", config.tokenName)
		}
		if acc == nil {
			return fmt.Errorf("discover account from %s returned no account", config.tokenName)
		}
		acc.EnsureAccountID()
		if acc.AccountID == "" {
			return fmt.Errorf("discover account from %s returned an incomplete identity", config.tokenName)
		}
		if _, duplicate := seenAccountIDs[acc.AccountID]; duplicate {
			continue
		}
		seenAccountIDs[acc.AccountID] = struct{}{}
		discovered = append(discovered, discoveredStartupAccount{config: config, account: acc})
	}

	for _, item := range discovered {
		if _, err := saveAccountToFile(item.account, accountsDir); err != nil {
			return fmt.Errorf("persist account from %s failed", item.config.tokenName)
		}
	}
	for _, item := range discovered {
		pool.AddAccount(item.account)
	}
	return nil
}

func loadInitialAccount(pool *proxy.AccountPool, accountsDir, tokenFile string) error {
	configs, err := accountDiscoveryConfigsFromEnvironment()
	if err != nil {
		return err
	}
	if len(configs) > 0 {
		return loadDiscoveredAccounts(pool, accountsDir, configs)
	}
	if pool.Count() > 0 {
		return nil
	}

	if _, err := os.Stat(tokenFile); err == nil {
		if err := pool.LoadSingle(tokenFile); err != nil {
			return fmt.Errorf("load %s: %w", tokenFile, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", tokenFile, err)
	}
	return nil
}

func loadStartupAccounts(pool *proxy.AccountPool, accountsDir, tokenFile string) error {
	stagedPool := proxy.NewAccountPool()
	if _, err := os.Stat(accountsDir); err == nil {
		if err := stagedPool.LoadFromDir(accountsDir); err != nil {
			log.Printf("[warn] %v", err)
		}
	}
	if err := loadInitialAccount(stagedPool, accountsDir, tokenFile); err != nil {
		return err
	}
	for _, account := range stagedPool.AccountsSnapshot() {
		pool.AddAccount(account)
	}
	return nil
}

func requiresAPIKey(path string) bool {
	return path == "/models" || strings.HasPrefix(path, "/v1/")
}

func apiKeyAuthMiddleware(apiKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresAPIKey(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		var key string
		if bearer := r.Header.Get("Authorization"); bearer != "" {
			key = strings.TrimPrefix(bearer, "Bearer ")
			if key == bearer {
				key = ""
			}
		}
		if key == "" {
			key = r.Header.Get("x-api-key")
		}
		if key == "" {
			http.Error(w, `{"error":{"message":"missing api key, use 'Authorization: Bearer <key>' or 'x-api-key: <key>'","type":"auth_error"}}`, http.StatusUnauthorized)
			return
		}
		if key != apiKey {
			http.Error(w, `{"error":{"message":"invalid api key","type":"auth_error"}}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func newMux(pool *proxy.AccountPool, accountsDir string, apiKey string, dashAuth *proxy.DashboardAuth, usageStats *proxy.UsageStats, regDeps *proxy.RegisterJobsDeps) *http.ServeMux {
	mux := http.NewServeMux()

	// Anthropic + OpenAI-compatible API endpoints
	mux.HandleFunc("/v1/messages", proxy.HandleAnthropicMessages(pool))
	mux.HandleFunc("/v1/chat/completions", proxy.HandleOpenAIChatCompletions(pool))
	mux.HandleFunc("/v1/responses", proxy.HandleOpenAIResponses(pool))
	mux.HandleFunc("/v1/models", proxy.HandlePublicModels(pool))
	mux.HandleFunc("/models", proxy.HandlePublicModels(pool))

	// Health check with quota details
	mux.HandleFunc("/health", proxy.HandleHealth(pool))

	// Admin API endpoints
	mux.HandleFunc("/admin/accounts", proxy.HandleAdminAccounts(pool, dashAuth))
	mux.HandleFunc("/admin/accounts/add", proxy.HandleAddAccount(pool, accountsDir, dashAuth))
	mux.HandleFunc("/admin/accounts/delete", proxy.HandleDeleteAccount(pool, accountsDir, dashAuth))
	mux.HandleFunc("/admin/models", proxy.HandleAdminModels(pool, dashAuth))
	mux.HandleFunc("/admin/refresh", proxy.HandleAdminRefresh(pool, accountsDir, dashAuth))
	mux.HandleFunc("/admin/settings", proxy.HandleAdminSettings("config.yaml", dashAuth))
	mux.HandleFunc("/admin/stats", proxy.HandleAdminStats(usageStats, dashAuth))

	// Bulk Microsoft-SSO registration. The legacy synchronous endpoint is
	// kept for parity with the dashboard's older "submit + wait" UI; the
	// async Job-based endpoints power the new register drawer (with SSE
	// progress at /admin/register/jobs/{id}/events).
	mux.HandleFunc("/admin/register", proxy.HandleAdminRegister(pool, accountsDir, dashAuth))
	mux.HandleFunc("/admin/register/providers", proxy.HandleAdminRegisterProviders(regDeps))
	mux.HandleFunc("/admin/register/start", proxy.HandleAdminRegisterStart(regDeps))
	mux.HandleFunc("/admin/register/jobs", proxy.HandleAdminRegisterJobsList(regDeps))
	mux.HandleFunc("/admin/register/jobs/", proxy.HandleAdminRegisterJobsRouter(regDeps))
	// REST-style DELETE /admin/accounts/{email}. Coexists with the older
	// POST /admin/accounts/delete handler — Go's mux prefers the more
	// specific exact-match route, so /add and /delete still win over the
	// catch-all /admin/accounts/.
	mux.HandleFunc("/admin/accounts/", proxy.HandleAdminDeleteAccount(regDeps))

	// Dashboard (React SPA with embedded API key + auth)
	mux.Handle("/dashboard/", proxy.HandleDashboard(apiKey, dashAuth))
	mux.HandleFunc("/dashboard", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusMovedPermanently)
	})

	// Proxy start: create targeted session for a specific account (requires dashboard auth)
	rp := proxy.NewReverseProxy(pool)
	mux.HandleFunc("/proxy/start", proxy.HandleProxyStart(pool, rp, dashAuth))

	// Catch-all: reverse proxy for paths with valid np_session, 404 for everything else
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rp.ServeHTTP(w, r)
	}))

	return mux
}

func main() {
	cfg, err := proxy.LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("[config] %v", err)
	}

	proxy.EnsureApiKey(cfg, "config.yaml")

	if cfg.Server.AdminPassword == "" {
		generated := proxy.GenerateAdminPassword()
		cfg.Server.AdminPassword = generated
		log.Printf("[config] no admin_password configured, generated: %s", generated)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  ========================================")
		fmt.Fprintf(os.Stderr, "  Dashboard password: %s\n", generated)
		fmt.Fprintln(os.Stderr, "  Please save this password. It will be")
		fmt.Fprintln(os.Stderr, "  hashed and cannot be recovered later.")
		fmt.Fprintln(os.Stderr, "  ========================================")
		fmt.Fprintln(os.Stderr, "")
	}

	proxy.EnsureAdminPassword(cfg, "config.yaml")
	proxy.ApplyConfig(cfg)

	port := cfg.Server.Port
	accountsDir := cfg.Server.AccountsDir
	tokenFile := cfg.Server.TokenFile

	pool := proxy.NewAccountPool()
	discoveryConfigured := accountDiscoveryConfigured()
	if err := loadStartupAccounts(pool, accountsDir, tokenFile); err != nil {
		if discoveryConfigured {
			log.Fatalf("[account] %v", err)
		}
		log.Printf("[warn] %v", err)
	}

	if pool.Count() == 0 {
		log.Printf("[warn] No accounts found. Place account JSON files in %s/ to enable API and proxy.", accountsDir)
	}

	// Startup refresh: kick off a quota+models check in the background so
	// the HTTP listener can come up immediately even with large pools.
	if pool.Count() > 0 {
		log.Printf("[startup] kicking off background quota refresh for %d account(s)", pool.Count())
		go pool.RefreshAll(accountsDir)
	}

	pool.StartRefreshLoop(cfg.RefreshInterval(), accountsDir)

	// Token usage statistics — persisted next to the account JSONs so they
	// share a backup target with .register_history.json.
	statsPath := filepath.Join(accountsDir, ".token_stats.json")
	usageStats := proxy.InitUsageStats(statsPath)
	usageStats.StartFlushLoop(5 * time.Second)

	// Async batch-register store + provider registry.
	regStore, err := regjob.NewStore(cfg.Register.HistoryFile, cfg.Register.HistoryMemoryCap)
	if err != nil {
		log.Fatalf("[regjob] init store at %s: %v", cfg.Register.HistoryFile, err)
	}
	registry := providers.NewRegistry()
	registry.Register(microsoft.New())

	apiKey := cfg.Server.ApiKey
	dashAuth := proxy.NewDashboardAuth(cfg.Server.AdminPassword, apiKey)

	regDeps := &proxy.RegisterJobsDeps{
		Pool:        pool,
		AccountsDir: accountsDir,
		Store:       regStore,
		Providers:   registry,
		Auth:        dashAuth,
	}

	cors := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, X-Web-Search, X-Workspace-Search")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	mux := newMux(pool, accountsDir, apiKey, dashAuth, usageStats, regDeps)

	log.Printf("=== notion-manager ===")
	log.Printf("Listening on :%s", port)
	log.Printf("Accounts: %d", pool.Count())
	log.Printf("API Key: %s", apiKey)
	log.Printf("Dashboard: password protected")
	log.Printf("Endpoints:")
	log.Printf("  GET  /dashboard/                  (Dashboard UI)")
	log.Printf("  GET  /proxy/start                 (Open proxy for account)")
	log.Printf("  POST /v1/messages                 (Anthropic Messages API)")
	log.Printf("  POST /v1/chat/completions         (OpenAI Chat Completions API)")
	log.Printf("  POST /v1/responses                (OpenAI Responses API)")
	log.Printf("  GET  /v1/models                   (OpenAI models API)")
	log.Printf("  GET  /models                      (OpenAI models alias)")
	log.Printf("  GET  /health")
	log.Printf("  GET  /admin/accounts")
	log.Printf("  GET  /admin/models")
	log.Printf("  GET  /admin/settings              (search/proxy/ASK settings)")
	log.Printf("  GET  /admin/stats                 (token usage stats)")
	log.Printf("  POST /admin/register              (bulk MS-SSO register, sync)")
	log.Printf("  POST /admin/register/start        (async job)")
	log.Printf("  GET  /admin/register/jobs/{id}/events (SSE progress)")
	log.Printf("  GET  /ai                          (Reverse Proxy -> notion.so)")

	if err := http.ListenAndServe(":"+port, cors(apiKeyAuthMiddleware(apiKey, mux))); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

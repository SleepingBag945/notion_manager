package proxy

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"notion-manager/internal/netutil"
)

// Default and maximum page sizes for /admin/accounts pagination.
// Default is small to keep dashboard payloads quick; the cap prevents
// callers from accidentally requesting the whole pool through the
// paginated path.
const (
	defaultAccountsPageSize = 50
	maxAccountsPageSize     = 500
)

const publicModelCreatedAt = int64(1735689600)

type publicModelResponse struct {
	Object string        `json:"object"`
	Data   []publicModel `json:"data"`
}

type publicModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// HandleHealth returns an HTTP handler for the /health endpoint
func HandleHealth(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"status":    "ok",
			"accounts":  pool.Count(),
			"available": pool.AvailableCount(),
			"quota":     pool.GetQuotaSummary(),
		}
		json.NewEncoder(w).Encode(resp)
	}
}

// HandlePublicModels returns an OpenAI-compatible models list for API clients.
func HandlePublicModels(pool *AccountPool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, `{"error":{"message":"method not allowed","type":"invalid_request_error"}}`, http.StatusMethodNotAllowed)
			return
		}

		resp := publicModelResponse{
			Object: "list",
			Data:   buildPublicModels(pool.AllModels()),
		}
		json.NewEncoder(w).Encode(resp)
	}
}

func buildPublicModels(models []ModelEntry) []publicModel {
	seen := make(map[string]bool, len(models))
	items := make([]publicModel, 0, len(models))
	for _, model := range models {
		id := publicModelID(model)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		items = append(items, publicModel{
			ID:      id,
			Object:  "model",
			Created: publicModelCreatedAt,
			OwnedBy: "notion-manager",
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

func publicModelID(model ModelEntry) string {
	if normalized := normalizeModelName(model.Name); normalized != "" {
		return normalized
	}
	return friendlyModelNameByInternalID(model.ID)
}

func friendlyModelNameByInternalID(id string) string {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return ""
	}

	snap := SnapshotModelMap()
	candidates := make([]string, 0, 1)
	for friendly, internalID := range snap {
		if internalID == trimmed {
			candidates = append(candidates, friendly)
		}
	}
	if len(candidates) == 0 {
		return ""
	}

	sort.Strings(candidates)
	return candidates[0]
}

// HandleAdminAccounts returns detailed account info including models, quota, and status.
//
// Query parameters (all optional, dashboard-friendly):
//   - q          : case-insensitive substring filter on email/name/plan/space.
//   - page       : 0-based page index. Defaults to 0.
//   - page_size  : max entries to return; clamped to [1, maxAccountsPageSize].
//
// When ANY of those parameters are present we apply the same sort the
// dashboard previously did client-side, filter, then slice — and add
// `page`, `page_size`, `filtered_total` fields to the response. Without
// them the response keeps its historical shape (the full unsorted list)
// so older scripts and integrations remain happy. The pool-wide
// `summary` block is added unconditionally because it's purely additive
// and lets the dashboard render headline cards without iterating the
// full account list.
func HandleAdminAccounts(pool *AccountPool, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		q := strings.TrimSpace(r.URL.Query().Get("q"))
		pageStr := r.URL.Query().Get("page")
		sizeStr := r.URL.Query().Get("page_size")
		paginated := pageStr != "" || sizeStr != "" || q != ""

		all := pool.GetAccountDetails()
		resp := map[string]interface{}{
			"total":     pool.Count(),
			"available": pool.AvailableCount(),
			"models":    pool.AllModels(),
			"refresh":   pool.GetRefreshStatus(),
			"summary":   summarizeAccounts(all),
		}

		if !paginated {
			// Backward-compatible path: hand back the full unsorted
			// list so existing scripts/integrations keep working.
			resp["accounts"] = all
			json.NewEncoder(w).Encode(resp)
			return
		}

		filtered := filterAccountDetails(all, q)
		sortAccountDetails(filtered)

		page, _ := strconv.Atoi(pageStr)
		if page < 0 {
			page = 0
		}
		size, _ := strconv.Atoi(sizeStr)
		if size <= 0 {
			size = defaultAccountsPageSize
		}
		if size > maxAccountsPageSize {
			size = maxAccountsPageSize
		}

		resp["accounts"] = paginateAccounts(filtered, page, size)
		resp["page"] = page
		resp["page_size"] = size
		resp["filtered_total"] = len(filtered)
		json.NewEncoder(w).Encode(resp)
	}
}

// HandleAdminStats returns aggregated Token usage statistics for the
// dashboard. It only requires a valid dashboard session - same auth
// surface as /admin/accounts. The response shape is documented on
// UsageSnapshot.
func HandleAdminStats(stats *UsageStats, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		if stats == nil {
			stats = GlobalUsageStats()
		}
		snap := stats.Snapshot(5)
		json.NewEncoder(w).Encode(snap)
	}
}

// HandleAdminRequestLogs returns recent individual request logs (ring buffer,
// newest first). In-memory only, not persisted across restarts.
func HandleAdminRequestLogs(stats *UsageStats, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		if stats == nil {
			stats = GlobalUsageStats()
		}
		logs := stats.GetRequestLogs()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"logs":  logs,
			"total": len(logs),
		})
	}
}

// HandleAdminRefresh handles GET (status) and POST (trigger) for quota refresh
func HandleAdminRefresh(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(pool.GetRefreshStatus())
		case "POST":
			started := pool.TriggerRefresh(accountsDir)
			resp := map[string]interface{}{
				"started": started,
			}
			if !started {
				resp["message"] = "refresh already in progress"
			}
			json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// HandleAdminToggleAccount enables or disables an account by email.
// The disabled flag persists to the account JSON file and causes the
// pool to skip the account for all request routing.
func HandleAdminToggleAccount(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Email    string `json:"email"`
			Disabled bool   `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(body.Email) == "" {
			http.Error(w, `{"error":"email is required"}`, http.StatusBadRequest)
			return
		}
		if err := pool.ToggleDisabled(body.Email, body.Disabled, accountsDir); err != nil {
			http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"email":    body.Email,
			"disabled": body.Disabled,
		})
	}
}

// HandleAdminModels returns the current model mapping (friendly name -> Notion internal ID)
func HandleAdminModels(pool *AccountPool, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}
		resp := map[string]interface{}{
			"model_map":        SnapshotModelMap(),
			"available_models": pool.AllModels(),
		}
		json.NewEncoder(w).Encode(resp)
	}
}

// HandleAdminSettings handles GET (read) and PUT (update) for dashboard-controlled settings.
// Settings are persisted to config.yaml using YAML node manipulation to preserve comments.
func HandleAdminSettings(configPath string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// Require dashboard session (admin password auth)
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"enable_web_search":         AppConfig.WebSearchEnabled(),
				"enable_workspace_search":   AppConfig.WorkspaceSearchEnabled(),
				"ask_mode_default":          AppConfig.AskModeDefault(),
				"max_concurrency":           AppConfig.MaxConcurrency(),
				"quota_strategy":            AppConfig.QuotaStrategy(),
				"allow_premium":             AppConfig.AllowPremium(),
				"premium_reserve_threshold": AppConfig.PremiumReserveThreshold(),
				"disable_notion_prompt":     AppConfig.Proxy.DisableNotionPrompt,
				"debug_logging":             AppConfig.Server.DebugLogging,
				"notion_proxy":              AppConfig.NotionProxyURL(),
			})

		case "PUT":
			var body struct {
				EnableWebSearch         *bool   `json:"enable_web_search"`
				EnableWorkspaceSearch   *bool   `json:"enable_workspace_search"`
				AskModeDefault          *bool   `json:"ask_mode_default"`
				MaxConcurrency          *int    `json:"max_concurrency"`
				QuotaStrategy           *string `json:"quota_strategy"`
				AllowPremium            *bool   `json:"allow_premium"`
				PremiumReserveThreshold *int    `json:"premium_reserve_threshold"`
				DebugLogging            *bool   `json:"debug_logging"`
				NotionProxy             *string `json:"notion_proxy"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
				return
			}

			changed := false
			rebuildTransport := false
			if body.EnableWebSearch != nil {
				AppConfig.Proxy.EnableWebSearch = body.EnableWebSearch
				changed = true
				log.Printf("[settings] enable_web_search → %v", *body.EnableWebSearch)
			}
			if body.EnableWorkspaceSearch != nil {
				AppConfig.Proxy.EnableWorkspaceSearch = body.EnableWorkspaceSearch
				changed = true
				log.Printf("[settings] enable_workspace_search → %v", *body.EnableWorkspaceSearch)
			}
			if body.AskModeDefault != nil {
				AppConfig.Proxy.AskModeDefault = body.AskModeDefault
				changed = true
				log.Printf("[settings] ask_mode_default → %v", *body.AskModeDefault)
			}
			if body.MaxConcurrency != nil {
				if *body.MaxConcurrency < 1 || *body.MaxConcurrency > 100 {
					http.Error(w, `{"error":"max_concurrency must be between 1 and 100"}`, http.StatusBadRequest)
					return
				}
				AppConfig.Proxy.MaxConcurrency = *body.MaxConcurrency
				changed = true
				log.Printf("[settings] max_concurrency → %d", *body.MaxConcurrency)
			}
			if body.QuotaStrategy != nil {
				next := strings.TrimSpace(*body.QuotaStrategy)
				if next != quotaStrategyBalanced && next != quotaStrategyBasicFirst && next != quotaStrategyPremiumFirst {
					http.Error(w, `{"error":"quota_strategy must be balanced, basic_first or premium_first"}`, http.StatusBadRequest)
					return
				}
				AppConfig.Proxy.QuotaStrategy = next
				changed = true
			}
			if body.AllowPremium != nil {
				AppConfig.Proxy.AllowPremium = body.AllowPremium
				changed = true
			}
			if body.PremiumReserveThreshold != nil {
				if *body.PremiumReserveThreshold < 0 {
					http.Error(w, `{"error":"premium_reserve_threshold must be >= 0"}`, http.StatusBadRequest)
					return
				}
				AppConfig.Proxy.PremiumReserveThreshold = *body.PremiumReserveThreshold
				changed = true
			}
			if body.DebugLogging != nil {
				AppConfig.Server.DebugLogging = *body.DebugLogging
				SetDebugLoggingEnabled(*body.DebugLogging)
				changed = true
				log.Printf("[settings] debug_logging → %v", *body.DebugLogging)
			}
			if body.NotionProxy != nil {
				next := strings.TrimSpace(*body.NotionProxy)
				if next != "" {
					if err := netutil.ValidateProxyURL(next); err != nil {
						// Surface scheme/format errors immediately so the
						// dashboard can roll back the input field instead
						// of waiting for the next dial to fail.
						http.Error(w, `{"error":"unsupported proxy scheme (want http/https/socks5)"}`, http.StatusBadRequest)
						return
					}
				}
				if AppConfig.Proxy.NotionProxy != next {
					AppConfig.Proxy.NotionProxy = next
					changed = true
					rebuildTransport = true
					if next == "" {
						log.Printf("[settings] notion_proxy cleared (direct dial)")
					} else {
						log.Printf("[settings] notion_proxy → %s", next)
					}
				}
			}

			// Persist to config.yaml
			if changed && configPath != "" {
				persistSearchSettings(configPath)
			}

			// Drop idle pooled connections so the next notion dial picks
			// up the new upstream proxy. Active in-flight requests
			// continue on their existing connection until completion.
			if rebuildTransport {
				RebuildChromeTransport()
			}

			json.NewEncoder(w).Encode(map[string]interface{}{
				"enable_web_search":         AppConfig.WebSearchEnabled(),
				"enable_workspace_search":   AppConfig.WorkspaceSearchEnabled(),
				"ask_mode_default":          AppConfig.AskModeDefault(),
				"max_concurrency":           AppConfig.MaxConcurrency(),
				"quota_strategy":            AppConfig.QuotaStrategy(),
				"allow_premium":             AppConfig.AllowPremium(),
				"premium_reserve_threshold": AppConfig.PremiumReserveThreshold(),
				"disable_notion_prompt":     AppConfig.Proxy.DisableNotionPrompt,
				"debug_logging":             AppConfig.Server.DebugLogging,
				"notion_proxy":              AppConfig.NotionProxyURL(),
			})

		default:
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// persistSearchSettings writes the current dashboard settings back to config.yaml.
func persistSearchSettings(configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("[settings] failed to read %s: %v", configPath, err)
		return
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || root.Kind == 0 {
		log.Printf("[settings] failed to parse %s: %v", configPath, err)
		return
	}

	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping := root.Content[0]
		proxyNode := getOrCreateYAMLMapping(mapping, "proxy")
		setYAMLBool(proxyNode, "enable_web_search", AppConfig.WebSearchEnabled())
		setYAMLBool(proxyNode, "enable_workspace_search", AppConfig.WorkspaceSearchEnabled())
		setYAMLBool(proxyNode, "ask_mode_default", AppConfig.AskModeDefault())
		setYAMLInt(proxyNode, "max_concurrency", AppConfig.MaxConcurrency())
		setYAMLString(proxyNode, "quota_strategy", AppConfig.QuotaStrategy())
		setYAMLBool(proxyNode, "allow_premium", AppConfig.AllowPremium())
		setYAMLInt(proxyNode, "premium_reserve_threshold", AppConfig.PremiumReserveThreshold())
		setYAMLString(proxyNode, "notion_proxy", AppConfig.Proxy.NotionProxy)

		serverNode := getOrCreateYAMLMapping(mapping, "server")
		setYAMLBool(serverNode, "debug_logging", AppConfig.Server.DebugLogging)
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		log.Printf("[settings] failed to marshal config: %v", err)
		return
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		log.Printf("[settings] failed to write %s: %v", configPath, err)
	}
}

func getOrCreateYAMLMapping(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			node := mapping.Content[i+1]
			if node.Kind != yaml.MappingNode {
				node.Kind = yaml.MappingNode
				node.Tag = "!!map"
				node.Content = nil
			}
			return node
		}
	}

	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		node,
	)
	return node
}

// setYAMLBool sets or creates a boolean field in a YAML mapping node
func setYAMLBool(mapping *yaml.Node, key string, value bool) {
	valStr := "false"
	if value {
		valStr = "true"
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = valStr
			mapping.Content[i+1].Tag = "!!bool"
			return
		}
	}
	// Key not found — append it
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: valStr, Tag: "!!bool"},
	)
}

// setYAMLInt sets or creates an integer field in a YAML mapping node.
func setYAMLInt(mapping *yaml.Node, key string, value int) {
	valStr := strconv.Itoa(value)
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = valStr
			mapping.Content[i+1].Tag = "!!int"
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: valStr, Tag: "!!int"},
	)
}

// setYAMLString sets or creates a string field in a YAML mapping node.
// Empty values are still written explicitly so the dashboard's "clear
// proxy" action persists across restarts (otherwise YAML would treat a
// missing key as "use default", which here is also "" — equivalent in
// practice but unfriendly for diffs/audits).
func setYAMLString(mapping *yaml.Node, key, value string) {
	style := yaml.Style(0)
	if value == "" {
		style = yaml.DoubleQuotedStyle
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = value
			mapping.Content[i+1].Tag = "!!str"
			mapping.Content[i+1].Style = style
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Value: value, Tag: "!!str", Style: style},
	)
}

// isFreePlan returns true if the account is on a free plan where basic credits (200 lifetime)
// never reset. Paid plans (plus, business, enterprise) have monthly premium credits that reset.
func isFreePlan(acc *Account) bool {
	quota := acc.quotaInfoSnapshot()
	if quota != nil && (quota.HasPremium || quota.PremiumLimit > 0 || quota.PremiumBalance > 0) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(acc.PlanType)) {
	case "personal", "free", "":
		return true
	default:
		// For team plans, check if they actually have a paid subscription
		// by looking at quota info — if no premium credits exist, treat as free.
		if quota != nil && !quota.HasPremium {
			return true
		}
		return false
	}
}

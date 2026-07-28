package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"notion-manager/internal/netutil"
)

// Config holds all configurable values for notion-manager.
// Priority: environment variable > config.yaml > default value.
type Config struct {
	Server   ServerConfig      `yaml:"server"`
	Proxy    ProxyConfig       `yaml:"proxy"`
	Timeouts TimeoutConfig     `yaml:"timeouts"`
	Refresh  RefreshConfig     `yaml:"refresh"`
	Browser  BrowserConfig     `yaml:"browser"`
	Register RegisterConfig    `yaml:"register"`
	ModelMap map[string]string `yaml:"model_map"`
}

type ServerConfig struct {
	Port          string `yaml:"port"`
	AccountsDir   string `yaml:"accounts_dir"`
	TokenFile     string `yaml:"token_file"`
	ApiKey        string `yaml:"api_key"`
	AdminPassword string `yaml:"admin_password"`
	LogFile       string `yaml:"log_file"`
	DebugLogging  bool   `yaml:"debug_logging"`
	APILogInput   bool   `yaml:"api_log_input"`
	APILogOutput  bool   `yaml:"api_log_output"`
	NotionLogReq  bool   `yaml:"notion_log_request"`
	NotionLogResp bool   `yaml:"notion_log_response"`
	DumpAPIInput  bool   `yaml:"dump_api_input"`
}

type ProxyConfig struct {
	NotionAPIBase         string `yaml:"notion_api_base"`
	ClientVersion         string `yaml:"client_version"`
	DefaultModel          string `yaml:"default_model"`
	MaxConcurrency        int    `yaml:"max_concurrency"`
	DisableNotionPrompt   bool   `yaml:"disable_notion_prompt"`
	EnableWebSearch       *bool  `yaml:"enable_web_search"`
	EnableWorkspaceSearch *bool  `yaml:"enable_workspace_search"`
	// AskModeDefault toggles Notion's ASK mode (frontend "Answers only,
	// won't make edits" — config.useReadOnlyMode=true on the workflow
	// thread). When true, all chat requests run in read-only mode by
	// default; per-request callers can still override via the model
	// suffix `-ask`. nil/false = default mode (model can edit).
	AskModeDefault *bool `yaml:"ask_mode_default"`
	// NotionProxy is the optional upstream proxy URL routed in front of every
	// outbound HTTPS request to notion.so (API calls + reverse-proxy /ai).
	// Supported schemes: http, https, socks5, socks5h. Empty = direct dial.
	// The dashboard exposes this field via /admin/settings so operators can
	// flip the upstream without redeploying.
	NotionProxy string `yaml:"notion_proxy"`
}

type TimeoutConfig struct {
	InferenceTimeout int `yaml:"inference_timeout"`
	ResearchTimeout  int `yaml:"research_timeout"`
	APITimeout       int `yaml:"api_timeout"`
	TLSDialTimeout   int `yaml:"tls_dial_timeout"`
}

type RefreshConfig struct {
	IntervalMinutes     int `yaml:"interval_minutes"`
	QuotaRecheckMinutes int `yaml:"quota_recheck_minutes"`
	Concurrency         int `yaml:"concurrency"`
	// LiveCheckSeconds is the minimum interval (in seconds) between live
	// per-request quota checks for the same account. A request always
	// re-checks an account whose cached quota is older than this. Set to 0
	// to force a live check on every request (slower but most up-to-date).
	// Default: 5 seconds.
	LiveCheckSeconds int `yaml:"live_check_seconds"`
}

type BrowserConfig struct {
	UserAgent       string `yaml:"user_agent"`
	SecChUA         string `yaml:"sec_ch_ua"`
	SecChUAPlatform string `yaml:"sec_ch_ua_platform"`
}

// RegisterConfig governs the bulk register history JobStore. Defaults are
// applied via DefaultConfig so a missing register: section in config.yaml
// still yields a working setup.
type RegisterConfig struct {
	// HistoryFile is the JSON file where completed jobs are persisted. The
	// path is relative to the working directory (typically the binary's
	// dir). Default: accounts/.register_history.json
	HistoryFile string `yaml:"history_file"`
	// HistoryMemoryCap caps how many jobs are kept in RAM (and reloaded
	// from disk on startup). Default: 100.
	HistoryMemoryCap int `yaml:"history_memory_cap"`
	// DefaultConcurrency is the value pre-filled in the dashboard's
	// register modal. The runner has no hard upper bound. Default: 1.
	DefaultConcurrency int `yaml:"default_concurrency"`
}

// AppConfig is the global configuration instance
var AppConfig *Config

// DefaultConfig returns the default configuration (matching original hardcoded values)
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Port:          "8081",
			AccountsDir:   "accounts",
			TokenFile:     "token.txt",
			ApiKey:        "",
			LogFile:       "",
			DebugLogging:  true,
			APILogInput:   false,
			APILogOutput:  false,
			NotionLogReq:  false,
			NotionLogResp: false,
		},
		Proxy: ProxyConfig{
			NotionAPIBase:         "https://www.notion.so/api/v3",
			ClientVersion:         "23.13.20260313.1423",
			DefaultModel:          "opus-4.6",
			MaxConcurrency:        1,
			EnableWebSearch:       boolPtr(true),
			EnableWorkspaceSearch: boolPtr(false),
			AskModeDefault:        boolPtr(false),
		},
		Timeouts: TimeoutConfig{
			InferenceTimeout: 300,
			ResearchTimeout:  360,
			APITimeout:       30,
			TLSDialTimeout:   30,
		},
		Refresh: RefreshConfig{
			IntervalMinutes:     30,
			QuotaRecheckMinutes: 30,
			Concurrency:         10,
			LiveCheckSeconds:    5,
		},
		Browser: BrowserConfig{
			UserAgent:       "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
			SecChUA:         `"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"`,
			SecChUAPlatform: `"Windows"`,
		},
		Register: RegisterConfig{
			HistoryFile:        "accounts/.register_history.json",
			HistoryMemoryCap:   100,
			DefaultConcurrency: 1,
		},
		ModelMap: map[string]string{
			"opus-4.6":         "avocado-froyo-medium",
			"sonnet-4.6":       "almond-croissant-low",
			"haiku-4.5":        "anthropic-haiku-4.5",
			"gpt-5.2":          "oatmeal-cookie",
			"gpt-5.4":          "oval-kumquat-medium",
			"gemini-2.5-flash": "vertex-gemini-2.5-flash",
			"gemini-3-flash":   "gingerbread",
			"minimax-m2.5":     "fireworks-minimax-m2.5",
		},
	}
}

// LoadConfig loads configuration with priority: env > config.yaml > defaults.
// configPath is the path to config.yaml (can be empty to skip file loading).
func LoadConfig(configPath string) (*Config, error) {
	cfg := DefaultConfig()
	loadedFromFile := false
	missingConfig := false

	// Step 1: Load from config.yaml if it exists
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			loadedFromFile = true
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config.yaml: %w", err)
			}
			// Ensure model_map is not nil after YAML parse
			if cfg.ModelMap == nil {
				cfg.ModelMap = DefaultConfig().ModelMap
			}
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read config.yaml: %w", err)
		} else {
			missingConfig = true
		}
	}

	// Step 2: Environment variables override config.yaml values
	if v := os.Getenv("PORT"); v != "" {
		cfg.Server.Port = v
	}
	if v := os.Getenv("ACCOUNTS_DIR"); v != "" {
		cfg.Server.AccountsDir = v
	}
	if v := os.Getenv("TOKEN_FILE"); v != "" {
		cfg.Server.TokenFile = v
	}
	if v := os.Getenv("API_KEY"); v != "" {
		cfg.Server.ApiKey = v
	}
	if v := os.Getenv("LOG_FILE"); v != "" {
		cfg.Server.LogFile = v
	}
	if v := os.Getenv("DEBUG_LOGGING"); v != "" {
		cfg.Server.DebugLogging = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("API_LOG_INPUT"); v != "" {
		cfg.Server.APILogInput = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("API_LOG_OUTPUT"); v != "" {
		cfg.Server.APILogOutput = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("NOTION_LOG_REQUEST"); v != "" {
		cfg.Server.NotionLogReq = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("NOTION_LOG_RESPONSE"); v != "" {
		cfg.Server.NotionLogResp = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("DUMP_API_INPUT"); v != "" {
		cfg.Server.DumpAPIInput = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("NOTION_API_BASE"); v != "" {
		cfg.Proxy.NotionAPIBase = v
	}
	if v := os.Getenv("CLIENT_VERSION"); v != "" {
		cfg.Proxy.ClientVersion = v
	}
	if v := os.Getenv("DEFAULT_MODEL"); v != "" {
		cfg.Proxy.DefaultModel = v
	}
	if v := os.Getenv("MAX_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 100 {
			cfg.Proxy.MaxConcurrency = n
		}
	}
	if v := os.Getenv("INFERENCE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.InferenceTimeout = n
		}
	}
	if v := os.Getenv("API_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.APITimeout = n
		}
	}
	if v := os.Getenv("TLS_DIAL_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.TLSDialTimeout = n
		}
	}
	if v := os.Getenv("REFRESH_INTERVAL_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Refresh.IntervalMinutes = n
		}
	}
	if v := os.Getenv("QUOTA_RECHECK_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Refresh.QuotaRecheckMinutes = n
		}
	}
	if v := os.Getenv("REFRESH_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Refresh.Concurrency = n
		}
	}
	if v := os.Getenv("QUOTA_LIVE_CHECK_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.Refresh.LiveCheckSeconds = n
		}
	}
	if v := os.Getenv("USER_AGENT"); v != "" {
		cfg.Browser.UserAgent = v
	}
	if v := os.Getenv("SEC_CH_UA"); v != "" {
		cfg.Browser.SecChUA = v
	}
	if v := os.Getenv("SEC_CH_UA_PLATFORM"); v != "" {
		cfg.Browser.SecChUAPlatform = v
	}
	if v := os.Getenv("ENABLE_WEB_SEARCH"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		cfg.Proxy.EnableWebSearch = &b
	}
	if v := os.Getenv("ENABLE_WORKSPACE_SEARCH"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		cfg.Proxy.EnableWorkspaceSearch = &b
	}
	if v := os.Getenv("ASK_MODE_DEFAULT"); v != "" {
		b := strings.EqualFold(v, "true") || v == "1"
		cfg.Proxy.AskModeDefault = &b
	}
	if v := os.Getenv("NOTION_PROXY"); v != "" {
		cfg.Proxy.NotionProxy = strings.TrimSpace(v)
	}
	// Validate the configured proxy URL once at startup; an invalid scheme
	// is logged and cleared so the runtime fails closed (direct dial)
	// rather than leaking the misconfigured URL into every notion request.
	if cfg.Proxy.NotionProxy != "" {
		if err := netutil.ValidateProxyURL(cfg.Proxy.NotionProxy); err != nil {
			log.Printf("[config] invalid notion_proxy %q: %v (falling back to direct dial)", cfg.Proxy.NotionProxy, err)
			cfg.Proxy.NotionProxy = ""
		}
	}

	// Register defaults — fill any zero values that survived YAML parsing.
	if cfg.Register.HistoryFile == "" {
		cfg.Register.HistoryFile = "accounts/.register_history.json"
	}
	if cfg.Register.HistoryMemoryCap <= 0 {
		cfg.Register.HistoryMemoryCap = 100
	}
	if cfg.Register.DefaultConcurrency <= 0 {
		cfg.Register.DefaultConcurrency = 1
	}

	if err := ConfigureLogOutput(cfg.Server.LogFile); err != nil {
		return nil, fmt.Errorf("configure log output %q: %w", cfg.Server.LogFile, err)
	}

	AppConfig = cfg
	SetDebugLoggingEnabled(cfg.Server.DebugLogging)
	SetAPILogInputEnabled(cfg.Server.APILogInput)
	SetAPILogOutputEnabled(cfg.Server.APILogOutput)
	SetNotionRequestLoggingEnabled(cfg.Server.NotionLogReq)
	SetNotionResponseLoggingEnabled(cfg.Server.NotionLogResp)
	if loadedFromFile {
		log.Printf("[config] loaded from %s", configPath)
	} else if missingConfig {
		log.Printf("[config] %s not found, using defaults", configPath)
	}
	return cfg, nil
}

// GenerateApiKey generates a random 32-character hex API key (sk-xxxx format)
func GenerateApiKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

// GenerateAdminPassword generates a random 16-character password for dashboard login.
func GenerateAdminPassword() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// AdminPasswordSalt returns the salt from a hashed admin password.
// Format: "$sha256$<salt>$<hash>"
func AdminPasswordSalt(hashed string) string {
	parts := strings.SplitN(hashed, "$", 4)
	if len(parts) == 4 && parts[1] == "sha256" {
		return parts[2]
	}
	return ""
}

// AdminPasswordHash returns the hash from a hashed admin password.
func AdminPasswordHash(hashed string) string {
	parts := strings.SplitN(hashed, "$", 4)
	if len(parts) == 4 && parts[1] == "sha256" {
		return parts[3]
	}
	return ""
}

// IsAdminPasswordHashed checks if the password is already hashed.
func IsAdminPasswordHashed(pw string) bool {
	return strings.HasPrefix(pw, "$sha256$")
}

// HashAdminPassword hashes a plaintext password with a random salt.
func HashAdminPassword(plaintext string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	saltHex := hex.EncodeToString(salt)
	h := sha256.Sum256([]byte(saltHex + plaintext))
	return "$sha256$" + saltHex + "$" + hex.EncodeToString(h[:])
}

// VerifyAdminPassword checks if a SHA256 hash matches the stored hash.
// The client sends SHA256(salt + password); server compares directly.
func VerifyAdminPassword(storedHash, clientHash string) bool {
	expected := AdminPasswordHash(storedHash)
	return expected != "" && strings.EqualFold(expected, clientHash)
}

// EnsureAdminPassword hashes the admin password on first run and writes it back.
func EnsureAdminPassword(cfg *Config, configPath string) {
	if cfg.Server.AdminPassword == "" || IsAdminPasswordHashed(cfg.Server.AdminPassword) {
		return
	}

	// Hash the plaintext password
	hashed := HashAdminPassword(cfg.Server.AdminPassword)
	cfg.Server.AdminPassword = hashed
	log.Printf("[config] admin_password hashed (SHA256+salt)")

	// Write back to config.yaml using YAML node manipulation
	if configPath == "" {
		return
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || root.Kind == 0 {
		return
	}
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping := root.Content[0]
		for i := 0; i < len(mapping.Content)-1; i += 2 {
			if mapping.Content[i].Value == "server" {
				serverNode := mapping.Content[i+1]
				found := false
				for j := 0; j < len(serverNode.Content)-1; j += 2 {
					if serverNode.Content[j].Value == "admin_password" {
						serverNode.Content[j+1].Value = hashed
						serverNode.Content[j+1].Tag = "!!str"
						serverNode.Content[j+1].Style = yaml.DoubleQuotedStyle
						found = true
						break
					}
				}
				if !found {
					serverNode.Content = append(serverNode.Content,
						&yaml.Node{Kind: yaml.ScalarNode, Value: "admin_password"},
						&yaml.Node{Kind: yaml.ScalarNode, Value: hashed, Tag: "!!str", Style: yaml.DoubleQuotedStyle},
					)
				}
				break
			}
		}
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		return
	}
	os.WriteFile(configPath, out, 0644)
}

// EnsureApiKey checks if an API key is configured. If not, generates one and
// writes it back to config.yaml so it persists across restarts.
func EnsureApiKey(cfg *Config, configPath string) {
	if cfg.Server.ApiKey != "" {
		return
	}

	cfg.Server.ApiKey = GenerateApiKey()
	log.Printf("[config] no api_key configured, generated: %s", cfg.Server.ApiKey)

	// Write the key back to config.yaml
	if configPath == "" {
		return
	}

	// Read existing file or start fresh
	var root yaml.Node
	data, err := os.ReadFile(configPath)
	if err == nil {
		if err := yaml.Unmarshal(data, &root); err != nil {
			log.Printf("[config] failed to parse %s for write-back: %v", configPath, err)
			return
		}
	}

	// If parsing failed or file is empty, write a minimal config
	if root.Kind == 0 {
		minimal := fmt.Sprintf("server:\n  api_key: \"%s\"\n", cfg.Server.ApiKey)
		if err := os.WriteFile(configPath, []byte(minimal), 0644); err != nil {
			log.Printf("[config] failed to write %s: %v", configPath, err)
		}
		return
	}

	// Find or create server.api_key in the YAML tree
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping := root.Content[0]
		var serverNode *yaml.Node
		for i := 0; i < len(mapping.Content)-1; i += 2 {
			if mapping.Content[i].Value == "server" {
				serverNode = mapping.Content[i+1]
				break
			}
		}
		if serverNode == nil {
			// Add server section
			mapping.Content = append(mapping.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "server"},
				&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
					{Kind: yaml.ScalarNode, Value: "api_key"},
					{Kind: yaml.ScalarNode, Value: cfg.Server.ApiKey, Tag: "!!str"},
				}},
			)
		} else {
			// Find or add api_key in server
			found := false
			for i := 0; i < len(serverNode.Content)-1; i += 2 {
				if serverNode.Content[i].Value == "api_key" {
					serverNode.Content[i+1].Value = cfg.Server.ApiKey
					serverNode.Content[i+1].Tag = "!!str"
					found = true
					break
				}
			}
			if !found {
				serverNode.Content = append(serverNode.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Value: "api_key"},
					&yaml.Node{Kind: yaml.ScalarNode, Value: cfg.Server.ApiKey, Tag: "!!str"},
				)
			}
		}
	}

	out, err := yaml.Marshal(&root)
	if err != nil {
		log.Printf("[config] failed to marshal config: %v", configPath)
		return
	}
	if err := os.WriteFile(configPath, out, 0644); err != nil {
		log.Printf("[config] failed to write %s: %v", configPath, err)
	}
}

// Helper methods for convenient access to typed timeout values

func (c *Config) InferenceTimeoutDuration() time.Duration {
	return time.Duration(c.Timeouts.InferenceTimeout) * time.Second
}

func (c *Config) ResearchTimeoutDuration() time.Duration {
	if c.Timeouts.ResearchTimeout <= 0 {
		return 360 * time.Second
	}
	return time.Duration(c.Timeouts.ResearchTimeout) * time.Second
}

func (c *Config) APITimeoutDuration() time.Duration {
	return time.Duration(c.Timeouts.APITimeout) * time.Second
}

func (c *Config) TLSDialTimeoutDuration() time.Duration {
	return time.Duration(c.Timeouts.TLSDialTimeout) * time.Second
}

func (c *Config) RefreshInterval() time.Duration {
	return time.Duration(c.Refresh.IntervalMinutes) * time.Minute
}

func (c *Config) QuotaRecheckInterval() time.Duration {
	return time.Duration(c.Refresh.QuotaRecheckMinutes) * time.Minute
}

// QuotaLiveCheckInterval returns the minimum interval between live per-request
// quota checks for the same account. Defaults to 5 seconds when unset.
func (c *Config) QuotaLiveCheckInterval() time.Duration {
	if c == nil {
		return 5 * time.Second
	}
	if c.Refresh.LiveCheckSeconds < 0 {
		return 0
	}
	return time.Duration(c.Refresh.LiveCheckSeconds) * time.Second
}

// WebSearchEnabled returns the effective web search setting (default: true)
func (c *Config) WebSearchEnabled() bool {
	if c.Proxy.EnableWebSearch == nil {
		return true
	}
	return *c.Proxy.EnableWebSearch
}

// WorkspaceSearchEnabled returns the effective workspace search setting (default: false)
func (c *Config) WorkspaceSearchEnabled() bool {
	if c.Proxy.EnableWorkspaceSearch == nil {
		return false
	}
	return *c.Proxy.EnableWorkspaceSearch
}

// AskModeDefault returns the effective default ASK-mode setting (default: false).
// ASK mode maps to Notion's workflow config.useReadOnlyMode — when true, the
// model answers without making page edits. Per-request `-ask` suffix on the
// model name still overrides the default for that single call.
func (c *Config) AskModeDefault() bool {
	if c == nil || c.Proxy.AskModeDefault == nil {
		return false
	}
	return *c.Proxy.AskModeDefault
}

// MaxConcurrency returns the per-account request concurrency cap.
// Missing/invalid config falls back to 1; the dashboard validates 1..100.
func (c *Config) MaxConcurrency() int {
	if c == nil || c.Proxy.MaxConcurrency <= 0 {
		return 1
	}
	if c.Proxy.MaxConcurrency > 100 {
		return 100
	}
	return c.Proxy.MaxConcurrency
}

// NotionProxyURL returns the upstream proxy applied to all notion-bound
// traffic. Empty string means dial directly. Reads from AppConfig at call
// time so a /admin/settings PUT takes effect on the next dial without a
// process restart.
func (c *Config) NotionProxyURL() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Proxy.NotionProxy)
}

func boolPtr(b bool) *bool { return &b }

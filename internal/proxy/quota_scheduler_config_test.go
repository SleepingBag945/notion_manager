package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func withTestQuotaScheduling(t *testing.T, strategy string, allowPremium bool, premiumReserveThreshold int) {
	t.Helper()
	prev := AppConfig
	cfg := DefaultConfig()
	if AppConfig != nil {
		clone := *AppConfig
		cfg = &clone
	}
	cfg.Proxy.QuotaStrategy = strategy
	cfg.Proxy.AllowPremium = boolPtr(allowPremium)
	cfg.Proxy.PremiumReserveThreshold = premiumReserveThreshold
	AppConfig = cfg
	t.Cleanup(func() { AppConfig = prev })
}

func quotaWithBasicAndPremium(basicRemainingValue, premiumBalance int) *QuotaInfo {
	limit := 200
	usage := limit - basicRemainingValue
	if usage < 0 {
		usage = 0
	}
	if usage > limit {
		usage = limit
	}
	return &QuotaInfo{
		IsEligible:     true,
		SpaceLimit:     limit,
		SpaceUsage:     usage,
		UserLimit:      limit,
		UserUsage:      usage,
		HasPremium:     premiumBalance > 0,
		PremiumBalance: premiumBalance,
		PremiumLimit:   premiumBalance,
	}
}

func TestConfigQuotaSchedulingDefaultsAndBounds(t *testing.T) {
	if got := (*Config)(nil).QuotaStrategy(); got != quotaStrategyBalanced {
		t.Fatalf("nil config quota strategy: got %q, want %q", got, quotaStrategyBalanced)
	}
	cfg := DefaultConfig()
	if got := cfg.QuotaStrategy(); got != quotaStrategyBalanced {
		t.Fatalf("default quota strategy: got %q, want %q", got, quotaStrategyBalanced)
	}
	if !cfg.AllowPremium() {
		t.Fatal("default AllowPremium() = false, want true")
	}
	if got := cfg.PremiumReserveThreshold(); got != 0 {
		t.Fatalf("default PremiumReserveThreshold() = %d, want 0", got)
	}

	cfg.Proxy.QuotaStrategy = "definitely-invalid"
	if got := cfg.QuotaStrategy(); got != quotaStrategyBalanced {
		t.Fatalf("invalid quota strategy should normalize to balanced, got %q", got)
	}
	cfg.Proxy.AllowPremium = nil
	if !cfg.AllowPremium() {
		t.Fatal("nil allow_premium should default true")
	}
	cfg.Proxy.PremiumReserveThreshold = -10
	if got := cfg.PremiumReserveThreshold(); got != 0 {
		t.Fatalf("negative premium threshold should clamp to 0, got %d", got)
	}
}

func TestLoadConfigQuotaSchedulingFromYAMLAndEnv(t *testing.T) {
	prev := AppConfig
	t.Cleanup(func() { AppConfig = prev })

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`proxy:
  quota_strategy: basic_first
  allow_premium: false
  premium_reserve_threshold: 25
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig yaml: %v", err)
	}
	if got := cfg.QuotaStrategy(); got != quotaStrategyBasicFirst {
		t.Fatalf("yaml quota_strategy: got %q, want %q", got, quotaStrategyBasicFirst)
	}
	if cfg.AllowPremium() {
		t.Fatal("yaml allow_premium=false was not applied")
	}
	if got := cfg.PremiumReserveThreshold(); got != 25 {
		t.Fatalf("yaml premium_reserve_threshold: got %d, want 25", got)
	}

	t.Setenv("QUOTA_STRATEGY", "premium_first")
	t.Setenv("ALLOW_PREMIUM", "true")
	t.Setenv("PREMIUM_RESERVE_THRESHOLD", "40")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig env: %v", err)
	}
	if got := cfg.QuotaStrategy(); got != quotaStrategyPremiumFirst {
		t.Fatalf("env quota_strategy: got %q, want %q", got, quotaStrategyPremiumFirst)
	}
	if !cfg.AllowPremium() {
		t.Fatal("env ALLOW_PREMIUM=true was not applied")
	}
	if got := cfg.PremiumReserveThreshold(); got != 40 {
		t.Fatalf("env PREMIUM_RESERVE_THRESHOLD: got %d, want 40", got)
	}
}

func TestAdminSettingsQuotaSchedulingGetPutPersistsYAML(t *testing.T) {
	prev := AppConfig
	t.Cleanup(func() { AppConfig = prev })
	AppConfig = DefaultConfig()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`proxy:
  enable_web_search: true
server:
  debug_logging: true
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	auth := NewDashboardAuth("", "")
	handler := HandleAdminSettings(path, auth)

	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/admin/settings", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	var getBody map[string]interface{}
	if err := json.Unmarshal(getRec.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if getBody["quota_strategy"] != quotaStrategyBalanced || getBody["allow_premium"] != true || int(getBody["premium_reserve_threshold"].(float64)) != 0 {
		t.Fatalf("GET missing quota scheduling defaults: %#v", getBody)
	}

	putBody := `{"quota_strategy":"premium_first","allow_premium":false,"premium_reserve_threshold":123}`
	putRec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(putRec, req)
	if putRec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", putRec.Code, putRec.Body.String())
	}
	if got := AppConfig.QuotaStrategy(); got != quotaStrategyPremiumFirst {
		t.Fatalf("AppConfig quota strategy: got %q, want %q", got, quotaStrategyPremiumFirst)
	}
	if AppConfig.AllowPremium() {
		t.Fatal("AppConfig allow_premium should be false")
	}
	if got := AppConfig.PremiumReserveThreshold(); got != 123 {
		t.Fatalf("AppConfig premium threshold: got %d, want 123", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var persisted struct {
		Proxy struct {
			QuotaStrategy           string `yaml:"quota_strategy"`
			AllowPremium            *bool  `yaml:"allow_premium"`
			PremiumReserveThreshold int    `yaml:"premium_reserve_threshold"`
		} `yaml:"proxy"`
	}
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("parse persisted yaml: %v\n%s", err, string(data))
	}
	if persisted.Proxy.QuotaStrategy != quotaStrategyPremiumFirst {
		t.Fatalf("persisted quota_strategy: got %q", persisted.Proxy.QuotaStrategy)
	}
	if persisted.Proxy.AllowPremium == nil || *persisted.Proxy.AllowPremium {
		t.Fatalf("persisted allow_premium: got %#v", persisted.Proxy.AllowPremium)
	}
	if persisted.Proxy.PremiumReserveThreshold != 123 {
		t.Fatalf("persisted premium_reserve_threshold: got %d", persisted.Proxy.PremiumReserveThreshold)
	}
}

func TestAdminSettingsRejectsInvalidQuotaScheduling(t *testing.T) {
	prev := AppConfig
	t.Cleanup(func() { AppConfig = prev })
	AppConfig = DefaultConfig()
	handler := HandleAdminSettings(filepath.Join(t.TempDir(), "config.yaml"), NewDashboardAuth("", ""))

	for _, body := range []string{
		`{"quota_strategy":"premium_only"}`,
		`{"premium_reserve_threshold":-1}`,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/admin/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status=%d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

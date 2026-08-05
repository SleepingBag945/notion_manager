package proxy

import "testing"

func TestStripAskModeSuffix(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantStrip   string
		wantEnabled bool
	}{
		{"plain sonnet", "claude-sonnet-4.6", "claude-sonnet-4.6", false},
		{"sonnet ask lower", "claude-sonnet-4.6-ask", "claude-sonnet-4.6", true},
		{"sonnet ask upper", "claude-sonnet-4.6-ASK", "claude-sonnet-4.6", true},
		{"opus ask mixed", "opus-4.6-Ask", "opus-4.6", true},
		{"haiku no suffix", "haiku-4.5", "haiku-4.5", false},
		{"dotted dated", "claude-sonnet-4-6-20250929-ask", "claude-sonnet-4-6-20250929", true},
		{"empty", "", "", false},
		{"shorter than suffix", "ask", "ask", false},
		{"exactly suffix", "-ask", "-ask", false},
		{"trailing 4.6 looks suffix-like", "opus-4.6", "opus-4.6", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, enabled := StripAskModeSuffix(tc.input)
			if got != tc.wantStrip || enabled != tc.wantEnabled {
				t.Fatalf("StripAskModeSuffix(%q) = (%q, %v); want (%q, %v)",
					tc.input, got, enabled, tc.wantStrip, tc.wantEnabled)
			}
		})
	}
}

func TestBuildConfigValueRespectsUseReadOnlyMode(t *testing.T) {
	prev := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = prev }()

	cases := []struct {
		name     string
		readOnly bool
	}{
		{"ask off", false},
		{"ask on", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := buildConfigValue("avocado-froyo-medium", "", false, true, nil, tc.readOnly, false, false)
			got, ok := cfg["useReadOnlyMode"].(bool)
			if !ok {
				t.Fatalf("useReadOnlyMode missing or not bool: %#v", cfg["useReadOnlyMode"])
			}
			if got != tc.readOnly {
				t.Fatalf("useReadOnlyMode = %v, want %v", got, tc.readOnly)
			}
			if cfg["type"] != "workflow" {
				t.Fatalf("expected type=workflow, got %#v", cfg["type"])
			}
		})
	}
}

func TestBuildConfigValueSetsModelForAllTurns(t *testing.T) {
	prev := AppConfig
	AppConfig = DefaultConfig()
	defer func() { AppConfig = prev }()

	tests := []struct {
		name              string
		isSubsequentTurn  bool
		wantModelFromUser bool
	}{
		{name: "first turn", isSubsequentTurn: false, wantModelFromUser: true},
		{name: "subsequent turn", isSubsequentTurn: true, wantModelFromUser: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := buildConfigValue("acai-budino-high", "", false, true, nil, false, false, tt.isSubsequentTurn)
			if got := cfg["model"]; got != "acai-budino-high" {
				t.Fatalf("model = %#v, want %q", got, "acai-budino-high")
			}
			if got := cfg["modelFromUser"]; got != tt.wantModelFromUser {
				t.Fatalf("modelFromUser = %#v, want %v", got, tt.wantModelFromUser)
			}
		})
	}
}

func TestAppConfigAskModeDefault(t *testing.T) {
	prev := AppConfig
	defer func() { AppConfig = prev }()

	cfg := DefaultConfig()
	AppConfig = cfg
	if cfg.AskModeDefault() {
		t.Fatalf("default AskModeDefault() = true; want false")
	}

	on := true
	cfg.Proxy.AskModeDefault = &on
	if !cfg.AskModeDefault() {
		t.Fatalf("AskModeDefault() = false after setting true")
	}

	cfg.Proxy.AskModeDefault = nil
	if cfg.AskModeDefault() {
		t.Fatalf("AskModeDefault() = true after reset to nil")
	}
}

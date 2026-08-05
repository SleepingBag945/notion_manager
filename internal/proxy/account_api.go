package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AccountDTO is the safe admin/dashboard representation.
// Never contains token_v2, cookies, or authorization headers.
type AccountDTO struct {
	AccountID    string     `json:"account_id"`
	UserEmail    string     `json:"user_email"`
	UserName     string     `json:"user_name"`
	SpaceName    string     `json:"space_name"`
	SpaceIDShort string     `json:"space_id_short"`
	PlanType     string     `json:"plan_type"`
	QuotaInfo    *QuotaInfo `json:"quota_info,omitempty"`
	IsExhausted  bool       `json:"is_exhausted"`
	IsPermanent  bool       `json:"is_permanently_exhausted"`
	SpaceCount   int        `json:"space_count"`
}

// NewAccountDTO builds a safe DTO from an Account (no secrets).
func NewAccountDTO(acc *Account) AccountDTO {
	acc.EnsureAccountID()
	quota := acc.quotaSnapshot()
	return AccountDTO{
		AccountID:    acc.AccountID,
		UserEmail:    acc.UserEmail,
		UserName:     acc.UserName,
		SpaceName:    acc.SpaceName,
		SpaceIDShort: acc.ShortSpaceID(),
		PlanType:     acc.PlanType,
		QuotaInfo:    quota.Info,
		IsExhausted:  quota.ExhaustedAt != nil,
		IsPermanent:  quota.PermanentlyExhausted,
		SpaceCount:   acc.SpaceCount,
	}
}

// DiscoverAccountFromToken calls Notion APIs using the given token_v2 to discover
// all account information (user, space, models, quota).
func DiscoverAccountFromToken(tokenV2 string) (*Account, error) {
	return DiscoverAccountFromTokenWithOptions(tokenV2, AccountDiscoveryOptions{})
}

// AccountDiscoveryOptions pins a multi-account Notion session to the intended
// active user and workspace. Empty fields retain the legacy automatic choice.
type AccountDiscoveryOptions struct {
	ActiveUserID  string
	SpaceID       string
	ExpectedEmail string
}

var (
	discoveryHTTPClient  = getChromeHTTPClient
	discoveryFetchModels = FetchModels
	discoveryCheckQuota  = CheckQuota
)

// DiscoverAccountFromTokenWithOptions discovers an account while applying
// optional exact selectors for multi-account browser sessions.
func DiscoverAccountFromTokenWithOptions(tokenV2 string, options AccountDiscoveryOptions) (*Account, error) {
	client := discoveryHTTPClient(AppConfig.APITimeoutDuration())

	// Step 1: Call loadUserContent to get user/space info
	req, err := http.NewRequest("POST", NotionAPIBase+"/loadUserContent", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, fmt.Errorf("create loadUserContent request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", AppConfig.Browser.UserAgent)
	activeUserID := strings.TrimSpace(options.ActiveUserID)
	cookieHeader := "token_v2=" + tokenV2
	if activeUserID != "" {
		cookieHeader = accountCookieHeader(&Account{TokenV2: tokenV2, UserID: activeUserID})
		req.Header.Set("x-notion-active-user-header", activeUserID)
	}
	req.Header.Set("Cookie", cookieHeader)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("loadUserContent request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("loadUserContent API error %d: %s", resp.StatusCode, truncate(string(body), 300))
	}

	// Parse the response
	var userData struct {
		RecordMap struct {
			NotionUser  map[string]json.RawMessage `json:"notion_user"`
			UserRoot    map[string]json.RawMessage `json:"user_root"`
			Space       map[string]json.RawMessage `json:"space"`
			UserSetting map[string]json.RawMessage `json:"user_settings"`
		} `json:"recordMap"`
	}
	if err := json.Unmarshal(body, &userData); err != nil {
		return nil, fmt.Errorf("parse loadUserContent: %w", err)
	}

	type spaceViewPointer struct {
		SpaceID string `json:"spaceId"`
		ID      string `json:"id"`
	}
	spaceViewPointersForUser := func(userID string) []spaceViewPointer {
		var pointers []spaceViewPointer
		raw, ok := userData.RecordMap.UserRoot[userID]
		if !ok {
			return pointers
		}
		var ur struct {
			Value struct {
				Value *struct {
					SpaceViewPointers []spaceViewPointer `json:"space_view_pointers"`
				} `json:"value"`
				SpaceViewPointers []spaceViewPointer `json:"space_view_pointers"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &ur); err != nil {
			return pointers
		}
		if ur.Value.Value != nil {
			return ur.Value.Value.SpaceViewPointers
		}
		return ur.Value.SpaceViewPointers
	}

	// Extract user ID and info
	expectedEmail := strings.TrimSpace(options.ExpectedEmail)
	requestedSpaceID := strings.TrimSpace(options.SpaceID)
	var userID, userName, userEmail string
	for id, raw := range userData.RecordMap.NotionUser {
		if activeUserID != "" && id != activeUserID {
			continue
		}
		var u struct {
			Value struct {
				Value *struct {
					Name  string `json:"name"`
					Email string `json:"email"`
				} `json:"value"`
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &u); err != nil {
			continue
		}
		candidateName := u.Value.Name
		candidateEmail := u.Value.Email
		if u.Value.Value != nil {
			candidateName = u.Value.Value.Name
			candidateEmail = u.Value.Value.Email
		}
		if expectedEmail != "" && !strings.EqualFold(candidateEmail, expectedEmail) {
			continue
		}
		if requestedSpaceID != "" {
			matchedSpace := false
			for _, ptr := range spaceViewPointersForUser(id) {
				if ptr.SpaceID == requestedSpaceID {
					matchedSpace = true
					break
				}
			}
			if !matchedSpace {
				continue
			}
		}
		userID = id
		userName = candidateName
		userEmail = candidateEmail
		break
	}
	if userID == "" {
		if activeUserID != "" || expectedEmail != "" || requestedSpaceID != "" {
			return nil, fmt.Errorf("no user matched the configured Notion account selectors")
		}
		return nil, fmt.Errorf("no user found in loadUserContent response")
	}

	// Extract space view pointers from user_root
	spaceViewPointers := spaceViewPointersForUser(userID)

	// Find the best space (AI enabled, non-free preferred)
	type spaceInfo struct {
		ID          string
		Name        string
		PlanType    string
		SpaceViewID string
		AIEnabled   bool
	}
	var bestSpace *spaceInfo
	for _, ptr := range spaceViewPointers {
		if requestedSpaceID != "" && ptr.SpaceID != requestedSpaceID {
			continue
		}
		raw, ok := userData.RecordMap.Space[ptr.SpaceID]
		if !ok {
			continue
		}
		var s struct {
			Value struct {
				Value *struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					PlanType string `json:"plan_type"`
					Settings struct {
						EnableAIFeature  *bool `json:"enable_ai_feature"`
						DisableAIFeature *bool `json:"disable_ai_feature"`
					} `json:"settings"`
				} `json:"value"`
				ID       string `json:"id"`
				Name     string `json:"name"`
				PlanType string `json:"plan_type"`
				Settings struct {
					EnableAIFeature  *bool `json:"enable_ai_feature"`
					DisableAIFeature *bool `json:"disable_ai_feature"`
				} `json:"settings"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			continue
		}
		var si spaceInfo
		si.SpaceViewID = ptr.ID
		if s.Value.Value != nil {
			si.ID = s.Value.Value.ID
			si.Name = s.Value.Value.Name
			si.PlanType = s.Value.Value.PlanType
			aiOff := s.Value.Value.Settings.DisableAIFeature != nil && *s.Value.Value.Settings.DisableAIFeature
			si.AIEnabled = !aiOff
		} else {
			si.ID = s.Value.ID
			si.Name = s.Value.Name
			si.PlanType = s.Value.PlanType
			aiOff := s.Value.Settings.DisableAIFeature != nil && *s.Value.Settings.DisableAIFeature
			si.AIEnabled = !aiOff
		}
		if si.ID == "" {
			si.ID = ptr.SpaceID
		}
		if requestedSpaceID != "" {
			if si.ID == requestedSpaceID {
				bestSpace = &si
			}
			break
		}
		if bestSpace == nil || (si.AIEnabled && si.PlanType != "free") {
			bestSpace = &si
		}
	}
	if bestSpace == nil {
		if requestedSpaceID != "" {
			return nil, fmt.Errorf("requested NOTION_SPACE_ID was not found for the active account")
		}
		return nil, fmt.Errorf("no workspace found for this account")
	}

	// Extract timezone from user_settings
	timezone := "UTC"
	if raw, ok := userData.RecordMap.UserSetting[userID]; ok {
		var us struct {
			Value struct {
				Value *struct {
					Settings struct {
						TimeZone string `json:"time_zone"`
					} `json:"settings"`
				} `json:"value"`
				Settings struct {
					TimeZone string `json:"time_zone"`
				} `json:"settings"`
			} `json:"value"`
		}
		if err := json.Unmarshal(raw, &us); err == nil {
			if us.Value.Value != nil && us.Value.Value.Settings.TimeZone != "" {
				timezone = us.Value.Value.Settings.TimeZone
			} else if us.Value.Settings.TimeZone != "" {
				timezone = us.Value.Settings.TimeZone
			}
		}
	}

	browserID := generateUUIDv4()
	deviceID := generateUUIDv4()

	acc := &Account{
		TokenV2:       tokenV2,
		UserID:        userID,
		UserName:      userName,
		UserEmail:     userEmail,
		SpaceID:       bestSpace.ID,
		SpaceName:     bestSpace.Name,
		SpaceViewID:   bestSpace.SpaceViewID,
		PlanType:      bestSpace.PlanType,
		Timezone:      timezone,
		ClientVersion: DefaultClientVersion,
		BrowserID:     browserID,
		DeviceID:      deviceID,
	}

	acc.EnsureAccountID()

	// Step 2: Fetch available models
	models, err := discoveryFetchModels(acc)
	if err != nil {
		log.Printf("[add-account] model fetch failed (non-fatal): %v", err)
	} else {
		acc.setModels(models)
	}

	// Step 3: Check quota
	quota, err := discoveryCheckQuota(acc)
	if err != nil {
		log.Printf("[add-account] quota check failed (non-fatal): %v", err)
	} else {
		now := time.Now()
		acc.setQuotaInfo(quota, &now)
	}

	return acc, nil
}

// SaveAccountToFile writes an Account to a JSON file in the accounts directory.
// The filename uses the full account_id for collision safety:
//
//	<account_id>__<sanitized_email>.json
//
// If a file with the same account_id already exists (possibly under an old
// naming scheme), it is overwritten at the same path. This prevents
// duplicate files after migration.
// findFileByAccountIDOnDisk scans dir for a JSON file whose "account_id" field
// (or computed account_id from user_id+space_id) matches the given accountID.
// Returns the full path if found, or "" if no match exists.
func findFileByAccountIDOnDisk(dir, accountID string) (string, error) {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return "", err
		}
		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		aid, _ := m["account_id"].(string)
		if aid == "" {
			uid, _ := m["user_id"].(string)
			sid, _ := m["space_id"].(string)
			if uid != "" && sid != "" {
				aid = ComputeAccountID(uid, sid)
			}
		}
		if aid == accountID {
			return path, nil
		}
	}
	return "", nil
}

func SaveAccountToFile(acc *Account, dir string) (string, error) {
	acc.EnsureAccountID()
	if err := ensurePrivateAccountsDir(dir); err != nil {
		return "", fmt.Errorf("create accounts dir: %w", err)
	}

	name := acc.UserEmail
	if name == "" {
		name = acc.UserName
	}
	if name == "" {
		name = "unknown"
	}
	// Sanitize filename
	name = strings.Map(func(r rune) rune {
		if r == '@' || r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, name)

	newFilename := acc.AccountID + "__" + name + ".json"

	// Check if an existing file already has this account_id (could be old
	// naming scheme like "<aid[:12]>_email.json"). If found, reuse that path
	// to avoid duplicates; otherwise use the new canonical name.
	filename := newFilename
	existingPath, err := findFileByAccountIDOnDisk(dir, acc.AccountID)
	if err != nil {
		return "", fmt.Errorf("find existing account file: %w", err)
	}
	if existingPath != "" {
		filename = filepath.Base(existingPath)
	}
	path := filepath.Join(dir, filename)

	// Build the JSON structure
	acc.EnsureAccountID()
	data := map[string]interface{}{
		"account_id":     acc.AccountID,
		"token_v2":       acc.TokenV2,
		"user_id":        acc.UserID,
		"user_name":      acc.UserName,
		"user_email":     acc.UserEmail,
		"space_id":       acc.SpaceID,
		"space_name":     acc.SpaceName,
		"space_view_id":  acc.SpaceViewID,
		"plan_type":      acc.PlanType,
		"timezone":       acc.Timezone,
		"client_version": acc.ClientVersion,
		"browser_id":     acc.BrowserID,
		"device_id":      acc.DeviceID,
	}
	modelSnapshot := acc.modelsSnapshot()
	quota := acc.quotaSnapshot()
	if len(modelSnapshot) > 0 {
		var models []map[string]string
		for _, m := range modelSnapshot {
			models = append(models, map[string]string{"id": m.ID, "name": m.Name})
		}
		data["available_models"] = models
	}
	if quota.Info != nil {
		data["quota_info"] = map[string]interface{}{
			"is_eligible":         quota.Info.IsEligible,
			"space_usage":         quota.Info.SpaceUsage,
			"space_limit":         quota.Info.SpaceLimit,
			"user_usage":          quota.Info.UserUsage,
			"user_limit":          quota.Info.UserLimit,
			"last_usage_at":       quota.Info.LastUsageAtMs,
			"research_mode_usage": quota.Info.ResearchModeUsage,
			"has_premium":         quota.Info.HasPremium,
			"premium_balance":     quota.Info.PremiumBalance,
			"premium_usage":       quota.Info.PremiumUsage,
			"premium_limit":       quota.Info.PremiumLimit,
		}
	}
	if quota.CheckedAt != nil {
		data["quota_checked_at"] = quota.CheckedAt.Format(time.RFC3339)
	}
	data["extracted_at"] = time.Now().Format(time.RFC3339)

	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal account JSON: %w", err)
	}

	// Ensure accounts directory exists
	if err := ensurePrivateAccountsDir(dir); err != nil {
		return "", fmt.Errorf("create accounts dir: %w", err)
	}

	if err := writePrivateAccountFile(path, append(out, '\n')); err != nil {
		return "", fmt.Errorf("write account file: %w", err)
	}

	return filename, nil
}

// AddAccount adds an account to the pool (hot-load, no restart needed).
func (p *AccountPool) AddAccount(acc *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Check for duplicate by account_id (same user_id + space_id)
	acc.EnsureAccountID()
	for i, existing := range p.accounts {
		existing.EnsureAccountID()
		if existing.AccountID != "" && existing.AccountID == acc.AccountID {
			// Replace existing (same workspace)
			p.accounts[i] = acc
			log.Printf("[account] replaced: %s (%s) aid=%s", acc.UserName, acc.UserEmail, acc.ShortSpaceID())
			return
		}
	}
	p.accounts = append(p.accounts, acc)
	log.Printf("[account] added: %s (%s) [%s]", acc.UserName, acc.UserEmail, acc.PlanType)
}

// DeleteAccountFile removes the JSON file for an account from the accounts directory.
// Matches by account_id first; falls back to user_id+space_id.
func DeleteAccountFile(accountID, dir string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var existing map[string]interface{}
		if err := json.Unmarshal(data, &existing); err != nil {
			continue
		}
		exAID, _ := existing["account_id"].(string)
		if exAID == "" {
			// Legacy file: compute from user_id+space_id
			exUID, _ := existing["user_id"].(string)
			exSID, _ := existing["space_id"].(string)
			if exUID != "" && exSID != "" {
				exAID = ComputeAccountID(exUID, exSID)
			}
		}
		if exAID == accountID {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("delete file %s: %w", entry.Name(), err)
			}
			log.Printf("[account] deleted file: %s", entry.Name())
			return nil
		}
	}
	return fmt.Errorf("account file not found for account_id %s", accountID)
}

// DeleteAccountFileByEmail removes the JSON file by email (legacy).
// Returns AmbiguousEmailError if multiple files match.
func DeleteAccountFileByEmail(email, dir string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	var matches []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var existing map[string]interface{}
		if err := json.Unmarshal(data, &existing); err != nil {
			continue
		}
		if e, _ := existing["user_email"].(string); strings.EqualFold(e, email) {
			matches = append(matches, path)
		}
	}
	switch len(matches) {
	case 0:
		return fmt.Errorf("account file not found for %s", email)
	case 1:
		if err := os.Remove(matches[0]); err != nil {
			return fmt.Errorf("delete file: %w", err)
		}
		log.Printf("[account] deleted file: %s", filepath.Base(matches[0]))
		return nil
	default:
		return &AmbiguousEmailError{Email: email, Count: len(matches)}
	}
}

// HandleAddAccount accepts a token_v2, discovers account info via Notion APIs,
// saves it to disk, and hot-loads it into the pool.
func HandleAddAccount(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Require dashboard session
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		var body struct {
			TokenV2      string `json:"token_v2"`
			NotionUserID string `json:"notion_user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		tokenV2 := strings.TrimSpace(body.TokenV2)
		if tokenV2 == "" {
			http.Error(w, `{"error":"token_v2 is required"}`, http.StatusBadRequest)
			return
		}

		log.Printf("[add-account] discovering account from token_v2 (%d chars)...", len(tokenV2))

		// Discover account info
		acc, err := DiscoverAccountFromTokenWithOptions(tokenV2, AccountDiscoveryOptions{
			ActiveUserID: strings.TrimSpace(body.NotionUserID),
		})
		if err != nil {
			log.Printf("[add-account] discovery failed: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("Failed to discover account: %v", err),
			})
			return
		}

		// Save to file
		filename, err := SaveAccountToFile(acc, accountsDir)
		if err != nil {
			log.Printf("[add-account] save failed: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("Failed to save account: %v", err),
			})
			return
		}

		// Hot-load into pool
		pool.AddAccount(acc)

		log.Printf("[add-account] success: %s (%s) → %s", acc.UserName, acc.UserEmail, filename)

		json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "ok",
			"filename": filename,
			"account": map[string]string{
				"name":      acc.UserName,
				"email":     acc.UserEmail,
				"space":     acc.SpaceName,
				"plan_type": acc.PlanType,
			},
		})
	}
}

// HandleDeleteAccount removes an account from the pool and deletes its file.
func HandleDeleteAccount(pool *AccountPool, accountsDir string, auth *DashboardAuth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method != "POST" {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}

		// Require dashboard session
		if auth.HasAdminPassword() && !auth.ValidateSession(r) {
			http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
			return
		}

		var body struct {
			Email string `json:"email"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}
		email := strings.TrimSpace(body.Email)
		if email == "" {
			http.Error(w, `{"error":"email is required"}`, http.StatusBadRequest)
			return
		}

		// Remove from pool and get account_id for file deletion
		acc := pool.GetByEmail(email)
		if acc == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "account not found in pool"})
			return
		}
		acc.EnsureAccountID()
		accountID := acc.AccountID
		pool.RemoveAccountByEmail(email)

		// Delete file by account_id
		if accountID != "" {
			if err := DeleteAccountFile(accountID, accountsDir); err != nil {
				log.Printf("[delete-account] file deletion warning: %v", err)
			}
		} else {
			// Legacy fallback: no account_id available
			if err := DeleteAccountFileByEmail(email, accountsDir); err != nil {
				log.Printf("[delete-account] file deletion warning (by email): %v", err)
			}
		}

		log.Printf("[delete-account] removed: %s (account_id=%s)", email, accountID)
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

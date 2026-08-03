package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"notion-manager/internal/regjob"
	"notion-manager/internal/regjob/providers"
)

// postRegisterRefreshTimeout caps how long we wait for the per-account
// quota check + disk write triggered after a successful registration. It
// is intentionally short — the worst case is "user sees the new account
// without quota until the next global refresh tick".
const postRegisterRefreshTimeout = 30 * time.Second

// postRegisterRefreshHook is a package-level seam so handler tests can
// observe (or fully replace) the quota refresh that fires when a fresh
// account is written. Production callers leave it nil and we use the
// pool's RefreshAndPersistAccount.
var postRegisterRefreshHook func(deps *RegisterJobsDeps, email string)

// triggerPostRegisterRefresh runs the per-account quota refresh in the
// background. We never block the runner goroutine on a Notion round-trip
// — if the request hangs the registration would appear stuck on the UI.
func triggerPostRegisterRefresh(deps *RegisterJobsDeps, email string) {
	if hook := postRegisterRefreshHook; hook != nil {
		hook(deps, email)
		return
	}
	if deps == nil || deps.Pool == nil {
		return
	}
	go func(em string) {
		// A panic in the quota path (e.g. nil AppConfig in odd test
		// setups, or a future regression) must not bring the server
		// down. Log and move on; the next /admin/refresh tick will
		// retry.
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[admin] post-register quota refresh %s: panic: %v", em, r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), postRegisterRefreshTimeout)
		defer cancel()
		if err := deps.Pool.RefreshAndPersistAccount(ctx, deps.AccountsDir, em); err != nil {
			log.Printf("[admin] post-register quota refresh %s: %v", em, err)
		}
	}(email)
}

// RegisterJobsDeps bundles the dependencies for the new /admin/register/*
// and /admin/accounts/{email} handlers. We pass the bundle by pointer so
// main.go owns a single instance that survives across requests.
type RegisterJobsDeps struct {
	Pool        *AccountPool
	AccountsDir string
	Store       regjob.Store
	// Providers is the registry of Provider implementations available to
	// HandleAdminRegisterStart. Required.
	Providers *providers.Registry
	Auth      *DashboardAuth
}

// authorize enforces dashboard auth on every endpoint in this file. If no
// admin password is configured, all requests are allowed (parity with the
// existing /admin/register handler).
func (d *RegisterJobsDeps) authorize(w http.ResponseWriter, r *http.Request) bool {
	if d == nil || d.Auth == nil {
		http.Error(w, `{"error":"server not configured"}`, http.StatusInternalServerError)
		return false
	}
	if !d.Auth.HasAdminPassword() {
		return true
	}
	if d.Auth.ValidateSession(r) {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	http.Error(w, `{"error":"unauthorized, dashboard login required"}`, http.StatusUnauthorized)
	return false
}

// HandleAdminRegisterProviders returns the list of registered Providers so
// the dashboard can render the provider tabs.
//
//	GET /admin/register/providers
func HandleAdminRegisterProviders(deps *RegisterJobsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !deps.authorize(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var list []providers.Info
		if deps.Providers != nil {
			list = deps.Providers.List()
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"providers": list})
	}
}

// HandleAdminRegisterStart accepts a JSON body
//
//	{ "provider": "microsoft", "input": "<bulk credentials>", "concurrency": 1 }
//
// parses tokens via the selected Provider, creates a regjob.Job, kicks off
// the runner in a background goroutine, and returns { "job_id": "..." }
// immediately. provider is optional and defaults to the first registered
// provider — which is "microsoft" in the current build.
func HandleAdminRegisterStart(deps *RegisterJobsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !deps.authorize(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if deps.Providers == nil {
			http.Error(w, `{"error":"no providers registered"}`, http.StatusInternalServerError)
			return
		}

		var body struct {
			Provider    string `json:"provider"`
			Input       string `json:"input"`
			Concurrency int    `json:"concurrency"`
			Proxy       string `json:"proxy"`
		}
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "application/json") {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"decode: %s"}`, err), http.StatusBadRequest)
				return
			}
		} else {
			raw, err := readRegisterBody(r)
			if err != nil {
				http.Error(w, fmt.Sprintf(`{"error":"read body: %s"}`, err), http.StatusBadRequest)
				return
			}
			body.Input = raw
		}
		proxyURL := strings.TrimSpace(body.Proxy)
		if err := validateProxyScheme(proxyURL); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusBadRequest)
			return
		}
		// Per-job proxy beats the global setting; an empty modal value
		// falls back to AppConfig.Proxy.NotionProxy so registrations follow
		// the same egress policy as runtime traffic. The global URL was
		// already validated at startup, so no extra check is needed here.
		if proxyURL == "" {
			proxyURL = AppConfig.NotionProxyURL()
		}

		providerID := strings.TrimSpace(body.Provider)
		if providerID == "" {
			ids := deps.Providers.IDs()
			if len(ids) == 0 {
				http.Error(w, `{"error":"no providers registered"}`, http.StatusInternalServerError)
				return
			}
			providerID = ids[0]
		}
		prov, ok := deps.Providers.Get(providerID)
		if !ok {
			http.Error(w, fmt.Sprintf(`{"error":"unknown provider: %s"}`, providerID), http.StatusBadRequest)
			return
		}

		creds, err := prov.Parse(body.Input)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"parse: %s"}`, err), http.StatusBadRequest)
			return
		}
		if len(creds) == 0 {
			http.Error(w, `{"error":"no credentials parsed"}`, http.StatusBadRequest)
			return
		}

		concurrency := body.Concurrency
		if concurrency <= 0 {
			concurrency = 1
		}

		emails := make([]string, len(creds))
		for i, c := range creds {
			emails[i] = c.Email
		}
		job := deps.Store.Create(prov.ID(), proxyURL, len(creds), concurrency, emails)

		// Persist the raw inputs side-by-side so the user can retry failed
		// rows later without re-entering credentials. Best-effort: a sidecar
		// write failure must not abort the run, only surface in the log.
		if err := saveRegisterInputs(deps.Store, job.ID, prov.ID(), proxyURL, creds); err != nil {
			log.Printf("[admin] save register inputs sidecar (%s): %v", job.ID, err)
		}

		if err := os.MkdirAll(deps.AccountsDir, 0o755); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"mkdir: %s"}`, err), http.StatusInternalServerError)
			return
		}

		// Detach from the request context so cancelled HTTP clients don't
		// kill the background run.
		runCtx := context.Background()
		go regjob.Run(runCtx, deps.Store, job.ID, prov, creds, regjob.RunOpts{
			Concurrency: concurrency,
			AccountsDir: deps.AccountsDir,
			Proxy:       proxyURL,
			OnSuccess: func(email string) {
				deps.Pool.ReloadFromDir(deps.AccountsDir)
				triggerPostRegisterRefresh(deps, email)
			},
		})

		resp := map[string]interface{}{
			"job_id":      job.ID,
			"provider":    prov.ID(),
			"total":       len(creds),
			"concurrency": concurrency,
			"proxy":       proxyURL,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// saveRegisterInputs serialises creds + provider + proxy into the store's
// sidecar so a retry can recover the exact original payload.
func saveRegisterInputs(store regjob.Store, jobID, provID, proxyURL string, creds []providers.Credential) error {
	dto := make([]regjob.SidecarCredential, len(creds))
	for i, c := range creds {
		dto[i] = regjob.SidecarCredential{Email: c.Email, Raw: c.Raw}
	}
	return store.SaveInputs(jobID, regjob.SidecarPayload{
		Provider:    provID,
		Proxy:       proxyURL,
		Credentials: dto,
	})
}

// validateProxyScheme accepts an empty string (no proxy) or one of
// http/https/socks5/socks5h URLs. Anything else is rejected so the user
// gets a 400 instead of a confusing dial error later.
func validateProxyScheme(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy URL: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
		// ok
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid proxy URL: missing host")
	}
	return nil
}

// HandleAdminRegisterJobsList returns the most recent jobs (default 50).
//
//	GET /admin/register/jobs?limit=50
func HandleAdminRegisterJobsList(deps *RegisterJobsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !deps.authorize(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 100 {
			limit = 100
		}
		jobs := deps.Store.List(limit)
		_ = json.NewEncoder(w).Encode(jobs)
	}
}

// HandleAdminRegisterJobsRouter handles routes under /admin/register/jobs/.
// Shapes:
//
//	GET    /admin/register/jobs/{id}         → job snapshot
//	GET    /admin/register/jobs/{id}/events  → SSE stream
//	POST   /admin/register/jobs/{id}/retry   → reprocess failed steps
//	DELETE /admin/register/jobs/{id}         → drop job + sidecar
func HandleAdminRegisterJobsRouter(deps *RegisterJobsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.authorize(w, r) {
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/admin/register/jobs/")
		if path == "" {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"missing job id"}`, http.StatusBadRequest)
			return
		}
		var id, sub string
		if i := strings.Index(path, "/"); i >= 0 {
			id = path[:i]
			sub = strings.Trim(path[i+1:], "/")
		} else {
			id = path
		}
		switch sub {
		case "events":
			serveJobEvents(deps, id, w, r)
			return
		case "retry":
			serveJobRetry(deps, id, w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			serveJobSnapshot(deps, id, w, r)
		case http.MethodDelete:
			serveJobDelete(deps, id, w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		}
	}
}

// serveJobDelete drops the job and its sidecar from the store. Returns 404
// if the job is unknown so the UI can show "already gone" instead of a
// silent success.
func serveJobDelete(deps *RegisterJobsDeps, id string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodDelete {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if _, ok := deps.Store.Get(id); !ok {
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
		return
	}
	deps.Store.Delete(id)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "deleted": id})
}

// serveJobRetry reprocesses just the failed steps of a previous run. The
// original credentials are recovered from the sidecar SaveInputs created
// at Start time. We create a *new* job (so the original history stays
// intact) and respond with the new job id.
func serveJobRetry(deps *RegisterJobsDeps, id string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if deps.Providers == nil {
		http.Error(w, `{"error":"no providers registered"}`, http.StatusInternalServerError)
		return
	}
	job, ok := deps.Store.Get(id)
	if !ok {
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
		return
	}
	if job.State != regjob.JobDone && job.State != regjob.JobCancelled {
		http.Error(w, `{"error":"job still running"}`, http.StatusConflict)
		return
	}
	// Collect failed emails from the original run.
	var failed []string
	for _, st := range job.Steps {
		if st.Status == regjob.StepFail {
			failed = append(failed, st.Email)
		}
	}
	if len(failed) == 0 {
		http.Error(w, `{"error":"nothing to retry"}`, http.StatusBadRequest)
		return
	}
	// Recover original credentials from sidecar. Without it we cannot
	// safely retry — surface 410 Gone so the UI suggests re-uploading.
	sc, ok := deps.Store.LoadInputs(id)
	if !ok {
		http.Error(w, `{"error":"original input no longer available"}`, http.StatusGone)
		return
	}
	provID := sc.Provider
	if provID == "" {
		provID = job.Provider
	}
	if provID == "" {
		ids := deps.Providers.IDs()
		if len(ids) == 0 {
			http.Error(w, `{"error":"no providers registered"}`, http.StatusInternalServerError)
			return
		}
		provID = ids[0]
	}
	prov, ok := deps.Providers.Get(provID)
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":"unknown provider: %s"}`, provID), http.StatusBadRequest)
		return
	}

	// Build a quick lookup from the saved sidecar so we keep the
	// original raw fields (passwords, refresh tokens, ...) intact.
	want := map[string]bool{}
	for _, e := range failed {
		want[strings.ToLower(strings.TrimSpace(e))] = true
	}
	var creds []providers.Credential
	for _, sCred := range sc.Credentials {
		if want[strings.ToLower(strings.TrimSpace(sCred.Email))] {
			creds = append(creds, providers.Credential{
				Email: sCred.Email,
				Raw:   sCred.Raw,
			})
		}
	}
	if len(creds) == 0 {
		http.Error(w, `{"error":"failed steps no longer in sidecar"}`, http.StatusGone)
		return
	}

	// Reuse the original concurrency cap; clamp to the new total so a
	// 1-row retry doesn't allocate 5 worker slots for nothing.
	concurrency := job.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > len(creds) {
		concurrency = len(creds)
	}

	emails := make([]string, len(creds))
	for i, c := range creds {
		emails[i] = c.Email
	}
	newJob := deps.Store.Create(prov.ID(), sc.Proxy, len(creds), concurrency, emails)
	// Persist a sidecar for the *new* job so a chained retry stays safe.
	if err := saveRegisterInputs(deps.Store, newJob.ID, prov.ID(), sc.Proxy, creds); err != nil {
		log.Printf("[admin] save retry sidecar (%s): %v", newJob.ID, err)
	}

	if err := os.MkdirAll(deps.AccountsDir, 0o755); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"mkdir: %s"}`, err), http.StatusInternalServerError)
		return
	}

	runCtx := context.Background()
	go regjob.Run(runCtx, deps.Store, newJob.ID, prov, creds, regjob.RunOpts{
		Concurrency: concurrency,
		AccountsDir: deps.AccountsDir,
		Proxy:       sc.Proxy,
		OnSuccess: func(email string) {
			deps.Pool.ReloadFromDir(deps.AccountsDir)
			triggerPostRegisterRefresh(deps, email)
		},
	})

	resp := map[string]interface{}{
		"job_id":      newJob.ID,
		"provider":    prov.ID(),
		"total":       len(creds),
		"concurrency": concurrency,
		"proxy":       sc.Proxy,
		"retry_of":    id,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// serveJobSnapshot returns the current snapshot for one job.
func serveJobSnapshot(deps *RegisterJobsDeps, id string, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	job, ok := deps.Store.Get(id)
	if !ok {
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(job)
}

// serveJobEvents streams Server-Sent Events for one job. The very first
// event is `event: snapshot` with the job's current state, followed by
// `event: step` for every UpdateStep and finally `event: done` when the
// runner calls store.Finish.
func serveJobEvents(deps *RegisterJobsDeps, id string, w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"streaming unsupported"}`, http.StatusInternalServerError)
		return
	}
	snap, ch, cancel, err := deps.Store.Subscribe(id)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"job not found"}`, http.StatusNotFound)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if err := writeSSE(w, "snapshot", snap); err != nil {
		return
	}
	flusher.Flush()

	if snap.State == regjob.JobDone || snap.State == regjob.JobCancelled {
		_ = writeSSE(w, "done", snap)
		flusher.Flush()
		return
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, alive := <-ch:
			if !alive {
				return
			}
			name := string(ev.Kind)
			var payload interface{} = ev.Payload
			if ev.Kind == regjob.EventStep {
				payload = map[string]interface{}{
					"step_idx": ev.StepIdx,
					"step":     ev.Payload,
				}
			}
			if err := writeSSE(w, name, payload); err != nil {
				return
			}
			flusher.Flush()
			if ev.Kind == regjob.EventDone {
				return
			}
		}
	}
}

// writeSSE encodes payload as JSON and writes a single SSE message.
func writeSSE(w http.ResponseWriter, event string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	return nil
}

// HandleAdminDeleteAccount removes one account JSON file from disk and from
// the live AccountPool. Path:
//
//	DELETE /admin/accounts/{identifier}
//
// The identifier is either an account_id (64-char hex) or an email (legacy).
func HandleAdminDeleteAccount(deps *RegisterJobsDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !deps.authorize(w, r) {
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		raw := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
		raw = strings.Trim(raw, "/")
		identifier, err := url.PathUnescape(raw)
		if err != nil || identifier == "" {
			http.Error(w, `{"error":"missing identifier"}`, http.StatusBadRequest)
			return
		}

		// If identifier looks like a 64-char hex account_id, delete by that;
		// otherwise fall back to email for backward compatibility.
		if isAccountID(identifier) {
			if err := deleteAccountByID(deps.Pool, deps.AccountsDir, identifier); err != nil {
				if os.IsNotExist(err) {
					http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
					return
				}
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "deleted": identifier})
		} else {
			if err := deleteAccountByEmail(deps.Pool, deps.AccountsDir, identifier); err != nil {
				if os.IsNotExist(err) {
					http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
					return
				}
				http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err), http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok", "deleted": identifier})
		}
	}
}

// deleteAccountByEmail removes the account JSON file whose user_email field
// matches and drops the corresponding pool entry. Returns os.ErrNotExist if
// no file matches; that's mapped to a 404 by the handler.
func deleteAccountByEmail(pool *AccountPool, dir, email string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	var target string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		if got, _ := raw["user_email"].(string); strings.EqualFold(got, email) {
			target = path
			break
		}
	}
	if target == "" {
		return os.ErrNotExist
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	if pool != nil {
		pool.RemoveAccountByEmail(email)
	}
	log.Printf("[admin] deleted account file: %s (%s)", target, email)
	return nil
}

// isAccountID returns true if s looks like a 64-char lowercase hex account_id.
func isAccountID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// deleteAccountByID removes the account JSON file whose account_id field
// matches (or can be computed from user_id+space_id) and drops the pool entry.
func deleteAccountByID(pool *AccountPool, dir, accountID string) error {
	entries, err := readPrivateAccountsDir(dir)
	if err != nil {
		return err
	}
	var target string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := readPrivateAccountFile(path)
		if err != nil {
			return err
		}
		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		aid, _ := raw["account_id"].(string)
		if aid == "" {
			uid, _ := raw["user_id"].(string)
			sid, _ := raw["space_id"].(string)
			if uid != "" && sid != "" {
				aid = ComputeAccountID(uid, sid)
			}
		}
		if aid == accountID {
			target = path
			break
		}
	}
	if target == "" {
		return os.ErrNotExist
	}
	if err := os.Remove(target); err != nil {
		return err
	}
	if pool != nil {
		pool.RemoveAccountByAccountID(accountID)
	}
	log.Printf("[admin] deleted account file by account_id: %s", target)
	return nil
}

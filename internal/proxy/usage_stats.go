package proxy

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// usageStatsRetainDays bounds the size of the per-day map so the persisted
// JSON file doesn't grow unbounded for long-running deployments. We keep a
// rolling 30-day window which is enough for the dashboard's "last 24h" /
// "today" / by_day chart needs.
const usageStatsRetainDays = 30

// usageBucket is the underlying counter shared by total / day / model /
// account aggregations. Count is only meaningful for model + account
// breakdowns; for day buckets we leave it as 0 (request count is folded
// into the global Requests field).
type usageBucket struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Count  int64 `json:"count,omitempty"`
}

// UsageStats aggregates Token usage across the lifetime of the process and
// across restarts (via JSON persistence). All access is protected by a
// single RWMutex; Record is the hot path so it grabs the write lock for
// the minimum amount of work.
type UsageStats struct {
	mu sync.RWMutex

	// Lifetime counters (since first Record ever, persisted across
	// restarts).
	TotalInput  int64 `json:"total_input"`
	TotalOutput int64 `json:"total_output"`
	Requests    int64 `json:"requests"`

	// Per-day input/output, keyed by YYYY-MM-DD in the server's local
	// timezone. Trimmed to usageStatsRetainDays on every Record.
	ByDay map[string]*usageBucket `json:"by_day"`

	// Per-model and per-account breakdowns (lifetime).
	ByModel   map[string]*usageBucket `json:"by_model"`
	ByAccount map[string]*usageBucket `json:"by_account"`

	// Last record timestamp (unix ms).
	LastRecordAtMs int64 `json:"last_record_at_ms"`

	// Request log ring buffer (last requestLogCapacity entries, in-memory only).
	// Not persisted across restarts to keep the stats file small.
	RequestLogs []RequestLogEntry `json:"-"`

	// Runtime-only.
	path  string
	dirty bool
}

// RequestLogEntry is a single request's usage record stored in the ring buffer.
type RequestLogEntry struct {
	Timestamp        time.Time `json:"timestamp"`
	Model            string    `json:"model"`
	Account          string    `json:"account"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
}

// requestLogCapacity is the maximum number of request log entries kept
// in the in-memory ring buffer.
const requestLogCapacity = 200

// usageStatsSingleton is the global instance accessed by the proxy hot
// path and the /admin/stats handler. It is initialised by InitUsageStats;
// callers must use GlobalUsageStats() so a no-op fallback is returned
// when initialisation hasn't completed yet (e.g. in unit tests that
// exercise leaf handlers without bootstrapping main).
var (
	usageStatsOnce      sync.Once
	usageStatsSingleton *UsageStats
	usageStatsFallback  = &UsageStats{
		ByDay:     map[string]*usageBucket{},
		ByModel:   map[string]*usageBucket{},
		ByAccount: map[string]*usageBucket{},
	}
)

// GlobalUsageStats returns the process-wide UsageStats. If InitUsageStats
// hasn't been called the fallback is used so Record is still a no-op-safe
// call (data goes into an in-memory bucket but is never persisted).
func GlobalUsageStats() *UsageStats {
	if usageStatsSingleton != nil {
		return usageStatsSingleton
	}
	return usageStatsFallback
}

// InitUsageStats loads the existing stats from disk (if present) and
// installs the global singleton. Calling it more than once is a no-op
// after the first successful call.
func InitUsageStats(path string) *UsageStats {
	usageStatsOnce.Do(func() {
		s := &UsageStats{
			ByDay:     map[string]*usageBucket{},
			ByModel:   map[string]*usageBucket{},
			ByAccount: map[string]*usageBucket{},
			path:      path,
		}
		if err := s.Load(path); err != nil && !os.IsNotExist(err) {
			log.Printf("[stats] load %s: %v (starting from zero)", path, err)
		}
		usageStatsSingleton = s
	})
	return usageStatsSingleton
}

// Record adds a single request's token usage to the aggregations. Safe to
// call from any goroutine. A nil receiver is treated as a no-op so leaf
// handlers don't have to guard their defer blocks.
func (s *UsageStats) Record(account, model string, prompt, completion int) {
	if s == nil {
		return
	}
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	if prompt == 0 && completion == 0 {
		// Defensive: don't pollute by-key maps with zero rows.
		return
	}

	now := time.Now()
	day := now.Format("2006-01-02")

	s.mu.Lock()
	defer s.mu.Unlock()

	s.TotalInput += int64(prompt)
	s.TotalOutput += int64(completion)
	s.Requests++
	s.LastRecordAtMs = now.UnixMilli()

	bumpBucket(s.ByDay, day, prompt, completion, false)
	if model != "" {
		bumpBucket(s.ByModel, model, prompt, completion, true)
	}
	if account != "" {
		bumpBucket(s.ByAccount, account, prompt, completion, true)
	}

	s.trimByDayLocked()
	s.dirty = true

	// Append to the request log ring buffer (newest at the end).
	entry := RequestLogEntry{
		Timestamp:        now,
		Model:            model,
		Account:          account,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}
	s.RequestLogs = append(s.RequestLogs, entry)
	if len(s.RequestLogs) > requestLogCapacity {
		s.RequestLogs = s.RequestLogs[len(s.RequestLogs)-requestLogCapacity:]
	}
}

func bumpBucket(m map[string]*usageBucket, key string, prompt, completion int, withCount bool) {
	b, ok := m[key]
	if !ok {
		b = &usageBucket{}
		m[key] = b
	}
	b.Input += int64(prompt)
	b.Output += int64(completion)
	if withCount {
		b.Count++
	}
}

// trimByDayLocked drops day buckets older than usageStatsRetainDays. The
// caller must hold the write lock.
func (s *UsageStats) trimByDayLocked() {
	if len(s.ByDay) <= usageStatsRetainDays {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -usageStatsRetainDays).Format("2006-01-02")
	for k := range s.ByDay {
		if k < cutoff {
			delete(s.ByDay, k)
		}
	}
}

// UsageBucketSnapshot is the JSON-friendly bucket emitted by /admin/stats.
type UsageBucketSnapshot struct {
	Input    int64 `json:"input"`
	Output   int64 `json:"output"`
	Total    int64 `json:"total"`
	Requests int64 `json:"requests,omitempty"`
}

// UsageDayPoint is a single point in the daily time series.
type UsageDayPoint struct {
	Date   string `json:"date"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
	Total  int64  `json:"total"`
}

// UsageRowSnapshot represents a model or account row in the top-N list.
type UsageRowSnapshot struct {
	Key    string `json:"-"`
	Model  string `json:"model,omitempty"`
	Email  string `json:"email,omitempty"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
	Total  int64  `json:"total"`
	Count  int64  `json:"count"`
}

// UsageSnapshot is the full read-only view exposed by /admin/stats.
type UsageSnapshot struct {
	Total          UsageBucketSnapshot `json:"total"`
	Today          UsageBucketSnapshot `json:"today"`
	Last24h        UsageBucketSnapshot `json:"last_24h"`
	ByDay          []UsageDayPoint     `json:"by_day"`
	TopModels      []UsageRowSnapshot  `json:"top_models"`
	TopAccounts    []UsageRowSnapshot  `json:"top_accounts"`
	LastRecordAtMs int64               `json:"last_record_at"`
}

// Snapshot builds the read-only view consumed by HandleAdminStats. topN
// controls how many model/account rows are returned (sorted by total
// tokens, descending). A topN <= 0 falls back to 5.
func (s *UsageStats) Snapshot(topN int) UsageSnapshot {
	if topN <= 0 {
		topN = 5
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")

	snap := UsageSnapshot{
		Total: UsageBucketSnapshot{
			Input:    s.TotalInput,
			Output:   s.TotalOutput,
			Total:    s.TotalInput + s.TotalOutput,
			Requests: s.Requests,
		},
		LastRecordAtMs: s.LastRecordAtMs,
	}

	if b, ok := s.ByDay[today]; ok {
		snap.Today = UsageBucketSnapshot{
			Input:  b.Input,
			Output: b.Output,
			Total:  b.Input + b.Output,
		}
	}

	// last_24h: today's bucket plus a fraction of yesterday based on
	// the current local time-of-day. Good enough for a UI subtitle —
	// we don't track hourly granularity.
	last24 := snap.Today
	if b, ok := s.ByDay[yesterday]; ok {
		secondsSinceMidnight := float64(now.Hour()*3600 + now.Minute()*60 + now.Second())
		ratio := 1 - secondsSinceMidnight/86400
		if ratio < 0 {
			ratio = 0
		}
		last24.Input += int64(float64(b.Input) * ratio)
		last24.Output += int64(float64(b.Output) * ratio)
		last24.Total = last24.Input + last24.Output
	}
	snap.Last24h = last24

	// by_day: stable chronological order, oldest first.
	days := make([]string, 0, len(s.ByDay))
	for k := range s.ByDay {
		days = append(days, k)
	}
	sort.Strings(days)
	snap.ByDay = make([]UsageDayPoint, 0, len(days))
	for _, d := range days {
		b := s.ByDay[d]
		snap.ByDay = append(snap.ByDay, UsageDayPoint{
			Date:   d,
			Input:  b.Input,
			Output: b.Output,
			Total:  b.Input + b.Output,
		})
	}

	snap.TopModels = topRows(s.ByModel, topN, func(key string, row *UsageRowSnapshot) {
		row.Model = key
	})
	snap.TopAccounts = topRows(s.ByAccount, topN, func(key string, row *UsageRowSnapshot) {
		row.Email = key
	})

	return snap
}

// GetRequestLogs returns a copy of the request log ring buffer in reverse
// chronological order (newest first). Safe to call from any goroutine.
func (s *UsageStats) GetRequestLogs() []RequestLogEntry {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := len(s.RequestLogs)
	out := make([]RequestLogEntry, n)
	// Reverse so newest is first.
	for i, entry := range s.RequestLogs {
		out[n-1-i] = entry
	}
	return out
}

func topRows(src map[string]*usageBucket, n int, label func(key string, row *UsageRowSnapshot)) []UsageRowSnapshot {
	rows := make([]UsageRowSnapshot, 0, len(src))
	for k, b := range src {
		row := UsageRowSnapshot{
			Key:    k,
			Input:  b.Input,
			Output: b.Output,
			Total:  b.Input + b.Output,
			Count:  b.Count,
		}
		label(k, &row)
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Total != rows[j].Total {
			return rows[i].Total > rows[j].Total
		}
		return rows[i].Key < rows[j].Key
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// Load reads a previously-saved JSON file from disk. Missing files are
// returned as os.IsNotExist errors so callers can ignore them.
func (s *UsageStats) Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var raw struct {
		TotalInput     int64                   `json:"total_input"`
		TotalOutput    int64                   `json:"total_output"`
		Requests       int64                   `json:"requests"`
		ByDay          map[string]*usageBucket `json:"by_day"`
		ByModel        map[string]*usageBucket `json:"by_model"`
		ByAccount      map[string]*usageBucket `json:"by_account"`
		LastRecordAtMs int64                   `json:"last_record_at_ms"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TotalInput = raw.TotalInput
	s.TotalOutput = raw.TotalOutput
	s.Requests = raw.Requests
	if raw.ByDay != nil {
		s.ByDay = raw.ByDay
	}
	if raw.ByModel != nil {
		s.ByModel = raw.ByModel
	}
	if raw.ByAccount != nil {
		s.ByAccount = raw.ByAccount
	}
	s.LastRecordAtMs = raw.LastRecordAtMs
	s.trimByDayLocked()
	s.dirty = false
	return nil
}

// Save writes the current state to disk atomically (via tmp + rename).
// Concurrent writers are serialised by the singleton flush loop, so this
// only takes a read lock to copy state into JSON.
func (s *UsageStats) Save(path string) error {
	if path == "" {
		return nil
	}
	s.mu.RLock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}

	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// markClean is called by the flush loop after a successful Save.
func (s *UsageStats) markClean() {
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
}

// isDirty reports whether there are unsaved changes.
func (s *UsageStats) isDirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// StartFlushLoop spawns a goroutine that periodically writes the stats to
// disk if any new records have been added since the last flush. The
// returned cancel function stops the loop and forces one final flush.
func (s *UsageStats) StartFlushLoop(interval time.Duration) func() {
	if s == nil || s.path == "" {
		return func() {}
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if s.isDirty() {
					if err := s.Save(s.path); err != nil {
						log.Printf("[stats] save %s: %v", s.path, err)
					} else {
						s.markClean()
					}
				}
			case <-stop:
				if s.isDirty() {
					if err := s.Save(s.path); err != nil {
						log.Printf("[stats] final save %s: %v", s.path, err)
					}
				}
				return
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

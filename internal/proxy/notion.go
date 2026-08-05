package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// ErrQuotaExhausted is returned when an account's AI usage limit is exceeded
var ErrQuotaExhausted = errors.New("AI usage limit exceeded")

// ErrPremiumFeatureUnavailable is returned when Notion rejects the request
// because the current account/thread cannot use the requested premium path.
var ErrPremiumFeatureUnavailable = errors.New("premium feature unavailable")

// ErrResearchQuotaExhausted is returned when research mode quota is exceeded.
// The account can still be used for normal chat — only research mode is blocked.
var ErrResearchQuotaExhausted = errors.New("research mode usage limit exceeded")

// ErrEmptyResponse is returned when inference completes but produces no text content.
// This typically means the account/thread is in a bad state and should be retried.
var ErrEmptyResponse = errors.New("empty response from inference")

var (
	NotionAPIBase        = "https://www.notion.so/api/v3"
	DefaultClientVersion = "23.13.20260313.1423"
)

var DefaultModelMap = map[string]string{
	"opus-4.6":         "avocado-froyo-medium",
	"sonnet-4.6":       "almond-croissant-low",
	"haiku-4.5":        "anthropic-haiku-4.5",
	"gpt-5.2":          "oatmeal-cookie",
	"gpt-5.4":          "oval-kumquat-medium",
	"gemini-2.5-flash": "vertex-gemini-2.5-flash",
	"gemini-3-flash":   "gingerbread",
	"minimax-m2.5":     "fireworks-minimax-m2.5",
}

var modelMapMu sync.RWMutex

// GetModelID returns the Notion internal ID for a friendly model name (thread-safe).
func GetModelID(name string) (string, bool) {
	modelMapMu.RLock()
	defer modelMapMu.RUnlock()
	id, ok := DefaultModelMap[name]
	return id, ok
}

// SetModelID sets a single entry in DefaultModelMap (thread-safe).
func SetModelID(name, id string) {
	modelMapMu.Lock()
	defer modelMapMu.Unlock()
	DefaultModelMap[name] = id
}

// ReplaceModelMap atomically replaces the entire DefaultModelMap (thread-safe).
func ReplaceModelMap(m map[string]string) {
	modelMapMu.Lock()
	defer modelMapMu.Unlock()
	DefaultModelMap = m
}

// SnapshotModelMap returns a shallow copy of DefaultModelMap (thread-safe).
func SnapshotModelMap() map[string]string {
	modelMapMu.RLock()
	defer modelMapMu.RUnlock()
	copy := make(map[string]string, len(DefaultModelMap))
	for k, v := range DefaultModelMap {
		copy[k] = v
	}
	return copy
}

// ApplyConfig applies loaded configuration to package-level variables.
func ApplyConfig(cfg *Config) {
	NotionAPIBase = cfg.Proxy.NotionAPIBase
	DefaultClientVersion = cfg.Proxy.ClientVersion
	ReplaceModelMap(cfg.ModelMap)
	SetDebugLoggingEnabled(cfg.Server.DebugLogging)
	SetAPILogInputEnabled(cfg.Server.APILogInput)
	SetAPILogOutputEnabled(cfg.Server.APILogOutput)
	SetNotionRequestLoggingEnabled(cfg.Server.NotionLogReq)
	SetNotionResponseLoggingEnabled(cfg.Server.NotionLogResp)
}

// anthropicModelAliases maps Anthropic SDK model names to our friendly aliases
var anthropicModelAliases = map[string]string{
	"claude-opus-4-6":   "opus-4.6",
	"claude-opus-4-5":   "opus-4.6", // older version → best available opus
	"claude-sonnet-4-6": "sonnet-4.6",
	"claude-haiku-4-5":  "haiku-4.5",
	"claude-sonnet-4-5": "sonnet-4.5",
	"claude-opus-4":     "opus-4",
	"claude-sonnet-4":   "sonnet-4",
	"claude-haiku-3-5":  "haiku-3.5",
}

// ResolveModel maps OpenAI/Anthropic model name → Notion internal model name
func ResolveModel(model string) string {
	snap := SnapshotModelMap()
	// Direct match
	if id, ok := snap[model]; ok {
		return id
	}
	// Try Anthropic alias (e.g. "claude-opus-4-6" → "opus-4.6")
	if alias, ok := anthropicModelAliases[model]; ok {
		if id, ok2 := snap[alias]; ok2 {
			return id
		}
	}
	// Strip date suffix (e.g. "claude-opus-4-6-20250929" → "claude-opus-4-6")
	if idx := strings.LastIndex(model, "-2"); idx > 0 && len(model)-idx >= 9 {
		stripped := model[:idx]
		if id, ok := snap[stripped]; ok {
			return id
		}
		if alias, ok := anthropicModelAliases[stripped]; ok {
			if id, ok2 := snap[alias]; ok2 {
				return id
			}
		}
	}
	// Fuzzy fallback: map unrecognized models by family keyword
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "opus"):
		if id, ok := snap["opus-4.6"]; ok {
			log.Printf("[model] fuzzy fallback %q → opus-4.6", model)
			return id
		}
	case strings.Contains(lower, "sonnet"):
		if id, ok := snap["sonnet-4.6"]; ok {
			log.Printf("[model] fuzzy fallback %q → sonnet-4.6", model)
			return id
		}
	case strings.Contains(lower, "haiku"):
		if id, ok := snap["haiku-4.5"]; ok {
			log.Printf("[model] fuzzy fallback %q → haiku-4.5", model)
			return id
		}
	}
	return model
}

// StreamCallback is called for each text delta during streaming
type StreamCallback func(delta string, done bool, usage *UsageInfo)

func buildWebSearchCallOptions(requestID string, reasoningEffort string) CallOptions {
	return CallOptions{
		EnableWebSearch: true,
		ReasoningEffort: reasoningEffort,
		RequestID:       requestID,
	}
}

// executeWebSearch runs a web search query via Notion's native search capability.
// It makes a separate CallInference call with useWebSearch=true and no tool framing,
// allowing Notion's model to use its built-in search tool (two-turn inference).
func executeWebSearch(acc *Account, query string, model string, requestID string, reasoningEffort string) (string, *UsageInfo, error) {
	var result strings.Builder
	var finalUsage *UsageInfo
	var knownCitationURLs []string
	var knownCitationDocs []CitationCandidate
	knownToolCallURLs := make(map[string][]string)

	messages := []ChatMessage{
		{Role: "user", Content: query},
	}

	callOpts := buildWebSearchCallOptions(requestID, reasoningEffort)
	callOpts.KnownCitationURLs = &knownCitationURLs
	callOpts.KnownCitationDocs = &knownCitationDocs
	callOpts.KnownToolCallURLs = &knownToolCallURLs

	err := CallInference(acc, messages, model, false, func(delta string, done bool, usage *UsageInfo) {
		if delta != "" {
			result.WriteString(delta)
		}
		if usage != nil {
			finalUsage = usage
		}
	}, callOpts)

	text := result.String()
	if err == nil && text != "" {
		text = cleanCitationsWithContext(text, knownToolCallURLs, knownCitationURLs, knownCitationDocs)
	}

	return text, finalUsage, err
}

// citationRe matches Notion citation formats:
// [^{{URL}}] — compressed URL with double braces
// [^URL]     — plain URL citation
var citationRe = regexp.MustCompile(`\[\^(?:\{\{)?([^\]\}]+?)(?:\}\})?\]`)

// researcherCitationRe matches researcher mode citation tags: [step-HEXID,artifact,N]
var researcherCitationRe = regexp.MustCompile(`\[step-[0-9a-f]+,artifact,\d+\]`)

// toolCitationTokenRe matches Notion's tool citation token format:
// "toolu_xxx" or "<index>toolu_xxx" where index is 1-based.
var toolCitationTokenRe = regexp.MustCompile(`^(\d+)?(toolu_[A-Za-z0-9]+)$`)

// internalCitationSuffixRe matches Notion-internal URI fragments that sometimes
// get appended to an otherwise valid citation URL payload.
var internalCitationSuffixRe = regexp.MustCompile(`(?i)(thread|view|page|block|space|database)://`)

// sanitizeURL trims non-URL content from Notion's compressed citation URLs.
// Notion's [^{{...}}] format sometimes includes page text/metadata after the
// actual URL. This function truncates at the first non-ASCII character (CJK
// text, replacement chars, etc.) since valid URLs use ASCII only.
func sanitizeURL(raw string) string {
	sanitized, _ := sanitizeURLDetails(raw)
	return sanitized
}

func sanitizeURLDetails(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	hadInternalSuffix := false

	if idx := strings.Index(strings.ToLower(raw), "https://"); idx > 0 {
		raw = raw[idx:]
	} else if idx := strings.Index(strings.ToLower(raw), "http://"); idx > 0 {
		raw = raw[idx:]
	}

	if match := internalCitationSuffixRe.FindStringIndex(raw); match != nil && match[0] > 0 {
		hadInternalSuffix = true
		raw = trimTrailingURLPunctuation(raw[:match[0]])
	}

	for i, ch := range raw {
		if ch > 127 {
			return trimTrailingURLPunctuation(raw[:i]), hadInternalSuffix
		}
	}
	return trimTrailingURLPunctuation(raw), hadInternalSuffix
}

func trimTrailingURLPunctuation(raw string) string {
	raw = strings.TrimSpace(raw)
	for raw != "" {
		switch raw[len(raw)-1] {
		case '.', ',', ';', ':', '!', '?':
			raw = raw[:len(raw)-1]
			continue
		case ')':
			if strings.Count(raw, "(") < strings.Count(raw, ")") {
				raw = raw[:len(raw)-1]
				continue
			}
		case '_', '/':
			raw = raw[:len(raw)-1]
			continue
		case '-':
			if strings.HasSuffix(raw, "://") {
				return raw
			}
			raw = raw[:len(raw)-1]
			continue
		}
		break
	}
	return raw
}

func isHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

func resolveToolCitationURL(raw string, toolCallURLs map[string][]string) (string, bool) {
	if len(toolCallURLs) == 0 {
		return "", false
	}
	toolCallID, idx, hasExplicitIndex, ok := parseToolCitationToken(raw)
	if !ok {
		return "", false
	}
	urls := toolCallURLs[toolCallID]
	if len(urls) == 0 {
		return "", false
	}
	if !hasExplicitIndex && len(urls) > 1 {
		return "", false
	}
	if idx > len(urls) {
		idx = 1
	}
	if !isHTTPURL(urls[idx-1]) {
		return "", false
	}
	return urls[idx-1], true
}

func parseToolCitationToken(raw string) (toolCallID string, idx int, hasExplicitIndex bool, ok bool) {
	m := toolCitationTokenRe.FindStringSubmatch(strings.TrimSpace(raw))
	if len(m) != 3 {
		return "", 0, false, false
	}
	idx = 1
	if m[1] != "" {
		hasExplicitIndex = true
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			idx = n
		}
	}
	return m[2], idx, hasExplicitIndex, true
}

// normalizeCitationTarget resolves and validates a citation payload.
// It accepts normal HTTP(S) URLs and mapped tool citation tokens.
func normalizeCitationTarget(raw string, toolCallURLs map[string][]string) (string, bool) {
	return normalizeCitationTargetWithCandidates(raw, toolCallURLs, nil)
}

func mergeCitationCandidates(urls []string, candidates []CitationCandidate) []CitationCandidate {
	merged := make([]CitationCandidate, 0, len(candidates)+len(urls))
	byURL := make(map[string]int, len(candidates)+len(urls))

	add := func(candidate CitationCandidate) {
		candidate.URL = sanitizeURL(candidate.URL)
		if !isHTTPURL(candidate.URL) {
			return
		}
		if idx, ok := byURL[candidate.URL]; ok {
			if merged[idx].Title == "" && candidate.Title != "" {
				merged[idx].Title = candidate.Title
			}
			if merged[idx].Text == "" && candidate.Text != "" {
				merged[idx].Text = candidate.Text
			}
			return
		}
		byURL[candidate.URL] = len(merged)
		merged = append(merged, candidate)
	}

	for _, candidate := range candidates {
		add(candidate)
	}
	for _, rawURL := range urls {
		add(CitationCandidate{URL: rawURL})
	}

	return merged
}

func citationCandidateURLs(candidates []CitationCandidate) []string {
	if len(candidates) == 0 {
		return nil
	}
	urls := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL != "" {
			urls = append(urls, candidate.URL)
		}
	}
	return urls
}

func matchKnownCitationURL(raw string, knownURLs []string) (string, int) {
	seen := make(map[string]bool, len(knownURLs))
	matches := 0
	var candidate string

	for _, known := range knownURLs {
		known = strings.TrimSpace(known)
		if known == "" || seen[known] {
			continue
		}
		seen[known] = true
		if known == raw {
			return known, 1
		}
		if strings.HasPrefix(known, raw) {
			matches++
			if matches == 1 {
				candidate = known
			}
		}
	}

	if matches == 1 {
		return candidate, 1
	}
	return "", matches
}

var citationContextStopwords = map[string]bool{
	"about":       true,
	"actually":    true,
	"analysis":    true,
	"and":         true,
	"anthropic":   true,
	"article":     true,
	"available":   true,
	"best":        true,
	"blog":        true,
	"claude":      true,
	"comparison":  true,
	"complete":    true,
	"com":         true,
	"content":     true,
	"cost":        true,
	"developer":   true,
	"features":    true,
	"guide":       true,
	"http":        true,
	"https":       true,
	"introducing": true,
	"latest":      true,
	"learn":       true,
	"million":     true,
	"model":       true,
	"models":      true,
	"more":        true,
	"news":        true,
	"page":        true,
	"performance": true,
	"per":         true,
	"pricing":     true,
	"report":      true,
	"section":     true,
	"source":      true,
	"sources":     true,
	"text":        true,
	"the":         true,
	"title":       true,
	"token":       true,
	"tokens":      true,
	"url":         true,
	"which":       true,
	"with":        true,
	"worth":       true,
	"www":         true,
}

var citationModelRefRe = regexp.MustCompile(`(?i)\bclaude(?:\s+(\d+(?:\.\d+)?))?\s+(sonnet|opus|haiku)(?:\s+(\d+(?:\.\d+)?))?\b`)

type citationModelReference struct {
	Full          string
	Family        string
	Version       string
	FamilyVersion string
	Compact       string
}

func containsASCIILetterOrDigit(s string) bool {
	for _, ch := range s {
		if ch <= 127 && (ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z') {
			return true
		}
	}
	return false
}

func extractCitationContextTerms(text string) map[string]bool {
	terms := make(map[string]bool)
	for _, field := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.'
	}) {
		field = strings.Trim(field, ".")
		if field == "" || !containsASCIILetterOrDigit(field) {
			continue
		}
		if citationContextStopwords[field] {
			continue
		}
		if !strings.ContainsRune(field, '.') && len(field) < 3 {
			if _, err := strconv.Atoi(field); err != nil {
				continue
			}
		}
		terms[field] = true
		if strings.ContainsRune(field, '.') {
			for _, part := range strings.Split(field, ".") {
				part = strings.Trim(part, ".")
				if part == "" || citationContextStopwords[part] {
					continue
				}
				if !containsASCIILetterOrDigit(part) {
					continue
				}
				if len(part) < 2 {
					if _, err := strconv.Atoi(part); err != nil {
						continue
					}
				}
				terms[part] = true
			}
		}
	}
	return terms
}

func extractCitationModelReferences(text string) []citationModelReference {
	matches := citationModelRefRe.FindAllStringSubmatch(strings.ToLower(text), -1)
	if len(matches) == 0 {
		return nil
	}
	refs := make([]citationModelReference, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		full := strings.TrimSpace(match[0])
		family := strings.TrimSpace(match[2])
		version := strings.TrimSpace(match[3])
		if version == "" {
			version = strings.TrimSpace(match[1])
		}
		ref := citationModelReference{
			Full:    full,
			Family:  family,
			Version: version,
		}
		if ref.Family != "" && ref.Version != "" {
			ref.FamilyVersion = strings.TrimSpace(ref.Family + " " + ref.Version)
			ref.Compact = strings.TrimSpace("claude " + ref.Version)
		}
		key := ref.Full + "|" + ref.FamilyVersion + "|" + ref.Compact
		if key == "||" || seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	return refs
}

func citationNormalizedTokens(text string) []string {
	if text == "" {
		return nil
	}
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.'
	})
}

func citationContainsTokenSequence(haystack string, needle string) bool {
	hayTokens := citationNormalizedTokens(haystack)
	needleTokens := citationNormalizedTokens(needle)
	if len(hayTokens) == 0 || len(needleTokens) == 0 || len(needleTokens) > len(hayTokens) {
		return false
	}
	for i := 0; i <= len(hayTokens)-len(needleTokens); i++ {
		match := true
		for j := range needleTokens {
			if hayTokens[i+j] != needleTokens[j] {
				match = false
				break
			}
		}
		if match {
			lastIdx := i + len(needleTokens) - 1
			if isNumericCitationToken(needleTokens[len(needleTokens)-1]) && lastIdx+1 < len(hayTokens) && isNumericCitationToken(hayTokens[lastIdx+1]) {
				continue
			}
			return true
		}
	}
	return false
}

func isNumericCitationToken(token string) bool {
	if token == "" {
		return false
	}
	hasDigit := false
	for _, ch := range token {
		switch {
		case ch >= '0' && ch <= '9':
			hasDigit = true
		case ch == '.':
			continue
		default:
			return false
		}
	}
	return hasDigit
}

func citationCandidateModelMatchLevel(candidate CitationCandidate, refs []citationModelReference) int {
	if len(refs) == 0 {
		return 0
	}
	lowerTitle := strings.ToLower(candidate.Title)
	lowerText := strings.ToLower(candidate.Text)
	lowerURL := strings.ToLower(candidate.URL)
	best := 0
	for _, ref := range refs {
		switch {
		case ref.Full != "" && (citationContainsTokenSequence(lowerTitle, ref.Full) || citationContainsTokenSequence(lowerURL, ref.Full)):
			if best < 4 {
				best = 4
			}
		case ref.FamilyVersion != "" && (citationContainsTokenSequence(lowerTitle, ref.FamilyVersion) || citationContainsTokenSequence(lowerURL, ref.FamilyVersion)):
			if best < 4 {
				best = 4
			}
		case ref.Compact != "" && (citationContainsTokenSequence(lowerTitle, ref.Compact) || citationContainsTokenSequence(lowerURL, ref.Compact)):
			if best < 3 {
				best = 3
			}
		case ref.Full != "" && citationContainsTokenSequence(lowerText, ref.Full):
			if best < 2 {
				best = 2
			}
		case ref.FamilyVersion != "" && citationContainsTokenSequence(lowerText, ref.FamilyVersion):
			if best < 2 {
				best = 2
			}
		}
	}
	return best
}

func citationContextWindow(text string, start, end int) string {
	const before = 240
	const after = 120

	left := max(0, start-before)
	for left > 0 && left < len(text) && !utf8.RuneStart(text[left]) {
		left--
	}
	right := min(len(text), end+after)
	for right < len(text) && !utf8.RuneStart(text[right]) {
		right++
	}
	if idx := strings.LastIndex(text[left:start], "\n"); idx >= 0 {
		left += idx + 1
	}
	if idx := strings.Index(text[end:right], "\n"); idx >= 0 {
		right = end + idx
	}
	return text[left:start] + " " + text[end:right]
}

func citationCandidatePool(raw string, candidates []CitationCandidate) []CitationCandidate {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(candidates) == 0 {
		return candidates
	}

	prefixMatches := make([]CitationCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate.URL, raw) {
			prefixMatches = append(prefixMatches, candidate)
		}
	}
	if len(prefixMatches) > 0 {
		return prefixMatches
	}
	return candidates
}

func filterCitationCandidatesByURL(candidates []CitationCandidate, urls []string) []CitationCandidate {
	if len(candidates) == 0 || len(urls) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(urls))
	for _, rawURL := range urls {
		if isHTTPURL(rawURL) {
			allowed[rawURL] = true
		}
	}
	if len(allowed) == 0 {
		return nil
	}
	filtered := make([]CitationCandidate, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL == "" || !allowed[candidate.URL] || seen[candidate.URL] {
			continue
		}
		filtered = append(filtered, candidate)
		seen[candidate.URL] = true
	}
	return filtered
}

func resolveCitationByContext(raw string, knownURLs []string, candidates []CitationCandidate, context string) (string, bool) {
	merged := mergeCitationCandidates(knownURLs, candidates)
	if len(merged) == 0 {
		return "", false
	}
	pool := citationCandidatePool(raw, merged)
	if len(pool) == 0 {
		return "", false
	}

	contextTerms := extractCitationContextTerms(context)
	if len(contextTerms) == 0 {
		return "", false
	}
	modelRefs := extractCitationModelReferences(context)
	if len(modelRefs) > 0 {
		bestLevel := 0
		bestURL := ""
		tied := false
		for _, candidate := range pool {
			level := citationCandidateModelMatchLevel(candidate, modelRefs)
			if level <= 0 {
				continue
			}
			if level > bestLevel {
				bestLevel = level
				bestURL = candidate.URL
				tied = false
			} else if level == bestLevel && candidate.URL != bestURL {
				tied = true
			}
		}
		if bestLevel > 0 && !tied && bestURL != "" {
			return bestURL, true
		}
	}

	poolFreq := make(map[string]int)
	candidateTerms := make([]map[string]bool, len(pool))
	for i, candidate := range pool {
		terms := extractCitationContextTerms(candidate.URL + " " + candidate.Title + " " + candidate.Text)
		candidateTerms[i] = terms
		for term := range terms {
			poolFreq[term]++
		}
	}

	bestScore := 0
	secondScore := 0
	bestURL := ""
	for i, candidate := range pool {
		score := 0
		lowerTitle := strings.ToLower(candidate.Title)
		lowerText := strings.ToLower(candidate.Text)
		lowerURL := strings.ToLower(candidate.URL)
		for term := range contextTerms {
			if !candidateTerms[i][term] {
				continue
			}
			freq := poolFreq[term]
			switch {
			case freq <= 1:
				score += 4
			case freq == 2:
				score += 2
			default:
				score++
			}
			if strings.ContainsRune(term, '.') || strings.IndexFunc(term, unicode.IsDigit) >= 0 {
				score++
			}
		}
		if raw != "" && strings.HasPrefix(candidate.URL, raw) {
			score += 2
		}
		for _, ref := range modelRefs {
			switch {
			case ref.Full != "" && (citationContainsTokenSequence(lowerTitle, ref.Full) || citationContainsTokenSequence(lowerURL, ref.Full)):
				score += 8
			case ref.FamilyVersion != "" && (citationContainsTokenSequence(lowerTitle, ref.FamilyVersion) || citationContainsTokenSequence(lowerURL, ref.FamilyVersion)):
				score += 7
			case ref.Compact != "" && (citationContainsTokenSequence(lowerTitle, ref.Compact) || citationContainsTokenSequence(lowerURL, ref.Compact)):
				score += 5
			}
			switch {
			case ref.Full != "" && citationContainsTokenSequence(lowerText, ref.Full):
				score += 4
			case ref.FamilyVersion != "" && citationContainsTokenSequence(lowerText, ref.FamilyVersion):
				score += 3
			}
		}
		if len(modelRefs) > 0 && strings.Contains(lowerURL, "anthropic.com") {
			score++
		}
		if len(modelRefs) > 0 && strings.Contains(lowerURL, "wikipedia.org") {
			score--
		}
		if score > bestScore {
			secondScore = bestScore
			bestScore = score
			bestURL = candidate.URL
		} else if score > secondScore {
			secondScore = score
		}
	}

	if bestScore < 3 || bestScore <= secondScore {
		return "", false
	}
	return bestURL, true
}

func resolveToolCitationURLWithContext(raw string, toolCallURLs map[string][]string, candidates []CitationCandidate, context string) (string, bool) {
	if len(toolCallURLs) == 0 {
		return "", false
	}
	toolCallID, idx, hasExplicitIndex, ok := parseToolCitationToken(raw)
	if !ok {
		return "", false
	}
	urls := toolCallURLs[toolCallID]
	if len(urls) == 0 {
		return "", false
	}
	if hasExplicitIndex || len(urls) == 1 {
		if idx > len(urls) {
			idx = 1
		}
		if isHTTPURL(urls[idx-1]) {
			return urls[idx-1], true
		}
		return "", false
	}
	if context == "" {
		return "", false
	}
	filteredCandidates := filterCitationCandidatesByURL(candidates, urls)
	if resolved, ok := resolveCitationByContext("", urls, filteredCandidates, context); ok {
		return resolved, true
	}
	return "", false
}

func normalizeCitationTargetWithContext(raw string, toolCallURLs map[string][]string, knownURLs []string, candidates []CitationCandidate, context string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}

	if decoded, err := url.QueryUnescape(raw); err == nil {
		raw = decoded
	}
	raw, hadInternalSuffix := sanitizeURLDetails(raw)

	if resolved, ok := resolveToolCitationURLWithContext(raw, toolCallURLs, candidates, context); ok {
		return resolved, true
	}

	mergedCandidates := mergeCitationCandidates(knownURLs, candidates)
	mergedURLs := citationCandidateURLs(mergedCandidates)
	if match, count := matchKnownCitationURL(raw, mergedURLs); match != "" {
		return match, true
	} else {
		if (hadInternalSuffix || !isHTTPURL(raw) || count > 0) && context != "" {
			if resolved, ok := resolveCitationByContext(raw, nil, mergedCandidates, context); ok {
				return resolved, true
			}
		}
		if count > 0 {
			// The streamed citation only matches known candidates by URL prefix.
			// If surrounding text can't disambiguate it, drop it instead of
			// emitting a truncated URL like ".../claude-son".
			return "", false
		}
	}

	if !isHTTPURL(raw) {
		return "", false
	}
	return raw, true
}

func normalizeCitationTargetWithCandidates(raw string, toolCallURLs map[string][]string, knownURLs []string) (string, bool) {
	return normalizeCitationTargetWithContext(raw, toolCallURLs, knownURLs, nil, "")
}

// expandToolCitationReferences replaces [^toolu_*] style references with [^https://...]
// when we have search result URL mappings for that tool call id.
func expandToolCitationReferences(text string, toolCallURLs map[string][]string) string {
	return expandToolCitationReferencesWithKnowledge(text, toolCallURLs, nil)
}

func expandToolCitationReferencesWithKnowledge(text string, toolCallURLs map[string][]string, candidates []CitationCandidate) string {
	if text == "" || len(toolCallURLs) == 0 {
		return text
	}
	matches := citationRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var out strings.Builder
	last := 0
	for _, match := range matches {
		out.WriteString(text[last:match[0]])
		raw := text[match[2]:match[3]]
		resolved, ok := normalizeCitationTargetWithContext(
			raw,
			toolCallURLs,
			nil,
			candidates,
			citationContextWindow(text, match[0], match[1]),
		)
		if ok {
			out.WriteString(fmt.Sprintf("[^%s]", resolved))
		} else {
			out.WriteString(text[match[0]:match[1]])
		}
		last = match[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

func normalizeObservedCitationFragment(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[^")
	raw = strings.TrimPrefix(raw, "{{")
	raw = strings.TrimSuffix(raw, "]")
	raw = strings.TrimSuffix(raw, "}}")
	raw = strings.TrimSuffix(raw, "}")
	return strings.TrimSpace(raw)
}

func trailingIncompleteCitationPayload(text string) string {
	start := -1
	state := 0
	for i, ch := range text {
		switch state {
		case 0:
			if ch == '[' {
				state = 1
				start = i
			}
		case 1:
			if ch == '^' {
				state = 2
			} else if ch == '[' {
				start = i
			} else {
				state = 0
				start = -1
			}
		case 2:
			if ch == ']' {
				state = 0
				start = -1
			}
		}
	}
	if state != 2 || start < 0 {
		return ""
	}
	return normalizeObservedCitationFragment(text[start:])
}

func hasHTTPURLPrefix(raw string) bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	return strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://")
}

func isInternalCitationFragment(raw string) bool {
	raw = normalizeObservedCitationFragment(raw)
	if raw == "" {
		return false
	}
	if decoded, err := url.QueryUnescape(raw); err == nil {
		raw = decoded
	}
	return internalCitationSuffixRe.MatchString(strings.ToLower(raw))
}

func shouldUsePendingObservedFragment(pending string, raw string) bool {
	pending = normalizeObservedCitationFragment(pending)
	raw = normalizeObservedCitationFragment(raw)
	if pending == "" {
		return false
	}
	if hasHTTPURLPrefix(pending) && isInternalCitationFragment(raw) {
		return true
	}
	return !isInternalCitationFragment(pending) && isInternalCitationFragment(raw)
}

func shouldReplacePendingCitationFragment(current string, next string) bool {
	current = normalizeObservedCitationFragment(current)
	next = normalizeObservedCitationFragment(next)
	if next == "" {
		return false
	}
	if current == "" {
		return true
	}
	if hasHTTPURLPrefix(current) && isInternalCitationFragment(next) {
		return false
	}
	return true
}

func resolveObservedCitationFragment(raw string, knownURLs []string) (string, bool) {
	raw = normalizeObservedCitationFragment(raw)
	if raw == "" {
		return "", false
	}
	if !hasHTTPURLPrefix(raw) {
		return raw, true
	}
	if match, count := matchKnownCitationURL(raw, knownURLs); match != "" {
		return match, true
	} else if count > 0 {
		return "", false
	}
	return raw, true
}

func recordObservedCitationFragments(text string, observed *[]string, pending *string) {
	if observed == nil || pending == nil {
		return
	}
	matches := citationRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) > len(*observed) {
		prevLen := len(*observed)
		pendingFragment := normalizeObservedCitationFragment(*pending)
		pendingUsed := false
		for idx := prevLen; idx < len(matches); idx++ {
			raw := normalizeObservedCitationFragment(text[matches[idx][2]:matches[idx][3]])
			candidate := raw
			if !pendingUsed && shouldUsePendingObservedFragment(pendingFragment, raw) {
				candidate = pendingFragment
				pendingUsed = true
			}
			*observed = append(*observed, normalizeObservedCitationFragment(candidate))
		}
		if len(matches) > prevLen {
			*pending = ""
		}
	}
	if trailing := trailingIncompleteCitationPayload(text); trailing != "" {
		if shouldReplacePendingCitationFragment(*pending, trailing) {
			*pending = normalizeObservedCitationFragment(trailing)
		}
	}
}

func rewriteInternalCitationsWithObserved(text string, observed []string, knownURLs []string) string {
	if text == "" || len(observed) == 0 {
		return text
	}
	matches := citationRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var out strings.Builder
	last := 0
	for idx, match := range matches {
		out.WriteString(text[last:match[0]])
		raw := text[match[2]:match[3]]
		replacement := raw
		if idx < len(observed) && observed[idx] != "" && isInternalCitationFragment(raw) {
			if resolved, ok := resolveObservedCitationFragment(observed[idx], knownURLs); ok {
				replacement = resolved
			}
		}
		out.WriteString("[^")
		out.WriteString(replacement)
		out.WriteString("]")
		last = match[1]
	}
	out.WriteString(text[last:])
	return out.String()
}

func extractSearchResultURL(resultID string) string {
	if resultID == "" {
		return ""
	}

	if strings.HasPrefix(resultID, "webpage://") {
		u, err := url.Parse(resultID)
		if err != nil {
			return ""
		}
		raw := u.Query().Get("url")
		if resolved, ok := normalizeCitationTarget(raw, nil); ok {
			return resolved
		}
		return ""
	}

	if resolved, ok := normalizeCitationTarget(resultID, nil); ok {
		return resolved
	}
	return ""
}

// cleanCitations converts Notion's [^{{URL}}] and [^URL] citation markers
// to numbered markdown references for clean terminal display.
// Input:  "伊朗局势紧张[^{{https://example.com/article}}]"
// Output: "伊朗局势紧张 [1]\n\n---\nSources:\n[1] https://example.com/article"
func cleanCitations(text string) string {
	return cleanCitationsWithCandidates(text, nil)
}

func cleanCitationsWithCandidates(text string, knownURLs []string) string {
	return cleanCitationsWithContext(text, nil, knownURLs, nil)
}

func cleanCitationsWithKnowledge(text string, knownURLs []string, candidates []CitationCandidate) string {
	return cleanCitationsWithContext(text, nil, knownURLs, candidates)
}

func cleanCitationsWithContext(text string, toolCallURLs map[string][]string, knownURLs []string, candidates []CitationCandidate) string {
	urlToNum := make(map[string]int)
	var urls []string
	matches := citationRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}

	var cleaned strings.Builder
	last := 0
	for _, match := range matches {
		start, end := match[0], match[1]
		rawStart, rawEnd := match[2], match[3]
		cleaned.WriteString(text[last:start])

		rawURL, ok := normalizeCitationTargetWithContext(
			text[rawStart:rawEnd],
			toolCallURLs,
			knownURLs,
			candidates,
			citationContextWindow(text, start, end),
		)
		if ok {
			num, exists := urlToNum[rawURL]
			if !exists {
				num = len(urls) + 1
				urlToNum[rawURL] = num
				urls = append(urls, rawURL)
			}
			cleaned.WriteString(fmt.Sprintf(" [%d]", num))
		}
		last = end
	}
	cleaned.WriteString(text[last:])

	if len(urls) == 0 {
		// If citations were present but all unresolved (e.g. toolu_*), keep
		// the cleaned text without appending a Sources section.
		cleanedText := cleaned.String()
		if cleanedText != text {
			return strings.TrimRight(cleanedText, "\n")
		}
		return text
	}

	// Append numbered reference list
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(cleaned.String(), "\n"))
	sb.WriteString("\n\n---\nSources:\n")
	for i, u := range urls {
		sb.WriteString(fmt.Sprintf("[%d] %s\n", i+1, u))
	}

	return sb.String()
}

// extractCitationURLs extracts unique URLs from Notion citation format [^{{URL}}] and [^URL].
// Used by streaming search to build a Sources section after streaming raw text.
func extractCitationURLs(text string) []string {
	return extractCitationURLsWithCandidates(text, nil)
}

func extractCitationURLsWithCandidates(text string, knownURLs []string) []string {
	matches := citationRe.FindAllStringSubmatchIndex(text, -1)
	seen := map[string]bool{}
	var urls []string
	for _, m := range matches {
		if len(m) < 4 {
			continue
		}
		rawURL, ok := normalizeCitationTargetWithContext(
			text[m[2]:m[3]],
			nil,
			knownURLs,
			nil,
			citationContextWindow(text, m[0], m[1]),
		)
		if !ok {
			continue
		}
		if !seen[rawURL] {
			seen[rawURL] = true
			urls = append(urls, rawURL)
		}
	}
	return urls
}

// IsResearcherModel returns true if the model name triggers researcher mode
func IsResearcherModel(model string) bool {
	return model == "researcher" || model == "fast-researcher"
}

// StripAskModeSuffix strips a trailing "-ask" mode suffix from the model
// name and returns (stripped, askEnabled). The suffix is matched
// case-insensitively. Callers should run this BEFORE ResolveModel so the
// fuzzy keyword matcher (opus/sonnet/haiku) sees the canonical name.
//
// Examples:
//
//	"claude-sonnet-4-6-ask"          -> ("claude-sonnet-4-6", true)
//	"sonnet-4.6-ask"                 -> ("sonnet-4.6", true)
//	"claude-sonnet-4-6-20250929-ask" -> ("claude-sonnet-4-6-20250929", true)
//	"claude-sonnet-4-6"              -> ("claude-sonnet-4-6", false)
func StripAskModeSuffix(model string) (string, bool) {
	const suffix = "-ask"
	if len(model) > len(suffix) && strings.EqualFold(model[len(model)-len(suffix):], suffix) {
		return model[:len(model)-len(suffix)], true
	}
	return model, false
}

func normalizeReasoningEffort(value string) (string, error) {
	effort := strings.ToLower(strings.TrimSpace(value))
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "max", "xhigh":
		return effort, nil
	default:
		return "", fmt.Errorf("unsupported reasoning effort %q", value)
	}
}

func uploadedAttachmentThreadID(attachments []UploadedAttachment) (string, error) {
	var threadID string
	for i, attachment := range attachments {
		candidate := strings.TrimSpace(attachment.SessionID)
		if candidate == "" {
			return "", fmt.Errorf("attachment %d is missing its upload thread ID", i+1)
		}
		if threadID == "" {
			threadID = candidate
			continue
		}
		if candidate != threadID {
			return "", fmt.Errorf("attachment %d upload thread %q does not match %q", i+1, candidate, threadID)
		}
	}
	return threadID, nil
}

func resolveFirstTurnInferenceThread(session *Session, attachmentThreadID string) (string, bool, error) {
	if session != nil {
		if attachmentThreadID != "" && attachmentThreadID != session.ThreadID {
			return "", false, fmt.Errorf("attachment upload thread %q does not match session thread %q", attachmentThreadID, session.ThreadID)
		}
		return session.ThreadID, attachmentThreadID == "", nil
	}
	if attachmentThreadID != "" {
		return attachmentThreadID, false, nil
	}
	return generateUUIDv4(), true, nil
}

func inferenceCreatedSource(hasAttachments bool) string {
	if hasAttachments {
		return "workflows"
	}
	return "ai_module"
}

// CallInference sends a request to Notion's runInferenceTranscript API
// and streams the response via callback.
// When opt.Session is non-nil with TurnCount > 0, it sends a partial transcript (subsequent turn).
// When opt.Session is non-nil with TurnCount == 0, it sends a full transcript (first turn).
// When opt.Session is nil, it falls back to the legacy single-turn behavior.
func CallInference(acc *Account, messages []ChatMessage, model string, disableBuiltinTools bool, cb StreamCallback, opts ...CallOptions) error {
	var opt CallOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	requestID := opt.RequestID
	reasoningEffort, err := normalizeReasoningEffort(opt.ReasoningEffort)
	if err != nil {
		return err
	}
	opt.ReasoningEffort = reasoningEffort

	// Per-request "-ask" suffix override. Defensive: most callers strip
	// this in their own pipeline (anthropic.go does), but we re-check here
	// so any code path that forwards the raw model name still picks up
	// ASK mode without an extra round-trip.
	if stripped, ask := StripAskModeSuffix(model); ask {
		if model != stripped {
			log.Printf("[ask-mode] model %q stripped → %q (useReadOnlyMode=true)", model, stripped)
		}
		model = stripped
		opt.UseReadOnlyMode = true
	}

	isResearcher := opt.IsResearcher || IsResearcherModel(model)

	if isResearcher {
		return callResearcherInference(acc, messages, cb, &opt)
	}

	notionModel := ResolveModel(model)
	if notionModel != model {
		log.Printf("[model] resolved %q → %q", model, notionModel)
	}

	enableWebSearch := opt.EnableWebSearch
	attachments := opt.Attachments
	session := opt.Session
	attachmentThreadID, err := uploadedAttachmentThreadID(attachments)
	if err != nil {
		return err
	}

	var reqBody NotionInferenceRequest
	createdSource := inferenceCreatedSource(attachmentThreadID != "")

	if session != nil && session.TurnCount > 0 {
		// ── Subsequent turn: partial transcript ──
		if attachmentThreadID != "" && attachmentThreadID != session.ThreadID {
			return fmt.Errorf("attachment upload thread %q does not match session thread %q", attachmentThreadID, session.ThreadID)
		}
		newUserContent := extractLastUserMessage(messages)
		if newUserContent == "" {
			newUserContent = "continue"
		}
		transcript := buildPartialTranscript(acc, newUserContent, notionModel, opt.ReasoningEffort, disableBuiltinTools, enableWebSearch, opt.EnableWorkspaceSearch, opt.UseReadOnlyMode, attachments, session)

		reqBody = NotionInferenceRequest{
			TraceID:                 generateUUIDv4(),
			SpaceID:                 acc.SpaceID,
			ThreadID:                session.ThreadID,
			Transcript:              transcript,
			CreateThread:            false,
			IsPartialTranscript:     true,
			GenerateTitle:           false,
			SaveAllThreadOperations: true,
			ThreadType:              "workflow",
		}
		log.Printf("[session] subsequent turn %d on thread %s (updated-configs=%d)",
			session.TurnCount+1, session.ThreadID, len(session.UpdatedConfigIDs))
	} else {
		// ── First turn (or legacy single-turn): full transcript ──
		var configID, contextID, contextPageID, now string
		if session != nil {
			// Pre-created session from HandleAnthropicMessages
			configID = session.ConfigID
			contextID = session.ContextID
			contextPageID = ensureSessionContextPageID(session)
			now = session.OriginalDatetime
		} else {
			// Legacy single-turn fallback (e.g. OpenAI-compatible handler)
			configID = generateUUIDv4()
			contextID = generateUUIDv4()
			contextPageID = generateUUIDv4()
			now = time.Now().Format(time.RFC3339Nano)
		}
		transcript := buildFullTranscript(acc, messages, notionModel, opt.ReasoningEffort, disableBuiltinTools, enableWebSearch, opt.EnableWorkspaceSearch, opt.UseReadOnlyMode, attachments, configID, contextID, contextPageID, now)

		// When attachments are present, reuse the upload thread instead of creating a new one.
		threadID, createThread, err := resolveFirstTurnInferenceThread(session, attachmentThreadID)
		if err != nil {
			return err
		}
		if !createThread {
			log.Printf("[upload] using upload thread %s for inference", threadID)
		}

		reqBody = NotionInferenceRequest{
			TraceID:                 generateUUIDv4(),
			SpaceID:                 acc.SpaceID,
			ThreadID:                threadID,
			Transcript:              transcript,
			CreateThread:            createThread,
			IsPartialTranscript:     false,
			GenerateTitle:           true,
			SaveAllThreadOperations: true,
			ThreadType:              "workflow",
		}

		if createThread {
			reqBody.ThreadParentPointer = &ThreadParentPointer{
				Table:   "space",
				ID:      acc.SpaceID,
				SpaceID: acc.SpaceID,
			}
		}

		log.Printf("[session] first turn, thread %s (session=%v)", threadID, session != nil)
	}
	applyWorkflowRequestProtocol(&reqBody, createdSource)

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	LogNotionRequestJSON(requestID, fmt.Sprintf("POST /runInferenceTranscript account=%s model=%s", acc.UserEmail, notionModel), reqBody)

	req, err := http.NewRequest("POST", NotionAPIBase+"/runInferenceTranscript", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	setNotionHeaders(req, acc)

	client := getChromeHTTPClient(AppConfig.InferenceTimeoutDuration())
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	LogNotionResponseJSON(requestID, "POST /runInferenceTranscript response meta", map[string]interface{}{
		"status":           resp.StatusCode,
		"content_type":     resp.Header.Get("Content-Type"),
		"content_encoding": resp.Header.Get("Content-Encoding"),
	})

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		LogNotionResponseText(requestID, "POST /runInferenceTranscript error body", string(body))
		return fmt.Errorf("notion API error %d: %s", resp.StatusCode, string(body[:min(len(body), 500)]))
	}

	// Decompress
	reader, cleanup, err := decompressBody(resp)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Parse NDJSON
	return parseNDJSONStream(reader, requestID, cb, opt.NativeToolUses, opt.ThinkingBlocks, opt.ThinkingCallback, opt.KnownCitationURLs, opt.KnownCitationDocs, opt.KnownToolCallURLs)
}

func applyWorkflowRequestProtocol(reqBody *NotionInferenceRequest, createdSource string) {
	reqBody.SetUnreadState = true
	reqBody.CreatedSource = createdSource
	reqBody.AsPatchResponse = true
	reqBody.PatchResponseVersion = 2
	reqBody.IsUserInAnySalesAssistedSpace = boolPtr(false)
	reqBody.IsSpaceSalesAssisted = boolPtr(false)
	reqBody.SupportsCustomAgentNudgeTranscriptStep = boolPtr(true)
	reqBody.DebugOverrides = DebugOverrides{
		EmitAgentSearchExtractedResults: true,
		CachedInferences:                &struct{}{},
		AnnotationInferences:            &struct{}{},
		EmitInferences:                  boolPtr(false),
	}
}

// buildConfigValue constructs the Notion config value map used in transcript config entries.
// enableWorkspaceSearch: nil = use AppConfig default, non-nil = per-request override
// useReadOnlyMode: when true, sets Notion's ASK-mode flag — model answers
// the prompt but skips page edits. Mirrors the frontend's "Ask" mode toggle.
func buildConfigValue(notionModel string, reasoningEffort string, disableBuiltinTools bool, enableWebSearch bool, enableWorkspaceSearch *bool, useReadOnlyMode bool, hasAttachments bool, isSubsequentTurn bool) map[string]interface{} {
	effectiveDisable := disableBuiltinTools

	// Resolve workspace search: per-request override > config default
	wsSearch := AppConfig.WorkspaceSearchEnabled()
	if enableWorkspaceSearch != nil {
		wsSearch = *enableWorkspaceSearch
	}

	// When workspace search is enabled, agent integrations must be on
	// (they control the built-in search tool availability)
	agentEnabled := !effectiveDisable || wsSearch

	configValue := map[string]interface{}{
		"type":                                           "workflow",
		"enableAgentAutomations":                         agentEnabled,
		"enableAgentIntegrations":                        agentEnabled,
		"enableCustomAgents":                             !effectiveDisable,
		"enableExperimentalIntegrations":                 false,
		"enableScriptAgent":                              !effectiveDisable,
		"enableScriptAgentAdvanced":                      false,
		"enableScriptAgentSearchConnectorsInCustomAgent": false,
		"enableScriptAgentGoogleDriveInCustomAgent":      false,
		"enableScriptAgentGoogleDriveOAuthInCustomAgent": false,
		"enableScriptAgentSlack":                         !effectiveDisable,
		"enableScriptAgentMcpServers":                    !effectiveDisable,
		"enableAgentDiffs":                               !effectiveDisable,
		"enableCsvAttachmentSupport":                     true,
		"showDatabaseAgentsDiscoverability":              true,
		"enableAgentThreadTools":                         false,
		"enableCrdtOperations":                           false,
		"enableAgentCardCustomization":                   true,
		"enableSystemPromptAsPage":                       false,
		"enableUserSessionContext":                       false,
		"enableLargeToolResultComputerOffload":           false,
		"enableScriptAgentGtm":                           false,
		"enablePitCrewTableViewTool":                     false,
		"enableComputer":                                 !effectiveDisable,
		"enableCustomAgentCreateGuidanceV2":              true,
		"enableSoftwareFactoryPage":                      false,
		"enableAgentGenerateImage":                       !effectiveDisable,
		"enableQueryCalendar":                            false,
		"enableQueryMail":                                false,
		"enableMailExplicitToolCalls":                    true,
		"enableMailNotificationPreferences":              false,
		"enableMailAgentMultiProviderSupport":            true,
		"enableNotionMailDeprecated":                     false,
		"enableWebResearch":                              false,
		"useRulePrioritization":                          true,
		"useWebSearch":                                   enableWebSearch,
		"isHipaa":                                        false,
		"internetAccess":                                 false,
		"manageWorkers":                                  false,
		"useReadOnlyMode":                                useReadOnlyMode,
		"writerMode":                                     false,
		"model":                                          notionModel,
		"modelFromUser":                                  true,
		"isCustomAgent":                                  false,
		"isCustomAgentBuilder":                           false,
		"isCustomAgentCreate":                            false,
		"isAgentResearchRequest":                         false,
		"useCustomAgentDraft":                            false,
		"enableMarkdownVNext":                            false,
		"enableAgentSkillsV2":                            false,
		"updatePageStaleViewGuardEnabled":                false,
		"enableUpdatePageOrderUpdates":                   true,
		"enableAgentSupportPropertyReorder":              true,
		"enableAgentAskSurvey":                           true,
		"databaseAgentConfigMode":                        false,
		"isOnboardingAgent":                              false,
		"isMobile":                                       false,
	}
	if reasoningEffort != "" {
		configValue["reasoningEffort"] = reasoningEffort
	}

	// searchScopes controls what the built-in search tool can access
	if wsSearch || enableWebSearch {
		configValue["searchScopes"] = []map[string]string{{"type": "everything"}}
	}

	if hasAttachments {
		configValue["enableCsvAttachmentSupport"] = true
	}

	if isSubsequentTurn {
		configValue["useContextualCoreDocsAutoLoad"] = false
		configValue["useDocPreviewsForCoreAutoLoad"] = true
	} else {
		configValue["availableConnectors"] = []interface{}{}
	}

	return configValue
}

// buildContextValue constructs the Notion context value map used in transcript context entries.
func buildContextValue(acc *Account, datetime, contextPageID string) map[string]interface{} {
	return map[string]interface{}{
		"timezone":        acc.Timezone,
		"userName":        acc.UserName,
		"userId":          acc.UserID,
		"userEmail":       acc.UserEmail,
		"spaceName":       acc.SpaceName,
		"spaceId":         acc.SpaceID,
		"spaceViewId":     acc.SpaceViewID,
		"currentDatetime": datetime,
		"surface":         "ai_module",
		"agentAccessory":  "paprika",
		"context_page_id": contextPageID,
	}
}

// buildFullTranscript builds a complete transcript for the first turn of a conversation.
// Uses ResearcherTranscriptMsg (with id field) to match Notion's real client format.
func buildFullTranscript(acc *Account, messages []ChatMessage, notionModel string, reasoningEffort string, disableBuiltinTools bool, enableWebSearch bool, enableWorkspaceSearch *bool, useReadOnlyMode bool, attachments []UploadedAttachment, configID, contextID, contextPageID, now string) []interface{} {
	hasAttachments := len(attachments) > 0
	configValue := buildConfigValue(notionModel, reasoningEffort, disableBuiltinTools, enableWebSearch, enableWorkspaceSearch, useReadOnlyMode, hasAttachments, false)
	contextValue := buildContextValue(acc, now, contextPageID)

	if hasAttachments {
		contextValue["surface"] = "workflows"
	}

	transcript := []interface{}{
		ResearcherTranscriptMsg{
			ID:    configID,
			Type:  "config",
			Value: configValue,
		},
		ResearcherTranscriptMsg{
			ID:    contextID,
			Type:  "context",
			Value: contextValue,
		},
	}

	// Insert attachment entries before user messages (matches Notion web behavior).
	for _, att := range attachments {
		transcript = append(transcript, BuildAttachmentTranscript(&att))
	}

	// Convert OpenAI messages to Notion transcript format
	// System messages → prepend to first user message
	// User messages → "user" type with id, userId, createdAt
	// Assistant messages → "assistant-reply" type (only for first-turn full transcript)
	var systemPrompt string
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			systemPrompt += msg.Content + "\n"
		case "user":
			content := msg.Content
			if systemPrompt != "" {
				content = systemPrompt + "\n" + content
				systemPrompt = ""
			}
			transcript = append(transcript, ResearcherTranscriptMsg{
				ID:        generateUUIDv4(),
				Type:      "user",
				Value:     [][]string{{content}},
				UserID:    acc.UserID,
				CreatedAt: now,
			})
		case "assistant":
			transcript = append(transcript, TranscriptMsg{
				Type: "assistant-reply",
				Value: []map[string]interface{}{
					{
						"type": "agent-inference",
						"value": []map[string]string{
							{"type": "text", "content": msg.Content},
						},
					},
				},
			})
		}
	}

	// If only system message, add an empty user message
	if systemPrompt != "" {
		transcript = append(transcript, ResearcherTranscriptMsg{
			ID:        generateUUIDv4(),
			Type:      "user",
			Value:     [][]string{{systemPrompt}},
			UserID:    acc.UserID,
			CreatedAt: now,
		})
	}

	return transcript
}

// buildPartialTranscript builds an incremental transcript for subsequent turns.
// It includes: config + context (reused IDs) + N updated-config placeholders + attachments + new user message.
func buildPartialTranscript(acc *Account, newUserContent string, notionModel string, reasoningEffort string, disableBuiltinTools bool, enableWebSearch bool, enableWorkspaceSearch *bool, useReadOnlyMode bool, attachments []UploadedAttachment, session *Session) []interface{} {
	configValue := buildConfigValue(notionModel, reasoningEffort, disableBuiltinTools, enableWebSearch, enableWorkspaceSearch, useReadOnlyMode, len(attachments) > 0, true)
	contextPageID := ensureSessionContextPageID(session)
	contextValue := buildContextValue(acc, session.OriginalDatetime, contextPageID)
	currentDatetime := time.Now().Format(time.RFC3339Nano)
	currentContextValue := buildContextValue(acc, currentDatetime, contextPageID)
	currentContextValue["surface"] = "full_page_chat"

	transcript := []interface{}{
		ResearcherTranscriptMsg{
			ID:    session.ConfigID,
			Type:  "config",
			Value: configValue,
		},
		ResearcherTranscriptMsg{
			ID:    session.ContextID,
			Type:  "context",
			Value: contextValue,
		},
		ResearcherTranscriptMsg{
			ID:    generateUUIDv4(),
			Type:  "context",
			Value: currentContextValue,
		},
	}

	// Add updated-config placeholders for each previous turn
	for _, ucID := range session.UpdatedConfigIDs {
		transcript = append(transcript, UpdatedConfigMsg{
			ID:   ucID,
			Type: "updated-config",
		})
	}
	for i := range attachments {
		transcript = append(transcript, BuildAttachmentTranscript(&attachments[i]))
	}

	// Add the new user message
	transcript = append(transcript, ResearcherTranscriptMsg{
		ID:        generateUUIDv4(),
		Type:      "user",
		Value:     [][]string{{newUserContent}},
		UserID:    acc.UserID,
		CreatedAt: currentDatetime,
	})

	return transcript
}

func ensureSessionContextPageID(session *Session) string {
	if session.ContextPageID == "" {
		session.ContextPageID = generateUUIDv4()
	}
	return session.ContextPageID
}

func setNotionHeaders(req *http.Request, acc *Account) {
	// Content negotiation
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")

	// Chrome Client Hints (sec-ch-ua) — critical for browser fingerprint
	req.Header.Set("sec-ch-ua", AppConfig.Browser.SecChUA)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", AppConfig.Browser.SecChUAPlatform)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	// Notion-specific headers
	req.Header.Set("x-notion-active-user-header", acc.UserID)
	req.Header.Set("x-notion-space-id", acc.SpaceID)
	req.Header.Set("notion-client-version", acc.ClientVersion)
	req.Header.Set("notion-audit-log-platform", "web")

	// Browser identity
	req.Header.Set("User-Agent", AppConfig.Browser.UserAgent)
	req.Header.Set("Origin", "https://www.notion.so")
	req.Header.Set("Referer", "https://www.notion.so/"+acc.SpaceID)

	// Cookies — use full_cookie if available, otherwise build minimal set
	if acc.FullCookie != "" {
		req.Header.Set("Cookie", acc.FullCookie)
	} else {
		browserID := acc.BrowserID
		if browserID == "" {
			browserID = generateUUIDv4()
		}
		deviceID := acc.DeviceID
		if deviceID == "" {
			deviceID = generateUUIDv4()
		}
		userIDNoDash := strings.ReplaceAll(acc.UserID, "-", "")
		cookie := fmt.Sprintf(
			"notion_browser_id=%s; device_id=%s; notion_user_id=%s; notion_locale=en-US/legacy; notion_users=[%%22%s%%22]; notion_check_cookie_consent=false; notion_cookie_sync_completed=%%7B%%22completed%%22%%3Atrue%%2C%%22version%%22%%3A4%%7D; _cioid=%s; token_v2=%s",
			browserID, deviceID, acc.UserID, acc.UserID, userIDNoDash, acc.TokenV2,
		)
		req.Header.Set("Cookie", cookie)
	}
}

// FetchModels calls Notion's getAvailableModels API to get the current model list
func FetchModels(acc *Account) ([]ModelEntry, error) {
	body, _ := json.Marshal(map[string]string{"spaceId": acc.SpaceID})
	req, err := http.NewRequest("POST", NotionAPIBase+"/getAvailableModels", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	setNotionHeadersJSON(req, acc)

	client := getChromeHTTPClient(AppConfig.APITimeoutDuration())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 300)]))
	}

	var result struct {
		Models []struct {
			Model        string `json:"model"`
			ModelMessage string `json:"modelMessage"`
			ModelFamily  string `json:"modelFamily"`
			IsDisabled   bool   `json:"isDisabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	var models []ModelEntry
	for _, m := range result.Models {
		if !m.IsDisabled {
			models = append(models, ModelEntry{ID: m.Model, Name: m.ModelMessage})
		}
	}
	return models, nil
}

// setNotionHeadersJSON sets Notion API headers with Accept: application/json (for non-streaming APIs)
func setNotionHeadersJSON(req *http.Request, acc *Account) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("sec-ch-ua", AppConfig.Browser.SecChUA)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", AppConfig.Browser.SecChUAPlatform)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("x-notion-active-user-header", acc.UserID)
	req.Header.Set("x-notion-space-id", acc.SpaceID)
	req.Header.Set("notion-client-version", acc.ClientVersion)
	req.Header.Set("User-Agent", AppConfig.Browser.UserAgent)
	req.Header.Set("Origin", "https://www.notion.so")

	if acc.FullCookie != "" {
		req.Header.Set("Cookie", acc.FullCookie)
	} else {
		browserID := acc.BrowserID
		if browserID == "" {
			browserID = generateUUIDv4()
		}
		deviceID := acc.DeviceID
		if deviceID == "" {
			deviceID = generateUUIDv4()
		}
		cookie := fmt.Sprintf(
			"notion_browser_id=%s; device_id=%s; notion_user_id=%s; notion_locale=en-US/legacy; notion_users=[%%22%s%%22]; token_v2=%s",
			browserID, deviceID, acc.UserID, acc.UserID, acc.TokenV2,
		)
		req.Header.Set("Cookie", cookie)
	}
}

// CheckQuota calls both V1 and V2 Notion quota APIs:
//   - V1 (getAIUsageEligibility): isEligible, basic credits, researchModeUsage
//   - V2 (getAIUsageEligibilityV2): premium credits (monthlyAllocated, etc.)
func CheckQuota(acc *Account) (*QuotaInfo, error) {
	body, _ := json.Marshal(map[string]string{"spaceId": acc.SpaceID})
	client := getChromeHTTPClient(AppConfig.APITimeoutDuration())

	// --- V1: get isEligible + researchModeUsage ---
	reqV1, err := http.NewRequest("POST", NotionAPIBase+"/getAIUsageEligibility", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create V1 request: %w", err)
	}
	setNotionHeadersJSON(reqV1, acc)

	respV1, err := client.Do(reqV1)
	if err != nil {
		return nil, fmt.Errorf("V1 request: %w", err)
	}
	defer respV1.Body.Close()
	bodyV1, _ := io.ReadAll(respV1.Body)
	if respV1.StatusCode != 200 {
		return nil, fmt.Errorf("V1 API error %d: %s", respV1.StatusCode, string(bodyV1[:min(len(bodyV1), 300)]))
	}
	var v1 quotaV1Response
	if err := json.Unmarshal(bodyV1, &v1); err != nil {
		return nil, fmt.Errorf("parse V1 response: %w", err)
	}

	// --- V2: get premium credits ---
	body2, _ := json.Marshal(map[string]string{"spaceId": acc.SpaceID})
	reqV2, err := http.NewRequest("POST", NotionAPIBase+"/getAIUsageEligibilityV2", bytes.NewReader(body2))
	if err != nil {
		return nil, fmt.Errorf("create V2 request: %w", err)
	}
	setNotionHeadersJSON(reqV2, acc)

	respV2, err := client.Do(reqV2)
	if err != nil {
		return nil, fmt.Errorf("V2 request: %w", err)
	}
	defer respV2.Body.Close()
	bodyV2, _ := io.ReadAll(respV2.Body)
	if respV2.StatusCode != 200 {
		return nil, fmt.Errorf("V2 API error %d: %s", respV2.StatusCode, string(bodyV2[:min(len(bodyV2), 300)]))
	}
	var v2 quotaV2Response
	if err := json.Unmarshal(bodyV2, &v2); err != nil {
		return nil, fmt.Errorf("parse V2 response: %w", err)
	}

	monthlyUsage := v2.PremiumCredits.PerSource.MonthlyAllocated.UsageTotal
	monthlyLimit := v2.PremiumCredits.PerSource.MonthlyAllocated.Limit
	premiumRemaining := v2.PremiumCredits.TotalCreditBalance
	// Some workspaces report remaining premium via monthlyAllocated usage/limit,
	// while totalCreditBalance can stay at 0.
	if premiumRemaining <= 0 && monthlyLimit > monthlyUsage {
		premiumRemaining = monthlyLimit - monthlyUsage
	}

	// Merge V1 + V2 into QuotaInfo
	info := &QuotaInfo{
		IsEligible:        v1.IsEligible,
		SpaceUsage:        v1.SpaceUsage,
		SpaceLimit:        v1.SpaceLimit,
		UserUsage:         v1.UserUsage,
		UserLimit:         v1.UserLimit,
		LastUsageAtMs:     v1.LastSpaceUsageAtMs,
		ResearchModeUsage: v1.ResearchModeUsage,
		// Premium credits from V2
		PremiumBalance: premiumRemaining,
		PremiumUsage:   monthlyUsage,
		PremiumLimit:   monthlyLimit,
	}
	info.HasPremium = info.PremiumLimit > 0 ||
		v2.PremiumCredits.PerSource.MonthlyCommitted.Limit > 0 ||
		v2.PremiumCredits.PerSource.YearlyElastic.Limit > 0 ||
		info.PremiumBalance > 0

	return info, nil
}

// CheckUserWorkspace probes /api/v3/loadUserContent and returns the number
// of accessible workspaces (`user_root.space_views` length) for this
// account.
//
// Background: when an account's Notion onboarding never completed (or the
// workspace was reaped), Notion still returns a user_root record but with
// no `space_views` field. The /ai SPA then loops on a skeleton screen
// because `someSpace.getSpaceId()` runs against `undefined`. Probing this
// up-front lets the pool blacklist such accounts so the dashboard's
// "best account" selector and the per-email click never lands on them.
//
// 0 with err==nil is a real, sticky signal — not a transient. Callers
// should persist the result so a restart doesn't have to re-probe every
// account.
func CheckUserWorkspace(acc *Account) (int, error) {
	body := []byte(`{}`)
	req, err := http.NewRequest("POST", NotionAPIBase+"/loadUserContent", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	setNotionHeadersJSON(req, acc)

	client := getChromeHTTPClient(AppConfig.APITimeoutDuration())
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("API error %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 300)]))
	}

	// We only care about user_root.{userId}.value.value.space_views; using
	// json.RawMessage keeps us tolerant to schema drift on every other
	// field.
	var parsed struct {
		RecordMap struct {
			UserRoot map[string]struct {
				Value struct {
					Value struct {
						SpaceViews []string `json:"space_views"`
					} `json:"value"`
				} `json:"value"`
			} `json:"user_root"`
		} `json:"recordMap"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("parse response: %w", err)
	}
	// Prefer the user_root keyed by this account's UserID. If Notion ever
	// switches to a different keying we still fall back to "any non-empty
	// space_views in the response" so a transient mismatch doesn't false-
	// positive an otherwise healthy account.
	if entry, ok := parsed.RecordMap.UserRoot[acc.UserID]; ok {
		return len(entry.Value.Value.SpaceViews), nil
	}
	for _, entry := range parsed.RecordMap.UserRoot {
		if n := len(entry.Value.Value.SpaceViews); n > 0 {
			return n, nil
		}
	}
	return 0, nil
}

func decompressBody(resp *http.Response) (io.Reader, func(), error) {
	enc := resp.Header.Get("Content-Encoding")
	switch enc {
	case "zstd":
		dec, err := zstd.NewReader(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("zstd decoder: %w", err)
		}
		return dec, func() { dec.Close() }, nil
	case "gzip":
		dec, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("gzip decoder: %w", err)
		}
		return dec, func() { dec.Close() }, nil
	case "br":
		return brotli.NewReader(resp.Body), nil, nil
	default:
		return resp.Body, nil, nil
	}
}

type notionResponseLogDeduper struct {
	requestID string
	label     string
	emitted   map[string]bool

	agentEntryContent         map[string]string
	agentEntrySignature       map[string]string
	agentToolUseIDs           map[string]bool
	agentFinishedAt           map[string]int64
	agentHasFinishedAt        map[string]bool
	agentInputTokens          map[string]int
	agentHasInputTokens       map[string]bool
	agentOutputTokens         map[string]int
	agentHasOutputTokens      map[string]bool
	agentModelByID            map[string]string
	researcherValueByID       map[string]string
	researcherOutputContent   map[string]string
	researcherRawOutput       map[string]string
	researcherRawSignature    map[string]string
	researcherRawProvider     map[string]string
	researcherRawModelName    map[string]string
	researcherReportValueByID map[string]string
}

func newNotionResponseLogDeduper(requestID, label string) *notionResponseLogDeduper {
	return &notionResponseLogDeduper{
		requestID:                 requestID,
		label:                     label,
		emitted:                   make(map[string]bool),
		agentEntryContent:         make(map[string]string),
		agentEntrySignature:       make(map[string]string),
		agentToolUseIDs:           make(map[string]bool),
		agentFinishedAt:           make(map[string]int64),
		agentHasFinishedAt:        make(map[string]bool),
		agentInputTokens:          make(map[string]int),
		agentHasInputTokens:       make(map[string]bool),
		agentOutputTokens:         make(map[string]int),
		agentHasOutputTokens:      make(map[string]bool),
		agentModelByID:            make(map[string]string),
		researcherValueByID:       make(map[string]string),
		researcherOutputContent:   make(map[string]string),
		researcherRawOutput:       make(map[string]string),
		researcherRawSignature:    make(map[string]string),
		researcherRawProvider:     make(map[string]string),
		researcherRawModelName:    make(map[string]string),
		researcherReportValueByID: make(map[string]string),
	}
}

func (d *notionResponseLogDeduper) LogLine(line string) {
	if !NotionResponseLoggingEnabled() {
		return
	}
	raw := d.DedupLine(strings.TrimSpace(line))
	if len(raw) == 0 {
		return
	}
	key := string(raw)
	if d.emitted[key] {
		return
	}
	d.emitted[key] = true
	LogNotionResponseJSONBytes(d.requestID, d.label, raw)
}

func (d *notionResponseLogDeduper) DedupLine(line string) []byte {
	if line == "" {
		return nil
	}

	var event NDJSONEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return []byte(line)
	}

	switch event.Type {
	case "agent-inference":
		return d.dedupAgentInference(line)
	case "researcher-next-steps":
		return d.dedupResearcherNextSteps(line)
	case "researcher-report":
		return d.dedupResearcherReport(line)
	default:
		return []byte(line)
	}
}

func (d *notionResponseLogDeduper) dedupAgentInference(line string) []byte {
	var step AgentInferenceEvent
	if err := json.Unmarshal([]byte(line), &step); err != nil {
		return []byte(line)
	}

	payload := map[string]interface{}{
		"type": "agent-inference",
	}
	changed := false
	if step.ID != "" {
		payload["id"] = step.ID
	}

	values := make([]map[string]interface{}, 0, len(step.Value))
	for idx, entry := range step.Value {
		entryKey := fmt.Sprintf("%s/value/%d/%s", step.ID, idx, entry.Type)
		switch entry.Type {
		case "thinking", "text":
			delta, changed := dedupLogContent(d.agentEntryContent[entryKey], entry.Content)
			d.agentEntryContent[entryKey] = entry.Content

			entryPayload := map[string]interface{}{
				"type": entry.Type,
			}
			if changed {
				entryPayload["content"] = delta
			}
			if entry.Signature != "" && d.agentEntrySignature[entryKey] != entry.Signature {
				entryPayload["signature"] = entry.Signature
				d.agentEntrySignature[entryKey] = entry.Signature
				changed = true
			}
			if changed {
				values = append(values, entryPayload)
			}
		case "tool_use":
			if entry.ID != "" {
				if d.agentToolUseIDs[entry.ID] {
					continue
				}
				d.agentToolUseIDs[entry.ID] = true
			}
			entryPayload := map[string]interface{}{
				"type": entry.Type,
			}
			if entry.ID != "" {
				entryPayload["id"] = entry.ID
			}
			if entry.Name != "" {
				entryPayload["name"] = entry.Name
			}
			if len(entry.Input) > 0 {
				entryPayload["input"] = json.RawMessage(entry.Input)
			}
			values = append(values, entryPayload)
		default:
			if entry.Type == "" {
				continue
			}
			entryPayload := map[string]interface{}{
				"type": entry.Type,
			}
			if entry.Content != "" {
				entryPayload["content"] = entry.Content
			}
			values = append(values, entryPayload)
		}
	}
	if len(values) > 0 {
		payload["value"] = values
		changed = true
	}
	if step.FinishedAt != nil {
		if !d.agentHasFinishedAt[step.ID] || d.agentFinishedAt[step.ID] != *step.FinishedAt {
			payload["finishedAt"] = *step.FinishedAt
			d.agentFinishedAt[step.ID] = *step.FinishedAt
			d.agentHasFinishedAt[step.ID] = true
			changed = true
		}
	}
	if step.InputTokens != nil {
		if !d.agentHasInputTokens[step.ID] || d.agentInputTokens[step.ID] != *step.InputTokens {
			payload["inputTokens"] = *step.InputTokens
			d.agentInputTokens[step.ID] = *step.InputTokens
			d.agentHasInputTokens[step.ID] = true
			changed = true
		}
	}
	if step.OutputTokens != nil {
		if !d.agentHasOutputTokens[step.ID] || d.agentOutputTokens[step.ID] != *step.OutputTokens {
			payload["outputTokens"] = *step.OutputTokens
			d.agentOutputTokens[step.ID] = *step.OutputTokens
			d.agentHasOutputTokens[step.ID] = true
			changed = true
		}
	}
	if step.Model != nil && *step.Model != "" {
		if d.agentModelByID[step.ID] != *step.Model {
			payload["model"] = *step.Model
			d.agentModelByID[step.ID] = *step.Model
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return mustMarshalLogPayload(payload)
}

func (d *notionResponseLogDeduper) dedupResearcherNextSteps(line string) []byte {
	var steps ResearcherNextStepsEvent
	if err := json.Unmarshal([]byte(line), &steps); err != nil {
		return []byte(line)
	}

	payload := map[string]interface{}{
		"type": "researcher-next-steps",
	}
	changed := false
	if steps.ID != "" {
		payload["id"] = steps.ID
	}
	if steps.Done {
		payload["done"] = true
		changed = true
	}

	if rawValue, err := json.Marshal(steps.Value); err == nil {
		current := string(rawValue)
		if d.researcherValueByID[steps.ID] != current {
			d.researcherValueByID[steps.ID] = current
			payload["value"] = steps.Value
			changed = true
		}
	}

	output := make([]map[string]interface{}, 0, len(steps.Output))
	for idx, entry := range steps.Output {
		entryKey := fmt.Sprintf("%s/output/%d/%s", steps.ID, idx, entry.Type)
		delta, changed := dedupLogContent(d.researcherOutputContent[entryKey], entry.Content)
		d.researcherOutputContent[entryKey] = entry.Content
		if !changed {
			continue
		}
		output = append(output, map[string]interface{}{
			"type":    entry.Type,
			"content": delta,
		})
	}
	if len(output) > 0 {
		payload["output"] = output
		changed = true
	}

	rawOutput := make([]map[string]interface{}, 0, len(steps.RawOutput))
	for idx, entry := range steps.RawOutput {
		entryKey := fmt.Sprintf("%s/rawOutput/%d/%s", steps.ID, idx, entry.Type)
		delta, changed := dedupLogContent(d.researcherRawOutput[entryKey], entry.Content)
		d.researcherRawOutput[entryKey] = entry.Content

		entryPayload := map[string]interface{}{
			"type": entry.Type,
		}
		if changed {
			entryPayload["content"] = delta
		}
		if entry.Signature != "" && d.researcherRawSignature[entryKey] != entry.Signature {
			entryPayload["signature"] = entry.Signature
			d.researcherRawSignature[entryKey] = entry.Signature
			changed = true
		}
		if entry.ModelProvider != "" && d.researcherRawProvider[entryKey] != entry.ModelProvider {
			entryPayload["modelProvider"] = entry.ModelProvider
			d.researcherRawProvider[entryKey] = entry.ModelProvider
			changed = true
		}
		if entry.NotionModelName != "" && d.researcherRawModelName[entryKey] != entry.NotionModelName {
			entryPayload["notionModelName"] = entry.NotionModelName
			d.researcherRawModelName[entryKey] = entry.NotionModelName
			changed = true
		}
		if changed {
			rawOutput = append(rawOutput, entryPayload)
		}
	}
	if len(rawOutput) > 0 {
		payload["rawOutput"] = rawOutput
		changed = true
	}

	if !changed {
		return nil
	}
	return mustMarshalLogPayload(payload)
}

func (d *notionResponseLogDeduper) dedupResearcherReport(line string) []byte {
	var report ResearcherReportEvent
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		return []byte(line)
	}

	reportKey := report.ID
	if reportKey == "" {
		reportKey = "report"
	}
	delta, changed := dedupLogContent(d.researcherReportValueByID[reportKey], report.Value)
	d.researcherReportValueByID[reportKey] = report.Value
	if !changed {
		return nil
	}

	payload := map[string]interface{}{
		"type":  "researcher-report",
		"value": delta,
	}
	if report.ID != "" {
		payload["id"] = report.ID
	}
	return mustMarshalLogPayload(payload)
}

func dedupLogContent(prev, next string) (string, bool) {
	if next == prev {
		return "", false
	}
	if prev != "" && strings.HasPrefix(next, prev) {
		return next[len(prev):], true
	}
	if next == "" {
		return "", false
	}
	return next, true
}

func mustMarshalLogPayload(payload map[string]interface{}) []byte {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return raw
}

func appendKnownCitationURLs(dst *[]string, urls []string) {
	if dst == nil || len(urls) == 0 {
		return
	}
	seen := make(map[string]bool, len(*dst))
	for _, existing := range *dst {
		seen[existing] = true
	}
	for _, u := range urls {
		if u == "" || seen[u] {
			continue
		}
		*dst = append(*dst, u)
		seen[u] = true
	}
}

func appendKnownCitationDocs(dst *[]CitationCandidate, candidates []CitationCandidate) {
	if dst == nil || len(candidates) == 0 {
		return
	}
	merged := mergeCitationCandidates(citationCandidateURLs(*dst), append(append([]CitationCandidate{}, *dst...), candidates...))
	*dst = merged
}

func collectSearchResultCitationDocs(evt searchToolResultEvent) []CitationCandidate {
	byURL := make(map[string]CitationCandidate)
	order := make([]string, 0)

	add := func(rawURL, id, title, text string) {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL == "" {
			rawURL = extractSearchResultURL(id)
		} else if resolved, ok := normalizeCitationTarget(rawURL, nil); ok {
			rawURL = resolved
		} else {
			rawURL = ""
		}
		if rawURL == "" {
			return
		}

		candidate := byURL[rawURL]
		if candidate.URL == "" {
			candidate.URL = rawURL
			order = append(order, rawURL)
		}
		if candidate.Title == "" && title != "" {
			candidate.Title = title
		}
		if candidate.Text == "" && text != "" {
			candidate.Text = text
		}
		byURL[rawURL] = candidate
	}

	for _, result := range evt.Result.StructuredContent.Results {
		add(result.URL, result.ID, result.Title, "")
	}
	for _, result := range evt.Result.ExtractedResults {
		add("", result.ID, result.Title, "")
	}
	for _, group := range evt.AdditionalResultForRender.WebResults {
		for _, result := range group {
			add("", result.ID, result.Title, result.Text)
		}
	}

	docs := make([]CitationCandidate, 0, len(order))
	for _, rawURL := range order {
		docs = append(docs, byURL[rawURL])
	}
	return docs
}

func parseNDJSONStream(reader io.Reader, requestID string, cb StreamCallback, nativeToolUses *[]AgentValueEntry, thinkingBlocks *[]ThinkingBlock, thinkingCb ThinkingDeltaCallback, knownCitationURLs *[]string, knownCitationDocs *[]CitationCandidate, knownToolCallURLs *map[string][]string) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	responseLogger := newNotionResponseLogDeduper(requestID, "runInferenceTranscript ndjson deduped")

	var rawText string   // raw accumulated text from Notion
	var sentClean string // cleaned text already emitted to callback
	lineCount := 0
	eventTypeCounts := make(map[string]int)
	emittedThinkingChars := 0

	// Accumulated usage across multi-turn inference (e.g., web search creates 2 turns)
	var totalUsage UsageInfo

	// Track which patch value indices are thinking vs text vs tool_use
	// Key: "/s/N/value/M" → entry type ("thinking", "text", "tool_use")
	patchValueTypes := make(map[string]string)
	// Counter: "/s/N" → how many value entries added so far
	patchValueCounts := make(map[string]int)
	patchNextStepIndex := 0
	// Accumulated thinking content from patch operations
	var patchThinkingContent string
	var patchThinkingSignature string

	// Track cumulative thinking per inference step ID (agent-inference events are cumulative:
	// each event contains the FULL content so far, not just the delta)
	type agentThinkingState struct {
		content   string
		signature string
	}
	type searchToolState struct {
		queryLines []searchQueryLine
		started    bool
		completed  bool
	}
	agentThinking := make(map[string]*agentThinkingState)
	searchToolStates := make(map[string]*searchToolState)
	// Deduplicate native tool_use forwarding across cumulative agent-inference events.
	seenNativeToolUseIDs := make(map[string]bool)
	// toolCallId -> ordered web result URLs (for resolving [^toolu_*] citations).
	toolCallSearchURLs := make(map[string][]string)
	if knownToolCallURLs != nil {
		if *knownToolCallURLs == nil {
			*knownToolCallURLs = make(map[string][]string)
		}
		toolCallSearchURLs = *knownToolCallURLs
	}
	var observedCitationFragments []string
	var pendingCitationFragment string
	seenSearchResultSummaries := make(map[string]bool)
	thinkingStarted := false
	thinkingClosed := false
	lastThinkingSignature := ""

	emitThinking := func(delta string) {
		if delta == "" || thinkingClosed {
			return
		}
		emittedThinkingChars += len(delta)
		thinkingStarted = true
		if thinkingCb != nil {
			thinkingCb(delta, false, "")
		}
	}

	closeThinking := func() {
		if !thinkingStarted || thinkingClosed {
			return
		}
		thinkingClosed = true
		if thinkingCb != nil {
			thinkingCb("", true, lastThinkingSignature)
		}
	}

	// emitDelta computes the clean text delta since last emit
	emitDelta := func() {
		recordObservedCitationFragments(rawText, &observedCitationFragments, &pendingCitationFragment)
		var citationKnownURLs []string
		if knownCitationURLs != nil {
			citationKnownURLs = *knownCitationURLs
		}
		rewritten := rewriteInternalCitationsWithObserved(rawText, observedCitationFragments, citationKnownURLs)
		cleaned := cleanAllLangTags(trimTrailingIncompleteCitation(rewritten))
		if cleaned == sentClean {
			return
		}

		start := 0
		if strings.HasPrefix(cleaned, sentClean) {
			start = len(sentClean)
		} else {
			// Rare patch rewrites can change earlier text. Use a rune-safe
			// common prefix to avoid slicing in the middle of UTF-8 bytes.
			start = longestCommonPrefixUTF8(cleaned, sentClean)
		}
		if start < len(cleaned) {
			closeThinking()
			cb(cleaned[start:], false, nil)
		}
		sentClean = cleaned
	}

	handlePatchValueEntry := func(statePrefix string, index int, entry AgentValueEntry) {
		patchValueTypes[fmt.Sprintf("%s/value/%d", statePrefix, index)] = entry.Type
		if patchValueCounts[statePrefix] <= index {
			patchValueCounts[statePrefix] = index + 1
		}

		switch entry.Type {
		case "thinking":
			prev := patchThinkingContent
			patchThinkingContent += entry.Content
			if entry.Signature != "" {
				patchThinkingSignature = entry.Signature
				lastThinkingSignature = entry.Signature
			}
			emitThinking(incrementalSuffix(prev, patchThinkingContent))
		case "text":
			if entry.Content != "" {
				rawText += entry.Content
				emitDelta()
			}
		case "tool_use":
			// Skip — tool state must not pollute text output.
			if entry.Name != "" && entry.ID != "" && !seenNativeToolUseIDs[entry.ID] {
				seenNativeToolUseIDs[entry.ID] = true
				if nativeToolUses != nil {
					*nativeToolUses = append(*nativeToolUses, entry)
				}
			}
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineCount++
		responseLogger.LogLine(line)

		var event NDJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		eventTypeCounts[event.Type]++

		switch event.Type {
		case "premium-feature-unavailable":
			return ErrPremiumFeatureUnavailable

		case "agent-tool-result":
			var toolEvt searchToolResultEvent
			if err := json.Unmarshal([]byte(line), &toolEvt); err != nil {
				continue
			}
			if toolEvt.ToolCallID == "" {
				continue
			}
			// Detect search: legacy (toolType/toolName=="search") or new callFunction format
			isSearch := toolEvt.ToolType == "search" || toolEvt.ToolName == "search"
			if !isSearch {
				_, isSearch = searchToolStates[toolEvt.ToolCallID]
			}
			if !isSearch && strings.Contains(toolEvt.Input.Function, "search") {
				isSearch = true
			}
			if !isSearch {
				continue
			}

			// Build/update search state from callFunction streaming events
			state := searchToolStates[toolEvt.ToolCallID]
			if state == nil {
				state = &searchToolState{}
				searchToolStates[toolEvt.ToolCallID] = state
			}

			// Extract query lines from callFunction input or cycleQueries
			if len(state.queryLines) == 0 {
				var ql []searchQueryLine
				for _, q := range toolEvt.Input.Args.Queries {
					question := strings.TrimSpace(q.Question)
					if question != "" {
						ql = append(ql, searchQueryLine{Label: "**Web Search**", Query: question})
					}
				}
				if len(ql) == 0 {
					for _, cq := range toolEvt.Result.CycleQueries {
						cq = strings.TrimSpace(cq)
						if cq != "" {
							ql = append(ql, searchQueryLine{Label: "**Web Search**", Query: cq})
						}
					}
				}
				if len(ql) > 0 {
					state.queryLines = ql
				}
			}

			// Emit search progress thinking on streaming events
			if !state.started && len(state.queryLines) > 0 {
				emitThinking(formatSearchQueryLines(state.queryLines))
				emitThinking(formatSearchStatusLines("**Searching**", state.queryLines))
				state.started = true
			}

			// On "applied" state: search is complete
			if toolEvt.State == "applied" {
				// Try legacy extractedResults first
				docs := collectSearchResultCitationDocs(toolEvt)
				// Also try parsing results from result.output JSON string (new format)
				if len(docs) == 0 && toolEvt.Result.Output != "" {
					docs = parseCallFunctionSearchOutput(toolEvt.Result.Output)
				}
				if len(docs) > 0 {
					appendKnownCitationDocs(knownCitationDocs, docs)
					appendKnownCitationURLs(knownCitationURLs, citationCandidateURLs(docs))
					if len(toolCallSearchURLs[toolEvt.ToolCallID]) == 0 {
						toolCallSearchURLs[toolEvt.ToolCallID] = citationCandidateURLs(docs)
					}
				}
				// Emit search completion + results summary BEFORE emitDelta
				// (emitDelta calls closeThinking, which would prevent further thinking)
				if !state.completed && len(state.queryLines) > 0 {
					emitThinking(formatSearchStatusLines("**Search Complete**", state.queryLines))
					state.completed = true
				}
				if !seenSearchResultSummaries[toolEvt.ToolCallID] {
					if len(toolEvt.Result.ExtractedResults) > 0 {
						emitThinking(formatSearchResultsSummary(toolEvt.Result.ExtractedResults))
						seenSearchResultSummaries[toolEvt.ToolCallID] = true
					} else if len(docs) > 0 {
						summary := formatCitationDocsSummary(docs)
						if summary != "" {
							emitThinking(summary)
							seenSearchResultSummaries[toolEvt.ToolCallID] = true
						}
					}
				}
				if len(docs) > 0 {
					emitDelta()
				}
			}

		case "agent-search-extracted-results":
			// Search citations can reference tool call IDs (toolu_*). Capture
			// extracted web result URLs so we can resolve these tokens in text.
			var searchEvt struct {
				ToolCallID string                  `json:"toolCallId"`
				Results    []extractedSearchResult `json:"results"`
			}
			if err := json.Unmarshal([]byte(line), &searchEvt); err != nil {
				continue
			}
			if searchEvt.ToolCallID == "" || len(searchEvt.Results) == 0 {
				continue
			}
			seen := make(map[string]bool)
			urls := make([]string, 0, len(searchEvt.Results))
			for _, r := range searchEvt.Results {
				u := extractSearchResultURL(r.ID)
				if u == "" || seen[u] {
					continue
				}
				seen[u] = true
				urls = append(urls, u)
			}
			if len(urls) > 0 {
				toolCallSearchURLs[searchEvt.ToolCallID] = urls
				appendKnownCitationURLs(knownCitationURLs, urls)
				emitDelta()
			}
			if state := searchToolStates[searchEvt.ToolCallID]; state != nil && !state.completed {
				emitThinking(formatSearchStatusLines("**Search Complete**", state.queryLines))
				state.completed = true
			}
			if !seenSearchResultSummaries[searchEvt.ToolCallID] {
				emitThinking(formatSearchResultsSummary(searchEvt.Results))
				seenSearchResultSummaries[searchEvt.ToolCallID] = true
			}

		case "agent-inference":
			var step AgentInferenceEvent
			if err := json.Unmarshal([]byte(line), &step); err != nil {
				continue
			}
			for _, entry := range step.Value {
				switch entry.Type {
				case "thinking":
					// agent-inference events are cumulative: each event for the same step ID
					// contains the FULL thinking content so far. Track per step ID and only
					// finalize when FinishedAt is received.
					if entry.Content != "" {
						if agentThinking[step.ID] == nil {
							agentThinking[step.ID] = &agentThinkingState{}
						}
						ts := agentThinking[step.ID]
						emitThinking(incrementalSuffix(ts.content, entry.Content))
						ts.content = entry.Content
						if entry.Signature != "" {
							ts.signature = entry.Signature
							lastThinkingSignature = entry.Signature
						}
					}
				case "text":
					newContent := entry.Content
					// agent-inference text is cumulative, but Notion can rewrite earlier
					// citation bytes (for example a partial URL becomes view://...), which
					// makes the raw text shorter while still being newer content.
					if cleanAllLangTags(newContent) != cleanAllLangTags(rawText) {
						rawText = newContent
						emitDelta()
					}
				case "tool_use":
					// Detect search tool by name OR by input structure (handles
					// Notion renaming the tool from "search" to "callFunction" etc.)
					if entry.ID != "" {
						isSearchEntry := entry.Name == "search"
						if !isSearchEntry {
							// Check if input contains search query structure
							if _, already := searchToolStates[entry.ID]; already {
								isSearchEntry = true
							} else if ql := extractSearchToolQueryLinesFromEntry(entry); len(ql) > 0 {
								isSearchEntry = true
							}
						}
						if isSearchEntry {
							state := searchToolStates[entry.ID]
							if state == nil {
								state = &searchToolState{}
								searchToolStates[entry.ID] = state
							}
							if len(state.queryLines) == 0 {
								state.queryLines = extractSearchToolQueryLinesFromEntry(entry)
							}
							if !state.started && len(state.queryLines) > 0 {
								emitThinking(formatSearchQueryLines(state.queryLines))
								emitThinking(formatSearchStatusLines("**Searching**", state.queryLines))
								state.started = true
							}
						}
					}
					if entry.Name != "" && entry.ID != "" && !seenNativeToolUseIDs[entry.ID] {
						seenNativeToolUseIDs[entry.ID] = true
						if nativeToolUses != nil {
							*nativeToolUses = append(*nativeToolUses, entry)
						}
					}
				}
			}
			if step.FinishedAt != nil {
				// Finalize thinking block for this step (only the latest cumulative content)
				if thinkingBlocks != nil {
					if ts, ok := agentThinking[step.ID]; ok && ts.content != "" {
						if ts.signature != "" {
							lastThinkingSignature = ts.signature
						}
						*thinkingBlocks = append(*thinkingBlocks, ThinkingBlock{
							Content:   ts.content,
							Signature: ts.signature,
						})
						delete(agentThinking, step.ID)
					}
				}
				// Final emit to flush any remaining text
				emitDelta()
				// Accumulate token usage across turns
				if step.InputTokens != nil && step.OutputTokens != nil {
					totalUsage.PromptTokens += *step.InputTokens
					totalUsage.CompletionTokens += *step.OutputTokens
					totalUsage.TotalTokens = totalUsage.PromptTokens + totalUsage.CompletionTokens
				}
				cb("", true, &totalUsage)
			}

		case "patch-start":
			var patchStart struct {
				Data struct {
					Steps []json.RawMessage `json:"s"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(line), &patchStart) == nil {
				patchNextStepIndex = len(patchStart.Data.Steps)
			}

		case "patch":
			var patch PatchEvent
			if err := json.Unmarshal([]byte(line), &patch); err != nil {
				continue
			}
			for _, op := range patch.V {
				if op.O == "a" && op.P == "/s/-" {
					stepIndex := patchNextStepIndex
					patchNextStepIndex++

					var step AgentInferenceEvent
					if json.Unmarshal(op.V, &step) == nil && step.Type == "agent-inference" {
						statePrefix := fmt.Sprintf("/s/%d", stepIndex)
						for idx, entry := range step.Value {
							handlePatchValueEntry(statePrefix, idx, entry)
						}
					}
				}

				// Track value entry types when new entries are added
				if op.O == "a" && strings.Contains(op.P, "/value/-") {
					var entry AgentValueEntry
					if json.Unmarshal(op.V, &entry) == nil {
						statePrefix := op.P[:strings.Index(op.P, "/value/")]
						idx := patchValueCounts[statePrefix]
						handlePatchValueEntry(statePrefix, idx, entry)
					}
				}

				// Handle signature for thinking blocks in patch format
				if op.O == "a" && strings.Contains(op.P, "/signature") {
					var sig string
					if json.Unmarshal(op.V, &sig) == nil {
						patchThinkingSignature = sig
						lastThinkingSignature = sig
					}
					continue
				}

				// Handle finishedAt for thinking → flush thinking block
				if op.O == "a" && strings.HasSuffix(op.P, "/finishedAt") {
					if patchThinkingContent != "" {
						if thinkingBlocks != nil {
							*thinkingBlocks = append(*thinkingBlocks, ThinkingBlock{
								Content:   patchThinkingContent,
								Signature: patchThinkingSignature,
							})
						}
						patchThinkingContent = ""
						patchThinkingSignature = ""
					}
					continue
				}

				// Handle token usage in patch format
				if op.O == "a" && strings.HasSuffix(op.P, "/inputTokens") {
					var tokens int
					if json.Unmarshal(op.V, &tokens) == nil {
						totalUsage.PromptTokens += tokens
					}
					continue
				}
				if op.O == "a" && strings.HasSuffix(op.P, "/outputTokens") {
					var tokens int
					if json.Unmarshal(op.V, &tokens) == nil {
						totalUsage.CompletionTokens += tokens
						totalUsage.TotalTokens = totalUsage.PromptTokens + totalUsage.CompletionTokens
						cb("", true, &totalUsage)
					}
					continue
				}

				if !strings.Contains(op.P, "content") {
					continue
				}

				// Determine if this content belongs to thinking, text, or tool_use
				entryType := classifyPatchContent(op.P, patchValueTypes)

				var text string
				if err := json.Unmarshal(op.V, &text); err == nil {
					switch entryType {
					case "thinking":
						prev := patchThinkingContent
						switch op.O {
						case "x":
							patchThinkingContent += text
						case "p":
							patchThinkingContent = text
						}
						emitThinking(incrementalSuffix(prev, patchThinkingContent))
					case "tool_use":
						// Skip — search input JSON should not pollute text output
					default: // "text" or unknown
						switch op.O {
						case "x":
							rawText += text
							emitDelta()
						case "p":
							rawText = handlePatchReplace(rawText, text)
							emitDelta()
						}
					}
				}
			}

		case "error":
			var errEvt struct {
				Message string `json:"message"`
			}
			json.Unmarshal([]byte(line), &errEvt)
			LogNotionResponseJSON(requestID, "runInferenceTranscript stream error event", map[string]interface{}{
				"line_count": lineCount,
				"message":    errEvt.Message,
			})
			return fmt.Errorf("notion error: %s", errEvt.Message)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	closeThinking()
	LogNotionResponseJSON(requestID, "runInferenceTranscript stream summary", map[string]interface{}{
		"line_count":            lineCount,
		"event_type_counts":     eventTypeCounts,
		"clean_text_chars":      len(sentClean),
		"thinking_chars":        emittedThinkingChars,
		"native_tool_use_count": len(seenNativeToolUseIDs),
		"search_result_sets":    len(toolCallSearchURLs),
		"usage":                 totalUsage,
	})
	return nil
}

// classifyPatchContent determines whether a patch content path belongs to
// a thinking, text, or tool_use entry based on tracked value types.
// Path format: /s/N/value/M/content
func classifyPatchContent(path string, valueTypes map[string]string) string {
	// Extract "/s/N/value/M" from "/s/N/value/M/content"
	contentIdx := strings.LastIndex(path, "/content")
	if contentIdx < 0 {
		return "text" // fallback
	}
	prefix := path[:contentIdx]
	if t, ok := valueTypes[prefix]; ok {
		return t
	}
	return "text" // fallback: treat unknown as text
}

func handlePatchReplace(current, replacement string) string {
	if idx := strings.LastIndex(current, "<lang"); idx >= 0 {
		return current[:idx] + replacement
	}
	return current + replacement
}

// cleanAllLangTags removes all complete <lang .../> tags from text.
// Incomplete tags at the end are also stripped (they'll be completed in a future delta).
func cleanAllLangTags(text string) string {
	for {
		start := strings.Index(text, "<lang")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "/>")
		if end < 0 {
			// Incomplete tag at end — strip it; will be completed later
			text = text[:start]
			break
		}
		text = text[:start] + text[start+end+2:]
	}
	return text
}

// trimTrailingIncompleteCitation drops an unfinished citation suffix so
// streaming emits only stable text. Notion can rewrite in-progress citations
// from partial URLs to internal view:// ids, and emitting those transient
// bytes causes duplicated/corrupted output downstream.
func trimTrailingIncompleteCitation(text string) string {
	state := 0
	start := -1
	for i, ch := range text {
		switch state {
		case 0:
			if ch == '[' {
				state = 1
				start = i
			}
		case 1:
			if ch == '^' {
				state = 2
			} else if ch == '[' {
				start = i
			} else {
				state = 0
				start = -1
			}
		case 2:
			if ch == ']' {
				state = 0
				start = -1
			}
		}
	}
	if state != 0 && start >= 0 {
		return text[:start]
	}
	return text
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// longestCommonPrefixUTF8 returns the byte index of the longest common prefix
// between a and b on rune boundaries.
func longestCommonPrefixUTF8(a, b string) int {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ar, asz := utf8.DecodeRuneInString(a[ai:])
		br, bsz := utf8.DecodeRuneInString(b[bi:])
		if ar != br || asz != bsz {
			break
		}
		ai += asz
		bi += bsz
	}
	return ai
}

func incrementalSuffix(prev, next string) string {
	if next == "" || next == prev {
		return ""
	}
	if strings.HasPrefix(next, prev) {
		return next[len(prev):]
	}
	start := longestCommonPrefixUTF8(prev, next)
	if start >= len(next) {
		return ""
	}
	return next[start:]
}

type searchQueryLine struct {
	Label string
	Query string
}

func formatSearchQueryLines(lines []searchQueryLine) string {
	var out strings.Builder
	for _, line := range lines {
		label := strings.TrimSpace(line.Label)
		query := strings.TrimSpace(line.Query)
		if label == "" || query == "" {
			continue
		}
		out.WriteString(label)
		out.WriteString(": ")
		out.WriteString(query)
		out.WriteString("\n")
	}
	return out.String()
}

func formatSearchStatusLines(label string, lines []searchQueryLine) string {
	if label == "" {
		return ""
	}

	var out strings.Builder
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		query := strings.TrimSpace(line.Query)
		if query == "" || seen[query] {
			continue
		}
		seen[query] = true
		out.WriteString(label)
		out.WriteString(": ")
		out.WriteString(query)
		out.WriteString("\n")
	}
	return out.String()
}

func searchToolInputPayload(entry AgentValueEntry) json.RawMessage {
	input := bytes.TrimSpace(entry.Input)
	if len(input) > 0 && !bytes.Equal(input, []byte("null")) {
		return input
	}

	content := strings.TrimSpace(entry.Content)
	if content == "" || content == "null" {
		return nil
	}
	return json.RawMessage(content)
}

func extractSearchToolQueryLinesFromEntry(entry AgentValueEntry) []searchQueryLine {
	return extractSearchToolQueryLines(searchToolInputPayload(entry))
}

func extractSearchToolQueryLines(input json.RawMessage) []searchQueryLine {
	input = bytes.TrimSpace(input)
	if len(input) == 0 || bytes.Equal(input, []byte("null")) {
		return nil
	}

	type searchQuerySpec struct {
		Query   string   `json:"query"`
		Queries []string `json:"queries"`
	}

	parseSpecQueries := func(label string, raw json.RawMessage) []searchQueryLine {
		var spec searchQuerySpec
		if err := json.Unmarshal(raw, &spec); err != nil {
			return nil
		}
		queries := make([]string, 0, len(spec.Queries)+1)
		if spec.Query != "" {
			queries = append(queries, spec.Query)
		}
		queries = append(queries, spec.Queries...)
		lines := make([]searchQueryLine, 0, len(queries))
		for _, query := range queries {
			query = strings.TrimSpace(query)
			if query == "" {
				continue
			}
			lines = append(lines, searchQueryLine{
				Label: label,
				Query: query,
			})
		}
		return lines
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(input, &raw); err != nil {
		return nil
	}

	lines := make([]searchQueryLine, 0)
	appendLines := func(label string, key string) {
		spec, ok := raw[key]
		if !ok {
			return
		}
		lines = append(lines, parseSpecQueries(label, spec)...)
		delete(raw, key)
	}

	for _, item := range []struct {
		Key   string
		Label string
	}{
		{Key: "web", Label: "**Web Search**"},
		{Key: "internal", Label: "**Workspace Search**"},
		{Key: "default", Label: "**Search**"},
		{Key: "users", Label: "**People Search**"},
	} {
		appendLines(item.Label, item.Key)
	}

	if len(lines) > 0 {
		return lines
	}

	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		label := "**Search**"
		if key != "" {
			label = fmt.Sprintf("**Search:%s**", key)
		}
		lines = append(lines, parseSpecQueries(label, raw[key])...)
	}

	if len(lines) > 0 {
		return lines
	}

	return parseSpecQueries("**Search**", input)
}

type extractedSearchResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type structuredSearchResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type renderedSearchResult struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text"`
}

type searchToolResultEvent struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	ToolType   string `json:"toolType"`
	State      string `json:"state"` // "streaming" or "applied"
	Input      struct {
		Function string `json:"function"`
		Args     struct {
			Queries []struct {
				Question string `json:"question"`
				Keywords string `json:"keywords"`
			} `json:"queries"`
			IncludeWebResults bool `json:"includeWebResults"`
		} `json:"args"`
	} `json:"input"`
	Result struct {
		ExtractedResults  []extractedSearchResult `json:"extractedResults"`
		StructuredContent struct {
			Results []structuredSearchResult `json:"results"`
		} `json:"structuredContent"`
		CycleQueries []string `json:"cycleQueries"`
		Output       string   `json:"output"` // JSON string with search results
	} `json:"result"`
	AdditionalResultForRender struct {
		WebResults [][]renderedSearchResult `json:"webResults"`
	} `json:"additionalResultForRender"`
}

// parseCallFunctionSearchOutput parses the JSON string in result.output from
// callFunction search results and returns CitationCandidate docs.
func parseCallFunctionSearchOutput(outputJSON string) []CitationCandidate {
	var parsed struct {
		Results []struct {
			ID    string `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &parsed); err != nil {
		return nil
	}
	var docs []CitationCandidate
	seen := make(map[string]bool)
	for _, r := range parsed.Results {
		u := r.URL
		if u == "" {
			u = extractSearchResultURL(r.ID)
		}
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		docs = append(docs, CitationCandidate{
			URL:   u,
			Title: r.Title,
		})
	}
	return docs
}

// formatCitationDocsSummary formats a summary of citation docs for thinking output.
func formatCitationDocsSummary(docs []CitationCandidate) string {
	if len(docs) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString(fmt.Sprintf("**Found %d Results**\n", len(docs)))
	for i, doc := range docs {
		title := strings.TrimSpace(doc.Title)
		if title == "" {
			title = doc.URL
		}
		if title == "" {
			continue
		}
		out.WriteString(fmt.Sprintf("%d. %s\n", i+1, title))
	}
	return out.String()
}

func formatSearchResultsSummary(results []extractedSearchResult) string {
	if len(results) == 0 {
		return ""
	}

	var out strings.Builder
	out.WriteString(fmt.Sprintf("**Found %d Results**\n", len(results)))
	for i := 0; i < len(results); i++ {
		title := strings.TrimSpace(strings.ReplaceAll(results[i].Title, "\n", " "))
		if title == "" {
			title = strings.TrimSpace(extractSearchResultURL(results[i].ID))
		}
		if title == "" {
			continue
		}
		out.WriteString("- ")
		out.WriteString(title)
		out.WriteString("\n")
	}
	return out.String()
}

// ========== Researcher Mode ==========

// callResearcherInference handles the researcher mode inference call
func callResearcherInference(acc *Account, messages []ChatMessage, cb StreamCallback, opt *CallOptions) error {
	log.Printf("[researcher] starting research mode inference")
	requestID := opt.RequestID

	transcript := buildResearcherTranscript(acc, messages)

	reqBody := NotionInferenceRequest{
		TraceID:                 generateUUIDv4(),
		SpaceID:                 acc.SpaceID,
		Transcript:              transcript,
		CreateThread:            true,
		GenerateTitle:           true,
		SaveAllThreadOperations: true,
		SetUnreadState:          false,
		ThreadType:              "researcher",
		AsPatchResponse:         false,
		DebugOverrides: DebugOverrides{
			EmitAgentSearchExtractedResults: true,
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal researcher request: %w", err)
	}
	LogNotionRequestJSON(requestID, fmt.Sprintf("POST /runInferenceTranscript researcher account=%s", acc.UserEmail), reqBody)

	req, err := http.NewRequest("POST", NotionAPIBase+"/runInferenceTranscript", bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("create researcher request: %w", err)
	}

	setNotionHeaders(req, acc)

	client := getChromeHTTPClient(AppConfig.ResearchTimeoutDuration())
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send researcher request: %w", err)
	}
	defer resp.Body.Close()
	LogNotionResponseJSON(requestID, "POST /runInferenceTranscript researcher response meta", map[string]interface{}{
		"status":           resp.StatusCode,
		"content_type":     resp.Header.Get("Content-Type"),
		"content_encoding": resp.Header.Get("Content-Encoding"),
	})

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		LogNotionResponseText(requestID, "POST /runInferenceTranscript researcher error body", string(body))
		return fmt.Errorf("notion researcher API error %d: %s", resp.StatusCode, string(body[:min(len(body), 500)]))
	}

	reader, cleanup, err := decompressBody(resp)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	return parseResearcherStream(reader, requestID, cb, opt.ThinkingBlocks, opt.ThinkingCallback)
}

// buildResearcherTranscript builds the minimal transcript for researcher mode.
// Researcher config only needs 3 fields: type, searchScope, useWebSearch.
// Each transcript step needs id, userId, createdAt (unlike workflow mode).
func buildResearcherTranscript(acc *Account, messages []ChatMessage) []interface{} {
	now := time.Now().Format(time.RFC3339Nano)

	configValue := map[string]interface{}{
		"type":         "researcher",
		"searchScope":  map[string]string{"type": "everything"},
		"useWebSearch": true,
	}

	contextValue := map[string]interface{}{
		"timezone":        acc.Timezone,
		"userName":        acc.UserName,
		"userId":          acc.UserID,
		"userEmail":       acc.UserEmail,
		"spaceName":       acc.SpaceName,
		"spaceId":         acc.SpaceID,
		"spaceViewId":     acc.SpaceViewID,
		"currentDatetime": now,
		"surface":         "ai_module",
	}

	transcript := []interface{}{
		ResearcherTranscriptMsg{
			ID:    generateUUIDv4(),
			Type:  "config",
			Value: configValue,
		},
		ResearcherTranscriptMsg{
			ID:    generateUUIDv4(),
			Type:  "context",
			Value: contextValue,
		},
	}

	// Only use the last user message for researcher mode (single-turn)
	var lastUserContent string
	for _, msg := range messages {
		if msg.Role == "user" {
			lastUserContent = msg.Content
		} else if msg.Role == "system" && lastUserContent == "" {
			lastUserContent = msg.Content
		}
	}
	if lastUserContent == "" {
		lastUserContent = "Research this topic"
	}

	transcript = append(transcript, ResearcherTranscriptMsg{
		ID:        generateUUIDv4(),
		Type:      "user",
		Value:     [][]string{{lastUserContent}},
		UserID:    acc.UserID,
		CreatedAt: now,
	})

	return transcript
}

// parseResearcherStream parses the NDJSON streaming response for researcher mode.
// Event sequence: researcher-text-observation → title → researcher-next-steps (×N) →
// researcher-agent (×N) → researcher-report (×N, each value is a text delta)
//
// Thinking deltas are emitted incrementally via thinkingCb so the client sees
// research progress in real-time instead of waiting for the report to start.
func parseResearcherStream(reader io.Reader, requestID string, cb StreamCallback, thinkingBlocks *[]ThinkingBlock, thinkingCb ThinkingDeltaCallback) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	responseLogger := newNotionResponseLogDeduper(requestID, "runInferenceTranscript researcher ndjson deduped")

	var reportEventCount int   // count of report events received
	var thinkingContent string // accumulated thinking content (for non-stream/fallback)
	var totalReportLen int     // total accumulated report text length
	lineCount := 0
	eventTypeCounts := make(map[string]int)
	thinkingDone := false                      // whether we've signaled thinking is complete
	stepNames := make(map[string]string)       // step key → display name from next-steps
	stepSearchTypes := make(map[string]string) // step key → search type (internal/web)
	var lastRealSignature string               // last real Anthropic signature captured from rawOutput
	// Track cumulative thinking content to compute deltas — researcher-next-steps events
	// contain FULL text (not incremental), while researcher-report events ARE true deltas.
	lastOutputByID := make(map[string]string)    // step ID → last seen output thinking content
	lastRawOutputByID := make(map[string]string) // step ID → last seen rawOutput thinking content

	// Buffered report emitter: strips [step-xxx,artifact,N] citation tags.
	// Since tags can be split across streaming deltas, we buffer text after
	// the last '[' and flush only the safe portion.
	var reportBuf strings.Builder
	flushReportBuf := func(final bool) {
		text := reportBuf.String()
		if text == "" {
			return
		}
		// Strip complete citation tags
		text = researcherCitationRe.ReplaceAllString(text, "")
		if !final {
			// Keep text from last '[' onwards in buffer (might be partial citation)
			if idx := strings.LastIndex(text, "["); idx >= 0 {
				safe := text[:idx]
				reportBuf.Reset()
				reportBuf.WriteString(text[idx:])
				text = safe
			} else {
				reportBuf.Reset()
			}
		} else {
			reportBuf.Reset()
		}
		if text != "" {
			totalReportLen += len(text)
			cb(text, false, nil)
		}
	}

	// emitThinking sends a thinking delta to the appropriate handler.
	// For streaming: calls thinkingCb directly for real-time SSE emission.
	// For non-streaming: accumulates in thinkingContent for later batch use.
	emitThinking := func(delta string) {
		thinkingContent += delta
		if thinkingCb != nil {
			thinkingCb(delta, false, "")
		}
	}

	// closeThinking signals the end of the thinking phase
	closeThinking := func() {
		if thinkingDone {
			return
		}
		thinkingDone = true
		if thinkingCb != nil {
			thinkingCb("", true, lastRealSignature)
		} else if thinkingBlocks != nil && thinkingContent != "" && len(*thinkingBlocks) == 0 {
			// Fallback for non-streaming: batch all thinking into one block
			// only if no real thinking blocks were already extracted from rawOutput
			sig := lastRealSignature
			if sig == "" {
				sig = generateFakeSignature()
			}
			*thinkingBlocks = append(*thinkingBlocks, ThinkingBlock{
				Content:   thinkingContent,
				Signature: sig,
			})
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineCount++
		responseLogger.LogLine(line)

		var event NDJSONEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		eventTypeCounts[event.Type]++

		switch event.Type {
		case "premium-feature-unavailable":
			return ErrResearchQuotaExhausted

		case "researcher-text-observation":
			// Query echo — ignore

		case "title":
			// Auto-generated title — ignore

		case "researcher-next-steps":
			var steps ResearcherNextStepsEvent
			if err := json.Unmarshal([]byte(line), &steps); err != nil {
				continue
			}
			// Build step key → display name map and emit search plan
			for _, step := range steps.Value.NextSteps {
				stepNames[step.Key] = step.DisplayName
				stepSearchTypes[step.Key] = step.SearchType
				var label string
				switch step.SearchType {
				case "internal":
					label = "**Workspace Search**"
				case "web":
					label = "**Web Search**"
				default:
					label = "**Search**"
				}
				stepLine := fmt.Sprintf("%s: %s\n", label, step.DisplayName)
				if !strings.Contains(thinkingContent, stepLine) {
					emitThinking(stepLine)
				}
			}
			// Extract thinking blocks from rawOutput (full Extended Thinking with real signature)
			// or fall back to output (condensed thinking).
			// NOTE: NDJSON events contain CUMULATIVE content — each event has the full text so far.
			// We must compute the delta by comparing with the last seen content for this step ID.
			thinkingExtracted := false
			for _, raw := range steps.RawOutput {
				if raw.Type == "thinking" && raw.Content != "" {
					prev := lastRawOutputByID[steps.ID]
					if len(raw.Content) > len(prev) {
						delta := raw.Content[len(prev):]
						emitThinking(delta)
					}
					lastRawOutputByID[steps.ID] = raw.Content
					// Track real Anthropic signature for closeThinking
					if raw.Signature != "" {
						lastRealSignature = raw.Signature
					}
					// Store/update thinking block with real Anthropic signature (for non-streaming)
					if thinkingBlocks != nil && steps.Done {
						*thinkingBlocks = append(*thinkingBlocks, ThinkingBlock{
							Content:   raw.Content,
							Signature: raw.Signature,
						})
					}
					thinkingExtracted = true
				}
			}
			if !thinkingExtracted {
				// Fallback: use condensed output thinking (also cumulative)
				for _, out := range steps.Output {
					if out.Type == "thinking" && out.Content != "" {
						prev := lastOutputByID[steps.ID]
						if len(out.Content) > len(prev) {
							delta := out.Content[len(prev):]
							emitThinking(delta)
						}
						lastOutputByID[steps.ID] = out.Content
					}
				}
			}

		case "researcher-agent":
			var agent ResearcherAgentEvent
			if err := json.Unmarshal([]byte(line), &agent); err != nil {
				continue
			}
			// Resolve step key to display name for readable output
			name := stepNames[agent.Value.Key]
			if name == "" {
				name = agent.Value.Key
			}
			switch agent.Value.Status {
			case "in-progress":
				emitThinking(fmt.Sprintf("**Searching**: %s\n", name))
			case "done":
				emitThinking(fmt.Sprintf("**Search Complete**: %s\n", name))
			}

		case "researcher-report":
			// First report event — close thinking phase
			if reportEventCount == 0 {
				closeThinking()
				log.Printf("[researcher] thinking phase complete (%d chars), starting report", len(thinkingContent))
			}
			reportEventCount++

			var report ResearcherReportEvent
			if err := json.Unmarshal([]byte(line), &report); err != nil {
				continue
			}

			// Value is a text delta — buffer and strip [step-xxx,artifact,N] citations
			if report.Value != "" {
				reportBuf.WriteString(report.Value)
				flushReportBuf(false)
			}

		case "error":
			var errEvt struct {
				Message string `json:"message"`
			}
			json.Unmarshal([]byte(line), &errEvt)
			LogNotionResponseJSON(requestID, "runInferenceTranscript researcher error event", map[string]interface{}{
				"line_count": lineCount,
				"message":    errEvt.Message,
			})
			return fmt.Errorf("notion researcher error: %s", errEvt.Message)
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	// Ensure thinking is closed even if no report events arrived
	closeThinking()

	// Flush any remaining buffered report text (final flush strips citations without holding back)
	flushReportBuf(true)

	log.Printf("[researcher] stream complete: %d report events, %d total chars", reportEventCount, totalReportLen)
	LogNotionResponseJSON(requestID, "runInferenceTranscript researcher summary", map[string]interface{}{
		"line_count":        lineCount,
		"event_type_counts": eventTypeCounts,
		"step_count":        len(stepNames),
		"thinking_chars":    len(thinkingContent),
		"report_chars":      totalReportLen,
		"report_events":     reportEventCount,
	})

	// Signal completion
	usage := UsageInfo{
		PromptTokens:     500,                // estimated
		CompletionTokens: totalReportLen / 4, // rough estimate
		TotalTokens:      500 + totalReportLen/4,
	}
	cb("", true, &usage)

	return nil
}

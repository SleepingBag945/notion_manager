package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func captureNotionObservations(t *testing.T) *[]map[string]interface{} {
	t.Helper()
	var (
		mu     sync.Mutex
		events []map[string]interface{}
	)
	notionObservationLogSink.Lock()
	previousWriter := notionObservationLogSink.write
	notionObservationLogSink.write = func(line string) {
		if !strings.HasPrefix(line, notionObservationPrefix+" ") {
			return
		}
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, notionObservationPrefix+" ")), &event); err != nil {
			t.Errorf("invalid observation JSON: %v", err)
			return
		}
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	notionObservationLogSink.Unlock()
	t.Cleanup(func() {
		notionObservationLogSink.Lock()
		notionObservationLogSink.write = previousWriter
		notionObservationLogSink.Unlock()
	})
	return &events
}

func lastObservationEvent(t *testing.T, events []map[string]interface{}, eventName string) map[string]interface{} {
	t.Helper()
	for i := len(events) - 1; i >= 0; i-- {
		if events[i]["event"] == eventName {
			return events[i]
		}
	}
	t.Fatalf("observation event %q not found: %#v", eventName, events)
	return nil
}

func TestParseNDJSONAlwaysEmitsSanitizedSummary(t *testing.T) {
	oldResponseLogging := NotionResponseLoggingEnabled()
	SetNotionResponseLoggingEnabled(false)
	t.Cleanup(func() { SetNotionResponseLoggingEnabled(oldResponseLogging) })

	tests := []struct {
		name         string
		stream       string
		wantOutcome  string
		wantErr      error
		wantLines    float64
		wantInvalid  float64
		wantTerminal string
		wantUnknown  string
	}{
		{name: "zero byte", wantOutcome: "empty", wantLines: 0},
		{name: "invalid and unknown", stream: "not-json\n{\"type\":\"mystery-terminal\"}\n", wantOutcome: "empty", wantLines: 2, wantInvalid: 1, wantTerminal: "unknown", wantUnknown: "mystery-terminal"},
		{name: "text", stream: "{\"type\":\"agent-inference\",\"value\":[{\"type\":\"text\",\"content\":\"secret-content\"}]}\n", wantOutcome: "text", wantLines: 1, wantTerminal: "agent-inference"},
		{name: "thinking only", stream: "{\"type\":\"agent-inference\",\"value\":[{\"type\":\"thinking\",\"content\":\"private-reasoning\"}]}\n", wantOutcome: "thinking_only", wantLines: 1, wantTerminal: "agent-inference"},
		{name: "premium early return", stream: "{\"type\":\"premium-feature-unavailable\"}\n", wantOutcome: "error", wantErr: ErrPremiumFeatureUnavailable, wantLines: 1, wantTerminal: "premium-feature-unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := captureNotionObservations(t)
			var thinking []string
			err := parseNDJSONStreamObserved(
				bytes.NewBufferString(tt.stream),
				"req-safe",
				inferenceResponseObservationContext{Kind: "workflow", Model: "grok-4.5", NotionModel: "strawberry", WorkspaceSHA256: "workspacehash", AccountSHA256: "accounthash"},
				func(string, bool, *UsageInfo) {}, nil, nil,
				func(delta string, _ bool, _ string) { thinking = append(thinking, delta) },
				nil, nil, nil,
			)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("parse error = %v, want %v", err, tt.wantErr)
			}
			summary := lastObservationEvent(t, *events, "ndjson_summary")
			if summary["outcome"] != tt.wantOutcome || summary["line_count"] != tt.wantLines || summary["invalid_json_line_count"] != tt.wantInvalid {
				t.Fatalf("summary = %#v", summary)
			}
			if summary["terminal_event_type"] != tt.wantTerminal {
				t.Fatalf("terminal_event_type = %#v, want %q", summary["terminal_event_type"], tt.wantTerminal)
			}
			if summary["workspace_sha256"] != "workspacehash" || summary["account_sha256"] != "accounthash" || summary["model"] != "grok-4.5" {
				t.Fatalf("attempt identity missing: %#v", summary)
			}
			if tt.wantUnknown != "" {
				unknown := summary["unknown_event_type_hashes"].(map[string]interface{})
				if unknown[shortSHA256(tt.wantUnknown)] != float64(1) || summary["unknown_event_type_count"] != float64(1) {
					t.Fatalf("unknown event counts = %#v", unknown)
				}
			}
			raw, _ := json.Marshal(summary)
			if strings.Contains(string(raw), "secret-content") || strings.Contains(string(raw), "private-reasoning") || strings.Contains(string(raw), "mystery-terminal") {
				t.Fatalf("summary leaked response content: %s", raw)
			}
		})
	}
}

func TestParseResearcherAlwaysEmitsEmptySummary(t *testing.T) {
	events := captureNotionObservations(t)
	err := parseResearcherStreamObserved(bytes.NewBuffer(nil), "req-research", inferenceResponseObservationContext{
		Kind: "researcher", Model: "researcher", WorkspaceSHA256: "workspacehash", AccountSHA256: "accounthash",
	}, func(string, bool, *UsageInfo) {}, nil, nil)
	if err != nil {
		t.Fatalf("parseResearcherStreamObserved() error = %v", err)
	}
	summary := lastObservationEvent(t, *events, "ndjson_summary")
	if summary["kind"] != "researcher" || summary["outcome"] != "empty" || summary["line_count"] != float64(0) {
		t.Fatalf("researcher summary = %#v", summary)
	}
}

func TestObservationResponseWriterCapturesErrorStatus(t *testing.T) {
	base := httptest.NewRecorder()
	wrapped, observed := newObservationResponseWriter(base)
	if _, ok := wrapped.(http.Flusher); !ok {
		t.Fatal("flushing base lost http.Flusher")
	}
	observed.WriteHeader(502)
	observed.WriteHeader(200)
	if observed.StatusCode() != 502 || base.Code != 502 {
		t.Fatalf("status = observed:%d base:%d, want 502", observed.StatusCode(), base.Code)
	}
}

func TestObservationResponseWriterPreservesMissingFlusher(t *testing.T) {
	base := &nonFlushingResponseWriter{header: make(http.Header)}
	wrapped, _ := newObservationResponseWriter(base)
	if _, ok := wrapped.(http.Flusher); ok {
		t.Fatal("non-flushing base unexpectedly gained http.Flusher")
	}
}

type nonFlushingResponseWriter struct {
	header http.Header
	status int
}

func (w *nonFlushingResponseWriter) Header() http.Header         { return w.header }
func (w *nonFlushingResponseWriter) WriteHeader(statusCode int)  { w.status = statusCode }
func (w *nonFlushingResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestUnknownEventTypeHashesAreBounded(t *testing.T) {
	hashes := make(map[string]int)
	for i := 0; i < 20; i++ {
		recordUnknownEventTypeHash("private-event-"+string(rune('a'+i)), hashes)
	}
	if len(hashes) > maxUnknownEventTypeHashes+1 {
		t.Fatalf("unknown event hash map grew to %d entries", len(hashes))
	}
	if hashes["overflow"] != 20-maxUnknownEventTypeHashes {
		t.Fatalf("overflow count = %d, want %d", hashes["overflow"], 20-maxUnknownEventTypeHashes)
	}
}

func TestInferenceObservationBoundsArbitraryModelLabels(t *testing.T) {
	events := captureNotionObservations(t)
	rawModel := "private prompt-like model label with spaces and secret=" + strings.Repeat("x", 200)
	logInferenceObservation("inference_http", map[string]interface{}{
		"model":        rawModel,
		"notion_model": rawModel,
	})

	event := lastObservationEvent(t, *events, "inference_http")
	want := "sha256:" + shortSHA256(rawModel)
	if event["model"] != want || event["notion_model"] != want {
		t.Fatalf("model labels = %#v / %#v, want %q", event["model"], event["notion_model"], want)
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), rawModel) {
		t.Fatalf("observation leaked arbitrary model label: %s", raw)
	}
}

func TestInferenceObservationSanitizesUpstreamHeaders(t *testing.T) {
	events := captureNotionObservations(t)
	rawSecret := "token_v2_secret123"
	logInferenceObservation("inference_http", map[string]interface{}{
		"content_type":     "application/x-ndjson; charset=utf-8",
		"content_encoding": rawSecret,
		"retry_after":      rawSecret,
	})

	event := lastObservationEvent(t, *events, "inference_http")
	if event["content_type"] != "application/x-ndjson" {
		t.Fatalf("content_type = %#v", event["content_type"])
	}
	wantSecret := "sha256:" + shortSHA256(rawSecret)
	if event["content_encoding"] != wantSecret || event["retry_after"] != wantSecret {
		t.Fatalf("sanitized headers = %#v", event)
	}
}

func TestInferenceObservationAddsInternalSourceCorrelation(t *testing.T) {
	events := captureNotionObservations(t)
	requestID := "msg_internal"
	registerInferenceRequestObservation(requestID, inferenceRequestObservationMetadata{
		SourceAPI: "responses", CorrelationID: "resp_internal",
	})
	t.Cleanup(func() { unregisterInferenceRequestObservation(requestID) })

	logInferenceObservation("inference_http", map[string]interface{}{"request_id": requestID})
	event := lastObservationEvent(t, *events, "inference_http")
	if event["source_api"] != "responses" || event["correlation_id"] != "resp_internal" {
		t.Fatalf("source correlation missing: %#v", event)
	}
}

func TestOpenAIStreamingChecksFlusherBeforeStartingInnerHandler(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request, http.HandlerFunc)
	}{
		{name: "chat", call: func(w http.ResponseWriter, r *http.Request, handler http.HandlerFunc) {
			streamAnthropicAsOpenAIChat(w, r, handler, &AnthropicRequest{Model: "grok-4.5"}, "chatcmpl_test", 1, false)
		}},
		{name: "responses", call: func(w http.ResponseWriter, r *http.Request, handler http.HandlerFunc) {
			streamAnthropicAsOpenAIResponses(w, r, handler, &AnthropicRequest{Model: "grok-4.5"}, "resp_test", 1, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := false
			handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { started = true })
			writer := &nonFlushingResponseWriter{header: make(http.Header)}
			tc.call(writer, httptest.NewRequest(http.MethodPost, "/", nil), handler)
			if started {
				t.Fatal("inner Anthropic handler started without streaming support")
			}
			if writer.status != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", writer.status)
			}
		})
	}
}

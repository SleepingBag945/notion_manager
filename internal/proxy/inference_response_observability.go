package proxy

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

const maxUnknownEventTypeHashes = 8

type inferenceResponseObservationContext struct {
	Kind            string
	Model           string
	NotionModel     string
	WorkspaceSHA256 string
	AccountSHA256   string
}

func recordUnknownEventTypeHash(eventType string, hashes map[string]int) {
	key := shortSHA256(eventType)
	if _, exists := hashes[key]; exists || len(hashes) < maxUnknownEventTypeHashes {
		hashes[key]++
		return
	}
	hashes["overflow"]++
}

func observationContentType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || len(mediaType) > 128 {
		return "sha256:" + shortSHA256(value)
	}
	return strings.ToLower(mediaType)
}

func observationContentEncoding(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ",")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		switch part {
		case "br", "gzip", "identity", "zstd":
			parts[i] = part
		default:
			return "sha256:" + shortSHA256(value)
		}
	}
	return strings.Join(parts, ",")
}

func observationRetryAfter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) <= 10 {
		if _, err := strconv.ParseUint(value, 10, 32); err == nil {
			return value
		}
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		return retryAt.UTC().Format(http.TimeFormat)
	}
	return "sha256:" + shortSHA256(value)
}

// observationResponseWriter records the first HTTP status while preserving
// streaming support. It does not change response bytes or routing decisions.
type observationResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

type observationFlushingResponseWriter struct {
	*observationResponseWriter
	flusher http.Flusher
}

func newObservationResponseWriter(w http.ResponseWriter) (http.ResponseWriter, *observationResponseWriter) {
	observed := &observationResponseWriter{ResponseWriter: w}
	if flusher, ok := w.(http.Flusher); ok {
		return &observationFlushingResponseWriter{observationResponseWriter: observed, flusher: flusher}, observed
	}
	return observed, observed
}

func (w *observationResponseWriter) WriteHeader(statusCode int) {
	if w.statusCode != 0 {
		return
	}
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *observationResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *observationFlushingResponseWriter) Flush() {
	if w.observationResponseWriter.statusCode == 0 {
		w.observationResponseWriter.statusCode = http.StatusOK
	}
	w.flusher.Flush()
}

func (w *observationResponseWriter) StatusCode() int {
	return w.statusCode
}

// observationCountingReader counts decoded response bytes without retaining
// any response content. It exists solely for safe inference diagnostics.
type observationCountingReader struct {
	reader io.Reader
	bytes  int64
}

func (r *observationCountingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += int64(n)
	return n, err
}

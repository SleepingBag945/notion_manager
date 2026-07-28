package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteOpenAIErrorUsesChineseStableCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIError(rec, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "")

	var body OpenAIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", rec.Code)
	}
	if body.Error.Code != "NM1001" {
		t.Fatalf("code = %#v", body.Error.Code)
	}
	if !strings.Contains(body.Error.Message, "[NM1001]") || !strings.Contains(body.Error.Message, "请求方法") {
		t.Fatalf("message = %q", body.Error.Message)
	}
}

func TestWriteAnthropicErrorMapsBusyToRetryableCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicError(rec, "req_test", http.StatusTooManyRequests, ErrAllAccountsBusy.Error(), "rate_limit_error")

	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != "NM2001" || body.Error.Type != "rate_limit_error" {
		t.Fatalf("error = %#v", body.Error)
	}
	if !strings.Contains(body.Error.Message, "并发上限") {
		t.Fatalf("message = %q", body.Error.Message)
	}
}

func TestClassifyPublicAPIErrorUpstreamTimeout(t *testing.T) {
	got := classifyPublicAPIError(http.StatusGatewayTimeout, "context deadline exceeded", "api_error")
	if got.Code != "NM3002" {
		t.Fatalf("code = %s", got.Code)
	}
}

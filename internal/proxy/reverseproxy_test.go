package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type reverseProxyRoundTripper func(*http.Request) (*http.Response, error)

func (f reverseProxyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func reverseProxyResponse(status int, location string) *http.Response {
	header := make(http.Header)
	if location != "" {
		header.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func TestAccountCookieHeaderFallback(t *testing.T) {
	acc := &Account{
		TokenV2:   "token",
		UserID:    "user",
		BrowserID: "browser",
		DeviceID:  "device",
	}
	want := "token_v2=token; notion_user_id=user; notion_browser_id=browser; device_id=device"
	if got := accountCookieHeader(acc); got != want {
		t.Fatalf("accountCookieHeader() = %q, want %q", got, want)
	}
}

func TestAccountCookieHeaderPreservesFullCookie(t *testing.T) {
	const fullCookie = "token_v2=full; custom=value; spaced=kept exactly"
	acc := &Account{TokenV2: "fallback", FullCookie: fullCookie}
	if got := accountCookieHeader(acc); got != fullCookie {
		t.Fatalf("accountCookieHeader() = %q, want exact FullCookie %q", got, fullCookie)
	}
}

func TestReverseProxyRedirectReinjectsCookieAndUpdatesHost(t *testing.T) {
	acc := &Account{TokenV2: "token"}
	var requests []*http.Request
	transport := reverseProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		if len(requests) == 1 {
			return reverseProxyResponse(http.StatusTemporaryRedirect, "https://app.notion.com/space/sessionSyncCallback"), nil
		}
		return reverseProxyResponse(http.StatusOK, ""), nil
	})

	req, err := http.NewRequest(http.MethodGet, "https://www.notion.so/sessionSyncCallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", accountCookieHeader(acc))
	resp, err := newReverseProxyHTTPClient(0, transport, acc).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if got := requests[1].Host; got != "app.notion.com" {
		t.Fatalf("redirect Host = %q, want app.notion.com", got)
	}
	if got := requests[1].Header.Get("Cookie"); got != "token_v2=token" {
		t.Fatalf("redirect Cookie = %q, want token fallback", got)
	}
}

func TestReverseProxyRedirectBlocksUnknownHostWithoutRequest(t *testing.T) {
	acc := &Account{TokenV2: "secret"}
	requests := 0
	evilRequests := 0
	transport := reverseProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Hostname() == "evil.example" {
			evilRequests++
			if req.Header.Get("Cookie") != "" {
				t.Fatal("cookie leaked to blocked redirect host")
			}
		}
		return reverseProxyResponse(http.StatusFound, "https://evil.example/steal"), nil
	})

	req, err := http.NewRequest(http.MethodGet, "https://www.notion.so/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cookie", accountCookieHeader(acc))
	_, err = newReverseProxyHTTPClient(0, transport, acc).Do(req)
	if err == nil {
		t.Fatal("unknown redirect unexpectedly succeeded")
	}
	if requests != 1 || evilRequests != 0 {
		t.Fatalf("requests = %d, evil requests = %d; want 1 and 0", requests, evilRequests)
	}
}

func TestReverseProxyRedirectLimit(t *testing.T) {
	requests := 0
	transport := reverseProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests++
		return reverseProxyResponse(http.StatusFound, "/hop/"+string(rune('0'+requests))), nil
	})

	req, err := http.NewRequest(http.MethodGet, "https://app.notion.com/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newReverseProxyHTTPClient(0, transport, &Account{TokenV2: "token"}).Do(req)
	if err == nil {
		t.Fatal("redirect chain unexpectedly succeeded")
	}
	if requests != maxProxyRedirectHops+1 {
		t.Fatalf("requests = %d, want %d", requests, maxProxyRedirectHops+1)
	}
}

func TestConfigPatchRewritesAppMsgstore(t *testing.T) {
	script := configPatchScript("https://proxy.example", &Account{TokenV2: "token"})
	for _, domain := range []string{`www\.notion\.so`, `app\.notion\.com`} {
		if !strings.Contains(script, domain) {
			t.Fatalf("config patch does not support msgstore domain %q", domain)
		}
	}
	if msgstoreOrigin != "https://msgstore.app.notion.com" {
		t.Fatalf("msgstoreOrigin = %q, want canonical app host", msgstoreOrigin)
	}
}

func TestProxyMsgstoreRejectsUntrustedHostBeforeRequest(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/capture", nil)
	rp := &ReverseProxy{}
	rp.proxyMsgstoreHTTP(recorder, request, &ProxySession{Account: &Account{TokenV2: "secret"}}, "attacker.example", "/capture")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestAllowedMsgstoreHosts(t *testing.T) {
	tests := map[string]bool{
		"msgstore.www.notion.so":          true,
		"msgstore-001.www.notion.so":      true,
		"msgstore.app.notion.com":         true,
		"msgstore-002.app.notion.com":     true,
		"attacker.example":                false,
		"msgstore.app.notion.com.evil":    false,
		"msgstore-001.app.notion.com:443": false,
	}
	for host, want := range tests {
		if got := isAllowedMsgstoreHost(host); got != want {
			t.Errorf("isAllowedMsgstoreHost(%q) = %v, want %v", host, got, want)
		}
	}
}

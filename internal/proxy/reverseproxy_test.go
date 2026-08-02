package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func cookieValues(header, name string) []string {
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Cookie", header)
	var values []string
	for _, cookie := range req.Cookies() {
		if cookie.Name == name {
			values = append(values, cookie.Value)
		}
	}
	return values
}

func TestAccountCookieHeaderFallback(t *testing.T) {
	acc := &Account{
		TokenV2:   "token",
		UserID:    "user",
		BrowserID: "browser",
		DeviceID:  "device",
	}
	want := "token_v2=token; notion_user_id=user; notion_users=%5B%22user%22%5D; " +
		"notion_browser_id=browser; device_id=device"
	if got := accountCookieHeader(acc); got != want {
		t.Fatalf("accountCookieHeader() = %q, want %q", got, want)
	}
}

func TestAccountCookieHeaderFiltersFullCookieAndKeepsStableSeeds(t *testing.T) {
	acc := &Account{
		TokenV2:   "stable-token",
		UserID:    "stable-user",
		BrowserID: "stable-browser",
		DeviceID:  "stable-device",
		FullCookie: "token_v2=stale-token; notion_user_id=stale-user; notion_users=%5B%22stale-user%22%5D; notion_locale=zh-CN; " +
			"notion_check_cookie_consent=true; sync_session=stale; sync_session_v2=stale; " +
			"session_sync_nonce=stale; session_sync_checked=stale; custom=value",
	}
	want := "token_v2=stable-token; notion_user_id=stable-user; notion_users=%5B%22stable-user%22%5D; " +
		"notion_browser_id=stable-browser; device_id=stable-device; notion_locale=zh-CN; notion_check_cookie_consent=true"
	if got := accountCookieHeader(acc); got != want {
		t.Fatalf("accountCookieHeader() = %q, want %q", got, want)
	}
}

func TestReverseProxyRedirectReinjectsCookieAndUpdatesHost(t *testing.T) {
	sess := newProxySession(&Account{TokenV2: "token"})
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
	setProxySessionCookies(req, sess)
	resp, err := newReverseProxyHTTPClient(0, transport, sess).Do(req)
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

func TestReverseProxyRedirectPersistsIntermediateCookie(t *testing.T) {
	sess := newProxySession(&Account{TokenV2: "token"})
	var requests []*http.Request
	transport := reverseProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		if len(requests) == 1 {
			resp := reverseProxyResponse(http.StatusTemporaryRedirect, "https://app.notion.com/space/sessionSyncCallback")
			resp.Header.Add("Set-Cookie", "sync_session=dynamic; Path=/; Secure; HttpOnly")
			return resp, nil
		}
		return reverseProxyResponse(http.StatusOK, ""), nil
	})

	req, err := http.NewRequest(http.MethodGet, "https://app.notion.com/ai", nil)
	if err != nil {
		t.Fatal(err)
	}
	setProxySessionCookies(req, sess)
	resp, err := newReverseProxyHTTPClient(0, transport, sess).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if got := cookieValues(requests[1].Header.Get("Cookie"), "sync_session"); len(got) != 1 || got[0] != "dynamic" {
		t.Fatalf("redirect sync_session cookies = %v, want [dynamic]", got)
	}
}

func TestReverseProxyFinalCookiePersistsAcrossRequests(t *testing.T) {
	sess := newProxySession(&Account{TokenV2: "token"})
	var requests []*http.Request
	transport := reverseProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		resp := reverseProxyResponse(http.StatusOK, "")
		if len(requests) == 1 {
			resp.Header.Add("Set-Cookie", "session_sync_checked=true; Path=/; Secure; HttpOnly")
		}
		return resp, nil
	})

	for _, path := range []string{"/ai", "/api/v3/loadUserContent"} {
		req, err := http.NewRequest(http.MethodGet, "https://app.notion.com"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		setProxySessionCookies(req, sess)
		resp, err := newReverseProxyHTTPClient(0, transport, sess).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	if got := cookieValues(requests[1].Header.Get("Cookie"), "session_sync_checked"); len(got) != 1 || got[0] != "true" {
		t.Fatalf("second request session_sync_checked cookies = %v, want [true]", got)
	}
}

func TestReverseProxyJarCookieOverridesSeedWithoutDuplicate(t *testing.T) {
	sess := newProxySession(&Account{TokenV2: "seed-token"})
	var requests []*http.Request
	transport := reverseProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Clone(req.Context()))
		if len(requests) == 1 {
			resp := reverseProxyResponse(http.StatusTemporaryRedirect, "/callback")
			resp.Header.Add("Set-Cookie", "token_v2=jar-token; Path=/; Secure; HttpOnly")
			return resp, nil
		}
		return reverseProxyResponse(http.StatusOK, ""), nil
	})

	req, err := http.NewRequest(http.MethodGet, "https://app.notion.com/ai", nil)
	if err != nil {
		t.Fatal(err)
	}
	setProxySessionCookies(req, sess)
	resp, err := newReverseProxyHTTPClient(0, transport, sess).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := cookieValues(requests[1].Header.Get("Cookie"), "token_v2"); len(got) != 1 || got[0] != "jar-token" {
		t.Fatalf("redirect token_v2 cookies = %v, want exactly [jar-token]", got)
	}
}

func TestReverseProxyCookieJarsAreIsolated(t *testing.T) {
	url, err := url.Parse("https://app.notion.com/ai")
	if err != nil {
		t.Fatal(err)
	}
	sessA := newProxySession(&Account{TokenV2: "token"})
	sessB := newProxySession(&Account{TokenV2: "token"})
	sessA.CookieJar.SetCookies(url, []*http.Cookie{{Name: "sync_session", Value: "session-a", Path: "/", Secure: true}})

	if got := cookieValues(proxySessionCookieHeader(sessA, url), "sync_session"); len(got) != 1 || got[0] != "session-a" {
		t.Fatalf("session A sync_session cookies = %v, want [session-a]", got)
	}
	if got := cookieValues(proxySessionCookieHeader(sessB, url), "sync_session"); len(got) != 0 {
		t.Fatalf("session B unexpectedly received session A cookies: %v", got)
	}
}

func TestReverseProxyRedirectBlocksUnknownHostWithoutRequest(t *testing.T) {
	sess := newProxySession(&Account{TokenV2: "secret"})
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
	setProxySessionCookies(req, sess)
	_, err = newReverseProxyHTTPClient(0, transport, sess).Do(req)
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
	sess := newProxySession(&Account{TokenV2: "token"})
	setProxySessionCookies(req, sess)
	_, err = newReverseProxyHTTPClient(0, transport, sess).Do(req)
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
	rp.proxyMsgstoreHTTP(recorder, request, newProxySession(&Account{TokenV2: "secret"}), "attacker.example", "/capture")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestProxyMsgstoreHTTPUsesSessionCookieJar(t *testing.T) {
	sess := newProxySession(&Account{TokenV2: "token"})
	var requests []*http.Request
	rp := &ReverseProxy{
		msgTransport: reverseProxyRoundTripper(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.Clone(req.Context()))
			resp := reverseProxyResponse(http.StatusOK, "")
			if len(requests) == 1 {
				resp.Header.Add("Set-Cookie", "AWSALBAPP-0=sticky; Path=/; Secure; HttpOnly")
			}
			return resp, nil
		}),
	}

	for range 2 {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/primus-v8/", nil)
		rp.proxyMsgstoreHTTP(recorder, request, sess, msgstoreHost, "/primus-v8/")
		if recorder.Code != http.StatusOK {
			t.Fatalf("msgstore status = %d, want %d", recorder.Code, http.StatusOK)
		}
		if got := recorder.Header().Values("Set-Cookie"); len(got) != 0 {
			t.Fatalf("browser received upstream Set-Cookie: %v", got)
		}
	}

	if got := cookieValues(requests[1].Header.Get("Cookie"), "AWSALBAPP-0"); len(got) != 1 || got[0] != "sticky" {
		t.Fatalf("second msgstore request sticky cookies = %v, want [sticky]", got)
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

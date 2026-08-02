package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"notion-manager/internal/netutil"
)

const (
	notionOrigin         = "https://app.notion.com"
	notionReferer        = notionOrigin + "/"
	msgstoreHost         = "msgstore.app.notion.com"
	msgstoreOrigin       = "https://" + msgstoreHost
	maxProxyRedirectHops = 3
)

// Strip analytics/tracking script/noscript tags from HTML
var reAnalyticsScript = regexp.MustCompile(`(?s)<(?:script|noscript)[^>]*>.*?(?:googletagmanager\.com|customer\.io|gtag/js).*?</(?:script|noscript)>`)

var reAllowedMsgstoreHost = regexp.MustCompile(`(?i)^msgstore(?:-[a-z0-9-]+)?\.(?:www\.notion\.so|app\.notion\.com)$`)

// ProxySession maps a proxy session cookie to a pooled account
type ProxySession struct {
	Account   *Account
	CreatedAt time.Time
	CookieJar http.CookieJar
}

// ReverseProxy proxies requests to notion.so with session/cookie injection
type ReverseProxy struct {
	pool         *AccountPool
	sessions     sync.Map // sessionID → *ProxySession
	msgTransport http.RoundTripper
}

// NewReverseProxy creates a reverse proxy backed by the given account pool
func NewReverseProxy(pool *AccountPool) *ReverseProxy {
	return &ReverseProxy{
		pool: pool,
		// Engine.IO requires sticky sessions: AWS ALB uses AWSALBAPP-0 cookie.
		// Each ProxySession's CookieJar stores those cookies independently while
		// this transport remains shared for connection reuse.
		// DialContext routes through AppConfig.Proxy.NotionProxy at dial
		// time so a /admin/settings flip applies to new msgstore
		// connections without restarting the process.
		msgTransport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return netutil.DialThroughProxy(ctx, network, addr, AppConfig.NotionProxyURL())
			},
			ForceAttemptHTTP2:   false,
			TLSNextProto:        make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

func newProxySession(acc *Account) *ProxySession {
	jar, _ := cookiejar.New(nil)
	return &ProxySession{
		Account:   acc,
		CreatedAt: time.Now(),
		CookieJar: jar,
	}
}

var safeFullCookieSeeds = map[string]bool{
	"notion_check_cookie_consent":  true,
	"notion_cookie_sync_completed": true,
	"notion_locale":                true,
}

func isTransientSessionCookie(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(name, "sync_session") || strings.HasPrefix(name, "session_sync_")
}

func parseCookieHeader(raw string) []*http.Cookie {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	req := &http.Request{Header: make(http.Header)}
	req.Header.Set("Cookie", raw)
	return req.Cookies()
}

func accountSeedCookies(acc *Account) []*http.Cookie {
	if acc == nil {
		return nil
	}

	acc.mu.RLock()
	defer acc.mu.RUnlock()

	values := []struct {
		name  string
		value string
	}{
		{name: "token_v2", value: acc.TokenV2},
		{name: "notion_user_id", value: acc.UserID},
		{name: "notion_users", value: notionUsersCookieValue(acc.UserID)},
		{name: "notion_browser_id", value: acc.BrowserID},
		{name: "device_id", value: acc.DeviceID},
	}
	cookies := make([]*http.Cookie, 0, len(values)+len(safeFullCookieSeeds))
	seen := make(map[string]bool, len(values)+len(safeFullCookieSeeds))
	for _, item := range values {
		if item.value == "" {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: item.name, Value: item.value})
		seen[item.name] = true
	}

	// FullCookie can contain stale session-sync state. Only copy explicitly
	// stable, non-authentication preferences; Account fields above remain the
	// source of truth, so token_v2 and identity cookies can never be replaced.
	for _, cookie := range parseCookieHeader(acc.FullCookie) {
		name := strings.ToLower(cookie.Name)
		if isTransientSessionCookie(name) || !safeFullCookieSeeds[name] || seen[name] {
			continue
		}
		cookies = append(cookies, &http.Cookie{Name: cookie.Name, Value: cookie.Value})
		seen[name] = true
	}
	return cookies
}

func notionUsersCookieValue(userID string) string {
	if userID == "" {
		return ""
	}
	users, err := json.Marshal([]string{userID})
	if err != nil {
		return ""
	}
	return url.PathEscape(string(users))
}

// accountCookieHeader returns the stable Notion cookie seed for an account.
func accountCookieHeader(acc *Account) string {
	cookies := accountSeedCookies(acc)
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		parts = append(parts, cookie.String())
	}
	return strings.Join(parts, "; ")
}

func setProxySessionCookies(req *http.Request, sess *ProxySession) {
	req.Header.Del("Cookie")
	jarNames := make(map[string]bool)
	if sess != nil && sess.CookieJar != nil {
		for _, cookie := range sess.CookieJar.Cookies(req.URL) {
			jarNames[cookie.Name] = true
		}
	}
	if sess == nil {
		return
	}
	for _, cookie := range accountSeedCookies(sess.Account) {
		if !jarNames[cookie.Name] {
			req.AddCookie(cookie)
		}
	}
}

func proxySessionCookieHeader(sess *ProxySession, targetURL *url.URL) string {
	req := &http.Request{Header: make(http.Header), URL: targetURL}
	setProxySessionCookies(req, sess)
	if sess != nil && sess.CookieJar != nil {
		for _, cookie := range sess.CookieJar.Cookies(targetURL) {
			req.AddCookie(cookie)
		}
	}
	return req.Header.Get("Cookie")
}

func reverseProxyHTTPClient(timeout time.Duration, sess *ProxySession) *http.Client {
	return newReverseProxyHTTPClient(timeout, getChromeRoundTripper(), sess)
}

func newReverseProxyHTTPClient(timeout time.Duration, transport http.RoundTripper, sess *ProxySession) *http.Client {
	var jar http.CookieJar
	if sess != nil {
		jar = sess.CookieJar
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		Jar:           jar,
		CheckRedirect: reverseProxyCheckRedirect(sess),
	}
}

func reverseProxyCheckRedirect(sess *ProxySession) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		// The standard client may copy sensitive headers before CheckRedirect.
		// Remove Cookie first so blocked destinations can never receive it.
		req.Header.Del("Cookie")

		if len(via) > maxProxyRedirectHops {
			return fmt.Errorf("reverse proxy redirect limit exceeded: %d hops", maxProxyRedirectHops)
		}
		if req.URL.Scheme != "https" || !isAllowedNotionRedirectHost(req.URL.Hostname()) {
			return fmt.Errorf("reverse proxy redirect blocked: %s", req.URL.Redacted())
		}
		for _, previous := range via {
			if previous.URL.String() == req.URL.String() {
				return fmt.Errorf("reverse proxy redirect loop blocked: %s", req.URL.Redacted())
			}
		}

		req.Host = req.URL.Host
		setProxySessionCookies(req, sess)
		return nil
	}
}

func isAllowedNotionRedirectHost(host string) bool {
	return strings.EqualFold(host, "www.notion.so") || strings.EqualFold(host, "app.notion.com")
}

// getSession retrieves an existing session for the request.
// Sessions are created exclusively via /proxy/start (dashboard account selection).
// Returns nil if no valid session exists — caller should redirect to /dashboard/.
func (rp *ReverseProxy) getSession(r *http.Request) *ProxySession {
	if c, err := r.Cookie("np_session"); err == nil {
		if s, ok := rp.sessions.Load(c.Value); ok {
			return s.(*ProxySession)
		}
	}
	return nil
}

// configPatchScript returns JS that:
// 1. Sets all notion cookies via document.cookie (before SPA reads them)
// 2. Intercepts window.CONFIG assignment to patch URLs
// 3. Unregisters Service Workers
func configPatchScript(origin string, acc *Account) string {
	// Build cookie-setting JS from full_cookie string
	cookieJS := ""
	for _, part := range strings.Split(accountCookieHeader(acc), ";") {
		part = strings.TrimSpace(part)
		if part != "" {
			cookieJS += fmt.Sprintf(`document.cookie=%q+";path=/";`, part)
		}
	}

	return fmt.Sprintf(`<script>(function(){`+
		// Step 1: Set cookies before any SPA code reads them
		`%s`+
		// Step 2: CONFIG interceptor
		`var o=%q,_c;`+
		`Object.defineProperty(window,'CONFIG',{`+
		`get:function(){return _c},`+
		`set:function(v){_c=v;if(v&&typeof v==='object'){`+
		`v.domainBaseUrl=o;`+
		`if(v.messageStore)v.messageStore.url=o+'/msgstore';`+
		`if(v.audioProcessor)v.audioProcessor.url=o+'/audioprocessor';`+
		`v.isLocalhost=false;v.isLocalDevelopment=true`+
		`}},configurable:true,enumerable:true});`+
		// Step 3: Unregister Service Workers
		`if(navigator.serviceWorker)navigator.serviceWorker.getRegistrations()`+
		`.then(function(r){r.forEach(function(x){x.unregister()})});`+
		// Step 4: Intercept fetch/XHR/WebSocket for msgstore URLs
		`var re=/https?:\/\/(msgstore[^\/]*\.(?:www\.notion\.so|app\.notion\.com))/;`+
		`var wre=/wss?:\/\/(msgstore[^\/]*\.(?:www\.notion\.so|app\.notion\.com))/;`+
		`var _bk=/googletagmanager\.com|customer\.io|app\.notion\.com\/exp|splunkcloud\.com|amplitude\.com/;`+
		`var _f=window.fetch;`+
		`window.fetch=function(u,i){`+
		`if(typeof u==='string'){`+
		`if(_bk.test(u))return Promise.resolve(new Response('',{status:200}));`+
		`u=u.replace(re,o+'/_msgproxy/$1')}`+
		`return _f.call(this,u,i)};`+
		`var _xo=XMLHttpRequest.prototype.open;`+
		`XMLHttpRequest.prototype.open=function(m,u){`+
		`if(typeof u==='string'){var a=[].slice.call(arguments);`+
		`a[1]=u.replace(re,o+'/_msgproxy/$1');`+
		`return _xo.apply(this,a)}return _xo.apply(this,arguments)};`+
		`var _W=window.WebSocket;`+
		`window.WebSocket=function(u,p){`+
		`if(typeof u==='string')u=u.replace(wre,`+
		`(location.protocol==='https:'?'wss:':'ws:')+'//'+location.host+'/_msgproxy/$1');`+
		`return p!==undefined?new _W(u,p):new _W(u)};`+
		`window.WebSocket.prototype=_W.prototype;`+
		`Object.keys(_W).forEach(function(k){window.WebSocket[k]=_W[k]});`+
		`})();</script>`, cookieJS, origin)
}

func (rp *ReverseProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Static assets: no auth needed, passthrough to notion.so CDN
	if strings.HasPrefix(path, "/_assets/") ||
		strings.HasPrefix(path, "/images/") ||
		path == "/sw.js" ||
		path == "/favicon.ico" {
		rpProxyPassthrough(w, r, notionOrigin)
		return
	}

	// Msgstore proxy via /_msgproxy/{targetHost}/...
	if strings.HasPrefix(path, "/_msgproxy/") {
		rest := strings.TrimPrefix(path, "/_msgproxy/")
		slashIdx := strings.Index(rest, "/")
		if slashIdx == -1 {
			http.Error(w, "invalid proxy path", http.StatusBadRequest)
			return
		}
		targetHost := rest[:slashIdx]
		targetPath := rest[slashIdx:]

		sess := rp.getSession(r)
		if sess == nil {
			http.NotFound(w, r)
			return
		}

		if isWebSocketUpgrade(r) {
			rp.proxyWebSocket(w, r, sess, targetHost, targetPath)
			return
		}
		rp.proxyMsgstoreHTTP(w, r, sess, targetHost, targetPath)
		return
	}

	// Ping: no session needed, return simple OK for GET
	if path == "/api/v3/ping" && r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
		return
	}

	// All other routes need a session (created via /proxy/start from dashboard)
	sess := rp.getSession(r)
	if sess == nil {
		http.NotFound(w, r)
		return
	}

	// MessageStore proxy (real-time sync)
	// Primus strips path from messageStore.url and uses origin + /primus-v8/
	if strings.HasPrefix(path, "/primus-v8/") || strings.HasPrefix(path, "/msgstore/") {
		targetHost := msgstoreHost
		targetPath := path
		if strings.HasPrefix(path, "/msgstore/") {
			targetPath = strings.TrimPrefix(path, "/msgstore")
		}
		if isWebSocketUpgrade(r) {
			rp.proxyWebSocket(w, r, sess, targetHost, targetPath)
			return
		}
		rp.proxyMsgstoreHTTP(w, r, sess, targetHost, targetPath)
		return
	}

	// Image proxy: rewrite embedded localhost URLs back to www.notion.so
	if strings.HasPrefix(path, "/image/") {
		scheme := "http"
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		proxyOrigin := scheme + "://" + r.Host
		// Fix embedded URL: replace proxy origin with notion origin
		fixedURI := strings.ReplaceAll(r.URL.RequestURI(), url.PathEscape(proxyOrigin), url.PathEscape(notionOrigin))
		fixedURI = strings.ReplaceAll(fixedURI, url.QueryEscape(proxyOrigin), url.QueryEscape(notionOrigin))
		r.URL, _ = url.Parse(fixedURI)
		rp.proxyGeneric(w, r, sess)
		return
	}

	// API proxy (with notion-specific headers)
	if strings.HasPrefix(path, "/api/") {
		rp.proxyAPI(w, r, sess)
		return
	}

	// HTML pages that need CONFIG injection
	if path == "/ai" || strings.HasPrefix(path, "/chat") {
		rp.proxyHTML(w, r, sess)
		return
	}

	// Everything else: proxy with cookies, no HTML injection
	rp.proxyGeneric(w, r, sess)
}

// proxyHTML fetches an HTML page, injects CONFIG patch, strips security headers
func (rp *ReverseProxy) proxyHTML(w http.ResponseWriter, r *http.Request, sess *ProxySession) {
	targetURL := notionOrigin + r.URL.RequestURI()

	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	req.Header.Set("User-Agent", AppConfig.Browser.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	if al := r.Header.Get("Accept-Language"); al != "" {
		req.Header.Set("Accept-Language", al)
	}
	setProxySessionCookies(req, sess)
	// Deliberately omit Accept-Encoding so we get uncompressed HTML for patching

	client := reverseProxyHTTPClient(30*time.Second, sess)
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Determine proxy origin from the incoming request
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	origin := scheme + "://" + r.Host

	html := string(body)

	// Strip analytics/tracking scripts (GTM, customer.io) to prevent connection errors
	html = reAnalyticsScript.ReplaceAllString(html, "")

	// Inject CONFIG interceptor before the very first <script> tag
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") || (len(html) > 15 && strings.Contains(strings.ToLower(html[:100]), "<!doctype")) {
		patch := configPatchScript(origin, sess.Account)
		if idx := strings.Index(html, "<script>"); idx != -1 {
			html = html[:idx] + patch + html[idx:]
		} else if idx := strings.Index(html, "</head>"); idx != -1 {
			html = html[:idx] + patch + html[idx:]
		}
	}

	rpCopyHeaders(w, resp, true)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Security-Policy", "script-src 'self' 'unsafe-inline' 'unsafe-eval' blob: 'wasm-unsafe-eval'")
	w.Header().Del("Content-Length") // body was modified
	w.WriteHeader(resp.StatusCode)
	w.Write([]byte(html))
}

// proxyAPI proxies /api/v3/* calls with cookie + notion header injection
func (rp *ReverseProxy) proxyAPI(w http.ResponseWriter, r *http.Request, sess *ProxySession) {
	targetURL := notionOrigin + r.URL.RequestURI()

	req, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy request headers, replacing sensitive ones
	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "cookie" || lk == "origin" || lk == "referer" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	acc := sess.Account
	setProxySessionCookies(req, sess)
	req.Header.Set("x-notion-active-user-header", acc.UserID)
	req.Header.Set("x-notion-space-id", acc.SpaceID)
	if acc.ClientVersion != "" {
		req.Header.Set("notion-client-version", acc.ClientVersion)
	}
	req.Header.Set("Origin", notionOrigin)
	req.Header.Set("Referer", notionReferer)

	// No timeout for streaming (runInferenceTranscript can stream for minutes)
	client := reverseProxyHTTPClient(0, sess)
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rpCopyHeaders(w, resp, true)
	w.WriteHeader(resp.StatusCode)
	rpStreamCopy(w, resp.Body)
}

// proxyGeneric proxies with cookie injection but no notion-specific headers
func (rp *ReverseProxy) proxyGeneric(w http.ResponseWriter, r *http.Request, sess *ProxySession) {
	targetURL := notionOrigin + r.URL.RequestURI()

	req, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "cookie" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	setProxySessionCookies(req, sess)

	client := reverseProxyHTTPClient(30*time.Second, sess)
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rpCopyHeaders(w, resp, true)
	w.WriteHeader(resp.StatusCode)
	rpStreamCopy(w, resp.Body)
}

// proxyWithCookies proxies to a different origin with path rewriting
func (rp *ReverseProxy) proxyWithCookies(w http.ResponseWriter, r *http.Request, sess *ProxySession, targetOrigin, path string) {
	targetURL := targetOrigin + path
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "cookie" || lk == "origin" || lk == "referer" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	setProxySessionCookies(req, sess)
	req.Header.Set("Origin", notionOrigin)
	req.Header.Set("Referer", notionReferer)

	client := reverseProxyHTTPClient(0, sess)
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rpCopyHeaders(w, resp, false)
	w.WriteHeader(resp.StatusCode)
	rpStreamCopy(w, resp.Body)
}

// rpProxyPassthrough proxies without any cookie injection (for public assets)
func rpProxyPassthrough(w http.ResponseWriter, r *http.Request, targetOrigin string) {
	targetURL := targetOrigin + r.URL.RequestURI()

	req, err := http.NewRequest(r.Method, targetURL, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	client := getChromeHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rpCopyHeaders(w, resp, true)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// rpCopyHeaders copies response headers, stripping security & set-cookie headers
func rpCopyHeaders(w http.ResponseWriter, resp *http.Response, stripSecurity bool) {
	skip := map[string]bool{
		"set-cookie":        true,
		"transfer-encoding": true,
	}
	if stripSecurity {
		skip["content-security-policy"] = true
		skip["content-security-policy-report-only"] = true
		skip["x-frame-options"] = true
		skip["strict-transport-security"] = true
	}

	for k, vals := range resp.Header {
		if skip[strings.ToLower(k)] {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func isAllowedMsgstoreHost(host string) bool {
	return reAllowedMsgstoreHost.MatchString(host)
}

// proxyWebSocket does TCP-level WebSocket proxying via HTTP hijack
func (rp *ReverseProxy) proxyWebSocket(w http.ResponseWriter, r *http.Request, sess *ProxySession, targetHost, targetPath string) {
	if !isAllowedMsgstoreHost(targetHost) {
		http.Error(w, "invalid msgstore host", http.StatusBadRequest)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket not supported", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		log.Printf("[rproxy-ws] hijack error: %v", err)
		return
	}
	defer clientConn.Close()

	// Connect to target with HTTP/1.1 only (WebSocket requires HTTP/1.1).
	// Tunnel through the configured global notion proxy when set so the
	// real-time channel obeys the same egress policy as the rest of the
	// reverse proxy.
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dialCancel()
	rawConn, err := netutil.DialThroughProxy(dialCtx, "tcp", targetHost+":443", AppConfig.NotionProxyURL())
	if err != nil {
		log.Printf("[rproxy-ws] dial %s error: %v", targetHost, err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	targetConn := tls.Client(rawConn, &tls.Config{
		ServerName: targetHost,
		NextProtos: []string{"http/1.1"},
	})
	if err := targetConn.HandshakeContext(dialCtx); err != nil {
		rawConn.Close()
		log.Printf("[rproxy-ws] tls handshake %s: %v", targetHost, err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// Reconstruct the HTTP upgrade request for the target
	reqURI := targetPath
	if r.URL.RawQuery != "" {
		reqURI += "?" + r.URL.RawQuery
	}
	var buf strings.Builder
	buf.WriteString(fmt.Sprintf("%s %s HTTP/1.1\r\n", r.Method, reqURI))
	buf.WriteString(fmt.Sprintf("Host: %s\r\n", targetHost))
	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "cookie" || lk == "origin" || lk == "referer" {
			continue
		}
		for _, v := range vals {
			buf.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
		}
	}
	// Include account seeds plus this proxy session's dynamic and ALB cookies.
	jarURL, _ := url.Parse("https://" + targetHost + targetPath)
	cookieStr := proxySessionCookieHeader(sess, jarURL)
	buf.WriteString(fmt.Sprintf("Cookie: %s\r\n", cookieStr))
	buf.WriteString("Origin: " + notionOrigin + "\r\n")
	buf.WriteString("\r\n")

	if _, err := targetConn.Write([]byte(buf.String())); err != nil {
		log.Printf("[rproxy-ws] write upgrade error: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}

	// Read target's response and forward to client
	targetBuf := bufio.NewReader(targetConn)
	resp, err := http.ReadResponse(targetBuf, nil)
	if err != nil {
		log.Printf("[rproxy-ws] read response error: %v", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	if sess.CookieJar != nil {
		sess.CookieJar.SetCookies(jarURL, resp.Cookies())
	}
	resp.Header.Del("Set-Cookie")
	resp.Write(clientConn)

	if resp.StatusCode != http.StatusSwitchingProtocols {
		log.Printf("[rproxy-ws] unexpected status: %d", resp.StatusCode)
		return
	}

	log.Printf("[rproxy-ws] upgraded: %s%s", targetHost, targetPath)

	// Pipe data bidirectionally
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(targetConn, clientBuf)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, targetBuf)
		done <- struct{}{}
	}()
	<-done
}

// proxyMsgstoreHTTP proxies msgstore HTTP requests using the shared persistent client
func (rp *ReverseProxy) proxyMsgstoreHTTP(w http.ResponseWriter, r *http.Request, sess *ProxySession, targetHost, targetPath string) {
	if !isAllowedMsgstoreHost(targetHost) {
		http.Error(w, "invalid msgstore host", http.StatusBadRequest)
		return
	}

	targetURL := "https://" + targetHost + targetPath
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if lk == "host" || lk == "cookie" || lk == "origin" || lk == "referer" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}
	setProxySessionCookies(req, sess)
	req.Header.Set("Origin", notionOrigin)
	req.Header.Set("Referer", notionReferer)

	// The transport is shared for connection reuse; cookies remain session-local.
	client := newReverseProxyHTTPClient(0, rp.msgTransport, sess)
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	rpCopyHeaders(w, resp, false)
	w.WriteHeader(resp.StatusCode)
	rpStreamCopy(w, resp.Body)
}

// rpStreamCopy copies data with flushing for streaming responses (NDJSON etc.)
func rpStreamCopy(w http.ResponseWriter, src io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

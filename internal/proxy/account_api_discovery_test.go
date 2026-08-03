package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type accountDiscoveryRoundTripper func(*http.Request) (*http.Response, error)

func (f accountDiscoveryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func stubAccountDiscovery(t *testing.T, responseBody string, inspect func(*http.Request)) {
	t.Helper()
	originalConfig := AppConfig
	originalHTTPClient := discoveryHTTPClient
	originalFetchModels := discoveryFetchModels
	originalCheckQuota := discoveryCheckQuota
	AppConfig = DefaultConfig()
	discoveryHTTPClient = func(time.Duration) *http.Client {
		return &http.Client{Transport: accountDiscoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
			if inspect != nil {
				inspect(req)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(responseBody)),
				Request:    req,
			}, nil
		})}
	}
	discoveryFetchModels = func(*Account) ([]ModelEntry, error) { return nil, nil }
	discoveryCheckQuota = func(*Account) (*QuotaInfo, error) { return nil, nil }
	t.Cleanup(func() {
		AppConfig = originalConfig
		discoveryHTTPClient = originalHTTPClient
		discoveryFetchModels = originalFetchModels
		discoveryCheckQuota = originalCheckQuota
	})
}

func TestDiscoverAccountFromTokenWithOptionsSelectsExactTarget(t *testing.T) {
	responseBody := `{
  "recordMap": {
    "notion_user": {
      "decoy-user": {"value":{"value":{"name":"Decoy","email":"decoy@example.com"}}},
      "target-user": {"value":{"value":{"name":"Target","email":"target@example.com"}}}
    },
    "user_root": {
      "decoy-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"decoy-space","id":"decoy-view"}]}}},
      "target-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"target-space","id":"target-view"}]}}}
    },
    "space": {
      "decoy-space": {"value":{"value":{"id":"decoy-space","name":"Decoy Space","plan_type":"enterprise"}}},
      "target-space": {"value":{"value":{"id":"target-space","name":"Target Space","plan_type":"team"}}}
    },
    "user_settings": {}
  }
}`
	stubAccountDiscovery(t, responseBody, func(req *http.Request) {
		if got := req.Header.Get("x-notion-active-user-header"); got != "target-user" {
			t.Fatalf("active user header = %q, want target-user", got)
		}
		for name, want := range map[string]string{
			"token_v2":       "secret-token",
			"notion_user_id": "target-user",
			"notion_users":   `%5B%22target-user%22%5D`,
		} {
			cookie, err := req.Cookie(name)
			if err != nil {
				t.Fatalf("cookie %s: %v", name, err)
			}
			if cookie.Value != want {
				t.Fatalf("cookie %s = %q, want %q", name, cookie.Value, want)
			}
		}
	})

	account, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{
		ActiveUserID:  "target-user",
		SpaceID:       "target-space",
		ExpectedEmail: "TARGET@example.com",
	})
	if err != nil {
		t.Fatalf("DiscoverAccountFromTokenWithOptions: %v", err)
	}
	if account.UserID != "target-user" || account.UserEmail != "target@example.com" || account.SpaceID != "target-space" {
		t.Fatalf("discovered wrong account: user=%q email=%q space=%q", account.UserID, account.UserEmail, account.SpaceID)
	}
}

func TestHandleAddAccountUsesOptionalNotionUserID(t *testing.T) {
	responseBody := `{
  "recordMap": {
    "notion_user": {
      "target-user": {"value":{"value":{"name":"Target","email":"target@example.com"}}}
    },
    "user_root": {
      "target-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"target-space","id":"target-view"}]}}}
    },
    "space": {
      "target-space": {"value":{"value":{"id":"target-space","name":"Target Space","plan_type":"team"}}}
    },
    "user_settings": {}
  }
}`
	stubAccountDiscovery(t, responseBody, func(req *http.Request) {
		if got := req.Header.Get("x-notion-active-user-header"); got != "target-user" {
			t.Fatalf("active user header = %q, want target-user", got)
		}
	})

	pool := NewAccountPool()
	request := httptest.NewRequest(http.MethodPost, "/admin/accounts/add", strings.NewReader(
		`{"token_v2":"secret-token","notion_user_id":" target-user "}`,
	))
	recorder := httptest.NewRecorder()
	HandleAddAccount(pool, t.TempDir(), NewDashboardAuth("", "")).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if pool.Count() != 1 {
		t.Fatalf("pool count = %d, want 1", pool.Count())
	}
	if account := pool.GetByEmail("target@example.com"); account == nil || account.UserID != "target-user" {
		t.Fatalf("imported account = %#v, want target-user", account)
	}
}

func TestDiscoverAccountFromTokenWithOptionsRejectsMismatchedSelector(t *testing.T) {
	responseBody := `{
  "recordMap": {
    "notion_user": {
      "decoy-user": {"value":{"value":{"name":"Decoy","email":"decoy@example.com"}}}
    },
    "user_root": {
      "decoy-user": {"value":{"value":{"space_view_pointers":[{"spaceId":"decoy-space","id":"decoy-view"}]}}}
    },
    "space": {
      "decoy-space": {"value":{"value":{"id":"decoy-space","name":"Decoy Space","plan_type":"team"}}}
    }
  }
}`
	stubAccountDiscovery(t, responseBody, nil)

	account, err := DiscoverAccountFromTokenWithOptions("secret-token", AccountDiscoveryOptions{
		ExpectedEmail: "target@example.com",
		SpaceID:       "target-space",
	})
	if err == nil || !strings.Contains(err.Error(), "selectors") {
		t.Fatalf("discovery error = %v", err)
	}
	if account != nil {
		t.Fatalf("account = %#v, want nil", account)
	}
}

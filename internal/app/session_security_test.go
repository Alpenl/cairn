package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/representation"
	"webtag/internal/session"
)

var sessionSecuritySigningKey = []byte("cairn-session-security-signing-key")

type sessionSecurityVersions struct{}

func (sessionSecurityVersions) Current(_ context.Context) (representation.ClientIdentity, error) {
	return representation.NewClientIdentity(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
}

func newSessionSecurityRouter(token string, signingKey []byte) http.Handler {
	return NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
		ExtensionAPIToken:    token,
		SessionSigningKey:    signingKey,
		InstallationIdentity: sessionSecurityVersions{},
	})
}

func createBrowserSession(router http.Handler, token string, forwardedProto string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(`{"token":"`+token+`"}`))
	req.Header.Set("Content-Type", "application/json")
	if forwardedProto != "" {
		req.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

type sessionIdentityWire struct {
	ClientDataNamespace    string `json:"client_data_namespace"`
	RepresentationContract string `json:"representation_contract"`
	ExpiresAt              string `json:"expires_at"`
}

func decodeSessionIdentity(t *testing.T, rec *httptest.ResponseRecorder) sessionIdentityWire {
	t.Helper()
	var body sessionIdentityWire
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode session identity response %q: %v", rec.Body.String(), err)
	}
	return body
}

func sessionRequest(router http.Handler, method string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/session", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf {
		req.Header.Set(session.HeaderName, "1")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestSessionExchangeAndBearerShareInstallationIdentity(t *testing.T) {
	const installationToken = "single-installation-secret"
	router := newSessionSecurityRouter(installationToken, sessionSecuritySigningKey)

	created := createBrowserSession(router, installationToken, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("POST session status = %d, want 201; body=%q", created.Code, created.Body.String())
	}
	postIdentity := decodeSessionIdentity(t, created)
	if postIdentity.ExpiresAt == "" || postIdentity.ClientDataNamespace == "" || postIdentity.RepresentationContract != representation.Contract {
		t.Fatalf("POST identity = %#v, want expiry, namespace, and %s contract", postIdentity, representation.Contract)
	}
	if got := created.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("POST Cache-Control = %q, want private, no-store", got)
	}
	if got := created.Header().Get(representation.DataNamespaceHeader); got != postIdentity.ClientDataNamespace {
		t.Fatalf("POST marker = %q, body namespace = %q", got, postIdentity.ClientDataNamespace)
	}

	cookies := created.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != session.CookieName || cookies[0].Value == "" {
		t.Fatalf("POST session cookies = %#v, want one %s cookie", cookies, session.CookieName)
	}
	claims, err := session.Parse(cookies[0].Value, sessionSecuritySigningKey, time.Now())
	if err != nil || claims.ExpiresAt.IsZero() {
		t.Fatalf("issued session claims = %#v, error = %v", claims, err)
	}

	fromCookie := sessionRequest(router, http.MethodGet, cookies[0], true)
	if fromCookie.Code != http.StatusOK {
		t.Fatalf("cookie GET session status = %d, want 200; body=%q", fromCookie.Code, fromCookie.Body.String())
	}
	cookieIdentity := decodeSessionIdentity(t, fromCookie)
	if cookieIdentity.ExpiresAt != "" || cookieIdentity.ClientDataNamespace != postIdentity.ClientDataNamespace {
		t.Fatalf("cookie identity = %#v, POST identity = %#v", cookieIdentity, postIdentity)
	}

	bearerReq := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	bearerReq.Header.Set("Authorization", "Bearer "+installationToken)
	bearer := httptest.NewRecorder()
	router.ServeHTTP(bearer, bearerReq)
	if bearer.Code != http.StatusOK {
		t.Fatalf("bearer GET session status = %d, want 200", bearer.Code)
	}
	if got := decodeSessionIdentity(t, bearer).ClientDataNamespace; got != postIdentity.ClientDataNamespace {
		t.Fatalf("bearer namespace = %q, session namespace = %q", got, postIdentity.ClientDataNamespace)
	}
}

func TestSessionCookieSecurityAttributes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		forwardedProto string
		wantSecure     bool
	}{
		{name: "plain HTTP remains usable for localhost"},
		{name: "TLS-terminating proxy marks cookie secure", forwardedProto: "https", wantSecure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := newSessionSecurityRouter("installation-secret", sessionSecuritySigningKey)
			rec := createBrowserSession(router, "installation-secret", tc.forwardedProto)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201", rec.Code)
			}
			cookies := rec.Result().Cookies()
			if len(cookies) != 1 {
				t.Fatalf("cookies = %#v, want one", cookies)
			}
			cookie := cookies[0]
			if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
				t.Fatalf("cookie attributes = HttpOnly:%v SameSite:%v Path:%q", cookie.HttpOnly, cookie.SameSite, cookie.Path)
			}
			if cookie.Secure != tc.wantSecure {
				t.Fatalf("Secure = %v, want %v", cookie.Secure, tc.wantSecure)
			}
		})
	}
}

func TestBrowserSessionRequiresCSRFHeader(t *testing.T) {
	router := newSessionSecurityRouter("installation-secret", sessionSecuritySigningKey)
	created := createBrowserSession(router, "installation-secret", "")
	cookie := created.Result().Cookies()[0]

	withoutHeader := sessionRequest(router, http.MethodGet, cookie, false)
	if withoutHeader.Code != http.StatusUnauthorized {
		t.Fatalf("cookie without CSRF header status = %d, want 401", withoutHeader.Code)
	}
	withHeader := sessionRequest(router, http.MethodGet, cookie, true)
	if withHeader.Code != http.StatusOK {
		t.Fatalf("cookie with CSRF header status = %d, want 200", withHeader.Code)
	}
}

func TestSessionLogoutClearsCookie(t *testing.T) {
	router := newSessionSecurityRouter("installation-secret", sessionSecuritySigningKey)
	created := createBrowserSession(router, "installation-secret", "")
	cookie := created.Result().Cookies()[0]

	deleted := sessionRequest(router, http.MethodDelete, cookie, false)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE session status = %d, want 204", deleted.Code)
	}
	cookies := deleted.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != session.CookieName || cookies[0].MaxAge >= 0 || cookies[0].Value != "" {
		t.Fatalf("cleared cookies = %#v", cookies)
	}
	if !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatalf("cleared cookie attributes drifted: %#v", cookies[0])
	}
}

func TestSessionExchangeRejectsInvalidTokenAndOldField(t *testing.T) {
	router := newSessionSecurityRouter("installation-secret", sessionSecuritySigningKey)
	for _, body := range []string{
		`{"token":"wrong"}`,
		`{"api_key":"installation-secret"}`,
		`{}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("body %s status = %d, want 401", body, rec.Code)
		}
		for _, cookie := range rec.Result().Cookies() {
			if cookie.Name == session.CookieName && cookie.Value != "" {
				t.Errorf("body %s issued a session cookie", body)
			}
		}
	}
}

func TestSessionRoutesAreFailClosedOrUnmountedWhenConfigurationIsMissing(t *testing.T) {
	t.Parallel()
	closed := newSessionSecurityRouter("", sessionSecuritySigningKey)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		rec := sessionRequest(closed, method, nil, false)
		want := http.StatusUnauthorized
		if method != http.MethodGet {
			want = http.StatusNotFound
		}
		if rec.Code != want {
			t.Errorf("closed %s status = %d, want %d", method, rec.Code, want)
		}
	}

	withoutSigning := newSessionSecurityRouter("installation-secret", nil)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		if rec := sessionRequest(withoutSigning, method, nil, false); rec.Code != http.StatusNotFound {
			t.Errorf("without signing key %s status = %d, want 404", method, rec.Code)
		}
	}
}

func TestInvalidAuthorizationCannotDowngradeToOpenOrCookie(t *testing.T) {
	router := newSessionSecurityRouter("installation-secret", sessionSecuritySigningKey)
	created := createBrowserSession(router, "installation-secret", "")
	cookie := created.Result().Cookies()[0]

	for _, authorization := range []string{"Bearer wrong", "Basic Zm9vOmJhcg==", "Bearer"} {
		req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
		req.Header.Set("Authorization", authorization)
		req.Header.Set(session.HeaderName, "1")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q status = %d, want 401", authorization, rec.Code)
		}
	}
}

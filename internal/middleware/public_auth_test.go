package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/representation"
	"webtag/internal/session"
)

type publicAuthVersions struct {
	namespace uuid.UUID
	err       error
	keys      []string
}

func (v *publicAuthVersions) Current(_ context.Context, components representation.ComponentSet) (representation.VersionBase, error) {
	v.keys = append(v.keys, components.Key())
	if v.err != nil {
		return representation.VersionBase{}, v.err
	}
	return representation.VersionBase{RepresentationNamespace: v.namespace}, nil
}

func publicAuthRouter(opts PublicAuthOptions) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(PublicAuth(opts))
	router.GET("/probe", func(c *gin.Context) {
		identity, ok := representation.ClientIdentityFromContext(c.Request.Context())
		if !ok {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.String(http.StatusOK, identity.ClientDataNamespace)
	})
	return router
}

func requestPublicAuth(router http.Handler, authorization string, cookie *http.Cookie, csrf bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
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

func TestPublicAuthCredentialModesShareInstallationNamespace(t *testing.T) {
	t.Parallel()
	namespace := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	versions := &publicAuthVersions{namespace: namespace}
	key := []byte("test-public-auth-session-signing-key")
	signed, err := session.Sign(session.Claims{ExpiresAt: time.Now().Add(time.Hour)}, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	router := publicAuthRouter(PublicAuthOptions{
		Authenticator:   NewInstallationAuthenticator("installation-secret"),
		AllowOpenAccess: true,
		SessionKey:      key,
		Representations: versions,
	})

	requests := []struct {
		name          string
		authorization string
		cookie        *http.Cookie
		csrf          bool
	}{
		{name: "static bearer", authorization: "Bearer installation-secret"},
		{name: "browser session", cookie: &http.Cookie{Name: session.CookieName, Value: signed}, csrf: true},
		{name: "explicit open access"},
	}
	var wantNamespace string
	for _, request := range requests {
		rec := requestPublicAuth(router, request.authorization, request.cookie, request.csrf)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200; body=%q", request.name, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(representation.DataNamespaceHeader); got == "" || got != rec.Body.String() {
			t.Fatalf("%s marker = %q, body namespace = %q", request.name, got, rec.Body.String())
		}
		if wantNamespace == "" {
			wantNamespace = rec.Body.String()
		} else if rec.Body.String() != wantNamespace {
			t.Fatalf("%s namespace = %q, want shared namespace %q", request.name, rec.Body.String(), wantNamespace)
		}
	}
	for i, key := range versions.keys {
		if key != "" {
			t.Fatalf("representation read %d requested route components %q", i, key)
		}
	}
}

func TestPublicAuthIsFailClosedByDefault(t *testing.T) {
	t.Parallel()
	versions := &publicAuthVersions{namespace: uuid.New()}
	router := publicAuthRouter(PublicAuthOptions{
		Authenticator:   NewInstallationAuthenticator("installation-secret"),
		Representations: versions,
	})
	for _, authorization := range []string{"", "Bearer wrong", "Basic Zm9vOmJhcg==", "Bearer"} {
		rec := requestPublicAuth(router, authorization, nil, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q status = %d, want 401", authorization, rec.Code)
		}
		if got := rec.Header().Get(representation.DataNamespaceHeader); got != "" {
			t.Errorf("Authorization %q forged namespace marker %q", authorization, got)
		}
	}
}

func TestPublicAuthDoesNotDowngradeSuppliedBadCredentialToOpenAccess(t *testing.T) {
	t.Parallel()
	router := publicAuthRouter(PublicAuthOptions{
		Authenticator:   NewInstallationAuthenticator("installation-secret"),
		AllowOpenAccess: true,
		Representations: &publicAuthVersions{namespace: uuid.New()},
	})
	for _, authorization := range []string{"Bearer wrong", "Basic Zm9vOmJhcg==", "Bearer"} {
		if rec := requestPublicAuth(router, authorization, nil, false); rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q status = %d, want 401", authorization, rec.Code)
		}
	}
}

func TestPublicAuthSessionRequiresCookieAndCSRFHeader(t *testing.T) {
	t.Parallel()
	key := []byte("test-public-auth-session-signing-key")
	signed, err := session.Sign(session.Claims{ExpiresAt: time.Now().Add(time.Hour)}, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	router := publicAuthRouter(PublicAuthOptions{
		Authenticator:   NewInstallationAuthenticator("installation-secret"),
		SessionKey:      key,
		Representations: &publicAuthVersions{namespace: uuid.New()},
	})
	validCookie := &http.Cookie{Name: session.CookieName, Value: signed}
	for _, tc := range []struct {
		name   string
		cookie *http.Cookie
		csrf   bool
	}{
		{name: "cookie without csrf", cookie: validCookie},
		{name: "csrf without cookie", csrf: true},
		{name: "invalid cookie", cookie: &http.Cookie{Name: session.CookieName, Value: "invalid"}, csrf: true},
	} {
		rec := requestPublicAuth(router, "", tc.cookie, tc.csrf)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s status = %d, want 401", tc.name, rec.Code)
		}
	}
	if rec := requestPublicAuth(router, "", validCookie, true); rec.Code != http.StatusOK {
		t.Fatalf("valid session status = %d, want 200", rec.Code)
	}
}

func TestPublicAuthFailsClosedWhenInstallationNamespaceUnavailable(t *testing.T) {
	t.Parallel()
	for _, versions := range []VersionReader{
		nil,
		&publicAuthVersions{namespace: uuid.Nil},
		&publicAuthVersions{namespace: uuid.New(), err: errors.New("database unavailable")},
	} {
		router := publicAuthRouter(PublicAuthOptions{
			Authenticator:   NewInstallationAuthenticator("installation-secret"),
			Representations: versions,
		})
		rec := requestPublicAuth(router, "Bearer installation-secret", nil, false)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("versions %#v status = %d, want 500", versions, rec.Code)
		}
	}
}

// 这是端到端契约：会话在临近过期时被使用，响应必须带回一张新 cookie。少了
// 它，会话每过一个 TTL 就准时死一次，而 Reader 在 session 模式下刻意不保存
// installation token，届时手里没有任何凭证可以恢复，用户只能看到
// 「无法确认当前身份：unauthorized」并手动重填 token。滑动续期正是「登录一次
// 就不再掉线」赖以成立的机制：TTL 放长只是把窗口拉宽，真正让会话无限延续的
// 是这里每次使用都把期限推回去。
func TestPublicAuthRenewsASessionThatIsRunningOut(t *testing.T) {
	t.Parallel()
	key := []byte("test-public-auth-renewal-signing-key")
	now := time.Now()
	signed, err := session.Sign(session.Claims{
		ExpiresAt:         now.Add(time.Hour),
		AbsoluteExpiresAt: now.Add(session.DefaultAbsoluteTTL),
	}, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	router := publicAuthRouter(PublicAuthOptions{
		Authenticator:   NewInstallationAuthenticator("installation-secret"),
		SessionKey:      key,
		Representations: &publicAuthVersions{namespace: uuid.New()},
	})

	rec := requestPublicAuth(router, "", &http.Cookie{Name: session.CookieName, Value: signed}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	refreshed := sessionCookieFrom(t, rec)
	if refreshed == nil {
		t.Fatal("a session one hour from expiry must come back with a refreshed cookie")
	}
	claims, err := session.Parse(refreshed.Value, key, now)
	if err != nil {
		t.Fatalf("refreshed cookie does not parse: %v", err)
	}
	if !claims.ExpiresAt.After(now.Add(time.Hour)) {
		t.Fatalf("refreshed ExpiresAt = %s, want later than the original", claims.ExpiresAt)
	}
}

// 反向：会话还很新时不应该在每个响应上都挂 Set-Cookie。
func TestPublicAuthLeavesAFreshSessionAlone(t *testing.T) {
	t.Parallel()
	key := []byte("test-public-auth-fresh-signing-key")
	now := time.Now()
	signed, err := session.Sign(session.Claims{
		ExpiresAt:         now.Add(session.DefaultTTL),
		AbsoluteExpiresAt: now.Add(session.DefaultAbsoluteTTL),
	}, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	router := publicAuthRouter(PublicAuthOptions{
		Authenticator:   NewInstallationAuthenticator("installation-secret"),
		SessionKey:      key,
		Representations: &publicAuthVersions{namespace: uuid.New()},
	})

	rec := requestPublicAuth(router, "", &http.Cookie{Name: session.CookieName, Value: signed}, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cookie := sessionCookieFrom(t, rec); cookie != nil {
		t.Fatal("a fresh session must not put a Set-Cookie on every response")
	}
}

func sessionCookieFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range (&http.Response{Header: rec.Header()}).Cookies() {
		if cookie.Name == session.CookieName {
			return cookie
		}
	}
	return nil
}

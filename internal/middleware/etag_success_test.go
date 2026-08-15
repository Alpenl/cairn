package middleware

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/representation"
	"webtag/internal/service"
	"webtag/internal/session"
)

type selectedVersionReader struct {
	base  representation.VersionBase
	calls int
}

func (r *selectedVersionReader) Current(context.Context, representation.ComponentSet) (representation.VersionBase, error) {
	r.calls++
	return r.base, nil
}

func libraryComponentSet(t *testing.T) representation.ComponentSet {
	t.Helper()
	components, err := representation.NewComponentSet(representation.LibraryComponent)
	if err != nil {
		t.Fatalf("NewComponentSet(library) error = %v", err)
	}
	return components
}

func installConditionalIdentity(t *testing.T) gin.HandlerFunc {
	t.Helper()
	clientIdentity, err := representation.NewClientIdentity(representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
	})
	if err != nil {
		t.Fatalf("NewClientIdentity() error = %v", err)
	}
	return func(c *gin.Context) {
		ctx := representation.WithClientIdentity(c.Request.Context(), clientIdentity)
		c.Request = c.Request.WithContext(ctx)
		c.Header(representation.DataNamespaceHeader, clientIdentity.ClientDataNamespace)
		c.Next()
	}
}

func TestConditionalGetWildcardReturns304OnlyAfterSuccessfulHandler(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	reader := &selectedVersionReader{base: representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 5,
		}},
	}}
	router := gin.New()
	router.Use(installConditionalIdentity(t))
	router.Use(ConditionalGet(reader, ConditionalGetPolicy{
		"/probe": {
			Components: libraryComponentSet(t),
			NormalizeQuery: func(url.Values) (string, bool) {
				return "", true
			},
		},
	}))
	handlerCalls := 0
	router.GET("/probe", func(c *gin.Context) {
		handlerCalls++
		CacheableJSON(c, http.StatusOK, gin.H{"ok": true})
	})

	rec := get(router, "/probe", "*")
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if handlerCalls != 1 {
		t.Fatalf("handler calls = %d, want 1", handlerCalls)
	}
	if reader.calls != 1 {
		t.Fatalf("version reads = %d, want 1", reader.calls)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 body bytes = %d, want 0", rec.Body.Len())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("304 response has no ETag")
	}
}

func TestConditionalGetPointExactMatchWaitsForResourceExistence(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	reader := &selectedVersionReader{base: representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 8,
		}},
	}}
	router := gin.New()
	router.Use(installConditionalIdentity(t))
	router.Use(ConditionalGet(reader, ConditionalGetPolicy{
		"/api/links/:id": {
			Components: libraryComponentSet(t),
			NormalizeQuery: func(url.Values) (string, bool) {
				return "", true
			},
			MatchBeforeHandler: false,
		},
	}))
	exists := true
	handlerCalls := 0
	router.GET("/api/links/:id", func(c *gin.Context) {
		handlerCalls++
		if !exists {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		CacheableJSON(c, http.StatusOK, gin.H{"id": c.Param("id")})
	})

	first := get(router, "/api/links/known", "")
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || tag == "" {
		t.Fatalf("first response status=%d ETag=%q", first.Code, tag)
	}

	matched := get(router, "/api/links/known", tag)
	if matched.Code != http.StatusNotModified {
		t.Fatalf("existing resource exact status = %d, want 304", matched.Code)
	}
	if handlerCalls != 2 {
		t.Fatalf("handler calls after delayed exact match = %d, want 2", handlerCalls)
	}

	exists = false
	missing := get(router, "/api/links/known", tag)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing resource status = %d, want 404", missing.Code)
	}
	if handlerCalls != 3 {
		t.Fatalf("handler calls = %d, want 3", handlerCalls)
	}
	if got := missing.Header().Get("ETag"); got != "" {
		t.Fatalf("missing resource ETag = %q, want empty", got)
	}
}

func TestConditionalGetCanonicalizesQueryBeforeVersionLookup(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	reader := &selectedVersionReader{base: representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 11,
		}},
	}}
	normalize := func(values url.Values) (string, bool) {
		scope, ok := service.NormalizeTagLibraryKind(values.Get("library_kind"))
		if !ok {
			return "", false
		}
		if scope == "" {
			return "", true
		}
		return url.Values{"library_kind": {scope}}.Encode(), true
	}
	router := gin.New()
	router.Use(installConditionalIdentity(t))
	router.Use(ConditionalGet(reader, ConditionalGetPolicy{
		"/api/tags": {
			Components:         libraryComponentSet(t),
			NormalizeQuery:     normalize,
			MatchBeforeHandler: true,
		},
	}))
	handlerCalls := 0
	router.GET("/api/tags", func(c *gin.Context) {
		handlerCalls++
		scope, ok := service.NormalizeTagLibraryKind(c.Query("library_kind"))
		if !ok {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid library_kind"})
			return
		}
		CacheableJSON(c, http.StatusOK, gin.H{"scope": scope})
	})

	first := get(router, "/api/tags?library_kind=site", "")
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || tag == "" {
		t.Fatalf("canonical response status=%d ETag=%q", first.Code, tag)
	}
	equivalent := get(router, "/api/tags?library_kind=%20SITE%20", tag)
	if equivalent.Code != http.StatusNotModified {
		t.Fatalf("equivalent query status = %d, want 304", equivalent.Code)
	}
	if handlerCalls != 1 {
		t.Fatalf("handler calls = %d, want 1 after canonical exact match", handlerCalls)
	}

	readsBeforeInvalid := reader.calls
	invalid := get(router, "/api/tags?library_kind=unknown", "*")
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid query status = %d, want 422", invalid.Code)
	}
	if reader.calls != readsBeforeInvalid {
		t.Fatalf("version reads for invalid query = %d, want no increase from %d", reader.calls, readsBeforeInvalid)
	}
	if got := invalid.Header().Get("ETag"); got != "" {
		t.Fatalf("invalid query ETag = %q, want empty", got)
	}
}

func TestConditionalGetCacheHeadersMatchBetween200And304(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	reader := &selectedVersionReader{base: representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 13,
		}},
	}}
	router := gin.New()
	router.Use(installConditionalIdentity(t))
	router.Use(ConditionalGet(reader, ConditionalGetPolicy{
		"/api/tags": {
			Components: libraryComponentSet(t),
			NormalizeQuery: func(url.Values) (string, bool) {
				return "", true
			},
			MatchBeforeHandler: true,
		},
	}))
	router.GET("/api/tags", func(c *gin.Context) {
		CacheableJSON(c, http.StatusOK, gin.H{"tags": []string{"go"}})
	})

	okResponse := get(router, "/api/tags", "")
	tag := okResponse.Header().Get("ETag")
	if okResponse.Code != http.StatusOK || tag == "" {
		t.Fatalf("200 response status=%d ETag=%q", okResponse.Code, tag)
	}
	notModified := get(router, "/api/tags", tag)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", notModified.Code)
	}

	for _, name := range []string{
		"ETag",
		"Cache-Control",
		representation.DataNamespaceHeader,
	} {
		if got, want := notModified.Header().Get(name), okResponse.Header().Get(name); got == "" || got != want {
			t.Errorf("304 %s = %q, want 200 value %q", name, got, want)
		}
	}
	if got, want := notModified.Header().Values("Vary"), okResponse.Header().Values("Vary"); !slices.Equal(got, want) {
		t.Errorf("304 Vary = %v, want %v", got, want)
	}
	for _, credential := range []string{"Authorization", "Cookie", session.HeaderName} {
		found := false
		for _, value := range okResponse.Header().Values("Vary") {
			if value == credential {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("200 Vary = %v, missing %s", okResponse.Header().Values("Vary"), credential)
		}
	}
}

func TestConditionalGetFailSoftResponseOptsOutOfValidator(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	reader := &selectedVersionReader{base: representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 21,
		}},
	}}
	router := gin.New()
	router.Use(installConditionalIdentity(t))
	router.Use(ConditionalGet(reader, ConditionalGetPolicy{
		"/probe": {
			Components: libraryComponentSet(t),
			NormalizeQuery: func(url.Values) (string, bool) {
				return "", true
			},
		},
	}))
	router.GET("/probe", func(c *gin.Context) {
		MarkResponseNonCacheable(c)
		CacheableJSON(c, http.StatusOK, gin.H{"partial": true})
	})

	rec := get(router, "/probe", "*")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Fatalf("ETag = %q, want empty", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("Cache-Control = %q, want empty", got)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("fail-soft response body is empty")
	}
}

func TestConditionalGetHandlerFailurePublishesNoValidatorAndRetryRuns(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	reader := &selectedVersionReader{base: representation.VersionBase{
		RepresentationNamespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Components: []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: 34,
		}},
	}}
	router := gin.New()
	router.Use(installConditionalIdentity(t))
	router.Use(ConditionalGet(reader, ConditionalGetPolicy{
		"/probe": {
			Components: libraryComponentSet(t),
			NormalizeQuery: func(url.Values) (string, bool) {
				return "", true
			},
		},
	}))
	fail := true
	handlerCalls := 0
	router.GET("/probe", func(c *gin.Context) {
		handlerCalls++
		if fail {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "load failed"})
			return
		}
		CacheableJSON(c, http.StatusOK, gin.H{"ok": true})
	})

	failed := get(router, "/probe", "")
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("failed status = %d, want 500", failed.Code)
	}
	if got := failed.Header().Get("ETag"); got != "" {
		t.Fatalf("failed ETag = %q, want empty", got)
	}

	fail = false
	retry := get(router, "/probe", "\"validator-from-failed-response\"")
	if retry.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200", retry.Code)
	}
	if handlerCalls != 2 {
		t.Fatalf("handler calls = %d, want 2", handlerCalls)
	}
	if got := retry.Header().Get("ETag"); got == "" {
		t.Fatal("successful retry has no ETag")
	}
}

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/representation"
)

type stubRevisions struct {
	namespace uuid.UUID
	revision  int64
	invalid   bool
	err       error
	calls     int
}

func (s *stubRevisions) Current(_ context.Context, components representation.ComponentSet) (representation.VersionBase, error) {
	s.calls++
	namespace := s.namespace
	if namespace == uuid.Nil {
		namespace = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	}
	if s.invalid {
		namespace = uuid.Nil
	}
	base := representation.VersionBase{
		RepresentationNamespace: namespace,
	}
	if components.Key() == string(representation.LibraryComponent) {
		base.Components = []representation.Component{{
			Name:     representation.LibraryComponent,
			Revision: s.revision,
		}}
	}
	return base, s.err
}

func withConditionalTestIdentity(c *gin.Context) {
	withConditionalTestNamespace(uuid.MustParse("11111111-1111-1111-1111-111111111111"))(c)
}

func withConditionalTestNamespace(namespace uuid.UUID) gin.HandlerFunc {
	clientIdentity, err := representation.NewClientIdentity(representation.VersionBase{RepresentationNamespace: namespace})
	if err != nil {
		panic(err)
	}
	return func(c *gin.Context) {
		ctx := representation.WithClientIdentity(c.Request.Context(), clientIdentity)
		c.Request = c.Request.WithContext(ctx)
		c.Header(representation.DataNamespaceHeader, clientIdentity.ClientDataNamespace)
		c.Next()
	}
}

func testConditionalPolicy(matchBeforeHandler bool, routes ...string) ConditionalGetPolicy {
	components, err := representation.NewComponentSet(representation.LibraryComponent)
	if err != nil {
		panic(err)
	}
	policies := make(ConditionalGetPolicy, len(routes))
	for _, route := range routes {
		policies[route] = ConditionalRoutePolicy{
			Components: components,
			NormalizeQuery: func(values url.Values) (string, bool) {
				return values.Encode(), true
			},
			MatchBeforeHandler: matchBeforeHandler,
		}
	}
	return policies
}

func conditionalRouter(revisions *stubRevisions, handlerCalls *int) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	namespace := revisions.namespace
	if namespace == uuid.Nil {
		namespace = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	}
	router.Use(withConditionalTestNamespace(namespace))
	router.Use(ConditionalGet(revisions, testConditionalPolicy(true, "/api/links", "/api/tags")))
	handler := func(c *gin.Context) {
		*handlerCalls++
		CacheableJSON(c, http.StatusOK, gin.H{"items": []string{"a"}})
	}
	router.GET("/api/links", handler)
	router.GET("/api/tags", handler)
	router.GET("/api/export", handler) // 白名单之外
	router.POST("/api/links", handler)
	return router
}

func get(router *gin.Engine, path string, ifNoneMatch string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// TestConditionalGetSkipsHandlerOn304 是本中间件存在的全部理由。
//
// 常见的「哈希响应体」式 ETag 只省带宽——数据库该查还得查。这里 ETag 由
// installation-level 读版本号派生，因此匹配时 **handler 根本不执行**，省下的是整条
// 查询链路（列表要扫索引 + 算 COUNT，聚合要全表 GROUP BY）。
//
// handlerCalls 就是这条断言的证据：它为 0 说明数据库没有被碰过。
func TestConditionalGetSkipsHandlerOn304(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	router := conditionalRouter(&stubRevisions{revision: 7}, &handlerCalls)

	first := get(router, "/api/links", "")
	if first.Code != http.StatusOK {
		t.Fatalf("首次 status = %d, want 200", first.Code)
	}
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("首次响应没有 ETag")
	}
	if handlerCalls != 1 {
		t.Fatalf("首次 handler 调用 = %d, want 1", handlerCalls)
	}

	second := get(router, "/api/links", tag)
	if second.Code != http.StatusNotModified {
		t.Fatalf("带 If-None-Match 的 status = %d, want 304", second.Code)
	}
	if second.Body.Len() != 0 {
		t.Fatalf("304 响应体 = %d 字节, want 0", second.Body.Len())
	}
	if handlerCalls != 1 {
		t.Fatalf("304 路径上 handler 被调用了（总计 %d 次）—— 数据库仍被查询，本中间件白做", handlerCalls)
	}
}

// TestConditionalGetBreaksAfterRevisionBump 锁定写入之后客户端立刻看得到新数据。
// 这是条件请求最危险的失败形态：数据变了但一直回 304。
func TestConditionalGetBreaksAfterRevisionBump(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	revisions := &stubRevisions{revision: 1}
	router := conditionalRouter(revisions, &handlerCalls)

	first := get(router, "/api/links", "")
	tag := first.Header().Get("ETag")

	// 模拟一次写入：触发器推进版本号。
	revisions.revision = 2

	second := get(router, "/api/links", tag)
	if second.Code != http.StatusOK {
		t.Fatalf("版本号推进后 status = %d, want 200（否则写入永远看不见）", second.Code)
	}
	if second.Header().Get("ETag") == tag {
		t.Fatal("版本号推进后 ETag 没变")
	}
}

func TestConditionalGetSeparatesInstallationNamespaces(t *testing.T) {
	t.Parallel()

	firstCalls := 0
	first := conditionalRouter(&stubRevisions{
		namespace: uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		revision:  1,
	}, &firstCalls)
	secondCalls := 0
	second := conditionalRouter(&stubRevisions{
		namespace: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		revision:  1,
	}, &secondCalls)

	firstTag := get(first, "/api/links", "").Header().Get("ETag")
	secondTag := get(second, "/api/links", "").Header().Get("ETag")
	if firstTag == "" || secondTag == "" {
		t.Fatalf("ETags = %q and %q, want both non-empty", firstTag, secondTag)
	}
	if firstTag == secondTag {
		t.Fatalf("different installation namespaces shared ETag %q at the same revision", firstTag)
	}
}

func TestConditionalGetV3InvalidatesOlderValidator(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	router := conditionalRouter(&stubRevisions{revision: 1}, &handlerCalls)

	// This validator belongs to an older representation contract.
	rec := get(router, "/api/links", `"Ol-3e48yd-H1jkFnPxKo-Q"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("old validator status = %d, want 200 after the v3 contract bump", rec.Code)
	}
	if handlerCalls != 1 {
		t.Fatalf("handler calls = %d, want 1 for a stale validator", handlerCalls)
	}
	if got := rec.Header().Get("ETag"); got == "" || got == `"Ol-3e48yd-H1jkFnPxKo-Q"` {
		t.Fatalf("v3 ETag = %q, want a non-empty validator distinct from the old contract", got)
	}
}

func TestAuthenticatedNamespaceMarkerSurvives304AndHandlerError(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	versions := &stubRevisions{revision: 4}
	router := gin.New()
	router.Use(withConditionalTestIdentity)
	router.Use(ConditionalGet(versions, testConditionalPolicy(true, "/ok", "/error")))
	router.GET("/ok", func(c *gin.Context) { CacheableJSON(c, http.StatusOK, gin.H{"ok": true}) })
	router.GET("/error", func(c *gin.Context) { c.JSON(http.StatusInternalServerError, gin.H{"error": true}) })

	first := get(router, "/ok", "")
	marker := first.Header().Get(representation.DataNamespaceHeader)
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || marker == "" || tag == "" {
		t.Fatalf("first response status=%d marker=%q ETag=%q", first.Code, marker, tag)
	}
	notModified := get(router, "/ok", tag)
	if notModified.Code != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", notModified.Code)
	}
	if got := notModified.Header().Get(representation.DataNamespaceHeader); got != marker {
		t.Fatalf("304 marker = %q, want %q", got, marker)
	}

	failed := get(router, "/error", "")
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("error status = %d, want 500", failed.Code)
	}
	if got := failed.Header().Get(representation.DataNamespaceHeader); got != marker {
		t.Fatalf("error marker = %q, want %q", got, marker)
	}
	if got := failed.Header().Get("ETag"); got != "" {
		t.Fatalf("error response ETag = %q, want empty", got)
	}
}

// TestConditionalGetSeparatesQueriesAndRoutes 锁定不同查询 / 不同路由不会
// 共用同一个 ETag —— 它们共享安装版本号，但内容毫不相干。
func TestConditionalGetSeparatesQueriesAndRoutes(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	router := conditionalRouter(&stubRevisions{revision: 5}, &handlerCalls)

	tagged := get(router, "/api/links?tags=go", "").Header().Get("ETag")
	domained := get(router, "/api/links?domain=example.com", "").Header().Get("ETag")
	tags := get(router, "/api/tags", "").Header().Get("ETag")

	if tagged == domained {
		t.Fatal("不同查询串派生出了相同的 ETag")
	}
	if tagged == tags {
		t.Fatal("不同路由派生出了相同的 ETag")
	}

	// 拿 A 查询的 ETag 去请求 B 查询，必须是 200。
	if code := get(router, "/api/links?domain=example.com", tagged).Code; code != http.StatusNotModified {
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
	} else {
		t.Fatal("跨查询串误判 304：客户端会拿到另一个筛选的结果")
	}
}

// TestConditionalGetOnlyAppliesToWhitelistedGets 锁定白名单与方法限制。
//
// /api/export 系列是流式端点（handler 直写 c.Writer、按游标批次遍历整个知识库）。
// 用排除法而非白名单的话，将来新增一个流式端点就会默认落进条件请求层。
func TestConditionalGetOnlyAppliesToWhitelistedGets(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	router := conditionalRouter(&stubRevisions{revision: 3}, &handlerCalls)

	export := get(router, "/api/export", "")
	if export.Header().Get("ETag") != "" {
		t.Fatal("白名单之外的路由被加了 ETag")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/links", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Header().Get("ETag") != "" {
		t.Fatal("写请求被加了 ETag")
	}
}

// TestConditionalGetFallsBackWhenRevisionUnavailable 锁定它只是优化：
// 版本号取不到就退回普通请求，不能成为故障放大器。
func TestConditionalGetFallsBackWhenRevisionUnavailable(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	router := conditionalRouter(&stubRevisions{err: errors.New("db down")}, &handlerCalls)

	rec := get(router, "/api/links", `"anything"`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if handlerCalls != 1 {
		t.Fatalf("handler 调用 = %d, want 1（版本号不可用时必须正常执行）", handlerCalls)
	}
	if tag := rec.Header().Get("ETag"); tag != "" {
		t.Fatalf("ETag = %q, want empty when representation base is unavailable", tag)
	}
}

func TestConditionalGetFallsBackWhenNamespaceIsInvalid(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	router := conditionalRouter(&stubRevisions{revision: 1, invalid: true}, &handlerCalls)

	rec := get(router, "/api/links", `"anything"`)
	if rec.Code != http.StatusOK || handlerCalls != 1 {
		t.Fatalf("status = %d, handler calls = %d; want 200 and 1", rec.Code, handlerCalls)
	}
	if tag := rec.Header().Get("ETag"); tag != "" {
		t.Fatalf("ETag = %q, want empty for a zero namespace", tag)
	}
}

func TestConditionalGetWildcardDefersExistenceAndValidationToHandler(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	handlerCalls := 0
	router := gin.New()
	router.Use(withConditionalTestIdentity)
	router.Use(ConditionalGet(
		&stubRevisions{revision: 1},
		testConditionalPolicy(false, "/api/links/:id", "/api/tags"),
	))
	router.GET("/api/links/:id", func(c *gin.Context) {
		handlerCalls++
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
	})
	router.GET("/api/tags", func(c *gin.Context) {
		handlerCalls++
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid library_kind"})
	})

	for _, tc := range []struct {
		path string
		want int
	}{
		{path: "/api/links/missing", want: http.StatusNotFound},
		{path: "/api/tags?library_kind=invalid", want: http.StatusUnprocessableEntity},
	} {
		rec := get(router, tc.path, "*")
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.path, rec.Code, tc.want)
		}
		if tag := rec.Header().Get("ETag"); tag != "" {
			t.Errorf("%s: ETag = %q, want empty for an error response", tc.path, tag)
		}
	}
	if handlerCalls != 2 {
		t.Fatalf("handler calls = %d, want 2", handlerCalls)
	}
}

func TestConditionalGetDoesNotPublishETagForErrorResponses(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	for _, status := range []int{
		http.StatusNotFound,
		http.StatusUnprocessableEntity,
		http.StatusInternalServerError,
	} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			router := gin.New()
			router.Use(withConditionalTestIdentity)
			router.Use(ConditionalGet(&stubRevisions{revision: 3}, testConditionalPolicy(false, "/probe")))
			router.GET("/probe", func(c *gin.Context) {
				c.JSON(status, gin.H{"status": status})
			})

			rec := get(router, "/probe", "")
			if rec.Code != status {
				t.Fatalf("status = %d, want %d", rec.Code, status)
			}
			if tag := rec.Header().Get("ETag"); tag != "" {
				t.Fatalf("ETag = %q, want empty", tag)
			}
		})
	}
}

// TestConditionalGetDeclaresVaryOnCredentials 锁定 Vary: Authorization。
// 同一个 URL 可能使用不同认证载体，共享缓存仍必须区分凭据头。
func TestConditionalGetDeclaresVaryOnCredentials(t *testing.T) {
	t.Parallel()
	handlerCalls := 0
	router := conditionalRouter(&stubRevisions{revision: 1}, &handlerCalls)

	rec := get(router, "/api/links", "")
	found := false
	for _, value := range rec.Header().Values("Vary") {
		if value == "Authorization" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Vary = %v, 缺 Authorization", rec.Header().Values("Vary"))
	}
}

// TestMatchesETagParsesListsAndWildcard 覆盖 RFC 9110 §13.1.2 的解析形态。
func TestMatchesETagParsesListsAndWildcard(t *testing.T) {
	t.Parallel()
	const tag = `"abc"`

	cases := []struct {
		header string
		want   bool
	}{
		{header: `"abc"`, want: true},
		{header: `W/"abc"`, want: true},        // 弱比较
		{header: `"other", "abc"`, want: true}, // 多值列表
		{header: `*`, want: false},             // 通配必须在确认表示存在后处理
		{header: `"other"`, want: false},
		{header: ``, want: false},
		{header: `"ab"`, want: false}, // 不做前缀匹配
	}
	for _, tc := range cases {
		if got := matchesETag(tc.header, tag); got != tc.want {
			t.Errorf("matchesETag(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}

// TestConditionalGetSeparatesPathParameters 锁定不同路径参数不共用 ETag。
//
// 这条是审查实测复现的缺陷：ETag 此前用 c.FullPath()（路由**模板**），
// /api/links/A 与 /api/links/B 都得到 "/api/links/:link_id"，于是同一租户在
// 同一 revision 下的所有链接详情共用一个 ETag —— 拿 A 的 ETag 请求 B 会直接
// 304，客户端把 A 的内容当成 B 显示出来。
func TestConditionalGetSeparatesPathParameters(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	handlerCalls := 0
	router := gin.New()
	router.Use(withConditionalTestIdentity)
	router.Use(ConditionalGet(&stubRevisions{revision: 9}, testConditionalPolicy(false, "/api/links/:id")))
	router.GET("/api/links/:id", func(c *gin.Context) {
		handlerCalls++
		CacheableJSON(c, http.StatusOK, gin.H{"id": c.Param("id")})
	})

	first := get(router, "/api/links/AAAA", "")
	tagA := first.Header().Get("ETag")
	if tagA == "" {
		t.Fatal("详情响应没有 ETag")
	}

	second := get(router, "/api/links/BBBB", "")
	tagB := second.Header().Get("ETag")
	if tagA == tagB {
		t.Fatal("两条不同链接的详情共用了同一个 ETag")
	}

	// 拿 A 的 ETag 请求 B：必须正常返回 B，而不是 304。
	before := handlerCalls
	crossed := get(router, "/api/links/BBBB", tagA)
	if crossed.Code == http.StatusNotModified {
		t.Fatal("跨链接误判 304：客户端会把 A 的内容当成 B 显示")
	}
	if handlerCalls == before {
		t.Fatal("handler 未执行，说明被当成了缓存命中")
	}
}

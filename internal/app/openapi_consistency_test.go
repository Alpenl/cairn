package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/handler"
	"webtag/internal/model"
	"webtag/internal/service"
)

// TestOpenAPIRoutesInSync 守护 internal/app/assets/openapi.json 与实际注册
// 的 Gin 路由之间不会出现 drift。每次 PR 加/删/改路由但忘了同步 openapi.json
// （或反向，改了 spec 但路由没动），这条测试会失败并明确指出双向差异。
//
// 检查策略：
//  1. 用 NewRouterWithDependencies 构造一个挂满所有可选依赖的路由实例。
//  2. router.Routes() 拿到 method+path 二元组，过滤掉运维/UI 路径（白名单）
//     得到 API 契约清单。
//  3. 解析 openapi.json，把 paths × methods 展开为同样的二元组。
//  4. 双向 diff：openapi 多的（spec drift）和 router 多的（缺 spec）都 fail。
//
// 例外清单见 nonAPIPath：探针、指标、profile 与 Reader 页面不进 API
// 契约对比。
func TestOpenAPIRoutesInSync(t *testing.T) {
	router := NewRouterWithDependencies(
		fullSmokeDeps(),
		nil, nil,
		// SessionSigningKey 非空才会挂载 POST/DELETE /api/session；受鉴权的
		// GET identity 与签发能力无关。这里提供 key，确保三条方法都进入
		// router/spec 双向清单。
		RouterOptions{
			ExtensionAPIToken:    "test-installation-token",
			SessionSigningKey:    []byte("test-session-key"),
			InstallationIdentity: sessionSecurityVersions{},
		},
	)

	routerSet := apiRoutesFromRouter(router)
	specSet := apiRoutesFromOpenAPI(t)

	// 路由有但 spec 缺：调用方拿不到契约文档，最常见的 drift。
	var missingFromSpec []string
	for key := range routerSet {
		if _, ok := specSet[key]; !ok {
			missingFromSpec = append(missingFromSpec, key)
		}
	}
	// spec 有但路由没注册：通常是删了路由忘了改 openapi，或者写了一个
	// 期望但还没实现的路径，照样要报错让人决策。
	var missingFromRouter []string
	for key := range specSet {
		if _, ok := routerSet[key]; !ok {
			missingFromRouter = append(missingFromRouter, key)
		}
	}

	sort.Strings(missingFromSpec)
	sort.Strings(missingFromRouter)

	if len(missingFromSpec) > 0 || len(missingFromRouter) > 0 {
		t.Fatalf("OpenAPI / router drift detected.\n  routes missing from openapi.json (%d):\n    %s\n  spec entries missing from router (%d):\n    %s",
			len(missingFromSpec), strings.Join(missingFromSpec, "\n    "),
			len(missingFromRouter), strings.Join(missingFromRouter, "\n    "),
		)
	}
}

// apiRoutesFromRouter 枚举 *gin.Engine 注册的全部路由，归一化 path（gin 的
// :param / *wildcard 转成 OpenAPI {param} 形式），过滤掉运维/UI 白名单，
// 返回 "METHOD path" 集合，便于和 spec 直接做差。
func apiRoutesFromRouter(router *gin.Engine) map[string]struct{} {
	out := make(map[string]struct{})
	for _, r := range router.Routes() {
		path := normalizeGinPathToOpenAPI(r.Path)
		if isNonAPIPath(path) {
			continue
		}
		out[r.Method+" "+path] = struct{}{}
	}
	return out
}

// apiRoutesFromOpenAPI 解析嵌入的 openapi.json，把 paths × method 展开成
// "METHOD path" 集合，过滤掉运维/UI 白名单。
func apiRoutesFromOpenAPI(t *testing.T) map[string]struct{} {
	t.Helper()
	data, err := readOpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec() returned error: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("openapi.json: unmarshal failed: %v", err)
	}

	// OpenAPI path-item 对象里除了 HTTP 方法名还能放 parameters/summary 等
	// 非动词字段；这里只挑出 method 字段（HTTP verb 小写）。
	httpMethods := map[string]string{
		"get":     http.MethodGet,
		"post":    http.MethodPost,
		"put":     http.MethodPut,
		"patch":   http.MethodPatch,
		"delete":  http.MethodDelete,
		"head":    http.MethodHead,
		"options": http.MethodOptions,
		"trace":   http.MethodTrace,
	}

	out := make(map[string]struct{})
	for path, item := range spec.Paths {
		if isNonAPIPath(path) {
			continue
		}
		for verb, method := range httpMethods {
			if _, ok := item[verb]; ok {
				out[method+" "+path] = struct{}{}
			}
		}
	}
	return out
}

// normalizeGinPathToOpenAPI 把 gin 风格的路径转为 OpenAPI {param} 形式：
//   - ":link_id" → "{link_id}"
//   - "*filepath" → "{filepath}"
func normalizeGinPathToOpenAPI(p string) string {
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		switch {
		case strings.HasPrefix(seg, ":"):
			segments[i] = "{" + seg[1:] + "}"
		case strings.HasPrefix(seg, "*"):
			segments[i] = "{" + seg[1:] + "}"
		}
	}
	return strings.Join(segments, "/")
}

// isNonAPIPath 是双侧共享的白名单：这些路径属于运维 / UI 层，不是公开
// API 契约的一部分，不参与 spec ↔ 路由的对比。
//
//   - /、/reader/* —— Reader 入口和静态产物通道，不算 API 端点。
//   - /health、/ready —— 探针端点，归 ops；spec 已经描述，但归类不属于
//     API 契约的核心，与路由侧一致跳过即可。
func isNonAPIPath(path string) bool {
	switch path {
	case "/health", "/ready", "/":
		return true
	}
	// /reader/* 是内嵌 Reader SPA 的静态产物通道（gin 注册为 /reader/*filepath）。
	if path == "/reader" || strings.HasPrefix(path, "/reader/") {
		return true
	}
	return false
}

// fullSmokeDeps 挂载全部业务路由，方便 router.Routes() 覆盖完整 API 表面。
func fullSmokeDeps() handler.Dependencies {
	return handler.Dependencies{
		// Route registration is intentionally exercised even though this test
		// never invokes the Reader handlers. An embedded interface is enough to
		// expose the complete method set without duplicating a no-op implementation
		// for every Reader vNext operation.
		Reader: handler.ReaderRoutes{
			Thoughts: smokeReaderService{}, Library: smokeReaderService{},
			Notes: smokeReaderService{}, Hosts: smokeReaderService{},
			Inbox: smokeReaderService{}, Todos: smokeReaderService{},
		},
		LinksWrite:        smokeLinkWriteService{},
		LinksRead:         smokeLinkReadService{},
		LinksContent:      smokeLinkContentService{},
		ConversionPreview: smokeConversionPreviewService{},
		ConversionExecute: smokeConversionExecuteService{},
		Translations:      smokeTranslationService{},
		Ingest:            smokeIngestService{},
		Tags:              smokeTagService{},
		Tree:              smokeTreeService{},
		LibrarySearch:     smokeLibrarySearchService{},
		SiteMerge:         smokeSiteMergeService{},
		SiteSplit:         smokeSiteSplitService{},
		ArchiveV2:         smokeArchiveV2Service{},
		Feeds:             smokeFeedService{},
	}
}

type smokeReaderService struct {
	handler.ReaderThoughtRoutes
	handler.ReaderNoteRoutes
	handler.ReaderInboxRoutes
	handler.ReaderTodoRoutes
	handler.ReaderLibraryRoutes
	handler.ReaderHostRoutes
}

func (smokeReaderService) RestoreHost(context.Context, string, string) (dto.ReaderHostLifecycleResponse, error) {
	return dto.ReaderHostLifecycleResponse{}, nil
}

func (smokeReaderService) PurgeHost(context.Context, string, string, dto.ReaderHostPurgeRequest) error {
	return nil
}

func (smokeReaderService) ListTrash(context.Context, string, string, int) (dto.ReaderTrashResponse, error) {
	return dto.ReaderTrashResponse{}, nil
}

type smokeTranslationService struct{}

type smokeLibrarySearchService struct{}

type smokeSiteMergeService struct{}
type smokeSiteSplitService struct{}
type smokeArchiveV2Service struct{}

func (smokeSiteMergeService) Preview(context.Context, dto.SiteMergePreviewRequest) (dto.SiteMergePreviewResponse, error) {
	return dto.SiteMergePreviewResponse{}, nil
}
func (smokeSiteMergeService) Execute(context.Context, dto.SiteMergeExecuteRequest) (dto.SiteMergeExecuteResponse, error) {
	return dto.SiteMergeExecuteResponse{}, nil
}
func (smokeSiteSplitService) Preview(context.Context, string, dto.SiteSplitRequest) (dto.SiteSplitPreviewResponse, error) {
	return dto.SiteSplitPreviewResponse{}, nil
}
func (smokeSiteSplitService) Execute(context.Context, string, dto.SiteSplitRequest) (dto.SiteSplitExecuteResponse, error) {
	return dto.SiteSplitExecuteResponse{}, nil
}
func (smokeArchiveV2Service) Export(context.Context, io.Writer, service.ArchiveV2ExportOptions) error {
	return nil
}

func (smokeLibrarySearchService) Search(context.Context, string, int, int, int, string) (dto.GroupedSearchResponse, error) {
	return dto.GroupedSearchResponse{}, nil
}

type smokeConversionPreviewService struct{}

func (smokeConversionPreviewService) Preview(context.Context, string, dto.ConversionPreviewRequest) (dto.ConversionPreviewResponse, error) {
	return dto.ConversionPreviewResponse{}, nil
}

type smokeConversionExecuteService struct{}

func (smokeConversionExecuteService) Execute(context.Context, string, dto.ConversionExecuteRequest) (dto.ConversionExecuteResponse, error) {
	return dto.ConversionExecuteResponse{}, nil
}

func (smokeTranslationService) Create(context.Context, uuid.UUID, model.TranslationRequest) (*model.LinkTranslation, error) {
	return &model.LinkTranslation{}, nil
}

func (smokeTranslationService) List(context.Context, uuid.UUID) (model.TranslationList, error) {
	return model.TranslationList{}, nil
}

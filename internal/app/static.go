package app

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed assets/openapi.json assets/scalar.html assets/admin_concept_merges.html
var assetFS embed.FS

// staticAssets is resolved once at init so StaticFS() does not pay the
// fs.Sub cost (and the unreachable error branch) on every invocation.
// The //go:embed directive guarantees assets/ exists at compile time, so
// any failure here means the binary itself is corrupt — fail-fast at
// load time, mirroring internal/security/host.go's mustCIDR pattern.
var staticAssets = mustSubAssets()

func mustSubAssets() http.FileSystem {
	sub, err := fs.Sub(assetFS, "assets")
	if err != nil {
		panic("internal/app: embedded assets/ subtree missing — build artifact corrupt: " + err.Error())
	}
	return http.FS(sub)
}

// OpenAPISpec returns the embedded OpenAPI 3.1 specification as JSON bytes.
// The spec is hand-written and committed alongside the handler code; PRs
// touching routes or DTOs must keep it in sync.
func OpenAPISpec() ([]byte, error) {
	return assetFS.ReadFile("assets/openapi.json")
}

// ScalarHTML returns the embedded Scalar API Reference viewer HTML, which
// loads /openapi.json from the same origin via a CDN-hosted script.
func ScalarHTML() ([]byte, error) {
	return assetFS.ReadFile("assets/scalar.html")
}

// AdminConceptMergesHTML returns the embedded review UI that drives the
// /api/admin/concept-merges endpoints. Mounted at /admin/concept-merges
// whenever the concept-merge review service is wired, including for proposals
// retained from older deployments.
func AdminConceptMergesHTML() ([]byte, error) {
	return assetFS.ReadFile("assets/admin_concept_merges.html")
}

// StaticFS 返回 assets/ 内嵌目录对应的 http.FileSystem，供 Gin 在
// /static/* 路径下直接 serve 静态资源。
func StaticFS() http.FileSystem {
	return staticAssets
}

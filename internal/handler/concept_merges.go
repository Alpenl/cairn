// Package handler 汇总 WebTag HTTP API 的全部 Gin handler：将路由绑定到
// 业务 service，承担请求体解码、参数校验、错误到 HTTP 状态码的映射等
// 表现层职责。各 service 接口在本包内定义（ISP），以便测试用轻量 stub
// 注入，同时让 service 层与 HTTP 框架完全解耦。
package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/middleware"
	"webtag/internal/repository"
)

// ConceptMergeService is the handler-facing contract for the admin
// review surface. Backed in production by concept.MergeAdminService,
// which wraps the proposal repo (list/get/reject) plus the merge
// transactional repo (approve).
//
// Reject is just MarkProposalDecided with a "rejected" status; it
// shares the handler shape with Approve so the JS in the admin UI
// can hit either endpoint with the same payload.
type ConceptMergeService interface {
	ListPending(ctx context.Context, limit, offset int) ([]ConceptMergeView, error)
	Approve(ctx context.Context, proposalID uuid.UUID, decidedBy string) error
	Reject(ctx context.Context, proposalID uuid.UUID, decidedBy string) error
}

// ConceptMergeView is the read DTO surfaced to the admin UI. We
// project the proposal + a snapshot of each side's primary_name so
// the UI does not need to round-trip back for labels.
type ConceptMergeView struct {
	ID             uuid.UUID `json:"id"`
	WinnerID       uuid.UUID `json:"winner_id"`
	WinnerName     string    `json:"winner_name"`
	WinnerUseCount int       `json:"winner_use_count"`
	LoserID        uuid.UUID `json:"loser_id"`
	LoserName      string    `json:"loser_name"`
	LoserUseCount  int       `json:"loser_use_count"`
	Score          float32   `json:"score"`
	LLMReason      string    `json:"llm_reason"`
	CreatedAtUnix  int64     `json:"created_at_unix"`
}

// RegisterConceptMergeRoutes 把 /api/admin/concept-merges 的列表 / 通过
// / 拒绝三条路由挂到传入的 group 上。参数采用更窄的 gin.IRoutes（仅暴
// 露 .GET / .POST 等方法），让调用方可以传入"已经 wrap 过 BearerAuth
// 的 router group"——这是 Wave 12.5 H2 把 admin 路由统一收到带鉴权
// group 之下的必要条件，主 router 不再有任何接触 admin 路由的入口。
//
// svc == nil 时直接 no-op：admin UI 是可选依赖，concept judge poller 关
// 闭时不挂这些路由也合理。
func RegisterConceptMergeRoutes(router gin.IRoutes, svc ConceptMergeService) {
	if svc == nil {
		return
	}
	router.GET("/api/admin/concept-merges", listConceptMerges(svc))
	router.POST("/api/admin/concept-merges/:id/approve", decideConceptMerge(svc, true))
	router.POST("/api/admin/concept-merges/:id/reject", decideConceptMerge(svc, false))
}

// listConceptMerges 处理 GET /api/admin/concept-merges：分页返回待人工
// 复核的概念合并候选。
func listConceptMerges(svc ConceptMergeService) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := parseIntQuery(c, "limit", 50, 1, 200)
		offset := parseIntQuery(c, "offset", 0, 0, 10_000)
		rows, err := svc.ListPending(c.Request.Context(), limit, offset)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": rows})
	}
}

// maxAdminActorLen 限制 X-Admin-Actor 头写入审计字段的最大字节数。
// 上游反向代理 / SSO 网关可能错配出一个超长 token（JWT、邮件 + 路径），
// 直接落到 audit 字段会撑大 DB 行、拖慢索引、给恶意客户端一个低成本
// 占用磁盘的 vector。64 字节够装 SSO username + uuid + 短前缀，超出
// 直接截断即可——审计场景关心"谁"，不关心多余的 query string。
const maxAdminActorLen = 64

// truncateAdminActor 把 actor 收敛到 maxAdminActorLen 字节内。按字节
// 而非 rune 截断是有意为之：审计目的不需要语义完整的 UTF-8 字符串，只
// 需要稳定可比对的 identifier；DB 字段存的也是 bytes。极端情况下截断
// 可能把多字节字符切一半，DB 层用 BYTEA / TEXT 都能容下，比"按 rune
// 截然后字节数还可能超"更可预测。
func truncateAdminActor(actor string) string {
	if len(actor) > maxAdminActorLen {
		return actor[:maxAdminActorLen]
	}
	return actor
}

// decideConceptMerge 处理 POST /api/admin/concept-merges/:id/approve
// 与 .../reject：根据 approve 参数把合并提案标记为已通过或已拒绝，并
// 把 X-Admin-Actor 头记入审计字段。
func decideConceptMerge(svc ConceptMergeService, approve bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := uuid.Parse(c.Param("id"))
		if err != nil {
			middleware.JSONErrorWithSlug(c, http.StatusBadRequest, middleware.ErrCodeInvalidProposalID, "invalid proposal id")
			return
		}
		// decided_by: prefer the X-Admin-Actor header (set by reverse
		// proxy or admin SSO) over a fallback "admin" label. Keeps the
		// audit field meaningful when the deployment integrates with
		// upstream identity; degrades gracefully otherwise. 同时硬性截断
		// 到 maxAdminActorLen 字节，防御性收敛上游误配的超长 token。
		actor := truncateAdminActor(c.GetHeader("X-Admin-Actor"))
		if actor == "" {
			actor = "admin"
		}
		if approve {
			err = svc.Approve(c.Request.Context(), id, actor)
		} else {
			err = svc.Reject(c.Request.Context(), id, actor)
		}
		if err != nil {
			if errors.Is(err, repository.ErrConceptMergeAlreadyDecided) {
				middleware.JSONErrorWithSlug(c, http.StatusConflict, middleware.ErrCodeProposalAlreadyDecided, "proposal already decided")
				return
			}
			if errors.Is(err, repository.ErrConceptMergeOrphaned) {
				middleware.JSONErrorWithSlug(c, http.StatusConflict, middleware.ErrCodeProposalAlreadyDecided, "proposal references a missing concept")
				return
			}
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// parseIntQuery reads c.Query(name), clamps to [min, max], falls
// back to def on parse failure or empty. Kept inline (vs a shared
// helper) because the rest of the handler package parses ints
// inline too.
func parseIntQuery(c *gin.Context, name string, def, min, max int) int {
	raw := c.Query(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

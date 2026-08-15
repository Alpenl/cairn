package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/middleware"
)

// submitLink 处理 POST /api/links：解码单条 URL 的提交请求，交给
// 写服务异步入库后返回 202 + job_id。
func submitLink(service LinkWriteService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req dto.LinkCreateRequest
		if !bindJSONWithLimit(c, &req, defaultMaxJSONBodyBytes) {
			return
		}

		resp, err := service.Submit(c.Request.Context(), req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, resp)
	}
}

// batchLinks 处理 POST /api/links/batch：批量提交 URL，逐条调度解析
// 并把每条结果聚合在响应中返回。
func batchLinks(service LinkWriteService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req dto.BatchCreateRequest
		if !bindJSONWithLimit(c, &req, defaultMaxJSONBodyBytes) {
			return
		}

		resp, err := service.Batch(c.Request.Context(), req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, resp)
	}
}

// ingestSubmission 处理 POST /api/ingest：通用 text/image 多源请求的 body
// 上限为 4 MiB。第一方 browser_capture 分别发送 512 KiB 以内的脱敏可读文本
// 与正文 HTML 结构片段，不上传整页 DOM 或图片列表。
func ingestSubmission(service IngestService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req dto.IngestRequest
		if !bindJSONWithLimit(c, &req, ingestMaxJSONBodyBytes) {
			return
		}

		resp, err := service.Ingest(c.Request.Context(), req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, resp)
	}
}

// refreshLink 处理 POST /api/links/:link_id/refresh：触发对已存在
// 链接的重新抓取与重新分析。
func refreshLink(service LinkWriteService) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := service.Refresh(c.Request.Context(), c.Param("link_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, resp)
	}
}

// getLinkContent handles GET /api/links/:link_id/content —— 只读已保存原文，
// 不抓取、不写库。没有已保存原文时返回 404（"还没保存"不是错误状态，是没东西可给）。
func getLinkContent(service LinkContentService) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := service.Get(c.Request.Context(), c.Param("link_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

// saveLinkContent handles idempotent POST /api/links/:link_id/content. It
// stores canonical plain text plus an optional safe structured document and
// is only valid for a completed link unless a snapshot already exists.
func saveLinkContent(service LinkContentService) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := service.Save(c.Request.Context(), c.Param("link_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

// replaceLinkContent handles PUT /api/links/:link_id/content. It is the
// explicit destructive counterpart to idempotent Save and always resolves a
// fresh snapshot for the current completed source revision.
func replaceLinkContent(service LinkContentService) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := service.Replace(c.Request.Context(), c.Param("link_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

// editLinkContent handles PATCH /api/links/:link_id/content. The body limit
// is larger than the service's decoded UTF-8 limit so JSON escaping does not
// reject otherwise valid source text; the service remains authoritative for
// the content-size and normalization rules.
func editLinkContent(service LinkContentService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req dto.ContentEditRequest
		if !bindJSONWithLimitAndCode(c, &req, contentEditMaxJSONBodyBytes, httperr.CodeContentTooLarge) {
			return
		}

		resp, err := service.Edit(c.Request.Context(), c.Param("link_id"), req)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

// listLinks 处理 GET /api/links：按 tags / domain / content_type 过滤
// 并分页返回链接列表，支持 ?after= 游标模式与传统 page/limit 偏移模式
// 两种翻页方式。
func listLinks(service LinkReadService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// after presence (even empty) opts into cursor mode. ?after=
		// alone means "start of stream"; ?after=<token> continues.
		// Plain GET stays in offset mode for the existing UI.
		after, cursorMode := c.GetQuery("after")
		resp, err := service.List(c.Request.Context(), dto.ListLinksRequest{
			Tags:          c.Query("tags"),
			Domain:        c.Query("domain"),
			ContentType:   c.Query("content_type"),
			LibraryKind:   c.Query("library_kind"),
			CreatedFrom:   c.Query("created_from"),
			CreatedBefore: c.Query("created_before"),
			LowConfidence: c.Query("low_confidence"),
			Status:        c.Query("status"),
			Query:         c.Query("q"),
			URL:           c.Query("url"),
			Page:          queryInt(c, "page", 1),
			Limit:         queryInt(c, "limit", 20),
			Cursor:        cursorMode,
			After:         after,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		middleware.CacheableJSON(c, http.StatusOK, resp)
	}
}

// getLink 处理 GET /api/links/:link_id：返回单条链接的完整详情。
func getLink(readService LinkReadService) gin.HandlerFunc {
	return func(c *gin.Context) {
		// include_content 默认 true：老客户端行为不变。只有显式传 false 的
		// 客户端（Reader）才拿到不含正文的详情。
		includeContent := c.Query("include_content") != "false"
		resp, err := readService.GetWithContent(c.Request.Context(), c.Param("link_id"), includeContent)
		if err != nil {
			writeError(c, err)
			return
		}
		middleware.CacheableJSON(c, http.StatusOK, resp)
	}
}

// deleteLink 处理 DELETE /api/links/:link_id：删除链接并附带相关缓存
// 失效，成功返回 200；重复软删是同样的成功 no-op。
func deleteLink(service LinkReadService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := service.Delete(c.Request.Context(), c.Param("link_id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusOK)
	}
}

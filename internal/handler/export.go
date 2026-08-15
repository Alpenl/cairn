package handler

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"webtag/internal/httperr"
	"webtag/internal/representation"
	"webtag/internal/service"
)

// exportLinks handles GET /api/export: streams the full done-link knowledge
// base as a JSON array straight to the response writer. The service walks the
// table in cursor batches, so neither the service nor this handler ever buffers
// more than one batch — the library can grow without blowing the response
// memory budget.
//
// Streaming means the 200 status + headers are committed the moment the first
// byte is written; a mid-stream service error can no longer become a clean JSON
// 500. We therefore record such errors via c.Error (picked up by the access-log
// middleware) and let the client detect the truncated array and retry. This is
// the standard trade-off for a streaming bulk endpoint and is documented on
// service.LinkReadService.Export.
func exportLinks(service LinkReadService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filename := "webtag-export-" + time.Now().UTC().Format("2006-01-02") + ".json"
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Status(http.StatusOK)

		if err := service.Export(c.Request.Context(), c.Writer); err != nil {
			// Headers/status are already on the wire — surface the error to the
			// access log without trying to rewrite the body. The array is left
			// truncated (invalid JSON) so the client knows to re-request.
			_ = c.Error(err)
			return
		}
	}
}

// exportConcepts handles GET /api/export/concepts by streaming the
// installation vocabulary as a JSON array. Like exportLinks, a mid-stream
// error truncates the array and is surfaced through c.Error.
func exportConcepts(service LinkReadService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filename := "webtag-concepts-" + time.Now().UTC().Format("2006-01-02") + ".json"
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Status(http.StatusOK)

		if err := service.ExportConcepts(c.Request.Context(), c.Writer); err != nil {
			_ = c.Error(err)
			return
		}
	}
}

func exportArchiveV2(archive interface {
	ValidateSelection(service.ArchiveV2Selection) error
	ExportSelected(context.Context, io.Writer, service.ArchiveV2ExportOptions) error
}) gin.HandlerFunc {
	return func(c *gin.Context) {
		query, queryErr := url.ParseQuery(c.Request.URL.RawQuery)
		if queryErr != nil {
			// URL.Query intentionally discards malformed pairs. This endpoint has
			// a closed selector contract, so let ParseArchiveV2Sections emit its
			// stable 422 instead of treating a dropped selector as omission.
			_, err := service.ParseArchiveV2Sections(nil, true)
			writeError(c, err)
			return
		}
		values, present := query["sections"]
		selection, err := service.ParseArchiveV2Sections(values, present)
		if err != nil {
			writeError(c, err)
			return
		}
		identity, ok := representation.ClientIdentityFromContext(c.Request.Context())
		if !ok {
			writeError(c, httperr.New(http.StatusInternalServerError, "archive identity is unavailable"))
			return
		}
		options := service.ArchiveV2ExportOptions{
			Selection:           selection,
			ClientDataNamespace: identity.ClientDataNamespace,
		}
		if err := archive.ValidateSelection(options.Selection); err != nil {
			writeError(c, err)
			return
		}
		filename := "webtag-archive-v2-" + time.Now().UTC().Format("2006-01-02") + ".json"
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Status(http.StatusOK)
		if err := archive.ExportSelected(c.Request.Context(), c.Writer, options); err != nil {
			_ = c.Error(err)
		}
	}
}

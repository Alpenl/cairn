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

func exportArchiveV2(archive interface {
	Export(context.Context, io.Writer, service.ArchiveV2ExportOptions) error
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
		filename := "webtag-archive-v2-" + time.Now().UTC().Format("2006-01-02") + ".json"
		c.Header("Content-Type", "application/json; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.Status(http.StatusOK)
		if err := archive.Export(c.Request.Context(), c.Writer, options); err != nil {
			_ = c.Error(err)
		}
	}
}

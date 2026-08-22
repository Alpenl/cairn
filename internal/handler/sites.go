package handler

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
)

type SiteReadService interface {
	List(context.Context, string, string, string, int, int) (dto.PaginatedSitesResponse, error)
	Get(context.Context, string) (dto.SiteDetailResponse, error)
}

type SiteManagementService interface {
	Update(context.Context, string, string, dto.SiteUpdateRequest) (dto.SiteDetailResponse, error)
	UpdateEntry(context.Context, string, string, string, dto.SiteEntryUpdateRequest) (dto.SiteDetailResponse, error)
	SetPrimaryEntry(context.Context, string, string, string) (dto.SiteDetailResponse, error)
	DeleteEntry(context.Context, string, string, string) (dto.SiteEntryDeleteResponse, error)
	Delete(context.Context, string, string, string) error
}

func listSites(service SiteReadService) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := service.List(c.Request.Context(), c.Query("view"), c.Query("tags"), c.Query("recent_cutoff"), queryInt(c, "page", 1), queryInt(c, "limit", 30))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}
func getSite(service SiteReadService) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := service.Get(c.Request.Context(), c.Param("site_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func updateSite(service SiteManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.SiteUpdateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.Update(c.Request.Context(), c.Param("site_id"), c.GetHeader("If-Match"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func updateSiteEntry(service SiteManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.SiteEntryUpdateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		out, err := service.UpdateEntry(c.Request.Context(), c.Param("site_id"), c.Param("entry_id"), c.GetHeader("If-Match"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func setSitePrimaryEntry(service SiteManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := service.SetPrimaryEntry(c.Request.Context(), c.Param("site_id"), c.Param("entry_id"), c.GetHeader("If-Match"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func deleteSiteEntry(service SiteManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := service.DeleteEntry(c.Request.Context(), c.Param("site_id"), c.Param("entry_id"), c.GetHeader("If-Match"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

func deleteSite(service SiteManagementService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := service.Delete(c.Request.Context(), c.Param("site_id"), c.GetHeader("If-Match"), c.Query("confirm_entry_count")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func sitesUnavailable() gin.HandlerFunc {
	return func(c *gin.Context) {
		writeError(c, httperr.NewWithCode(http.StatusServiceUnavailable, "site_library_unavailable", "site library is not configured"))
	}
}

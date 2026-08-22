package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func capabilities(readerVNext bool, readerCapabilities *dto.ReaderCapabilitiesResponse) gin.HandlerFunc {
	return func(c *gin.Context) {
		reader := dto.ReaderCapabilitiesResponse{}
		if readerVNext && readerCapabilities != nil {
			reader = *readerCapabilities
		}
		c.JSON(http.StatusOK, dto.CapabilitiesResponse{
			LibraryKinds:           true,
			SiteLibrary:            true,
			SiteManagement:         true,
			SiteAdvancedManagement: true,
			ArchiveVersions:        []int{2},
			ReaderVNext:            readerVNext,
			Reader:                 reader,
		})
	}
}

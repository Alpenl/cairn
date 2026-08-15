package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
)

func capabilities(features WebsiteFeatures) gin.HandlerFunc {
	return func(c *gin.Context) {
		reader := dto.ReaderCapabilitiesResponse{}
		if features.ReaderVNext && features.ReaderCapabilities != nil {
			reader = *features.ReaderCapabilities
		}
		c.JSON(http.StatusOK, dto.CapabilitiesResponse{
			LibraryKinds:           features.LibraryKindAPI,
			SiteLibrary:            features.LibraryKindAPI,
			SiteAutoClassification: features.LibraryKindAPI && features.SiteAutoClassification,
			SiteManagement:         features.LibraryKindAPI && features.SiteLibraryWrite,
			SiteAdvancedManagement: features.LibraryKindAPI && features.SiteLibraryWrite && features.SiteAdvancedManagement,
			ArchiveVersions:        archiveVersions(features),
			ReaderVNext:            features.ReaderVNext,
			Reader:                 reader,
		})
	}
}

func archiveVersions(features WebsiteFeatures) []int {
	if !features.LibraryKindAPI {
		return []int{1}
	}
	return []int{1, 2}
}

package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
)

func createLinkTranslation(service LinkTranslationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req dto.TranslationCreateRequest
		if !bindJSONWithLimit(c, &req, defaultMaxJSONBodyBytes) {
			return
		}
		linkID, ok := translationLinkID(c)
		if !ok {
			return
		}
		item, err := service.Create(c.Request.Context(), linkID, model.TranslationRequest{
			Scope:                   model.TranslationScope(strings.TrimSpace(req.Scope)),
			BlockKey:                req.BlockKey,
			StartOffset:             req.StartOffset,
			EndOffset:               req.EndOffset,
			SourceText:              req.SourceText,
			ExpectedContentRevision: req.ExpectedContentRevision,
			ExpectedSourceHash:      req.ExpectedSourceHash,
			Force:                   req.Force,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		resp := translationResponse(*item)
		status := http.StatusOK
		if item.Status == model.TranslationStatusPending || item.Status == model.TranslationStatusProcessing {
			status = http.StatusAccepted
		}
		c.JSON(status, resp)
	}
}

func listLinkTranslations(service LinkTranslationService) gin.HandlerFunc {
	return func(c *gin.Context) {
		linkID, ok := translationLinkID(c)
		if !ok {
			return
		}
		result, err := service.List(c.Request.Context(), linkID)
		if err != nil {
			writeError(c, err)
			return
		}
		resp := dto.TranslationListResponse{
			CurrentContentRevision:   result.CurrentContentRevision,
			CurrentSummarySourceHash: result.CurrentSummarySourceHash,
			Items:                    make([]dto.TranslationResponse, 0, len(result.Items)),
		}
		for _, item := range result.Items {
			resp.Items = append(resp.Items, translationResponse(item))
		}
		c.JSON(http.StatusOK, resp)
	}
}

func translationLinkID(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(strings.TrimSpace(c.Param("link_id")))
	if err != nil {
		writeError(c, httperr.NewWithCode(http.StatusBadRequest, httperr.CodeInvalidLinkID, "invalid link id"))
		return uuid.Nil, false
	}
	return id, true
}

func translationResponse(item model.LinkTranslation) dto.TranslationResponse {
	return dto.TranslationResponse{
		ID:                    item.ID.String(),
		LinkID:                item.LinkID.String(),
		Scope:                 string(item.Scope),
		BlockKey:              item.BlockKey,
		StartOffset:           item.StartOffset,
		EndOffset:             item.EndOffset,
		SourceText:            item.SourceText,
		TranslatedText:        item.TranslatedText,
		SourceFormat:          string(item.SourceFormat),
		TargetLanguage:        item.TargetLanguage,
		SourceContentRevision: item.SourceContentRevision,
		Status:                string(item.Status),
		Model:                 item.Model,
		ErrorMsg:              item.ErrorMsg,
		Stale:                 item.Stale,
		CreatedAt:             item.CreatedAt,
		UpdatedAt:             item.UpdatedAt,
	}
}

package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

// ClassificationRuleService is the complete user-owned classification-rule
// surface. Rules remain optional while a deployment is rolling out the
// additive schema, matching the optional site-management wiring pattern.
type ClassificationRuleService interface {
	List(context.Context) ([]model.LibraryClassificationRule, error)
	Create(context.Context, repository.CreateClassificationRuleParams) (*model.LibraryClassificationRule, error)
	Update(context.Context, repository.UpdateClassificationRuleParams) (*model.LibraryClassificationRule, error)
	Delete(context.Context, string, int64) (bool, error)
}

func listClassificationRules(service ClassificationRuleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		rules, err := service.List(c.Request.Context())
		if err != nil {
			writeError(c, err)
			return
		}
		out := make([]dto.ClassificationRuleResponse, 0, len(rules))
		for _, rule := range rules {
			out = append(out, classificationRuleResponse(rule))
		}
		c.JSON(http.StatusOK, out)
	}
}

func createClassificationRule(service ClassificationRuleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ClassificationRuleCreateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		enabled := true
		if request.Enabled != nil {
			enabled = *request.Enabled
		}
		rule, err := service.Create(c.Request.Context(), repository.CreateClassificationRuleParams{
			Host: request.Host, IdentityAdapter: request.IdentityAdapter, PathPrefix: request.PathPrefix,
			TargetKind: model.LibraryKind(request.TargetKind), Enabled: enabled,
		})
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, classificationRuleResponse(*rule))
	}
}

func updateClassificationRule(service ClassificationRuleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ClassificationRuleUpdateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		id, err := uuid.Parse(c.Param("rule_id"))
		if err != nil {
			writeError(c, httperr.NewWithCode(http.StatusBadRequest, "invalid_classification_rule_id", "invalid classification rule id"))
			return
		}
		revision, err := parseRuleRevision(c.GetHeader("If-Match"))
		if err != nil {
			writeError(c, err)
			return
		}
		params := repository.UpdateClassificationRuleParams{ID: id, Revision: revision, Host: request.Host, Enabled: request.Enabled}
		if request.TargetKind != nil {
			kind := model.LibraryKind(*request.TargetKind)
			params.TargetKind = &kind
		}
		if request.IdentityAdapter.Set {
			params.IdentityAdapter = &request.IdentityAdapter.Value
		}
		if request.PathPrefix.Set {
			params.PathPrefix = &request.PathPrefix.Value
		}
		rule, err := service.Update(c.Request.Context(), params)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, classificationRuleResponse(*rule))
	}
}

func parseRuleRevision(raw string) (int64, error) {
	clean := strings.Trim(strings.TrimSpace(raw), "\"")
	revision, err := strconv.ParseInt(clean, 10, 64)
	if err != nil || revision < 1 {
		return 0, httperr.NewWithCode(http.StatusPreconditionRequired, "classification_rule_revision_required", "If-Match must contain the current rule revision")
	}
	return revision, nil
}

func deleteClassificationRule(service ClassificationRuleService) gin.HandlerFunc {
	return func(c *gin.Context) {
		revision, err := parseRuleRevision(c.GetHeader("If-Match"))
		if err != nil {
			writeError(c, err)
			return
		}
		if _, err := service.Delete(c.Request.Context(), c.Param("rule_id"), revision); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func classificationRuleResponse(rule model.LibraryClassificationRule) dto.ClassificationRuleResponse {
	return dto.ClassificationRuleResponse{ID: rule.ID.String(), Host: rule.Host, IdentityAdapter: rule.IdentityAdapter,
		PathPrefix: rule.PathPrefix, TargetKind: string(rule.TargetKind), Enabled: rule.Enabled, Revision: rule.Revision,
		CreatedAt: rule.CreatedAt, UpdatedAt: rule.UpdatedAt}
}

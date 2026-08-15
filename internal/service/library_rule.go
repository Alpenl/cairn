package service

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/google/uuid"

	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

const invalidClassificationRuleCode = "invalid_classification_rule"

type ClassificationRuleService struct {
	rules repository.ClassificationRuleStore
}

func NewClassificationRuleService(rules repository.ClassificationRuleStore) *ClassificationRuleService {
	return &ClassificationRuleService{rules: rules}
}

func (s *ClassificationRuleService) List(ctx context.Context) ([]model.LibraryClassificationRule, error) {
	return s.rules.ListClassificationRules(ctx)
}

func (s *ClassificationRuleService) Create(ctx context.Context, p repository.CreateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	if err := normalizeCreateRule(&p); err != nil {
		return nil, err
	}
	return s.rules.CreateClassificationRule(ctx, p)
}

func (s *ClassificationRuleService) Update(ctx context.Context, p repository.UpdateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	if err := normalizeUpdateRule(&p); err != nil {
		return nil, err
	}
	return s.rules.UpdateClassificationRule(ctx, p)
}

func (s *ClassificationRuleService) Delete(ctx context.Context, id string, revision int64) (bool, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || revision < 1 {
		return false, invalidRule("invalid rule id or revision")
	}
	deleted, err := s.rules.DeleteClassificationRule(ctx, parsed, revision)
	if err != nil {
		return false, err
	}
	if !deleted {
		// A revision-guarded DELETE deliberately does not disclose whether the
		// row disappeared or changed after the client read it.
		return false, httperr.NewWithCode(http.StatusConflict, httperr.CodeRevisionConflict, "classification rule was changed by another request")
	}
	return true, nil
}

func normalizeCreateRule(p *repository.CreateClassificationRuleParams) error {
	host, err := normalizeRuleHost(p.Host)
	if err != nil {
		return err
	}
	p.Host = host
	adapter, prefix, err := normalizeRuleScope(host, p.IdentityAdapter, p.PathPrefix)
	if err != nil {
		return err
	}
	p.IdentityAdapter, p.PathPrefix = adapter, prefix
	if p.TargetKind != model.LibraryKindReading && p.TargetKind != model.LibraryKindSite {
		return invalidRule("target kind must be reading or site")
	}
	return nil
}

func normalizeUpdateRule(p *repository.UpdateClassificationRuleParams) error { //nolint:gocyclo // 逐字段归一化规则更新，同 normalizeSiteUpdate
	if p.Revision < 1 {
		return invalidRule("revision is required")
	}
	if p.Host != nil {
		host, err := normalizeRuleHost(*p.Host)
		if err != nil {
			return err
		}
		p.Host = &host
	}
	if p.TargetKind != nil && *p.TargetKind != model.LibraryKindReading && *p.TargetKind != model.LibraryKindSite {
		return invalidRule("target kind must be reading or site")
	}
	if p.Host == nil && p.IdentityAdapter == nil && p.PathPrefix == nil && p.TargetKind == nil && p.Enabled == nil {
		return invalidRule("rule update must include at least one field")
	}
	// The repository supports partial updates. Scope safety is fully checked on
	// create; an update that touches adapter/prefix must provide both values so
	// it cannot widen an existing shared-platform rule accidentally.
	if p.IdentityAdapter != nil || p.PathPrefix != nil {
		if p.Host == nil || p.IdentityAdapter == nil || p.PathPrefix == nil {
			return invalidRule("scope updates require host, adapter, and path prefix")
		}
		adapter, prefix, err := normalizeRuleScope(*p.Host, *p.IdentityAdapter, *p.PathPrefix)
		if err != nil {
			return err
		}
		p.IdentityAdapter, p.PathPrefix = &adapter, &prefix
	}
	return nil
}

func normalizeRuleHost(raw string) (string, error) {
	v := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	v = strings.TrimPrefix(v, "www.")
	if v == "" || strings.ContainsAny(v, "/:@?#[ ]") || net.ParseIP(v) != nil || !strings.Contains(v, ".") {
		return "", invalidRule("host must be a public domain")
	}
	if _, err := url.Parse("https://" + v); err != nil {
		return "", invalidRule("invalid host")
	}
	return v, nil
}

func normalizeRuleScope(host string, adapter, prefix *string) (*string, *string, error) {
	expected := map[string]string{"github.com": "github", "gitlab.com": "gitlab", "notion.so": "notion", "notion.site": "notion"}[host]
	if adapter == nil {
		if expected != "" {
			return nil, nil, invalidRule("shared platform rules require an adapter and path prefix")
		}
		if prefix != nil {
			return nil, nil, invalidRule("path prefix requires an identity adapter")
		}
		return nil, nil, nil
	}
	a := strings.ToLower(strings.TrimSpace(*adapter))
	if expected == "" || a != expected || prefix == nil {
		return nil, nil, invalidRule("shared platform rules require a matching adapter and path prefix")
	}
	p := strings.TrimSpace(*prefix)
	if !strings.HasPrefix(p, "/") {
		return nil, nil, invalidRule("path prefix must start with /")
	}
	p = path.Clean(p)
	if p == "/" {
		return nil, nil, invalidRule("shared platform rules cannot target the whole host")
	}
	return &a, &p, nil
}

func invalidRule(message string) error {
	return httperr.NewWithCode(422, invalidClassificationRuleCode, message)
}

package service

import (
	"context"
	"net/url"
	"path"
	"strings"

	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/siteidentity"
)

// ClassificationRuleMatcher is the parse-pipeline read boundary for explicit
// personal rules. It returns a decision only; it never writes a link or
// creates a site, which keeps prediction and final persistence separate.
type ClassificationRuleMatcher interface {
	MatchClassificationRule(context.Context, string) (*ClassificationRuleMatch, error)
}

type ClassificationRuleMatch struct {
	TargetKind model.LibraryKind
	Reason     string
}

type classificationRuleMatcher struct {
	store repository.ClassificationRuleStore
}

func NewClassificationRuleMatcher(store repository.ClassificationRuleStore) ClassificationRuleMatcher {
	if store == nil {
		return nil
	}
	return &classificationRuleMatcher{store: store}
}

func (m *classificationRuleMatcher) MatchClassificationRule(ctx context.Context, rawURL string) (*ClassificationRuleMatch, error) {
	rules, err := m.store.ListClassificationRules(ctx)
	if err != nil {
		return nil, err
	}
	return matchClassificationRule(rules, rawURL), nil
}

// matchClassificationRule chooses the most specific enabled rule. Shared
// platform rules must match both the derived adapter and a path segment
// boundary, so /owner/repo cannot accidentally include /owner/repository.
func matchClassificationRule(rules []model.LibraryClassificationRule, rawURL string) *ClassificationRuleMatch { //nolint:gocyclo // 按规则类型逐一匹配，分支数等于支持的匹配模式数
	identity, err := siteidentity.FromURL(rawURL)
	if err != nil {
		return nil
	}
	parsed, err := url.Parse(identity.NormalizedURL)
	if err != nil {
		return nil
	}
	requestPath := path.Clean("/" + parsed.Path)
	var selected *model.LibraryClassificationRule
	for i := range rules {
		rule := &rules[i]
		if !rule.Enabled || rule.Host != identity.Host {
			continue
		}
		if rule.IdentityAdapter == nil {
			if selected == nil {
				selected = rule
			}
			continue
		}
		if *rule.IdentityAdapter != identity.Adapter || rule.PathPrefix == nil || !pathPrefixMatches(requestPath, *rule.PathPrefix) {
			continue
		}
		if selected == nil || selected.PathPrefix == nil || len(*rule.PathPrefix) > len(*selected.PathPrefix) {
			selected = rule
		}
	}
	if selected == nil {
		return nil
	}
	reason := "personal_rule_host"
	if selected.IdentityAdapter != nil {
		reason = "personal_rule_scoped"
	}
	return &ClassificationRuleMatch{TargetKind: selected.TargetKind, Reason: reason}
}

func pathPrefixMatches(requestPath, prefix string) bool {
	prefix = path.Clean("/" + strings.TrimSpace(prefix))
	return requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/")
}

var _ ClassificationRuleMatcher = (*classificationRuleMatcher)(nil)

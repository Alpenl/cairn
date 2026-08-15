package service

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/model"
	"webtag/internal/repository"
)

type classificationRuleStoreFake struct {
	created *repository.CreateClassificationRuleParams
}

func (f *classificationRuleStoreFake) ListClassificationRules(context.Context) ([]model.LibraryClassificationRule, error) {
	return nil, nil
}
func (f *classificationRuleStoreFake) CreateClassificationRule(_ context.Context, p repository.CreateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	f.created = &p
	return &model.LibraryClassificationRule{ID: uuid.New(), Host: p.Host, IdentityAdapter: p.IdentityAdapter, PathPrefix: p.PathPrefix, TargetKind: p.TargetKind, Enabled: p.Enabled}, nil
}
func (f *classificationRuleStoreFake) UpdateClassificationRule(context.Context, repository.UpdateClassificationRuleParams) (*model.LibraryClassificationRule, error) {
	return nil, nil
}
func (f *classificationRuleStoreFake) DeleteClassificationRule(context.Context, uuid.UUID, int64) (bool, error) {
	return true, nil
}

func TestClassificationRuleServiceNormalizesGenericHost(t *testing.T) {
	t.Parallel()
	store := &classificationRuleStoreFake{}
	_, err := NewClassificationRuleService(store).Create(context.Background(), repository.CreateClassificationRuleParams{Host: " WWW.Example.COM. ", TargetKind: model.LibraryKindReading, Enabled: true})
	if err != nil || store.created == nil || store.created.Host != "example.com" {
		t.Fatalf("Create() err=%v params=%#v", err, store.created)
	}
}

func TestClassificationRuleServiceRequiresNarrowSharedScope(t *testing.T) {
	t.Parallel()
	github, path := "github", "/owner/repo/"
	valid := repository.CreateClassificationRuleParams{Host: "github.com", IdentityAdapter: &github, PathPrefix: &path, TargetKind: model.LibraryKindSite, Enabled: true}
	store := &classificationRuleStoreFake{}
	if _, err := NewClassificationRuleService(store).Create(context.Background(), valid); err != nil {
		t.Fatalf("valid GitHub rule error = %v", err)
	}
	if store.created.PathPrefix == nil || *store.created.PathPrefix != "/owner/repo" {
		t.Fatalf("path was not normalized: %#v", store.created)
	}
	for _, invalid := range []repository.CreateClassificationRuleParams{
		{Host: "github.com", TargetKind: model.LibraryKindSite, Enabled: true},
		{Host: "github.com", IdentityAdapter: &github, TargetKind: model.LibraryKindSite, Enabled: true},
		{Host: "github.com", IdentityAdapter: &github, PathPrefix: stringPtr("/"), TargetKind: model.LibraryKindSite, Enabled: true},
		{Host: "example.com", IdentityAdapter: &github, PathPrefix: &path, TargetKind: model.LibraryKindSite, Enabled: true},
	} {
		if _, err := NewClassificationRuleService(&classificationRuleStoreFake{}).Create(context.Background(), invalid); err == nil {
			t.Fatalf("invalid shared scope was accepted: %#v", invalid)
		}
	}
}

func TestMatchClassificationRuleHonorsSharedScopeBoundaries(t *testing.T) {
	rules := []model.LibraryClassificationRule{
		{Host: "github.com", IdentityAdapter: stringPtr("github"), PathPrefix: stringPtr("/openai/gpt-5"), TargetKind: model.LibraryKindReading, Enabled: true},
		{Host: "example.com", TargetKind: model.LibraryKindSite, Enabled: true},
	}
	for _, tt := range []struct {
		url  string
		kind model.LibraryKind
		ok   bool
	}{
		{"https://github.com/openai/gpt-5/issues/1", model.LibraryKindReading, true},
		{"https://github.com/openai/gpt-50", "", false},
		{"https://example.com/docs", model.LibraryKindSite, true},
	} {
		got := matchClassificationRule(rules, tt.url)
		if (got != nil) != tt.ok || got != nil && got.TargetKind != tt.kind {
			t.Fatalf("matchClassificationRule(%q) = %#v, want matched=%t kind=%q", tt.url, got, tt.ok, tt.kind)
		}
	}
}

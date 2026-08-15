package service

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
	"webtag/internal/observability"
)

func TestConversionAndSiteOperationMetricsExposeBoundedResults(t *testing.T) {
	metrics := observability.NewMetrics()
	linkID, siteID, entryID := uuid.New(), uuid.New(), uuid.New()
	from := model.LibraryKindReading
	auto := model.LibraryKindSourceAuto
	reason := "personal_rule_host"
	writer := &conversionWriterFake{result: ConvertLinkResult{LinkID: linkID, Kind: model.LibraryKindSite, SiteID: &siteID, EntryID: &entryID, ContentRevision: 2, Status: model.LinkStatusDone}}
	svc := NewConversionExecuteServiceWithMetrics(conversionLinksFake{link: &model.Link{ID: linkID, Status: model.LinkStatusDone, LibraryKind: &from, LibraryKindSource: &auto, ClassificationReason: &reason, ContentRevision: 1}}, writer, metrics)
	if _, err := svc.Execute(context.Background(), linkID.String(), dto.ConversionExecuteRequest{TargetKind: "site", ExpectedContentRevision: 1, ConfirmDestructive: true}); err != nil {
		t.Fatal(err)
	}
	(&SiteMergeService{metrics: metrics}).recordMerge("conflict")
	(&SiteSplitService{metrics: metrics}).recordSplit("success")

	recorder := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, want := range []string{
		`webtag_site_conversion_total{from="reading",result="success",to="site"} 1`,
		`webtag_library_classification_correction_total{from="reading",had_rule="true",to="site"} 1`,
		`webtag_site_merge_total{result="conflict"} 1`,
		`webtag_site_split_total{result="success"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, body)
		}
	}
}

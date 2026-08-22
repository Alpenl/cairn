package app

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
)

// TestOpenAPIFrontendSchemasMatchDTOs keeps the response schemas consumed by
// the TypeScript clients aligned with the JSON that the Go handlers emit.
// TypeScript types are generated from the same OpenAPI document. This test
// checks the Go-visible parts of that contract: properties, required fields,
// primitive JSON types and required pointer nullability. Format constraints
// such as uuid cannot be inferred from a Go string and remain OpenAPI-owned.
func TestOpenAPIFrontendSchemasMatchDTOs(t *testing.T) {
	schemas := openAPISchemas(t)

	for name, value := range map[string]any{
		"LinkCreateRequest":               dto.LinkCreateRequest{},
		"IngestRequest":                   dto.IngestRequest{},
		"IngestSource":                    dto.IngestSource{},
		"SubmitResponse":                  dto.SubmitResponse{},
		"ContentEditRequest":              dto.ContentEditRequest{},
		"LinkContentResponse":             dto.LinkContentResponse{},
		"ConversionPreviewRequest":        dto.ConversionPreviewRequest{},
		"ConversionPreviewResponse":       dto.ConversionPreviewResponse{},
		"ConversionSiteCandidateResponse": dto.ConversionSiteCandidateResponse{},
		"ConversionExecuteRequest":        dto.ConversionExecuteRequest{},
		"ConversionExecuteResponse":       dto.ConversionExecuteResponse{},
		"SiteRevisionRef":                 dto.SiteRevisionRef{},
		"SiteMergePreviewRequest":         dto.SiteMergePreviewRequest{},
		"SiteMergePreviewEntryResponse":   dto.SiteMergePreviewEntryResponse{},
		"SiteMergeFieldConflictResponse":  dto.SiteMergeFieldConflictResponse{},
		"SiteMergePreviewResponse":        dto.SiteMergePreviewResponse{},
		"SiteMergeFieldResolutionRequest": dto.SiteMergeFieldResolutionRequest{},
		"SiteMergeExecuteRequest":         dto.SiteMergeExecuteRequest{},
		"SiteMergeExecuteResponse":        dto.SiteMergeExecuteResponse{},
		"RelatedReadingResponse":          dto.RelatedReadingResponse{},
		"SiteSplitRequest":                dto.SiteSplitRequest{},
		"SiteSplitIdentityResponse":       dto.SiteSplitIdentityResponse{},
		"SiteSplitPreviewResponse":        dto.SiteSplitPreviewResponse{},
		"SiteSplitExecuteResponse":        dto.SiteSplitExecuteResponse{},
		"ReaderTarget":                    dto.ReaderTarget{},
		"TranslationCreateRequest":        dto.TranslationCreateRequest{},
		"TranslationResponse":             dto.TranslationResponse{},
		"TranslationListResponse":         dto.TranslationListResponse{},
		"TranslationSourceIdentity":       dto.TranslationSourceIdentity{},
		"LinkResponse":                    dto.LinkResponse{},
		"PaginatedLinksResponse":          dto.PaginatedLinksResponse{},
		"TagCountResponse":                dto.TagCountResponse{},
		"DomainTreeSummaryResponse":       dto.DomainTreeSummaryResponse{},
		"DomainTreeSummaryEnvelope":       dto.DomainTreeSummaryEnvelope{},
		"TreeNodeResponse":                dto.TreeNodeResponse{},
		"TreeResponse":                    dto.TreeResponse{},
		"ErrorDetail":                     dto.ErrorDetail{},
		"ErrorResponse":                   dto.ErrorResponse{},
		"LibrarySearchGroup":              dto.LibrarySearchGroup{},
		"SiteSearchEntryResponse":         dto.SiteSearchEntryResponse{},
		"SiteSearchResultResponse":        dto.SiteSearchResultResponse{},
		"SiteSearchGroup":                 dto.SiteSearchGroup{},
		"ReaderThoughtSearchResponse":     dto.ReaderThoughtSearchResponse{},
		"ReaderThoughtSearchGroup":        dto.ReaderThoughtSearchGroup{},
		"ReaderNoteSearchResponse":        dto.ReaderNoteSearchResponse{},
		"ReaderNoteSearchGroup":           dto.ReaderNoteSearchGroup{},
		"GroupedSearchResponse":           dto.GroupedSearchResponse{},
		// The Inbox list projection is registered here on purpose: the whole
		// point of the narrow DTO is that it cannot drift back into carrying
		// the capture body, and the spec is what the TypeScript client trusts.
		"ReaderInboxListItemResponse": dto.ReaderInboxListItemResponse{},
		"FeedFolder":                  model.FeedFolder{},
		"FeedSubscription":            model.FeedSubscription{},
		"FeedCounts":                  model.FeedCounts{},
		"FeedSubscriptionsResponse":   model.FeedSubscriptionsResponse{},
		"FeedItem":                    model.FeedItem{},
		"PaginatedFeedItems":          model.PaginatedFeedItems{},
		"FeedCandidate":               model.FeedCandidate{},
		"FeedDiscoveryResponse":       model.FeedDiscoveryResponse{},
		"OPMLImportResponse":          model.OPMLImportResponse{},
	} {
		t.Run(name, func(t *testing.T) {
			assertOpenAPISchemaMatchesJSONTags(t, schemas, name, reflect.TypeOf(value))
		})
	}
}

func TestReaderTodoPatchOpenAPIContract(t *testing.T) {
	t.Parallel()

	data, err := readOpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec() error: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Description string                     `json:"description"`
			Responses   map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Description string `json:"description"`
					Minimum     *int64 `json:"minimum"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal OpenAPI: %v", err)
	}

	operation, ok := spec.Paths["/api/todos/{id}"]["patch"]
	if !ok {
		t.Fatal("PATCH /api/todos/{id} is missing from OpenAPI")
	}
	for _, phrase := range []string{
		"unknown IDs return 404",
		"negative values and 0",
		"todo_host_revision_not_applicable",
		"without changing text, due_at, done, completed_at, or updated_at",
		"revision_conflict",
		"Only a matching desired-state request writes back",
	} {
		if !strings.Contains(operation.Description, phrase) {
			t.Errorf("TODO PATCH description is missing %q", phrase)
		}
	}
	for _, status := range []string{"404", "409", "422"} {
		if _, ok := operation.Responses[status]; !ok {
			t.Errorf("TODO PATCH OpenAPI responses are missing %s", status)
		}
	}

	property, ok := spec.Components.Schemas["ReaderTodoPatchRequest"].Properties["expected_host_revision"]
	if !ok {
		t.Fatal("ReaderTodoPatchRequest.expected_host_revision is missing from OpenAPI")
	}
	for _, phrase := range []string{
		"Only for projected TODOs",
		"Standalone TODO PATCH requests must omit this property",
		"any JSON integer",
		"negative values and 0",
		"todo_host_revision_not_applicable",
		"revision_conflict",
	} {
		if !strings.Contains(property.Description, phrase) {
			t.Errorf("expected_host_revision description is missing %q", phrase)
		}
	}
	if property.Minimum != nil {
		t.Errorf("expected_host_revision minimum = %d, want no transport-level lower bound", *property.Minimum)
	}
}

func TestTranslationSourceIdentityOpenAPIContract(t *testing.T) {
	t.Parallel()

	type schemaShape struct {
		Ref         string                 `json:"$ref"`
		Description string                 `json:"description"`
		Required    []string               `json:"required"`
		Const       any                    `json:"const"`
		Enum        []any                  `json:"enum"`
		Minimum     *int64                 `json:"minimum"`
		OneOf       []schemaShape          `json:"oneOf"`
		Properties  map[string]schemaShape `json:"properties"`
	}

	data, err := readOpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec() returned error: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Description string                     `json:"description"`
			Responses   map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas   map[string]schemaShape `json:"schemas"`
			Responses map[string]struct {
				Content map[string]struct {
					Schema schemaShape `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("openapi.json: unmarshal failed: %v", err)
	}

	operation, ok := spec.Paths["/api/links/{link_id}/translations"]["post"]
	if !ok {
		t.Fatal("translation POST operation is missing")
	}
	for _, phrase := range []string{
		"content_revision_conflict",
		"source_block_conflict",
		"Strict source identity is required",
	} {
		if !strings.Contains(operation.Description, phrase) {
			t.Errorf("translation POST description is missing %q", phrase)
		}
	}
	for _, stalePromise := range []string{
		"deep-research",
		"legacy-unverified",
		"2026-09-01T00:00:00Z",
		"contract migration",
		"rollback cutoff",
		"expand compatibility window",
		"translation_schema_transition",
	} {
		if strings.Contains(string(data), stalePromise) {
			t.Errorf("OpenAPI still promises retired behavior %q", stalePromise)
		}
	}
	request := spec.Components.Schemas["TranslationCreateRequest"]
	if description := request.Properties["expected_content_revision"].Description; !strings.Contains(description, "It must be omitted for summary") {
		t.Errorf("TranslationCreateRequest.expected_content_revision does not define the summary boundary: %q", description)
	}
	if description := request.Properties["expected_source_hash"].Description; !strings.Contains(description, "Required for summary") {
		t.Errorf("TranslationCreateRequest.expected_source_hash does not define the summary identity: %q", description)
	}
	response := spec.Components.Schemas["TranslationResponse"]
	if description := response.Properties["source_content_revision"].Description; !strings.Contains(description, "Null is reserved for summary identities") {
		t.Errorf("TranslationResponse.source_content_revision does not define its null case: %q", description)
	}
	identity := spec.Components.Schemas["TranslationSourceIdentity"]
	if !strings.Contains(identity.Description, "Saved content uses content_revision and summary uses source_hash") {
		t.Errorf("TranslationSourceIdentity does not define both current identity domains: %q", identity.Description)
	}
	if minimum := identity.Properties["content_revision"].Minimum; minimum == nil {
		t.Error("TranslationSourceIdentity.content_revision minimum is missing, want 0 because an authoritative empty generation is valid")
	} else if *minimum != 0 {
		t.Errorf("TranslationSourceIdentity.content_revision minimum = %d, want 0 because an authoritative empty generation is valid", *minimum)
	}
	var conflict struct {
		Ref string `json:"$ref"`
	}
	if err := json.Unmarshal(operation.Responses["409"], &conflict); err != nil {
		t.Fatalf("decode translation 409 response: %v", err)
	}
	if conflict.Ref != "#/components/responses/AuthenticatedTranslationConflict" {
		t.Errorf("translation 409 ref = %q, want dedicated source-conflict response", conflict.Ref)
	}

	conflictResponse := spec.Components.Responses["AuthenticatedTranslationConflict"]
	conflictSchema := conflictResponse.Content["application/json"].Schema
	if conflictSchema.Ref != "#/components/schemas/TranslationConflictErrorResponse" {
		t.Errorf("AuthenticatedTranslationConflict schema ref = %q, want precise translation conflict union", conflictSchema.Ref)
	}
	union := spec.Components.Schemas["TranslationConflictErrorResponse"]
	wantEnvelopeRefs := []string{
		"#/components/schemas/TranslationOperationalConflictResponse",
		"#/components/schemas/TranslationContentRevisionConflictResponse",
		"#/components/schemas/TranslationSourceBlockConflictResponse",
	}
	if len(union.OneOf) != len(wantEnvelopeRefs) {
		t.Errorf("TranslationConflictErrorResponse oneOf count = %d, want %d", len(union.OneOf), len(wantEnvelopeRefs))
	} else {
		for i, want := range wantEnvelopeRefs {
			if got := union.OneOf[i].Ref; got != want {
				t.Errorf("TranslationConflictErrorResponse.oneOf[%d] = %q, want %q", i, got, want)
			}
		}
	}

	wantVariants := []struct {
		envelope string
		detail   string
		code     string
		identity string
	}{
		{
			envelope: "TranslationOperationalConflictResponse",
			detail:   "TranslationOperationalConflictDetail",
			code:     "operational_conflict",
		},
		{
			envelope: "TranslationContentRevisionConflictResponse",
			detail:   "TranslationContentRevisionConflictDetail",
			code:     "content_revision_conflict",
			identity: "#/components/schemas/SavedTranslationSourceIdentity",
		},
		{
			envelope: "TranslationSourceBlockConflictResponse",
			detail:   "TranslationSourceBlockConflictDetail",
			code:     "source_block_conflict",
		},
	}
	for _, variant := range wantVariants {
		t.Run(variant.code, func(t *testing.T) {
			envelope := spec.Components.Schemas[variant.envelope]
			if !slices.Contains(envelope.Required, "error") {
				t.Errorf("%s must require error", variant.envelope)
			}
			wantDetailRef := "#/components/schemas/" + variant.detail
			if got := envelope.Properties["error"].Ref; got != wantDetailRef {
				t.Errorf("%s.error ref = %q, want %q", variant.envelope, got, wantDetailRef)
			}
			detail := spec.Components.Schemas[variant.detail]
			for _, field := range []string{"code", "error_code", "message"} {
				if !slices.Contains(detail.Required, field) {
					t.Errorf("%s must require %s", variant.detail, field)
				}
			}
			if variant.code == "operational_conflict" {
				if slices.Contains(detail.Required, "current_identity") {
					t.Errorf("%s must not require current_identity", variant.detail)
				}
			} else if !slices.Contains(detail.Required, "current_identity") {
				t.Errorf("%s must require current_identity", variant.detail)
			}
			if got := detail.Properties["code"].Const; got != float64(409) {
				t.Errorf("%s.code const = %#v, want 409", variant.detail, got)
			}
			if variant.code == "operational_conflict" {
				gotCodes := make([]string, 0, len(detail.Properties["error_code"].Enum))
				for _, value := range detail.Properties["error_code"].Enum {
					if code, ok := value.(string); ok {
						gotCodes = append(gotCodes, code)
					}
				}
				slices.Sort(gotCodes)
				wantCodes := []string{"link_not_ready", "site_original_content_forbidden", "translation_content_unavailable"}
				if !slices.Equal(gotCodes, wantCodes) {
					t.Errorf("%s.error_code enum = %v, want %v", variant.detail, gotCodes, wantCodes)
				}
			} else if got := detail.Properties["error_code"].Const; got != variant.code {
				t.Errorf("%s.error_code const = %#v, want %q", variant.detail, got, variant.code)
			}
			currentIdentity := detail.Properties["current_identity"]
			if variant.identity != "" && currentIdentity.Ref != variant.identity {
				t.Errorf("%s.current_identity ref = %q, want %q", variant.detail, currentIdentity.Ref, variant.identity)
			}
			if variant.code == "source_block_conflict" {
				wantIdentityRefs := []string{
					"#/components/schemas/SavedTranslationSourceIdentity",
					"#/components/schemas/SummaryTranslationSourceIdentity",
				}
				if len(currentIdentity.OneOf) != len(wantIdentityRefs) {
					t.Errorf("%s.current_identity oneOf count = %d, want %d", variant.detail, len(currentIdentity.OneOf), len(wantIdentityRefs))
				} else {
					for i, want := range wantIdentityRefs {
						if got := currentIdentity.OneOf[i].Ref; got != want {
							t.Errorf("%s.current_identity.oneOf[%d] = %q, want %q", variant.detail, i, got, want)
						}
					}
				}
			}
		})
	}

	savedIdentity := spec.Components.Schemas["SavedTranslationSourceIdentity"]
	for _, field := range []string{"content_revision", "block_key"} {
		if !slices.Contains(savedIdentity.Required, field) {
			t.Errorf("SavedTranslationSourceIdentity must require %s", field)
		}
	}
	if minimum := savedIdentity.Properties["content_revision"].Minimum; minimum == nil {
		t.Error("SavedTranslationSourceIdentity.content_revision minimum is missing, want 0")
	} else if *minimum != 0 {
		t.Errorf("SavedTranslationSourceIdentity.content_revision minimum = %d, want 0", *minimum)
	}
	summaryIdentity := spec.Components.Schemas["SummaryTranslationSourceIdentity"]
	for _, field := range []string{"block_key"} {
		if !slices.Contains(summaryIdentity.Required, field) {
			t.Errorf("SummaryTranslationSourceIdentity must require %s", field)
		}
	}
	if slices.Contains(summaryIdentity.Required, "source_hash") {
		t.Error("SummaryTranslationSourceIdentity.source_hash must remain optional when the authoritative summary is absent")
	}
	if got := summaryIdentity.Properties["block_key"].Const; got != "summary" {
		t.Errorf("SummaryTranslationSourceIdentity.block_key const = %#v, want summary", got)
	}
}

func TestOpenAPIOmitsRetiredContracts(t *testing.T) {
	t.Parallel()

	data, err := readOpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec() returned error: %v", err)
	}
	lower := strings.ToLower(string(data))
	for _, retired := range []string{
		"webhook",
		`/api/links/batch`,
		`/api/annotations/{id}/reattach`,
		`BatchCreateRequest`,
		`ReaderThoughtReattachRequest`,
	} {
		if strings.Contains(lower, strings.ToLower(retired)) {
			t.Fatalf("OpenAPI still publishes retired contract %q", retired)
		}
	}
}

func TestOpenAPIWireEnumsMatchDomainModels(t *testing.T) {
	schemas := openAPISchemas(t)
	linkStatuses := []string{
		string(model.LinkStatusPending),
		string(model.LinkStatusProcessing),
		string(model.LinkStatusDone),
		string(model.LinkStatusFailed),
	}
	workStatuses := []string{
		string(model.LinkStatusPending),
		string(model.LinkStatusProcessing),
		string(model.LinkStatusDone),
		string(model.LinkStatusFailed),
	}
	contentTypes := []string{
		string(model.ContentTypeArticle),
		string(model.ContentTypeListing),
		string(model.ContentTypeHomepage),
		string(model.ContentTypeUnknown),
	}

	for _, check := range []struct {
		schema string
		field  string
		want   []string
	}{
		{schema: "LinkResponse", field: "status", want: linkStatuses},
		{schema: "TreeNodeResponse", field: "status", want: linkStatuses},
		{schema: "LinkResponse", field: "content_type", want: contentTypes},
		{schema: "TreeNodeResponse", field: "content_type", want: contentTypes},
		{schema: "SubmitResponse", field: "status", want: workStatuses},
		{schema: "FeedItem", field: "analysis_status", want: workStatuses},
		{schema: "FeedCandidate", field: "type", want: []string{"rss", "atom"}},
		{
			schema: "TranslationCreateRequest",
			field:  "scope",
			want: []string{
				string(model.TranslationScopeSelection),
				string(model.TranslationScopeFull),
			},
		},
		{
			schema: "TranslationResponse",
			field:  "status",
			want: []string{
				string(model.TranslationStatusPending),
				string(model.TranslationStatusProcessing),
				string(model.TranslationStatusDone),
				string(model.TranslationStatusFailed),
			},
		},
		{
			schema: "TranslationResponse",
			field:  "source_format",
			want: []string{
				string(model.TranslationFormatPlain),
				string(model.TranslationFormatMarkdown),
			},
		},
	} {
		t.Run(check.schema+"/"+check.field, func(t *testing.T) {
			schema, ok := schemas[check.schema]
			if !ok {
				t.Fatalf("schema %q is missing", check.schema)
			}
			raw, ok := schema.Properties[check.field]
			if !ok {
				t.Fatalf("property %q is missing", check.field)
			}
			_, got := openAPIPropertyShape(t, raw)
			want := slices.Clone(check.want)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("enum = %v, domain values = %v", got, want)
			}
		})
	}
}

func TestContentSaveOpenAPIDocumentsRuntimeFailureResponses(t *testing.T) {
	t.Parallel()

	data, err := readOpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec() returned error: %v", err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("openapi.json: unmarshal failed: %v", err)
	}
	for _, method := range []string{"post", "put", "patch"} {
		operation, ok := spec.Paths["/api/links/{link_id}/content"][method]
		if !ok {
			t.Errorf("content %s operation is missing", strings.ToUpper(method))
			continue
		}
		statuses := []string{"409", "502", "503"}
		if method == "patch" {
			statuses = []string{"400", "409", "413", "422", "500"}
		}
		for _, status := range statuses {
			if _, ok := operation.Responses[status]; !ok {
				t.Errorf("content %s response %s is missing", strings.ToUpper(method), status)
			}
		}
	}
}

func TestReaderFeedOpenAPIContract(t *testing.T) {
	t.Parallel()

	schemas := openAPISchemas(t)
	for _, name := range []string{
		"ArchiveV2ReaderFeedSnapshot",
		"ReaderFeedAction",
		"ReaderFeedSectionResponse",
		"ReaderFeedSourceResponse",
		"ReaderFeedPendingConfirmationParams",
		"ReaderFeedSavedLibraryParams",
		"ReaderFeedSubscriptionRecentParams",
		"ReaderFeedUnreadParams",
		"ReaderFeedReadLaterParams",
		"ReaderFeedChronologicalFallbackParams",
		"ReaderFeedScoreContributions",
		"ReaderFeedScoreSignal",
		"ReaderFeedReasonCode",
		"ReaderFeedReasonTuple",
		"ReaderRankedFeedItemResponse",
		"ReaderFeedSaveAssociationResponse",
		"ArchiveV2ReaderFeedFeedback",
	} {
		if _, ok := schemas[name]; ok {
			t.Errorf("obsolete OpenAPI schema %q is still present", name)
		}
	}

	item := schemas["ReaderFeedItemResponse"]
	wantItemFields := []string{
		"key", "source", "resource_key", "title", "summary", "url", "link_id",
		"inbox_id", "feed_item_id", "read", "read_later", "saved", "event_at",
	}
	gotItemFields := make([]string, 0, len(item.Properties))
	for field := range item.Properties {
		gotItemFields = append(gotItemFields, field)
	}
	slices.Sort(gotItemFields)
	slices.Sort(wantItemFields)
	if !slices.Equal(gotItemFields, wantItemFields) {
		t.Fatalf("ReaderFeedItemResponse fields = %#v, want %#v", gotItemFields, wantItemFields)
	}

	response := schemas["ReaderFeedResponse"]
	if !slices.Equal(response.Required, []string{"items", "mode"}) || len(response.Properties) != 3 {
		t.Fatalf("ReaderFeedResponse = required %#v properties %#v", response.Required, response.Properties)
	}
	var items struct {
		Items struct {
			Ref string `json:"$ref"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Properties["items"], &items); err != nil {
		t.Fatalf("ReaderFeedResponse.items: unmarshal failed: %v", err)
	}
	if items.Items.Ref != "#/components/schemas/ReaderFeedItemResponse" {
		t.Fatalf("ReaderFeedResponse.items ref = %q, want minimal Feed item", items.Items.Ref)
	}

	feedback := schemas["ReaderFeedFeedbackResponse"]
	feedbackFields := make([]string, 0, len(feedback.Properties))
	for field := range feedback.Properties {
		feedbackFields = append(feedbackFields, field)
	}
	slices.Sort(feedbackFields)
	if !slices.Equal(feedback.Required, []string{"item_key", "action"}) || !slices.Equal(feedbackFields, []string{"action", "item_key", "link_id"}) {
		t.Fatalf("ReaderFeedFeedbackResponse = required %#v fields %#v", feedback.Required, feedbackFields)
	}
}

type openAPISchema struct {
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
}

func openAPISchemas(t *testing.T) map[string]openAPISchema {
	t.Helper()

	data, err := readOpenAPISpec()
	if err != nil {
		t.Fatalf("OpenAPISpec() returned error: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]openAPISchema `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("openapi.json: unmarshal failed: %v", err)
	}
	return spec.Components.Schemas
}

func assertOpenAPISchemaMatchesJSONTags(
	t *testing.T,
	schemas map[string]openAPISchema,
	name string,
	typ reflect.Type,
) {
	t.Helper()

	schema, ok := schemas[name]
	if !ok {
		t.Fatalf("OpenAPI schema %q is missing", name)
	}

	var fields []string
	var required []string
	structFields := make(map[string]reflect.StructField, typ.NumField())
	for i := range typ.NumField() {
		field := typ.Field(i)
		tag := field.Tag.Get("json")
		parts := strings.Split(tag, ",")
		if parts[0] == "" || parts[0] == "-" {
			continue
		}
		fields = append(fields, parts[0])
		structFields[parts[0]] = field
		if !slices.Contains(parts[1:], "omitempty") {
			required = append(required, parts[0])
		}
	}

	properties := make([]string, 0, len(schema.Properties))
	for field := range schema.Properties {
		properties = append(properties, field)
	}
	slices.Sort(fields)
	slices.Sort(properties)
	slices.Sort(required)
	slices.Sort(schema.Required)

	if !slices.Equal(properties, fields) {
		t.Errorf("properties = %v, DTO JSON fields = %v", properties, fields)
	}
	if !slices.Equal(schema.Required, required) {
		t.Errorf("required = %v, non-omitempty DTO fields = %v", schema.Required, required)
	}
	for name, field := range structFields {
		assertOpenAPIPropertyType(t, name, field, schema.Properties[name])
	}
}

func assertOpenAPIPropertyType(
	t *testing.T,
	name string,
	field reflect.StructField,
	raw json.RawMessage,
) {
	t.Helper()
	types, _ := openAPIPropertyShape(t, raw)
	expected := goJSONType(field.Type)
	if expected != "" && !slices.Contains(types, expected) {
		t.Errorf("property %q types = %v, Go JSON type = %s", name, types, expected)
	}

	parts := strings.Split(field.Tag.Get("json"), ",")
	omitempty := slices.Contains(parts[1:], "omitempty")
	isPointer := field.Type.Kind() == reflect.Pointer
	allowsNull := slices.Contains(types, "null")
	if isPointer && !omitempty && !allowsNull {
		t.Errorf("property %q is a required Go pointer but OpenAPI disallows null", name)
	}
	if !isPointer && allowsNull {
		t.Errorf("property %q is a Go value but OpenAPI allows null", name)
	}
}

func goJSONType(typ reflect.Type) string {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(uuid.UUID{}) {
		return "string"
	}
	if typ == reflect.TypeOf(time.Time{}) {
		return "string"
	}
	switch typ.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Array, reflect.Slice:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return ""
	}
}

func openAPIPropertyShape(t *testing.T, raw json.RawMessage) ([]string, []string) {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode property schema: %v", err)
	}
	typeSet := make(map[string]struct{})
	enumSet := make(map[string]struct{})
	collectOpenAPIShape(value, typeSet, enumSet)
	types := make([]string, 0, len(typeSet))
	for value := range typeSet {
		types = append(types, value)
	}
	enums := make([]string, 0, len(enumSet))
	for value := range enumSet {
		enums = append(enums, value)
	}
	slices.Sort(types)
	slices.Sort(enums)
	return types, enums
}

func collectOpenAPIShape(value any, types, enums map[string]struct{}) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	switch typ := object["type"].(type) {
	case string:
		types[typ] = struct{}{}
	case []any:
		for _, item := range typ {
			if name, ok := item.(string); ok {
				types[name] = struct{}{}
			}
		}
	}
	if _, ok := object["$ref"].(string); ok {
		types["object"] = struct{}{}
	}
	if oneOf, ok := object["oneOf"].([]any); ok {
		for _, option := range oneOf {
			collectOpenAPIShape(option, types, enums)
		}
	}
	if values, ok := object["enum"].([]any); ok {
		for _, value := range values {
			if name, ok := value.(string); ok {
				enums[name] = struct{}{}
			}
		}
	}
}

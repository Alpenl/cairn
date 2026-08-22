package analyzer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"webtag/internal/fetcher"
	"webtag/internal/jsonx"
	"webtag/internal/model"
)

func newStructuredTestAnalyzer(t *testing.T, baseURL string, client *http.Client, opts ...func(*OpenAIAnalyzerOptions)) *OpenAIAnalyzer {
	t.Helper()
	o := OpenAIAnalyzerOptions{
		BaseURL:              baseURL,
		APIKey:               "secret-key",
		Model:                "gpt-test",
		HTTPClient:           client,
		EmptyResponseRetries: 3,
		MaxSummaryChars:      200,
		MinTags:              1,
		MaxTags:              5,
		MaxTagChars:          20,
	}
	for _, apply := range opts {
		apply(&o)
	}
	return NewOpenAIAnalyzer(o)
}

// productionAnalyzeRequest mirrors what pipeline.go actually builds. The
// RequestedLibraryKind is the load-bearing part: requestedKindForLink
// resolves "auto" for every link whose library_kind is NULL — i.e. every
// freshly captured one — so a request with an EMPTY kind does not occur in
// production and must not be what these tests assert on. An earlier version
// of this feature excluded any non-empty kind from structured output, which
// made it dead code that a test using an empty kind reported as working.
func productionAnalyzeRequest() AnalyzeRequest {
	return AnalyzeRequest{
		Content: fetcher.Content{
			URL:   "https://example.com/post",
			Title: "Example",
			Body:  "content",
		},
		ContentType:          "article",
		RequestedLibraryKind: model.RequestedLibraryKindAuto,
	}
}

// responseFormatOf digs the strict schema block out of a built payload.
// Returns nil when the payload carries no response_format at all.
func responseFormatOf(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	raw, present := payload["response_format"]
	if !present {
		return nil
	}
	block, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("response_format is %T, want map[string]any", raw)
	}
	return block
}

func schemaOf(t *testing.T, block map[string]any) map[string]any {
	t.Helper()
	if block == nil {
		t.Fatal("payload has no response_format")
	}
	if block["type"] != "json_schema" {
		t.Fatalf("response_format type = %v, want json_schema", block["type"])
	}
	jsonSchema, ok := block["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("json_schema is %T, want map[string]any", block["json_schema"])
	}
	if jsonSchema["strict"] != true {
		t.Fatalf("json_schema.strict = %v, want true", jsonSchema["strict"])
	}
	schema, ok := jsonSchema["schema"].(map[string]any)
	if !ok {
		t.Fatalf("schema is %T, want map[string]any", jsonSchema["schema"])
	}
	return schema
}

// assertStrictModeInvariants walks every object node and checks the two
// rules OpenAI strict mode rejects a schema for: required must list exactly
// the declared properties, and additionalProperties must be false. Walking
// recursively matters because the v2 schema nests two profile objects — a
// nested violation fails the request just as hard as a root one, and is far
// easier to miss by eye.
func assertStrictModeInvariants(t *testing.T, path string, node map[string]any) {
	t.Helper()

	props, hasProps := node["properties"].(map[string]any)
	if !hasProps {
		return
	}
	if node["additionalProperties"] != false {
		t.Fatalf("%s: additionalProperties = %v, want false", path, node["additionalProperties"])
	}
	required, ok := node["required"].([]any)
	if !ok {
		t.Fatalf("%s: required is %T, want []any", path, node["required"])
	}
	if len(required) != len(props) {
		t.Fatalf("%s: required has %d entries but %d properties are declared — strict mode needs every property required",
			path, len(required), len(props))
	}
	for _, name := range required {
		key, ok := name.(string)
		if !ok {
			t.Fatalf("%s: required entry %v is %T, want string", path, name, name)
		}
		if _, present := props[key]; !present {
			t.Fatalf("%s: required lists %q which is not a declared property", path, key)
		}
	}
	for key, raw := range props {
		if child, ok := raw.(map[string]any); ok {
			assertStrictModeInvariants(t, path+"."+key, child)
		}
	}
}

// TestProductionPayloadPinsLibrarySchema is the core contract, and the
// regression guard for the bug that made this feature dead code: the shape
// production actually sends must carry a schema, and it must be the v2
// discriminated-union schema rather than the bare {title,summary,tags} one,
// because buildAnalyzePayload appends libraryOutputContract for every
// non-empty kind — including "auto".
func TestProductionPayloadPinsLibrarySchema(t *testing.T) {
	t.Parallel()

	a := newStructuredTestAnalyzer(t, "https://example.invalid", http.DefaultClient)
	block := responseFormatOf(t, a.buildAnalyzePayload(productionAnalyzeRequest()))
	schema := schemaOf(t, block)
	assertStrictModeInvariants(t, "schema", schema)

	props, _ := schema["properties"].(map[string]any)
	for _, want := range []string{
		"schema_version", "library_kind",
		"reading_profile", "site_profile",
	} {
		if _, present := props[want]; !present {
			t.Fatalf("v2 schema is missing the %q property: %#v", want, props)
		}
	}
	if _, plain := props["summary"]; plain {
		t.Fatalf("production payload pinned the plain schema, but the prompt asks for the v2 union: %#v", props)
	}

	// Both profiles must be nullable: strict mode forbids a root anyOf, so
	// "one of these two" is expressed by letting the unused branch be null.
	// validateLibraryAnalysisResponse only reads the branch matching
	// library_kind, so a non-nullable unused branch would force the model to
	// invent a whole second profile.
	for _, key := range []string{"reading_profile", "site_profile"} {
		profile, ok := props[key].(map[string]any)
		if !ok {
			t.Fatalf("%s is %T, want map[string]any", key, props[key])
		}
		types, ok := profile["type"].([]any)
		if !ok || len(types) != 2 {
			t.Fatalf("%s type = %#v, want a nullable object union", key, profile["type"])
		}
	}

	// site_profile must not declare summary: libraryOutputContract says
	// "site 不得生成 summary", and additionalProperties:false now enforces it.
	siteProfile, _ := props["site_profile"].(map[string]any)
	siteProps, _ := siteProfile["properties"].(map[string]any)
	if _, present := siteProps["summary"]; present {
		t.Fatalf("site_profile declares summary, which the v2 contract forbids: %#v", siteProps)
	}

	if _, err := jsonx.Marshal(block); err != nil {
		t.Fatalf("response_format does not marshal: %v", err)
	}
}

// TestExplicitKindPinsLibraryKindEnum: when the caller already decided the
// collection, constraining the enum makes the "library_kind conflicts with
// explicit request" rejection in validateLibraryAnalysisResponse
// unreachable rather than merely recoverable.
func TestExplicitKindPinsLibraryKindEnum(t *testing.T) {
	t.Parallel()

	cases := map[model.RequestedLibraryKind][]any{
		model.RequestedLibraryKindReading: {"reading"},
		model.RequestedLibraryKindSite:    {"site"},
		model.RequestedLibraryKindAuto:    {"reading", "site"},
	}
	for kind, want := range cases {
		kind, want := kind, want
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			a := newStructuredTestAnalyzer(t, "https://example.invalid", http.DefaultClient)
			req := productionAnalyzeRequest()
			req.RequestedLibraryKind = kind
			schema := schemaOf(t, responseFormatOf(t, a.buildAnalyzePayload(req)))
			props, _ := schema["properties"].(map[string]any)
			libraryKind, _ := props["library_kind"].(map[string]any)
			got, _ := libraryKind["enum"].([]any)
			if len(got) != len(want) {
				t.Fatalf("library_kind enum = %#v, want %#v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("library_kind enum = %#v, want %#v", got, want)
				}
			}
		})
	}
}

// TestUnpinnedPathsOmitStructuredOutput guards the two shapes that must stay
// free-form. URL-direct replies {"accessible":false} alone when the model
// cannot fetch — a shape strict mode would reject, and the one runURLDirect
// depends on to fall back to the local fetcher. The eval override owns its
// own output shape by definition.
func TestUnpinnedPathsOmitStructuredOutput(t *testing.T) {
	t.Parallel()

	cases := map[string]func(AnalyzeRequest) AnalyzeRequest{
		"url-direct": func(r AnalyzeRequest) AnalyzeRequest {
			r.URLDirect = true
			return r
		},
		"prompt-override": func(r AnalyzeRequest) AnalyzeRequest {
			r.SystemPromptOverride = "custom eval prompt"
			return r
		},
	}

	for name, mutate := range cases {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newStructuredTestAnalyzer(t, "https://example.invalid", http.DefaultClient)
			payload := a.buildAnalyzePayload(mutate(productionAnalyzeRequest()))
			if block := responseFormatOf(t, payload); block != nil {
				t.Fatalf("%s payload must not pin a strict schema: %#v", name, block)
			}
		})
	}
}

// v2Reply is a well-formed schema_version=2 reading response, i.e. what the
// prompt actually asks production for.
const v2Reply = `{"choices":[{"message":{"content":"{\"schema_version\":2,\"library_kind\":\"reading\",\"reading_profile\":{\"title\":\"T\",\"summary\":\"这是一段足够长的中文摘要，用来通过摘要校验。\",\"tags\":[\"Go\"]},\"site_profile\":null}"}}]}`

// Asserted at the parser rather than through Analyze: Analyze additionally
// runs summary conformance, which rejects any result with an empty summary —
// and a site result never has one. That is a pre-existing defect on main
// independent of this schema, and fixing it is not this change's business.
// What IS this change's business is that the schema's mandatory
// "reading_profile": null does not disturb site parsing.
func TestSiteResponseWithNullReadingProfileParses(t *testing.T) {
	t.Parallel()

	a := &OpenAIAnalyzer{maxTags: 5, maxTagChars: 20}
	result, err := a.parseAnalysisResponseForRequest(
		`{"schema_version":2,"library_kind":"site",`+
			`"reading_profile":null,`+
			`"site_profile":{"name":"Excalidraw","intro":"一个手绘风格的在线白板工具。",`+
			`"entry_name":"白板","purpose":"画架构草图","tags":["白板"]}}`,
		120, model.RequestedLibraryKindSite,
	)
	if err != nil {
		t.Fatalf("parse error = %v — the schema forces reading_profile:null on site responses", err)
	}
	if result.LibraryKind != model.LibraryKindSite {
		t.Fatalf("LibraryKind = %q, want site", result.LibraryKind)
	}
	if result.SiteName != "Excalidraw" || result.EntryName != "白板" {
		t.Fatalf("site profile lost in parsing: %#v", result)
	}
	if len(result.Tags) == 0 {
		t.Fatalf("site tags dropped: %#v", result)
	}
}

// The reading direction of the same concern: site_profile:null must not
// disturb reading parsing either.
func TestReadingResponseWithNullSiteProfileParses(t *testing.T) {
	t.Parallel()

	a := &OpenAIAnalyzer{maxTags: 5, maxTagChars: 20}
	result, err := a.parseAnalysisResponseForRequest(
		`{"schema_version":2,"library_kind":"reading",`+
			`"reading_profile":{"title":"T","summary":"这是一段中文摘要。","tags":["Go"]},`+
			`"site_profile":null}`,
		120, model.RequestedLibraryKindReading,
	)
	if err != nil {
		t.Fatalf("parse error = %v", err)
	}
	if result.LibraryKind != model.LibraryKindReading || result.Summary == "" {
		t.Fatalf("reading profile lost in parsing: %#v", result)
	}
}

// TestStructuredOutputDemotesOnSchemaRejection is the production-safety
// case: a gateway that does not implement structured outputs answers 400.
// Without demotion that 400 is classified non-retryable, so every analysis
// would fail the moment this feature shipped against such a gateway.
func TestStructuredOutputDemotesOnSchemaRejection(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	var sawStructured, sawPlain atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var body map[string]any
		if err := jsonx.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request %d: %v", n, err)
		}
		_, structured := body["response_format"]
		w.Header().Set("Content-Type", "application/json")
		if structured {
			sawStructured.Store(true)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format is not supported"}}`))
			return
		}
		sawPlain.Store(true)
		_, _ = w.Write([]byte(v2Reply))
	}))
	defer server.Close()

	a := newStructuredTestAnalyzer(t, server.URL, server.Client())

	result, err := a.Analyze(context.Background(), productionAnalyzeRequest())
	if err != nil {
		t.Fatalf("Analyze() error = %v, want success after demotion", err)
	}
	if !sawStructured.Load() || !sawPlain.Load() {
		t.Fatalf("expected one structured attempt then one demoted attempt (structured=%v plain=%v)", sawStructured.Load(), sawPlain.Load())
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want exactly 2 (reject + demoted resend)", calls.Load())
	}
	if len(result.Tags) == 0 {
		t.Fatalf("demoted result lost its tags: %#v", result)
	}

	// The latch is process-wide: a second Analyze must not re-probe the
	// gateway, otherwise every future request pays a wasted round trip.
	if _, err := a.Analyze(context.Background(), productionAnalyzeRequest()); err != nil {
		t.Fatalf("second Analyze() error = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("HTTP calls = %d, want 3 — the second Analyze re-sent response_format", calls.Load())
	}
	if block := responseFormatOf(t, a.buildAnalyzePayload(productionAnalyzeRequest())); block != nil {
		t.Fatalf("latched analyzer still builds a schema: %#v", block)
	}
}

// TestDemotedResendDoesNotConsumeRetryBudget: AI_RETRY_ATTEMPTS=1 is a legal
// config (config/validate.go only requires > 0). The demoted resend is a
// different request shape, not a retry of the same one — charging it to the
// budget would fail the first link outright, having never actually sent the
// demoted request.
func TestDemotedResendDoesNotConsumeRetryBudget(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		if err := jsonx.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, structured := body["response_format"]; structured {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field response_format"}}`))
			return
		}
		_, _ = w.Write([]byte(v2Reply))
	}))
	defer server.Close()

	a := newStructuredTestAnalyzer(t, server.URL, server.Client(), func(o *OpenAIAnalyzerOptions) {
		o.EmptyResponseRetries = 1
	})

	if _, err := a.Analyze(context.Background(), productionAnalyzeRequest()); err != nil {
		t.Fatalf("Analyze() error = %v; the demoted resend was swallowed by the retry budget", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("HTTP calls = %d, want 2 (reject + demoted resend) even with a budget of 1", calls.Load())
	}
}

// TestUnrelated400DoesNotDemoteAfterProvenSuccess is the counterpart to the
// demotion test: once the gateway has accepted a structured request, a later
// 400 is something else. 400 is also what gateways return for
// context_length_exceeded, a bad max_tokens, or a content-filter refusal, and
// the latch is process-wide and one-way — a single over-long article must not
// cost the whole process its structured output.
func TestUnrelated400DoesNotDemoteAfterProvenSuccess(t *testing.T) {
	t.Parallel()

	bodies := map[string]string{
		"context length": `{"error":{"code":"context_length_exceeded","message":"maximum context length is 8192 tokens"}}`,
		"content filter": `{"error":{"code":"content_filter","message":"the request was rejected"}}`,
		"bad max_tokens": `{"error":{"param":"max_tokens","message":"invalid value for max_tokens"}}`,
		// The reason mentionsResponseSchema reads error.param/message rather
		// than the raw body: gateways that echo the request back would
		// otherwise make every 4xx look like a schema rejection, because our
		// own request contains both field names.
		"echoed request": `{"error":{"code":"context_length_exceeded","message":"too long"},` +
			`"request":{"response_format":{"type":"json_schema","json_schema":{"name":"content_analysis"}}}}`,
	}
	for name, errBody := range bodies {
		name, errBody := name, errBody
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if n == 1 {
					// First call succeeds, proving the gateway understands
					// response_format.
					_, _ = w.Write([]byte(v2Reply))
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(errBody))
			}))
			defer server.Close()

			a := newStructuredTestAnalyzer(t, server.URL, server.Client())
			if _, err := a.Analyze(context.Background(), productionAnalyzeRequest()); err != nil {
				t.Fatalf("priming Analyze() error = %v", err)
			}
			if !a.structuredProven.Load() {
				t.Fatal("a 2xx to a structured request did not mark the gateway as proven")
			}

			if _, err := a.Analyze(context.Background(), productionAnalyzeRequest()); err == nil {
				t.Fatal("Analyze() error = nil, want the upstream 400")
			}
			if a.structuredUnsupported.Load() {
				t.Fatalf("%q 400 wrongly latched structured output off for the whole process", name)
			}
			if calls.Load() != 2 {
				t.Fatalf("HTTP calls = %d, want 2 — an unrelated 400 is non-retryable and must not trigger a resend", calls.Load())
			}
		})
	}
}

// TestUnnamed400BeforeProofStillDemotes is the fail-closed guard. A gateway
// that rejects response_format without naming it (`{"error":"Invalid request
// body"}`, an empty body, a WAF HTML page) would otherwise fail EVERY link:
// 400 is non-retryable, so each parse dies on its first attempt and the site
// stops tagging unless automatic compatibility demotion handles the rejection.
//
// Before any structured request has succeeded, response_format is the only
// thing that changed about our request shape, so an unexplained 400/422 is
// treated as its rejection. Being wrong here costs the process its
// structured output; being wrong the other way costs every link.
func TestUnnamed400BeforeProofStillDemotes(t *testing.T) {
	t.Parallel()

	bodies := map[string]string{
		"unnamed json": `{"error":{"message":"Invalid request body"}}`,
		"empty body":   ``,
		"waf html":     `<html><body>403 Forbidden</body></html>`,
	}
	for name, errBody := range bodies {
		name, errBody := name, errBody
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				if err := jsonx.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				if _, structured := body["response_format"]; structured {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(errBody))
					return
				}
				_, _ = w.Write([]byte(v2Reply))
			}))
			defer server.Close()

			a := newStructuredTestAnalyzer(t, server.URL, server.Client())
			if _, err := a.Analyze(context.Background(), productionAnalyzeRequest()); err != nil {
				t.Fatalf("Analyze() error = %v; an unnamed rejection must degrade, not fail every link", err)
			}
			if calls.Load() != 2 {
				t.Fatalf("%q: HTTP calls = %d, want 2 (reject + demoted resend)", name, calls.Load())
			}
			if !a.structuredUnsupported.Load() {
				t.Fatalf("%q: gateway rejected the field but structured output was not disabled", name)
			}
		})
	}
}

// TestStructuredOutputDoesNotDemoteOnUnrelatedStatuses keeps the one-way
// latch from firing on statuses that say nothing about response_format
// support, even when the body happens to mention it.
func TestStructuredOutputDoesNotDemoteOnUnrelatedStatuses(t *testing.T) {
	t.Parallel()

	for name, status := range map[string]int{
		"unauthorized": http.StatusUnauthorized,
		"forbidden":    http.StatusForbidden,
		"not-found":    http.StatusNotFound,
		"rate-limited": http.StatusTooManyRequests,
	} {
		name, status := name, status
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				// Body deliberately names response_format: status alone must
				// be enough to refuse the latch.
				_, _ = w.Write([]byte(`{"error":"response_format nope"}`))
			}))
			defer server.Close()

			a := newStructuredTestAnalyzer(t, server.URL, server.Client())
			if _, err := a.Analyze(context.Background(), productionAnalyzeRequest()); err == nil {
				t.Fatal("Analyze() error = nil, want upstream failure")
			}
			if a.structuredUnsupported.Load() {
				t.Fatalf("status %d wrongly latched structured output off", status)
			}
		})
	}
}

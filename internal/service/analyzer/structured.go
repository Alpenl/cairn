package analyzer

import (
	"errors"
	"net/http"
	"strings"

	"webtag/internal/model"
)

// structuredOutputSchemaName labels the schema in the request; upstreams echo
// it back in error messages, so keep it descriptive.
const structuredOutputSchemaName = "content_analysis"

// strict:true makes the provider constrain decoding to the schema, which
// removes the whole "model ignored the JSON contract" failure class — the one
// the jsonCandidates / scanJSONObjectCandidates recovery ladder in
// response.go exists to survive. That ladder stays: it still covers the
// unpinned paths (URL-direct, prompt overrides) and gateways that accept
// response_format but do not actually enforce it.
//
// strict mode constraints these schemas are built around, verified against
// the current OpenAI structured-outputs schema-support reference:
//
//   - the root must be an object and must not use anyOf;
//   - every property must appear in `required`, so "optional" is expressed as
//     a nullable type union rather than by omission;
//   - additionalProperties must be false on every object;
//   - enum (including on integer) is supported.
//
// Beyond those the schemas stay on the smallest portable subset: no numeric
// bounds, no string lengths, no minItems. Every such keyword is validated
// again in response.go anyway, so schema-side duplicates buy nothing while
// being the first thing a non-OpenAI gateway drops.
//
// A gateway that rejects any of this answers 400/422 naming the field, which
// demoteStructuredOutput turns into a one-time fallback rather than an
// outage, so a stricter-than-documented upstream degrades instead of failing.

// librarySchema pins the v2 discriminated union from libraryOutputContract —
// the shape every production analysis actually returns, since
// requestedKindForLink resolves to "auto" / "reading" / "site" and
// buildAnalyzePayload appends the v2 contract for all three.
//
// The union is expressed with both profile objects nullable rather than as a
// root-level anyOf, which strict mode forbids. That matches
// validateLibraryAnalysisResponse: it reads reading_profile only when
// library_kind is "reading" and site_profile only when it is "site", so the
// unused branch being null is exactly what it expects.
//
// requested pins the library_kind enum when the caller already decided. That
// is a real gain beyond JSON well-formedness: it makes the
// "library_kind conflicts with explicit request" rejection at
// response.go:117 unreachable instead of merely recoverable.
func librarySchema(requested model.RequestedLibraryKind) map[string]any {
	kinds := []any{string(model.LibraryKindReading), string(model.LibraryKindSite)}
	switch requested {
	case model.RequestedLibraryKindReading:
		kinds = []any{string(model.LibraryKindReading)}
	case model.RequestedLibraryKindSite:
		kinds = []any{string(model.LibraryKindSite)}
	}

	readingProfile := map[string]any{
		// Nullable object: present when library_kind is "reading", null
		// otherwise.
		//
		// The merged type-array form is deliberate — do NOT "fix" this into
		// anyOf:[{object},{null}]. OpenAI's strict validator recognises the
		// type-array union for nullability (the docs describe emulating an
		// optional field with "a union type with null"); the anyOf spelling is
		// the less reliable one, and anyOf is outright rejected at the ROOT,
		// which is why the two profiles are nullable siblings here instead of
		// a root-level union of two response shapes.
		"type": []any{"object", "null"},
		"properties": map[string]any{
			"title":   map[string]any{"type": "string"},
			"summary": map[string]any{"type": "string"},
			"tags":    stringArraySchema(),
		},
		"required":             []any{"title", "summary", "tags"},
		"additionalProperties": false,
	}
	siteProfile := map[string]any{
		"type": []any{"object", "null"},
		"properties": map[string]any{
			"name":       map[string]any{"type": "string"},
			"intro":      map[string]any{"type": "string"},
			"entry_name": map[string]any{"type": "string"},
			"purpose":    map[string]any{"type": "string"},
			"tags":       stringArraySchema(),
		},
		// No summary: libraryOutputContract states "site 不得生成 summary",
		// and additionalProperties:false now enforces that rather than
		// trusting the model to have read it.
		"required":             []any{"name", "intro", "entry_name", "purpose", "tags"},
		"additionalProperties": false,
	}

	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"schema_version":  map[string]any{"type": "integer", "enum": []any{2}},
			"library_kind":    map[string]any{"type": "string", "enum": kinds},
			"reading_profile": readingProfile,
			"site_profile":    siteProfile,
		},
		"required": []any{
			"schema_version", "library_kind", "reading_profile", "site_profile",
		},
		"additionalProperties": false,
	}
}

func stringArraySchema() map[string]any {
	// No minItems: the interstitial guard asks for an empty array on
	// blocked/error pages, and validateTags accepts one.
	return map[string]any{
		"type":  "array",
		"items": map[string]any{"type": "string"},
	}
}

// analysisResponseFormat wraps the schema matching this request's output
// contract in the OpenAI response_format envelope. Returns nil when the
// request must not be schema-pinned.
func (a *OpenAIAnalyzer) analysisResponseFormat(req AnalyzeRequest) map[string]any {
	if !a.wantsStructuredOutput(req) {
		return nil
	}
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   structuredOutputSchemaName,
			"strict": true,
			"schema": librarySchema(req.RequestedLibraryKind),
		},
	}
}

// wantsStructuredOutput reports whether this request may carry a strict
// response_format. Two shapes stay unpinned:
//
//   - URLDirect adds an `accessible` field and, when the model cannot fetch
//     the page, replies with that field alone — a shape strict mode would
//     reject, and one runURLDirect depends on to fall back to the local
//     fetcher. It also runs through third-party gateways (grok2api) whose
//     response_format support is unknown.
//   - SystemPromptOverride is the eval path: the experiment owns the output
//     shape, so pinning it here would silently invalidate the experiment.
//
// RequestedLibraryKind selects which library_kind values the schema accepts.
//
// The sticky runtime flag is checked first so a gateway that rejected the
// block once stops receiving it for the rest of the process lifetime.
func (a *OpenAIAnalyzer) wantsStructuredOutput(req AnalyzeRequest) bool {
	if a.structuredUnsupported.Load() {
		return false
	}
	if req.URLDirect {
		return false
	}
	return strings.TrimSpace(req.SystemPromptOverride) == ""
}

// demoteStructuredOutput handles a gateway that rejects the response_format
// block outright. OpenAI-compatible proxies that do not implement structured
// outputs answer with a 4xx rather than ignoring the field, which would
// otherwise turn every analysis into a hard failure the moment the feature
// ships against such a gateway.
//
// On that signal it strips the block from the in-flight payload and latches
// a.structuredUnsupported so later requests never rebuild it. It reports
// whether the caller should immediately re-attempt with the mutated payload.
//
// Status must be 400 or 422. 401/403 (bad credentials), 404 (wrong base URL
// or model), 429 (rate limit) and 5xx (upstream outage) say nothing about
// whether the gateway understands response_format; demoting on them would
// trade a transient failure for a permanent capability loss.
//
// Within 400/422 the question is which way to be wrong, and the two errors
// are not symmetric:
//
//   - Demoting when we should not have costs the process its structured
//     output. Analysis keeps working via the recovery ladder in response.go;
//     this is the behaviour that shipped before this feature existed.
//   - NOT demoting when we should have costs every link. A 400 is
//     non-retryable, so each parse fails at the first attempt and the whole
//     site stops tagging until the process is fixed or restarted.
//
// So the rule biases toward demoting, bounded by structuredProven: until a
// structured request has actually succeeded once, ANY 400/422 demotes,
// because the only thing that changed about our request shape is this field.
// After the first proven success the gateway has demonstrated it understands
// the field, so a later 400 is something else — context_length_exceeded is
// the common one — and only an error that explicitly names the field demotes.
//
// The demotion is reported to the caller so it can be logged; a silent
// one-way capability loss is not something an operator should have to infer
// from a rise in malformed-JSON retries.
func (a *OpenAIAnalyzer) demoteStructuredOutput(payload map[string]any, err error) bool {
	if _, present := payload["response_format"]; !present {
		return false
	}
	var callErr *analyzerCallError
	if !errors.As(err, &callErr) {
		return false
	}
	if !isSchemaRejectionStatus(callErr.statusCode) {
		return false
	}
	if a.structuredProven.Load() && !callErr.schemaRejected {
		return false
	}
	delete(payload, "response_format")
	// Log before latching so the notice fires exactly once: the payload no
	// longer carries response_format, so no later call reaches this point.
	if a.logger != nil {
		a.logger.Warn("upstream rejected structured output; disabling it for this process",
			"status", callErr.statusCode,
			"model", a.model,
			// named=false means we demoted on the pre-proof rule rather than
			// an explicit rejection — the distinction an operator needs to
			// tell "this gateway lacks structured outputs" from "we guessed".
			"named", callErr.schemaRejected,
		)
	}
	a.structuredUnsupported.Store(true)
	return true
}

// isSchemaRejectionStatus reports the two statuses an OpenAI-compatible
// gateway uses to reject a request it cannot parse. Shared with the HTTP
// layer so the demotion rule and the body-inspection gate cannot drift.
func isSchemaRejectionStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnprocessableEntity
}

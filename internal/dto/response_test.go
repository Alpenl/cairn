package dto

import (
	"encoding/json"
	"testing"
)

func TestSubmitResponseJSONShape(t *testing.T) {
	payload, err := json.Marshal(SubmitResponse{
		LinkID: "link-1",
		Status: "pending",
	})
	if err != nil {
		t.Fatalf("Marshal() returned error: %v", err)
	}

	got := string(payload)
	for _, want := range []string{`"link_id"`, `"status"`} {
		if !contains(got, want) {
			t.Fatalf("SubmitResponse JSON = %s, missing %s", got, want)
		}
	}
	if contains(got, `"job_id"`) {
		t.Fatalf("SubmitResponse JSON = %s, legacy job_id must not be exposed", got)
	}
}

func TestErrorResponseJSONShape(t *testing.T) {
	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Code:      404,
			ErrorCode: "link_not_found",
			Message:   "link not found",
		},
	})
	if err != nil {
		t.Fatalf("Marshal() returned error: %v", err)
	}

	got := string(payload)
	for _, want := range []string{`"error"`, `"code"`, `"error_code"`, `"link_not_found"`, `"message"`} {
		if !contains(got, want) {
			t.Fatalf("ErrorResponse JSON = %s, missing %s", got, want)
		}
	}
}

// TestErrorDetailOmitsErrorCodeWhenEmpty 锁定 omitempty：旧响应路径未填
// slug 时不应出现空串 "error_code":""（多余且容易让客户端误判）。
// middleware.JSONError / JSONErrorWithSlug 默认会给出 default_<status>，
// 但 DTO 自身仍需保留对零值的兼容性以便其他直接构造 ErrorDetail 的代码
// 不会泄露空字段。
func TestErrorDetailOmitsErrorCodeWhenEmpty(t *testing.T) {
	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Code:    500,
			Message: "boom",
		},
	})
	if err != nil {
		t.Fatalf("Marshal() returned error: %v", err)
	}
	got := string(payload)
	if contains(got, `"error_code"`) {
		t.Fatalf("ErrorResponse JSON = %s, expected error_code field to be omitted when empty", got)
	}
}

func TestLinkResponseJSONShapeIncludesDiagnosticFields(t *testing.T) {
	lowConfidenceReason := "search_fallback"
	errorCategory := "upstream_http"

	linkPayload, err := json.Marshal(LinkResponse{
		ID:                  "link-1",
		URL:                 "https://example.com/post",
		Status:              "done",
		Tags:                []string{"Go"},
		IsLowConfidence:     true,
		LowConfidenceReason: &lowConfidenceReason,
		ErrorCategory:       &errorCategory,
	})
	if err != nil {
		t.Fatalf("Marshal(LinkResponse) returned error: %v", err)
	}

	linkJSON := string(linkPayload)
	for _, want := range []string{`"low_confidence_reason"`, `"error_category"`, `"is_low_confidence"`} {
		if !contains(linkJSON, want) {
			t.Fatalf("LinkResponse JSON = %s, missing %s", linkJSON, want)
		}
	}
}

func contains(got, want string) bool {
	return len(got) >= len(want) && (got == want || jsonContains(got, want))
}

func jsonContains(got, want string) bool {
	for i := 0; i+len(want) <= len(got); i++ {
		if got[i:i+len(want)] == want {
			return true
		}
	}
	return false
}

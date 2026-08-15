package dto

import (
	"errors"
	"net/http"
	"testing"

	"webtag/internal/httperr"
)

func strptr(s string) *string { return &s }

func TestNormalizeParseDepth(t *testing.T) {
	cases := []struct {
		name    string
		in      *string
		wantVal string
		wantErr bool
	}{
		{"nil pointer", nil, "", false},
		{"empty string", strptr(""), "", false},
		{"whitespace only", strptr("   "), "", false},
		{"light lower", strptr("light"), "light", false},
		{"LIGHT upper", strptr("LIGHT"), "light", false},
		{"Deep mixed", strptr("Deep"), "deep", false},
		{"full alias", strptr("full"), "full", false},
		{"with surrounding whitespace", strptr("  light  "), "light", false},
		{"invalid value", strptr("medium"), "", true},
		{"garbage", strptr("🚀"), "", true},
		{"sql injection-ish", strptr("'; DROP TABLE--"), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeParseDepth(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil with val=%q", got)
				}
				var hp httperr.StatusCarrier
				if !errors.As(err, &hp) {
					t.Fatalf("error %v does not satisfy httperr.StatusCarrier", err)
				}
				if hp.HTTPStatus() != http.StatusUnprocessableEntity {
					t.Errorf("status = %d, want 422", hp.HTTPStatus())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantVal {
				t.Errorf("got %q, want %q", got, tc.wantVal)
			}
		})
	}
}

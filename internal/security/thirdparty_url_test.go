package security

import "testing"

func TestThirdPartyURLProjectionSeparatesDisclosureFromDelegation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		want     string
		delegate bool
	}{
		{
			name:     "public URL",
			raw:      "https://example.com/private/path",
			want:     "https://example.com/private/path",
			delegate: true,
		},
		{
			name:     "fragment is projected away",
			raw:      "https://example.com/post#fragment-secret",
			want:     "https://example.com/post",
			delegate: true,
		},
		{
			name:     "userinfo blocks delegation",
			raw:      "https://alice:password@example.com/post",
			want:     "https://example.com/post",
			delegate: false,
		},
		{
			name:     "query blocks delegation",
			raw:      "https://example.com/post?signature=secret#fragment-secret",
			want:     "https://example.com/post",
			delegate: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, delegate := ThirdPartyURLProjection(tt.raw)
			if got != tt.want || delegate != tt.delegate {
				t.Fatalf("ThirdPartyURLProjection() = (%q, %v), want (%q, %v)", got, delegate, tt.want, tt.delegate)
			}
		})
	}
}

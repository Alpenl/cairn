package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestValidRequestIDAccepted(t *testing.T) {
	cases := []string{
		"req-123",
		"abc_DEF-789",
		"0123456789",
		strings.Repeat("a", maxRequestIDLength),
	}
	for _, c := range cases {
		if !validRequestID(c) {
			t.Fatalf("validRequestID(%q) = false, want true", c)
		}
	}
}

func TestValidRequestIDRejected(t *testing.T) {
	cases := []string{
		"", // empty
		strings.Repeat("a", maxRequestIDLength+1), // too long
		"req\nid",            // newline
		"req\rid",            // carriage return
		"req id",             // space
		"\x1b[31mred\x1b[0m", // ANSI escapes
		"req\x00id",          // NUL
		"req/id",             // slash
		"req:id",             // colon
		"中文",                 // non-ASCII
	}
	for _, c := range cases {
		if validRequestID(c) {
			t.Fatalf("validRequestID(%q) = true, want false", c)
		}
	}
}

func TestRequestIDDropsInvalidIncomingHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(RequestID(nil))
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, c.GetString(RequestIDContextKey))
	})

	for _, malicious := range []string{"req\nid", strings.Repeat("a", maxRequestIDLength+1), "req id"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/ping", nil)
		req.Header.Set(RequestIDHeader, malicious)
		router.ServeHTTP(rec, req)

		if rec.Header().Get(RequestIDHeader) == malicious {
			t.Fatalf("X-Request-ID = %q, expected fallback to UUID", malicious)
		}
		if rec.Header().Get(RequestIDHeader) == "" {
			t.Fatal("expected fallback X-Request-ID header to be set")
		}
		if rec.Body.String() == malicious {
			t.Fatalf("body propagated malicious id %q", malicious)
		}
	}
}

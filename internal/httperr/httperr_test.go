package httperr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"webtag/internal/problem"
)

func TestAsMapsApplicationProblemKinds(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		kind   problem.Kind
		status int
	}{
		{problem.Internal, http.StatusInternalServerError},
		{problem.Malformed, http.StatusBadRequest},
		{problem.Invalid, http.StatusUnprocessableEntity},
		{problem.NotFound, http.StatusNotFound},
		{problem.Conflict, http.StatusConflict},
		{problem.Precondition, http.StatusPreconditionRequired},
		{problem.TooLarge, http.StatusRequestEntityTooLarge},
		{problem.RateLimited, http.StatusTooManyRequests},
		{problem.Forbidden, http.StatusForbidden},
		{problem.Unavailable, http.StatusServiceUnavailable},
		{problem.Upstream, http.StatusBadGateway},
		{problem.Timeout, http.StatusGatewayTimeout},
		{problem.Canceled, 499},
	} {
		applicationError := problem.NewWithCode(test.kind, "stable_code", "safe message")
		carrier, ok := As(fmt.Errorf("wrapped: %w", applicationError))
		if !ok {
			t.Fatalf("kind %d did not map to an HTTP carrier", test.kind)
		}
		if carrier.HTTPStatus() != test.status || carrier.HTTPMessage() != "safe message" {
			t.Fatalf("kind %d maps to status=%d message=%q, want %d/safe message", test.kind, carrier.HTTPStatus(), carrier.HTTPMessage(), test.status)
		}
		coder, ok := carrier.(ErrorCoder)
		if !ok {
			t.Fatalf("kind %d did not map to a coded carrier", test.kind)
		}
		if coder.HTTPErrorCode() != "stable_code" {
			t.Fatalf("kind %d code = %q", test.kind, coder.HTTPErrorCode())
		}
	}
}

func TestAsMapsApplicationProblemMetadata(t *testing.T) {
	t.Parallel()

	revision := int64(7)
	applicationError := problem.NewWithCodeAndCurrentIdentity(
		problem.Conflict,
		problem.CodeContentRevisionConflict,
		"changed",
		problem.ConflictIdentity{ContentRevision: &revision, BlockKey: "content"},
	)
	carrier, ok := As(applicationError)
	if !ok {
		t.Fatal("application problem did not map to HTTP carrier")
	}
	provider, ok := carrier.(CurrentIdentityProvider)
	if !ok {
		t.Fatal("application conflict did not map identity metadata")
	}
	identity, present := provider.HTTPCurrentIdentity()
	if !present || identity.ContentRevision == nil || *identity.ContentRevision != revision || identity.BlockKey != "content" {
		t.Fatalf("current identity = %+v, present=%v", identity, present)
	}
}

func TestAs_NilError(t *testing.T) {
	t.Parallel()

	got, ok := As(nil)
	if ok {
		t.Fatalf("As(nil) ok = true, want false")
	}
	if got != nil {
		t.Fatalf("As(nil) carrier = %v, want nil", got)
	}
}

// TestAs_TypedNil ensures the typed-nil interface trap (a *Error whose
// underlying pointer is nil but whose interface tuple is non-nil) does not
// surface to handlers as a usable carrier. errors.As alone would say
// "ok=true" and the handler would call HTTPStatus on a nil receiver; the
// nil-receiver guards on *Error keep that from panicking, and As filters
// the typed-nil out so the caller falls back to its own generic-500 path.
func TestAs_TypedNil(t *testing.T) {
	t.Parallel()

	var typedNil *Error
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("As(typed nil) panicked: %v", r)
		}
	}()

	carrier, ok := As(error(typedNil))
	if ok {
		t.Fatalf("As(typed nil) ok = true, want false")
	}
	if carrier != nil {
		t.Fatalf("As(typed nil) carrier = %v, want nil", carrier)
	}

	// Even if a future regression flips the As policy back to "report
	// match", the receiver methods must be safe to call. Drive them
	// through the nil pointer directly to make that contract explicit.
	if got := typedNil.HTTPStatus(); got != http.StatusInternalServerError {
		t.Fatalf("(*Error)(nil).HTTPStatus() = %d, want 500", got)
	}
	if got := typedNil.HTTPMessage(); got != "" {
		t.Fatalf("(*Error)(nil).HTTPMessage() = %q, want empty", got)
	}
	if got := typedNil.Error(); got != "" {
		t.Fatalf("(*Error)(nil).Error() = %q, want empty", got)
	}
}

func TestAs_Wrapped(t *testing.T) {
	t.Parallel()

	inner := New(http.StatusNotFound, "missing")
	wrapped := fmt.Errorf("load thing: %w", inner)

	carrier, ok := As(wrapped)
	if !ok {
		t.Fatalf("As(wrapped) ok = false, want true")
	}
	if carrier.HTTPStatus() != http.StatusNotFound {
		t.Fatalf("HTTPStatus() = %d, want %d", carrier.HTTPStatus(), http.StatusNotFound)
	}
	if carrier.HTTPMessage() != "missing" {
		t.Fatalf("HTTPMessage() = %q, want %q", carrier.HTTPMessage(), "missing")
	}
}

func TestAs_DoubleWrapped(t *testing.T) {
	t.Parallel()

	inner := New(http.StatusBadRequest, "bad input")
	once := fmt.Errorf("layer1: %w", inner)
	twice := fmt.Errorf("layer2: %w", once)

	carrier, ok := As(twice)
	if !ok {
		t.Fatalf("As(double-wrapped) ok = false, want true")
	}
	if carrier.HTTPStatus() != http.StatusBadRequest {
		t.Fatalf("HTTPStatus() = %d, want %d", carrier.HTTPStatus(), http.StatusBadRequest)
	}
	if carrier.HTTPMessage() != "bad input" {
		t.Fatalf("HTTPMessage() = %q, want %q", carrier.HTTPMessage(), "bad input")
	}

	// errors.As-equivalence: As must agree with errors.As about whether a
	// concrete *Error sits in the unwrap chain.
	var target *Error
	if !errors.As(twice, &target) {
		t.Fatal("errors.As baseline disagrees: chain has no *Error")
	}
}

func TestError_Methods(t *testing.T) {
	t.Parallel()

	e := New(http.StatusTeapot, "i am a teapot")
	if e.HTTPStatus() != http.StatusTeapot {
		t.Fatalf("HTTPStatus() = %d, want %d", e.HTTPStatus(), http.StatusTeapot)
	}
	if e.HTTPMessage() != "i am a teapot" {
		t.Fatalf("HTTPMessage() = %q, want %q", e.HTTPMessage(), "i am a teapot")
	}
	if e.Error() != "i am a teapot" {
		t.Fatalf("Error() = %q, want %q", e.Error(), "i am a teapot")
	}

	// Compile-time confirmation that *Error satisfies StatusCarrier.
	var _ StatusCarrier = e
}

// TestAs_NonCarrier confirms a plain error in the chain does not fool As
// into reporting a match (regression guard for the typed-nil fix).
func TestAs_NonCarrier(t *testing.T) {
	t.Parallel()

	got, ok := As(errors.New("plain"))
	if ok {
		t.Fatalf("As(plain) ok = true, want false")
	}
	if got != nil {
		t.Fatalf("As(plain) carrier = %v, want nil", got)
	}
}

// TestNewWithCode 锁住三件事：1) 设置的 status/message/code 全部可读回；
// 2) HTTPErrorCode 返回的是 slug 原值；3) 不携带 RetryAfter 时
// RetryAfterSeconds 为 0。Wave 6 起 service 层用 NewWithCode 把稳定的
// error_code 串到 carrier 给前端，回归这条链会让前端 fallback 到
// default_<status>，前端按 token 分支处理的逻辑会断。
func TestNewWithCode(t *testing.T) {
	t.Parallel()

	e := NewWithCode(http.StatusBadRequest, "url_too_long", "URL exceeds the 2048-character limit")
	if e.HTTPStatus() != http.StatusBadRequest {
		t.Errorf("HTTPStatus() = %d, want %d", e.HTTPStatus(), http.StatusBadRequest)
	}
	if e.HTTPMessage() != "URL exceeds the 2048-character limit" {
		t.Errorf("HTTPMessage() = %q", e.HTTPMessage())
	}
	if e.HTTPErrorCode() != "url_too_long" {
		t.Errorf("HTTPErrorCode() = %q, want %q", e.HTTPErrorCode(), "url_too_long")
	}
	if e.RetryAfterSeconds() != 0 {
		t.Errorf("RetryAfterSeconds() = %d, want 0", e.RetryAfterSeconds())
	}
}

// TestNewWithRetryAfter 锁住两点：1) 正数 retrySeconds 原样保留；
// 2) 负数被收敛到 0（避免 presentation 层把负值塞进 Retry-After header
// 让客户端解析失败）。这条契约在 internal/middleware 的限流路径上是
// 调用方默认会信任的。
func TestNewWithRetryAfter(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   int
		want int
	}{
		{"positive_preserved", 30, 30},
		{"zero_preserved", 0, 0},
		{"negative_clamped_to_zero", -5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := NewWithRetryAfter(http.StatusTooManyRequests, "slow down", tc.in)
			if got := e.RetryAfterSeconds(); got != tc.want {
				t.Errorf("RetryAfterSeconds() = %d, want %d", got, tc.want)
			}
			// slug 应为空（未通过 NewWithCodeAndRetryAfter 设过）。
			if got := e.HTTPErrorCode(); got != "" {
				t.Errorf("HTTPErrorCode() = %q, want empty", got)
			}
		})
	}
}

// TestNewWithCodeAndRetryAfter 验证两个维度可以同时承载：slug 透传
// 给前端做分支；retrySeconds 透传给客户端做退避。负数仍被收敛到 0。
func TestNewWithCodeAndRetryAfter(t *testing.T) {
	t.Parallel()

	e := NewWithCodeAndRetryAfter(http.StatusTooManyRequests, "rate_limited", "rate limit exceeded", 60)
	if e.HTTPStatus() != http.StatusTooManyRequests {
		t.Errorf("HTTPStatus() = %d, want %d", e.HTTPStatus(), http.StatusTooManyRequests)
	}
	if e.HTTPErrorCode() != "rate_limited" {
		t.Errorf("HTTPErrorCode() = %q", e.HTTPErrorCode())
	}
	if e.RetryAfterSeconds() != 60 {
		t.Errorf("RetryAfterSeconds() = %d, want 60", e.RetryAfterSeconds())
	}

	// 负数被收敛。
	neg := NewWithCodeAndRetryAfter(http.StatusServiceUnavailable, "cold_start", "service warming up", -10)
	if got := neg.RetryAfterSeconds(); got != 0 {
		t.Errorf("negative RetryAfterSeconds() = %d, want 0", got)
	}
}

// TestNilReceiver_AccessorsSafe 把 *Error 的几个访问器在 nil 接收者下都
// 走一遍，确保 typed-nil 漏到 handler.writeError 时不会 panic。这条保险
// 与 As 的 typed-nil 防御呼应：As 会过滤掉 typed-nil，但万一未来有新路径
// 绕过 As，访问器自身得能扛住。
func TestNilReceiver_AccessorsSafe(t *testing.T) {
	t.Parallel()

	var nilErr *Error
	if got := nilErr.HTTPErrorCode(); got != "" {
		t.Errorf("(*Error)(nil).HTTPErrorCode() = %q, want empty", got)
	}
	if got := nilErr.RetryAfterSeconds(); got != 0 {
		t.Errorf("(*Error)(nil).RetryAfterSeconds() = %d, want 0", got)
	}
}

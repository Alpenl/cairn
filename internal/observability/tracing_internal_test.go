// observability/tracing_internal_test.go —— package-internal 测试，专门
// 验证 clampSamplingRatio 这种不能从外部包断言的内部行为。
// sdk TracerProvider 的 sampler 字段未导出，故无法在 observability_test
// 包里直接断言 ratio 被 clamp，因此把单点行为下沉到内部测试。
package observability

import (
	"context"
	"testing"
	"time"
)

// TestStripScheme 锁定 stripScheme 的兜底行为：endpoint 配置历史上允许
// 运维写完整 URL（"http://...", "https://...", "grpc://..."），otlptracegrpc
// 只接受 host:port，stripScheme 替运维把 scheme 削掉。回归会让
// otlptracegrpc.New 拒绝整个 endpoint，OTLP 全链路静默失效。
func TestStripScheme(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"http_prefix", "http://collector:4317", "collector:4317"},
		{"https_prefix", "https://collector:4317", "collector:4317"},
		{"grpc_prefix", "grpc://collector:4317", "collector:4317"},
		{"no_prefix_passthrough", "collector:4317", "collector:4317"},
		{"ip_no_prefix", "127.0.0.1:4317", "127.0.0.1:4317"},
		{"empty_string", "", ""},
		// 中间出现 :// 不应被删（只削首部）。
		{"scheme_only_at_head", "host://path:4317", "host://path:4317"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := stripScheme(tc.in); got != tc.want {
				t.Errorf("stripScheme(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEnvString 锁定本地 envString helper 的兜底约定：缺失或全空格时
// 返回 def。这条 helper 是 observability 包刻意复制的（避免反向依赖
// config 包），所以必须独立验证而不能假定 config.envString 的覆盖率
// 顺带覆盖了它。
//
// 注意：用 t.Setenv 时不能再 t.Parallel（标准库会 panic），所以三条
// 子测试都不并行。
func TestEnvString(t *testing.T) {
	t.Run("returns_default_when_unset", func(t *testing.T) {
		// 真正测 unset 分支：用一个从不在测试环境出现的 key，不调
		// t.Setenv，从而走 os.Getenv 返回 "" 的"未注册"路径。
		if got := envString("WEBTAG_TEST_DEFINITELY_NEVER_SET_2026_05_18", "fallback"); got != "fallback" {
			t.Errorf("envString unset key = %q, want %q", got, "fallback")
		}
	})

	t.Run("returns_default_when_empty_value", func(t *testing.T) {
		// set-as-empty 与 unset 在 os.Getenv 语义下等价（都返回 ""），
		// 但形成的代码路径来源不同，单独保留一条 case 防回归。
		t.Setenv("WEBTAG_TEST_EMPTY_KEY", "")
		if got := envString("WEBTAG_TEST_EMPTY_KEY", "fallback"); got != "fallback" {
			t.Errorf("envString empty value = %q, want %q", got, "fallback")
		}
	})

	t.Run("returns_default_when_whitespace_only", func(t *testing.T) {
		t.Setenv("WEBTAG_TEST_WS_KEY", "   \t\n  ")
		if got := envString("WEBTAG_TEST_WS_KEY", "fallback"); got != "fallback" {
			t.Errorf("envString whitespace-only = %q, want %q", got, "fallback")
		}
	})

	t.Run("trims_and_returns_value", func(t *testing.T) {
		t.Setenv("WEBTAG_TEST_VALID_KEY", "  webtag-svc  ")
		if got := envString("WEBTAG_TEST_VALID_KEY", "fallback"); got != "webtag-svc" {
			t.Errorf("envString trim = %q, want %q", got, "webtag-svc")
		}
	})
}

// TestClampSamplingRatio 锁定双层防御的内层：超出 [0, 1] 范围一律回到
// 默认 0.05，避免 InitTracer 装配出无意义的 sampler。validateConfig 在
// boot 时也会拒绝越界值，但内部 clamp 是兜底保险。
func TestClampSamplingRatio(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"zero_falls_to_default", 0, 0.05},
		{"negative_falls_to_default", -0.5, 0.05},
		{"above_one_falls_to_default", 1.5, 0.05},
		{"at_lower_bound_passes_through", 0.001, 0.001},
		{"at_upper_bound_passes_through", 1.0, 1.0},
		{"recommended_value_passes_through", 0.05, 0.05},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampSamplingRatio(tc.in); got != tc.want {
				t.Errorf("clampSamplingRatio(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestTracerShutdownFallbackPreservesExistingDeadline(t *testing.T) {
	t.Parallel()

	wantDeadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), wantDeadline)
	defer cancel()
	var gotDeadline time.Time
	shutdown := withTracerShutdownFallback(func(shutdownCtx context.Context) error {
		var ok bool
		gotDeadline, ok = shutdownCtx.Deadline()
		if !ok {
			t.Fatal("shutdown context has no deadline")
		}
		return nil
	})

	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if !gotDeadline.Equal(wantDeadline) {
		t.Fatalf("shutdown deadline = %v, want existing deadline %v", gotDeadline, wantDeadline)
	}
}

func TestTracerShutdownFallbackBoundsContextWithoutDeadline(t *testing.T) {
	t.Parallel()

	startedAt := time.Now()
	var gotDeadline time.Time
	shutdown := withTracerShutdownFallback(func(shutdownCtx context.Context) error {
		var ok bool
		gotDeadline, ok = shutdownCtx.Deadline()
		if !ok {
			t.Fatal("shutdown context has no deadline")
		}
		return nil
	})

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown() error = %v", err)
	}
	if minimum := startedAt.Add(time.Second); gotDeadline.Before(minimum) {
		t.Fatalf("shutdown deadline = %v, want after %v", gotDeadline, minimum)
	}
	if maximum := startedAt.Add(2 * time.Second); gotDeadline.After(maximum) {
		t.Fatalf("shutdown deadline = %v, want before %v", gotDeadline, maximum)
	}
}

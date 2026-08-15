package observability_test

import (
	"context"
	"net"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"webtag/internal/observability"
)

// TestInitTracer_EmptyEndpoint 验证 endpoint 为空时走 noop 路径：返回
// 非 nil shutdown、shutdown 可以调用且不报错，全局 tracer provider 是
// noop 实现（不是 sdk *TracerProvider）。
func TestInitTracer_EmptyEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	shutdown, err := observability.InitTracer(context.Background(), observability.InitTracerOptions{})
	if err != nil {
		t.Fatalf("InitTracer returned unexpected error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("shutdown func is nil")
	}

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatalf("expected noop provider for empty endpoint, got sdk *TracerProvider")
	}

	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned unexpected error: %v", err)
	}
}

// TestInitTracer_WithEndpointBuildsSDKProvider 验证 endpoint 非空时：
// 真实安装 sdk *TracerProvider（不再 fallback 到 noop）、shutdown 可
// 调且幂等、采样比例越界时被钳到默认值。
//
// 不真实起 collector：otlptracegrpc.New 默认是 non-blocking dial，
// 拨号失败不会让 New 返回 error，足够覆盖"装配链路通了"这个验证目标。
// 用 net.Listen 占一个本地端口仅为了让 endpoint 看起来合法。
func TestInitTracer_WithEndpointBuildsSDKProvider(t *testing.T) {
	// 占一个随机本地端口，给 endpoint 用。listener 在测试结束时关闭；
	// 即便 BatchSpanProcessor 后台 flush 时尝试连接，5s 后 shutdown 也
	// 会被超时兜底，不会卡测试。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	shutdown, err := observability.InitTracer(context.Background(), observability.InitTracerOptions{
		Endpoint:      ln.Addr().String(),
		SamplingRatio: 1.5, // 越界 → 应该被钳到默认 0.05，但 InitTracer 本身不报错
		Insecure:      true,
		AppEnv:        "test",
		ServiceName:   "webtag-test",
	})
	if err != nil {
		t.Fatalf("InitTracer returned unexpected error: %v", err)
	}
	t.Cleanup(func() {
		// 给 shutdown 一个独立超时，防止后台 batch flush 把测试拖慢。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(ctx)
	})

	tp, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("expected sdk *TracerProvider, got %T", otel.GetTracerProvider())
	}
	if tp == nil {
		t.Fatal("sdk tracer provider is nil")
	}

	// 调一次 Tracer().Start 验证整个链路不 panic（noop 也不会 panic，但
	// 此处确认拿到的就是 sdk 实例）。
	tracer := observability.Tracer("test-tracer")
	if tracer == nil {
		t.Fatal("Tracer() returned nil")
	}
	_, span := tracer.Start(context.Background(), "smoke-span")
	span.End()

	// clamp 行为在独立单元测试里覆盖（package observability 内 _internal_test.go），
	// 此处只验证装配链路通了 + Tracer 可调；sdk TracerProvider 的 sampler
	// 字段未导出，没办法在 _test 包里直接断言。
}

// TestInitTracer_ShutdownIdempotent 验证 shutdown 多次调用安全。OTel
// SDK 的 TracerProvider.Shutdown 内部用 sync.Once 保护，再调一次返回
// nil 即可，不允许 panic。
func TestInitTracer_ShutdownIdempotent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	shutdown, err := observability.InitTracer(context.Background(), observability.InitTracerOptions{
		Endpoint: ln.Addr().String(),
		Insecure: true,
	})
	if err != nil {
		t.Fatalf("InitTracer: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Errorf("first shutdown: %v", err)
	}
	if err := shutdown(ctx); err != nil {
		t.Errorf("second shutdown should be a no-op: %v", err)
	}
}

// TestTracer_NotNil 保证 noop 路径下 Tracer() 仍返回非 nil 句柄。
func TestTracer_NotNil(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	_, _ = observability.InitTracer(context.Background(), observability.InitTracerOptions{})

	tr := observability.Tracer("test-tracer")
	if tr == nil {
		t.Error("Tracer() returned nil")
	}
}

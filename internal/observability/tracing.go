// Package observability – tracing init.
//
// 第二轮工程审计 H4 阶段实装：InitTracer 不再是占位 noop，当配置了
// OTLP endpoint 时会真实构造 OTLP gRPC exporter + BatchSpanProcessor
// + ParentBased(TraceIDRatioBased(ratio)) 采样器，并把 TracerProvider
// 注册为全局，配合 W3C TraceContext + Baggage propagator，让全链路
// （Gin / outbound HTTP / pgx）的 span 真正落到 collector。
//
// 关键非阻塞性约定：
//   - endpoint 为空 → 安装 noop tracer，shutdown 是廉价 no-op。
//     （旧行为，保持本地 / 测试 / 默认部署不动）
//   - endpoint 非空但 collector 暂时不可达 → otlptracegrpc.New 是
//     fire-and-forget 连接（gRPC dial 默认非阻塞），不会卡住启动；
//     真实 Export 时如果连接仍未建立会在 backoff 内重试。
//   - shutdown 必须由调用方在进程退出前 invoke，确保 BatchSpanProcessor
//     flush 缓冲区。已有 deadline 原样消费；只有独立调用未提供 deadline
//     时，wrapper 才添加 1.5s fallback。
//
// 用法：
//
//	shutdown, err := observability.InitTracer(ctx, observability.InitTracerOptions{
//	    Endpoint:      cfg.OTELEndpoint,
//	    SamplingRatio: cfg.OTELSamplingRatio,
//	    Insecure:      cfg.OTELInsecure,
//	    AppEnv:        cfg.AppEnv,
//	    ServiceName:   "webtag",
//	    ServiceVersion: buildinfo.VersionValue(),
//	})
//	if err != nil { … }
//	defer shutdown(context.Background())
package observability

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	// semconv 必须与 resource.Default() 使用的 schema URL 完全一致，否则
	// resource.Merge 会直接报 "conflicting Schema URL" 而不是自行取舍。
	// 这一版跟随 otel SDK 升级从 v1.40.0 移到 v1.43.0——SDK 一升，
	// resource.Default() 的 schema 就跟着走，semconv 落后就会让链路追踪
	// 初始化整体失败。
	//
	// v1.43.0 起不再提供 DeploymentEnvironmentName() 这个 helper，只保留
	// DeploymentEnvironmentNameKey，下面相应改用 Key.String()。
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerShutdown 是 InitTracer 返回的清理函数签名，与 sdk/trace
// TracerProvider.Shutdown 对齐，调用方切换实现时不用改 defer 站点。
type TracerShutdown func(context.Context) error

const tracerShutdownFallbackTimeout = 1500 * time.Millisecond

// noopShutdown 是 noop tracer 路径下的廉价兜底，调用立即返回 nil。
var noopShutdown TracerShutdown = func(context.Context) error { return nil }

// InitTracerOptions 收拢 InitTracer 的所有可调字段，避免位置参数膨胀。
// 所有字段都允许零值：
//   - Endpoint=="" → 装 noop tracer，所有其它字段被忽略。
//   - SamplingRatio<=0 或 >1 → 退回到 0.05（5%）。
//   - Insecure 默认 false 走 TLS；本地调试可设 true 用明文 gRPC。
//   - ServiceName=="" → 退回到 "webtag"。
//   - ServiceVersion=="" → 退回到 "unknown"。
//   - AppEnv=="" → 退回到 "prod"。
type InitTracerOptions struct {
	// Endpoint 是 OTLP collector 的 gRPC 地址（host:port，例如
	// "otel-collector:4317"）。空值 → 安装 noop tracer。
	Endpoint string
	// SamplingRatio 是 ParentBased + TraceIDRatioBased 的根采样比例。
	// 0.05 = 5%，适合 prod 控制 trace 后端体量；本地调试可调到 1.0。
	SamplingRatio float64
	// Insecure=true 时 gRPC 不启 TLS，仅用于本地 collector 调试。
	// prod 必须显式 false 走 TLS。
	Insecure bool
	// ServiceName 写入 resource service.name 属性，trace 后端按此聚合。
	ServiceName string
	// ServiceVersion 写入 resource service.version 属性。
	ServiceVersion string
	// AppEnv 写入 resource deployment.environment.name 属性，区分
	// dev / staging / prod 的 trace 流。
	AppEnv string
}

// InitTracer 安装全局 OpenTelemetry tracer。
//
// 返回值：
//   - TracerShutdown：进程退出前必须调用，flush 缓冲区 + 关闭 exporter。
//     noop 路径返回的 shutdown 是 no-op。
//   - error：仅在配置非法（如 ratio 越界）或 exporter 构造失败时返回；
//     gRPC 连接错误不会立即返回（fire-and-forget）。
//
// 关键决策：
//   - sampler 选 ParentBased，让上游传下来的 sampling decision 优先生效，
//     单服务采样决定不会破坏跨服务 trace 完整性。
//   - propagator 同时启用 TraceContext + Baggage，匹配大多数 collector 的
//     默认配置；不接 b3 / jaeger，避免引入额外 contrib 依赖。
//
// clampSamplingRatio 把超出 [0, 1] 范围的采样比例兜底到默认 0.05。
// validateConfig 也会在 boot 时拒绝越界值，本函数是双层防御里的内层 —
// 单元测试可直接验证 clamp 行为，无需走 sdk TracerProvider 的 unexported
// sampler 字段。
func clampSamplingRatio(ratio float64) float64 {
	if ratio <= 0 || ratio > 1 {
		return 0.05
	}
	return ratio
}

func InitTracer(ctx context.Context, opts InitTracerOptions) (TracerShutdown, error) {
	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		// 默认/本地路径：装 noop 即可，跳过 exporter / resource 构造。
		installNoopTracer()
		return noopShutdown, nil
	}

	// 把默认值兜底逻辑都集中到这里，构造 TracerProvider 时不再判空。
	serviceName := opts.ServiceName
	if serviceName == "" {
		serviceName = envString("OTEL_SERVICE_NAME", "webtag")
	}
	serviceVersion := opts.ServiceVersion
	if serviceVersion == "" {
		serviceVersion = "unknown"
	}
	appEnv := opts.AppEnv
	if appEnv == "" {
		appEnv = "prod"
	}
	ratio := clampSamplingRatio(opts.SamplingRatio)

	// Resource：把 service / deploy / instance 元数据打到所有 span 上。
	// hostname 取失败时降级为 "unknown"，不阻止启动；attributes 缺失对
	// trace 后端聚合的影响仅是少一个维度。
	hostname, hostErr := os.Hostname()
	if hostErr != nil || hostname == "" {
		hostname = "unknown"
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
			semconv.ServiceInstanceID(hostname),
			semconv.DeploymentEnvironmentNameKey.String(appEnv),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build otel resource: %w", err)
	}

	// gRPC exporter 选项：endpoint + 可选 insecure。WithEndpoint 接受
	// host:port，自带 scheme 也会被裁掉，避免调用方误传 "http://" 前缀
	// 影响连接。
	exporterOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(stripScheme(endpoint)),
	}
	if opts.Insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}
	exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("init otlp grpc exporter: %w", err)
	}

	// BatchSpanProcessor 默认参数（5s schedule、512 batch、2048 queue）
	// 对本服务量级足够；显式不传 BatchTimeout/MaxQueue，跟随上游默认便于
	// 后续升级 SDK 时拿到新优化。
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	slog.InfoContext(ctx, "otel tracer initialised",
		"endpoint", endpoint,
		"insecure", opts.Insecure,
		"sampling_ratio", ratio,
		"service", serviceName,
		"version", serviceVersion,
		"env", appEnv,
		"instance", hostname,
	)

	return withTracerShutdownFallback(tp.Shutdown), nil
}

func withTracerShutdownFallback(shutdown TracerShutdown) TracerShutdown {
	return func(shutdownCtx context.Context) error {
		if _, hasDeadline := shutdownCtx.Deadline(); hasDeadline {
			return shutdown(shutdownCtx)
		}
		ctx, cancel := context.WithTimeout(shutdownCtx, tracerShutdownFallbackTimeout)
		defer cancel()
		return shutdown(ctx)
	}
}

// installNoopTracer 设置全局 tracer provider 为 noop 实现。
func installNoopTracer() {
	otel.SetTracerProvider(noop.NewTracerProvider())
	// noop 路径下也设一份 propagator，避免下游业务读到 nil propagator
	// 出现 panic（otel 包默认 propagator 已经是空实现，这里显式覆写主要
	// 是为了对齐 sdk 路径的行为）。
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
}

// Tracer 返回全局 provider 上的一个命名 tracer。包装一层 otel.Tracer 让
// 业务包不直接依赖 go.opentelemetry.io/otel，便于以后切换实现。
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// envString 读环境变量，缺失或全空格时返回 def。复制一份避免 observability
// 包反向依赖 config 包。
func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// stripScheme 从 endpoint 移除 http:// / https:// / grpc:// 前缀，
// otlptracegrpc.WithEndpoint 只接受 host:port。配置文档里允许人写
// 完整 URL，这里替运维兜一层。
func stripScheme(endpoint string) string {
	for _, prefix := range []string{"https://", "http://", "grpc://"} {
		if strings.HasPrefix(endpoint, prefix) {
			return strings.TrimPrefix(endpoint, prefix)
		}
	}
	return endpoint
}

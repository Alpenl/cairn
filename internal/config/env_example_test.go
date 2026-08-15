package config

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestEnvExampleIncludesCurrentGoRuntimeVariables(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	content := string(data)

	required := []string{
		"DATABASE_URL=",
		"AI_BASE_URL=",
		"AI_API_KEY=",
		"AI_MODEL=",
		"PARSE_CONCURRENCY=",
		"LISTEN_ADDR=",
		"CORS_ORIGINS=",
		"TRUSTED_PROXY_CIDRS=",
		"READ_HEADER_TIMEOUT_MS=",
		"READ_TIMEOUT_MS=",
		"WRITE_TIMEOUT_MS=",
		"IDLE_TIMEOUT_MS=",
		"FETCH_RETRY_ATTEMPTS=",
		"FETCH_RETRY_DELAY_MS=",
		"PARSE_MODE=",
		"AI_RETRY_ATTEMPTS=",
		"AI_RETRY_DELAY_MS=",
		"AI_REQUEST_TIMEOUT_MS=",
		"AI_ALLOW_UNSAFE_TARGETS=",
		"AI_DISABLE_STRUCTURED_OUTPUT=",
		"TAG_CACHE_TTL_MS=",
		"TREE_CACHE_TTL_MS=",
		"GZIP_ENABLED=",
		"GZIP_MIN_LENGTH=",
		"DB_MAX_CONNS=",
		"DB_MIN_CONNS=",
		// Wave 12.5 安全加固新增的 env：
		"APP_ENV=",
		"LOG_LEVEL=",
		"ADMIN_AUTH_TOKEN=",
		"METRICS_AUTH_TOKEN=",
		"PPROF_ENABLED=",
		"RATE_LIMIT_RPS=",
		"RATE_LIMIT_BURST=",
		"OTEL_EXPORTER_OTLP_ENDPOINT=",
		// Wave 2 H4 OTel 真实接入新增：
		"OTEL_SAMPLING_RATIO=",
		"OTEL_EXPORTER_OTLP_INSECURE=",
		// Wave 2 DB 加固 + 可观测性新增的 env：
		"DB_MAX_CONN_LIFETIME_MS=",
		"DB_MAX_CONN_IDLE_TIME_MS=",
		"DB_HEALTH_CHECK_PERIOD_MS=",
		"LOG_FORMAT=",
		// Wave 5 PROD HIGH P2 新增：优雅停机预算。
		"SHUTDOWN_TIMEOUT_MS=",
		// Wave 9 MED M5 新增：游标 HMAC 签名密钥。
		"CURSOR_SIGNING_KEY=",
		"READER_CURSOR_SIGNING_KEY=",
		// 浏览器扩展 Task 1B 新增：公开 API 的 opt-in Bearer token。
		"EXTENSION_API_TOKEN=",
		// RF6A：translation River job v1→v2 三阶段滚动协议。
		"TRANSLATION_JOBS_ROLLOUT=",
		// RF5A：translation source identity 兼容/严格发布阶段。
		"TRANSLATION_SOURCE_ROLLOUT=",
		// RF6B：translation terminal reconciler cadence and scan bounds.
		"TRANSLATION_RECONCILE_INTERVAL_MS=",
		"TRANSLATION_RECONCILE_BATCH=",
		"TRANSLATION_RECONCILE_ROUND_TIMEOUT_MS=",
		"TRANSLATION_RECONCILE_MISSING_AFTER_MS=",
		// RF6C：parse missing recovery and bounded River terminal retention.
		"PARSE_RECONCILE_INTERVAL_MS=",
		"PARSE_RECONCILE_BATCH=",
		"PARSE_RECONCILE_ROUND_TIMEOUT_MS=",
		"PARSE_RECONCILE_MISSING_AFTER_MS=",
		"RIVER_TERMINAL_RETENTION_MS=",
		"RIVER_MAX_RECOVERY_DOWNTIME_MS=",
		// Phase 5 v3.0 embedding client. Retired concept-normalization settings
		// are intentionally absent so a fresh deployment cannot enable no-op
		// behavior by following the example.
		"EMBEDDING_MODEL=",
		"EMBEDDING_BASE_URL=",
		"EMBEDDING_API_KEY=",
		"EMBEDDING_DIMENSIONS=",
		"SITE_MIGRATION_ENABLED=",
		"SITE_MIGRATION_DRY_RUN=",
		"SITE_MIGRATION_BATCH=",
		"SITE_MIGRATION_INTERVAL_MS=",
	}

	for _, needle := range required {
		if !strings.Contains(content, needle) {
			t.Fatalf(".env.example missing %q", needle)
		}
	}

	for _, want := range []string{
		"AI_BASE_URL=" + placeholderAIBaseURL,
		"AI_API_KEY=\n",
		"AI_MODEL=grok-4.3-fast",
		"\nPARSE_MODE=grok_direct\n",
		"\nTRANSLATION_SOURCE_ROLLOUT=strict\n",
		"\nTRANSLATION_RECONCILE_INTERVAL_MS=60000\n",
		"\nTRANSLATION_RECONCILE_BATCH=100\n",
		"\nTRANSLATION_RECONCILE_ROUND_TIMEOUT_MS=30000\n",
		"\nTRANSLATION_RECONCILE_MISSING_AFTER_MS=21600000\n",
		"\nPARSE_RECONCILE_INTERVAL_MS=60000\n",
		"\nPARSE_RECONCILE_BATCH=100\n",
		"\nPARSE_RECONCILE_ROUND_TIMEOUT_MS=30000\n",
		"\nPARSE_RECONCILE_MISSING_AFTER_MS=21600000\n",
		"\nRIVER_TERMINAL_RETENTION_MS=604800000\n",
		"\nRIVER_MAX_RECOVERY_DOWNTIME_MS=86400000\n",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf(".env.example must document the Grok production defaults; missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(content), "mimo-v2.5-pro") {
		t.Fatal(".env.example must not reference the retired MiMo model")
	}
	// .env.example 进版本控制，仓库是公开的。一个可达的推理网关地址，加上
	// 「明文 HTTP、8000 端口」这两条同文档记着的信息，本身就是攻击面。
	// 这条挡住真实地址被顺手写回来。
	if matched := realGatewayPattern.FindString(content); matched != "" {
		t.Fatalf(".env.example 里出现了疑似真实网关地址 %q；示例文件只能放占位符，真实值放部署环境的 secret", matched)
	}
	assertNoBareGatewayIP(t)

	deprecated := []string{
		"WIKIDATA_ENABLED=",
		"CONCEPT_JUDGE_ENABLED=",
		"CONCEPT_RETRIEVE_TOP_K=",
	}
	for _, needle := range deprecated {
		if strings.Contains(content, needle) {
			t.Fatalf(".env.example must not advertise ignored setting %q", needle)
		}
	}
}

// placeholderAIBaseURL 是 .env.example 里应当出现的占位地址。
const placeholderAIBaseURL = "http://your-grok-gateway.internal:8000/v1"

// realGatewayPattern 匹配裸 IP 形式的 base URL —— 示例文件里不该出现这种东西。
var realGatewayPattern = regexp.MustCompile(`AI_BASE_URL=https?://\d{1,3}(\.\d{1,3}){3}`)

// assertNoBareGatewayIP 防止示例配置重新出现可达的部署端点。
func assertNoBareGatewayIP(t *testing.T) {
	t.Helper()

	for _, rel := range []string{"../../.env.example"} {
		data, err := os.ReadFile(rel)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", rel, err)
		}
		for _, m := range bareIPEndpointPattern.FindAllStringSubmatch(string(data), -1) {
			ip := m[1]
			if ip == "" {
				ip = m[2]
			}
			if ip == "" || privateOrDocIP.MatchString(ip) || !validOctets(ip) {
				continue
			}
			t.Errorf("%s 出现疑似真实主机地址 %q。可达的推理网关 / 部署主机地址本身就是攻击面，"+
				"加上同文档记着的「明文 HTTP、8000 端口」尤其如此——真实值放部署 secret", rel, m[0])
		}
	}
}

// bareIPEndpointPattern 匹配裸 IPv4 的 http(s) 端点与 ssh 目标（root@1.2.3.4）。
// 刻意不匹配文档里合法引用的私网示例（127.0.0.1 / localhost / 10.x / 192.168.x），
// 那些是说明用的，不是会泄漏的真实地址。
// 三种形态：带 scheme 的端点、ssh 的 user@host、以及裸写的 host:port
// （`PGHOST=13.107.42.14:5432`）。只认前两种时，写成第三种就能绕过去。
//
// **刻意不认**「四段数字后面什么都不跟」这种最宽的形态：文档里到处是版本号
// （`v3.35.5.35.5`）、耗时、覆盖率，那样会天天误报——而一个会对无关文本报警的
// 门禁，最后只会被人无视，等于没有。已知代价：`server 1.2.3.4;` 这类无端口的
// 裸地址扫不出来。宁可漏一种写法，也不要一个没人看的红。
var bareIPEndpointPattern = regexp.MustCompile(
	`(?i)(?:https?://|@)((?:\d{1,3}\.){3}\d{1,3})|\b((?:\d{1,3}\.){3}\d{1,3}):\d{2,5}\b`)

// privateOrDocIP 是允许出现在示例配置里的地址：回环、私网、链路本地、CGNAT，
// 外加 RFC5737 的示例段（192.0.2.x / 198.51.100.x / 203.0.113.x）。
//
// 名字里带 "Doc" 却不含文档段是上一版的问题：那会让 RFC 标准示例地址反而被
// 门禁拦下，逼着写文档的人去用真实地址。
var privateOrDocIP = regexp.MustCompile(
	`^(?:127\.|10\.|192\.168\.|172\.(?:1[6-9]|2\d|3[01])\.|169\.254\.|` +
		`100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])\.|0\.0\.0\.0|` +
		`192\.0\.2\.|198\.51\.100\.|203\.0\.113\.)`)

// validOctets 过滤掉每段大于 255 的四段数字——那不是 IP，多半是版本号或耗时。
func validOctets(ip string) bool {
	for _, part := range strings.Split(ip, ".") {
		n, err := strconv.Atoi(part)
		if err != nil || n > 255 {
			return false
		}
	}
	return true
}

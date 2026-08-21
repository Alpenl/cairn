package config

import (
	"fmt"
	"net"
	"os"
	"strings"
)

func loadServerConfig() (ServerConfig, error) {
	trustedProxyCIDRs, err := loadTrustedProxyCIDRs()
	if err != nil {
		return ServerConfig{}, err
	}
	readHeaderTimeoutMS, err := envInt("READ_HEADER_TIMEOUT_MS", 2000)
	if err != nil {
		return ServerConfig{}, err
	}
	readTimeoutMS, err := envInt("READ_TIMEOUT_MS", 10000)
	if err != nil {
		return ServerConfig{}, err
	}
	writeTimeoutMS, err := envInt("WRITE_TIMEOUT_MS", 15000)
	if err != nil {
		return ServerConfig{}, err
	}
	idleTimeoutMS, err := envInt("IDLE_TIMEOUT_MS", 60000)
	if err != nil {
		return ServerConfig{}, err
	}
	shutdownTimeoutMS, err := envInt("SHUTDOWN_TIMEOUT_MS", 30000)
	if err != nil {
		return ServerConfig{}, err
	}
	return ServerConfig{
		ListenAddr:          envString("LISTEN_ADDR", ":8000"),
		CORSOrigins:         envList("CORS_ORIGINS", []string{"http://localhost:3000"}),
		TrustedProxyCIDRs:   trustedProxyCIDRs,
		ReadHeaderTimeoutMS: readHeaderTimeoutMS,
		ReadTimeoutMS:       readTimeoutMS,
		WriteTimeoutMS:      writeTimeoutMS,
		IdleTimeoutMS:       idleTimeoutMS,
		ShutdownTimeoutMS:   shutdownTimeoutMS,
	}, nil
}

func loadTrustedProxyCIDRs() ([]string, error) {
	raw := strings.TrimSpace(os.Getenv("TRUSTED_PROXY_CIDRS"))
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS must not contain blank entries")
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS must contain valid CIDRs: %q", part)
		}
		ones, bits := network.Mask.Size()
		if ones == 0 && (bits == 32 || bits == 128) {
			return nil, fmt.Errorf("TRUSTED_PROXY_CIDRS must not trust all addresses")
		}
		canonical := network.String()
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		out = append(out, canonical)
	}
	return out, nil
}

func loadDBConfig() (DBConfig, error) {
	maxConns, err := envInt("DB_MAX_CONNS", 30)
	if err != nil {
		return DBConfig{}, err
	}
	minConns, err := envInt("DB_MIN_CONNS", 5)
	if err != nil {
		return DBConfig{}, err
	}
	parseConcurrency, err := envInt("PARSE_CONCURRENCY", 3)
	if err != nil {
		return DBConfig{}, err
	}
	maxConnLifetimeMS, err := envInt("DB_MAX_CONN_LIFETIME_MS", 30*60*1000)
	if err != nil {
		return DBConfig{}, err
	}
	maxConnIdleTimeMS, err := envInt("DB_MAX_CONN_IDLE_TIME_MS", 10*60*1000)
	if err != nil {
		return DBConfig{}, err
	}
	healthCheckPeriodMS, err := envInt("DB_HEALTH_CHECK_PERIOD_MS", 60*1000)
	if err != nil {
		return DBConfig{}, err
	}
	return DBConfig{
		MaxConns:            maxConns,
		MinConns:            minConns,
		ParseConcurrency:    parseConcurrency,
		MaxConnLifetimeMS:   maxConnLifetimeMS,
		MaxConnIdleTimeMS:   maxConnIdleTimeMS,
		HealthCheckPeriodMS: healthCheckPeriodMS,
	}, nil
}

func loadFetcherConfig() (FetcherConfig, error) {
	retryAttempts, err := envInt("FETCH_RETRY_ATTEMPTS", 2)
	if err != nil {
		return FetcherConfig{}, err
	}
	retryDelayMS, err := envInt("FETCH_RETRY_DELAY_MS", 25)
	if err != nil {
		return FetcherConfig{}, err
	}
	// PARSE_MODE 默认 grok_direct：普通网页优先本地完整抓取，X/Twitter
	// 优先由联网 Grok 自抓 URL，本地抓取失败时也用 URL 直连兜底。
	// 显式设为 fetcher 会关闭 URL 直连分析。
	parseMode := strings.ToLower(strings.TrimSpace(envString("PARSE_MODE", "grok_direct")))
	var urlDirect bool
	switch parseMode {
	case "fetcher":
		urlDirect = false
	case "grok_direct":
		urlDirect = true
	default:
		return FetcherConfig{}, fmt.Errorf("PARSE_MODE must be %q or %q: got %q", "fetcher", "grok_direct", parseMode)
	}
	return FetcherConfig{
		RetryAttempts: retryAttempts,
		RetryDelayMS:  retryDelayMS,
		URLDirect:     urlDirect,
	}, nil
}

func loadAnalyzerConfig() (AnalyzerConfig, error) {
	retryAttempts, err := envInt("AI_RETRY_ATTEMPTS", 3)
	if err != nil {
		return AnalyzerConfig{}, err
	}
	retryDelayMS, err := envInt("AI_RETRY_DELAY_MS", 25)
	if err != nil {
		return AnalyzerConfig{}, err
	}
	requestTimeoutMS, err := envInt("AI_REQUEST_TIMEOUT_MS", 60000)
	if err != nil {
		return AnalyzerConfig{}, err
	}
	return AnalyzerConfig{
		BaseURL:          strings.TrimSpace(os.Getenv("AI_BASE_URL")),
		APIKey:           strings.TrimSpace(os.Getenv("AI_API_KEY")),
		Model:            strings.TrimSpace(os.Getenv("AI_MODEL")),
		RetryAttempts:    retryAttempts,
		RetryDelayMS:     retryDelayMS,
		RequestTimeoutMS: requestTimeoutMS,
	}, nil
}

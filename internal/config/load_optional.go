package config

// scalarKnobs 把几个互不相关、各自只读一个 env 的标量配置项收成一束，让
// loadRuntimeConfig 少几条独立的 err 分支（降圈复杂度），不改变各项语义。
type scalarKnobs struct {
	tagCacheTTLMS  int
	treeCacheTTLMS int
	ytdlpTimeoutMS int
	pprofEnabled   bool
	gzipEnabled    bool
	gzipMinLength  int
}

// loadScalarKnobs 读取 TAG_CACHE_TTL_MS / TREE_CACHE_TTL_MS / YTDLP_TIMEOUT_MS / PPROF_ENABLED /
// GZIP_ENABLED / GZIP_MIN_LENGTH。
func loadScalarKnobs() (scalarKnobs, error) {
	tagCacheTTLMS, err := envInt("TAG_CACHE_TTL_MS", 300000)
	if err != nil {
		return scalarKnobs{}, err
	}
	treeCacheTTLMS, err := envInt("TREE_CACHE_TTL_MS", 300000)
	if err != nil {
		return scalarKnobs{}, err
	}
	ytdlpTimeoutMS, err := envInt("YTDLP_TIMEOUT_MS", 30000)
	if err != nil {
		return scalarKnobs{}, err
	}
	pprofEnabled, err := envBool("PPROF_ENABLED", false)
	if err != nil {
		return scalarKnobs{}, err
	}
	gzipEnabled, err := envBool("GZIP_ENABLED", true)
	if err != nil {
		return scalarKnobs{}, err
	}
	gzipMinLength, err := envInt("GZIP_MIN_LENGTH", 1024)
	if err != nil {
		return scalarKnobs{}, err
	}
	return scalarKnobs{
		tagCacheTTLMS:  tagCacheTTLMS,
		treeCacheTTLMS: treeCacheTTLMS,
		ytdlpTimeoutMS: ytdlpTimeoutMS,
		pprofEnabled:   pprofEnabled,
		gzipEnabled:    gzipEnabled,
		gzipMinLength:  gzipMinLength,
	}, nil
}

func loadIdempotencyConfig() (idempotencyEnv, error) {
	enabled, err := envBool("IDEMPOTENCY_ENABLED", true)
	if err != nil {
		return idempotencyEnv{}, err
	}
	ttlMS, err := envInt("IDEMPOTENCY_TTL_MS", 0)
	if err != nil {
		return idempotencyEnv{}, err
	}
	return idempotencyEnv{enabled: enabled, ttlMS: ttlMS}, nil
}

func loadOTelConfig() (otelEnv, error) {
	ratio, err := envFloat("OTEL_SAMPLING_RATIO", 0.05)
	if err != nil {
		return otelEnv{}, err
	}
	insecure, err := envBool("OTEL_EXPORTER_OTLP_INSECURE", true)
	if err != nil {
		return otelEnv{}, err
	}
	return otelEnv{samplingRatio: ratio, insecure: insecure}, nil
}

func loadRateLimitConfig() (RateLimitConfig, error) {
	rps, err := envFloat("RATE_LIMIT_RPS", 0)
	if err != nil {
		return RateLimitConfig{}, err
	}
	burst, err := envInt("RATE_LIMIT_BURST", 0)
	if err != nil {
		return RateLimitConfig{}, err
	}
	return RateLimitConfig{
		RPS:   rps,
		Burst: burst,
	}, nil
}

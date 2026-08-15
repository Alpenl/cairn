package config

import (
	"os"
	"strings"
)

// deprecatedEnvVars enumerates the env var names v3 (Spec 检索式打标)
// retired: the Wikidata anchoring path and the pg_trgm + LLM concept
// judge poller. They are still tolerated (a stale .env does not fail
// the boot) but ignored. detectDeprecatedEnvs reports which of them an
// operator has left set so app.warnUnsafeBootDefaults can nudge a
// cleanup.
var deprecatedEnvVars = []string{
	"WIKIDATA_ENABLED",
	"WIKIDATA_USER_AGENT",
	"WIKIDATA_RATE_LIMIT_PER_HOUR",
	"WIKIDATA_BASE_URL",
	"CONCEPT_JUDGE_ENABLED",
	"CONCEPT_JUDGE_INTERVAL_MS",
	"CONCEPT_JUDGE_BATCH_SIZE",
	"CONCEPT_JUDGE_SIMILARITY_THRESHOLD",
	"CONCEPT_RETRIEVE_TOP_K",
	"CONCEPT_AUTO_MERGE_THRESHOLD",
	"CONCEPT_NEW_THRESHOLD",
	"CONCEPT_COLD_START_MIN",
}

// detectDeprecatedEnvs returns the subset of deprecatedEnvVars present
// with a non-empty (after trim) value in the environment, preserving
// declaration order so the boot WARNs are stable. Empty / unset vars
// are silent — only a value an operator deliberately carried over earns
// a nudge.
func detectDeprecatedEnvs() []string {
	var out []string
	for _, name := range deprecatedEnvVars {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			out = append(out, name)
		}
	}
	return out
}

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

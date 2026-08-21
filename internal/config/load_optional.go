package config

func loadIdempotencyTTLMS() (int, error) {
	return envInt("IDEMPOTENCY_TTL_MS", 0)
}

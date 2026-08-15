package concept

func (r *Resolver) logf(level, msg string, args ...any) {
	if r.logger == nil {
		return
	}
	switch level {
	case "warn":
		r.logger.Warn(msg, args...)
	case "info":
		r.logger.Info(msg, args...)
	default:
		r.logger.Debug(msg, args...)
	}
}

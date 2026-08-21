package app

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/buildinfo"
)

func registerOperationalRoutes(router *gin.Engine, readiness ReadinessChecker, logger *slog.Logger) {
	registerReaderRoutes(router)
	router.GET("/health", func(c *gin.Context) {
		body := gin.H{
			"status":     "ok",
			"version":    buildinfo.VersionValue(),
			"commit":     buildinfo.CommitValue(),
			"build_time": buildinfo.BuildTimeValue(),
		}
		c.JSON(http.StatusOK, body)
	})
	router.GET("/ready", func(c *gin.Context) {
		if readiness != nil {
			if err := readiness.Ready(c.Request.Context()); err != nil {
				if logger != nil {
					logger.Warn("readiness check failed", "error", err)
				}
				body := gin.H{"status": "degraded", "ready": false}
				if failed := FailedNamesFromReadinessError(err); len(failed) > 0 {
					body["failed"] = failed
				}
				c.JSON(http.StatusServiceUnavailable, body)
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "ready": true})
	})
}

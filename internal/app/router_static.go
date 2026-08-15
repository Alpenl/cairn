package app

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"webtag/internal/buildinfo"
)

func registerStaticAndHealthRoutes(router *gin.Engine, readiness ReadinessChecker, logger *slog.Logger, metricsHandler http.Handler) {
	router.StaticFS("/static", StaticFS())
	registerReaderRoutes(router)
	router.GET("/docs", serveDocs)
	router.GET("/openapi.json", serveOpenAPI)
	router.GET("/admin/concept-merges", serveAdminConceptMerges)
	router.GET("/health", func(c *gin.Context) {
		body := gin.H{
			"status":     "ok",
			"version":    buildinfo.VersionValue(),
			"commit":     buildinfo.CommitValue(),
			"build_time": buildinfo.BuildTimeValue(),
		}
		if extra, ok := c.Get("health_extra_fields"); ok {
			if asMap, ok := extra.(map[string]any); ok {
				for k, v := range asMap {
					body[k] = v
				}
			}
		}
		c.JSON(http.StatusOK, body)
	})
	router.GET("/ready", func(c *gin.Context) {
		if readiness != nil {
			if err := readiness.Ready(c.Request.Context()); err != nil {
				if logger != nil {
					logger.Warn("readiness check failed", "error", err)
				}
				body := gin.H{
					"status": "degraded",
					"ready":  false,
				}
				// 聚合探针失败时，把每个未通过的 check Name 暴露给客户端。
				// k8s readiness probe 不读 body，但运维 `curl /ready` 排查
				// 时这是最快定位"是 DB 慢还是 queue 还没 seed 完"的方法。
				// 单一探针（仅 DB Ping）的旧路径走 nil，response 保持以往
				// 形状，避免破坏既有监控告警。
				if failed := FailedNamesFromReadinessError(err); len(failed) > 0 {
					body["failed"] = failed
				}
				c.JSON(http.StatusServiceUnavailable, body)
				return
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"status": "ok",
			"ready":  true,
		})
	})
	if metricsHandler != nil {
		router.GET("/metrics", gin.WrapH(metricsHandler))
	}
}

func serveDocs(c *gin.Context) {
	data, err := ScalarHTML()
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load API docs")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

func serveOpenAPI(c *gin.Context) {
	data, err := OpenAPISpec()
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load OpenAPI spec")
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", data)
}

func serveAdminConceptMerges(c *gin.Context) {
	data, err := AdminConceptMergesHTML()
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load admin page")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

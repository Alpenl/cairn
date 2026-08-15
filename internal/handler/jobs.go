package handler

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// getJob 处理 GET /api/jobs/:job_id：返回解析任务的当前状态以及（已
// 完成时）关联的 link 详情。
func getJob(service JobService) gin.HandlerFunc {
	return func(c *gin.Context) {
		resp, err := service.Get(c.Request.Context(), c.Param("job_id"))
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

// listJobs 处理 GET /api/jobs?ids=a,b,c：批量返回多个解析任务的当前状态，
// 让前端轮询时把一批活跃任务合并成一次请求。
func listJobs(service JobService) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.Query("ids"))
		ids := splitCSV(raw)
		resp, err := service.List(c.Request.Context(), ids)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, resp)
	}
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func groupedSearch(service LibrarySearchService) gin.HandlerFunc {
	return func(c *gin.Context) {
		out, err := service.Search(
			c.Request.Context(),
			c.Query("q"),
			queryInt(c, "reading_limit", 10),
			queryInt(c, "site_limit", 10),
			queryInt(c, "thought_limit", 20),
			c.Query("thought_after"),
		)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

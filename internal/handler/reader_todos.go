package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"webtag/internal/dto"
	"webtag/internal/httperr"
)

func readerCreateTodo(reader ReaderTodoRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderTodoCreateRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.CreateTodo(c.Request.Context(), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusCreated, response)
	}
}

func readerListTodos(reader ReaderTodoRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := 200
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 200 {
				writeError(c, httperr.NewWithCode(http.StatusUnprocessableEntity, "invalid_todo_limit", "limit must be between 1 and 200"))
				return
			}
			limit = parsed
		}
		response, err := reader.ListTodos(c.Request.Context(), c.Query("after"), limit)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerPatchTodo(reader ReaderTodoRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request dto.ReaderTodoPatchRequest
		if !bindJSONWithLimit(c, &request, defaultMaxJSONBodyBytes) {
			return
		}
		response, err := reader.PatchTodo(c.Request.Context(), c.Param("id"), request)
		if err != nil {
			writeError(c, err)
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

func readerDeleteTodo(reader ReaderTodoRoutes) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := reader.DeleteTodo(c.Request.Context(), c.Param("id")); err != nil {
			writeError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	}
}

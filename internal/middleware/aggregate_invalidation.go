package middleware

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AggregateInvalidator 抽象「让当前安装的聚合缓存失效」的能力。
// 生产由 service.MultiCacheInvalidator（标签 + 域名两份聚合）实现。
type AggregateInvalidator interface {
	Invalidate(context.Context)
}

const aggregateInvalidationHandledKey = "webtag.aggregate-invalidation.handled"

// MarkAggregateInvalidationHandled tells the generic write middleware that a
// command has already made its own change-aware aggregate invalidation
// decision. Call it only after a successful command response is available.
func MarkAggregateInvalidationHandled(c *gin.Context) {
	c.Set(aggregateInvalidationHandledKey, true)
}

func aggregateInvalidationHandled(c *gin.Context) bool {
	handled, ok := c.Get(aggregateInvalidationHandledKey)
	return ok && handled == true
}

// InvalidateAggregatesOnWrite 在任何**成功的写请求**之后失效安装级聚合缓存。
// 除非命令已显式负责其变更感知的失效决定。
//
// # 为什么放在中间件而不是逐个 service 注入
//
// 标签聚合（`/api/tags`，含各 library_kind 切面）与域名聚合
// （`/api/tree?view=domains`）会被大量互不相邻的写路径改变：链接的增删改、
// 解析完成、阅读↔网站转换、站点资料与标签编辑、站点合并 / 拆分 / 删除、
// 审核队列的 move_to_site、历史迁移批处理……逐个 service 注入失效器意味着
// **每加一个新写端点都要记得再接一次**，而漏接的症状是「操作成功了但侧栏
// 数字要过几分钟才变」——一个没人会当场发现、也没有测试会红的退化。
//
// 这正是本仓已经踩过三次的同一个坑（中间件测试手搭 gin.New、service 测试
// 手工装配缓存、组件测试手工构造稳定 prop），所以这里选一个**结构上无法遗漏**
// 的位置：公开 API 鉴权组的出口。任何经由 HTTP 写入的路径，无论今天还是以后
// 新增的，都必然经过这里。
//
// 后台路径（解析管线、历史迁移 worker）不走 HTTP，仍需各自显式失效；
// 它们数量有限且集中，见 service.collectionFinalizer.invalidateAggregates。
//
// # 过度失效是刻意的默认取舍
//
// 本中间件通常不区分「这个写请求到底动没动聚合」——改一条站点备注同样会失效。
// 代价是下一次读多跑一次聚合查询（有 singleflight 合并，不会打穿）；
// 收益是判断逻辑不会随端点演化而腐化。上一版在解析路径上用
// `!sameTagSet(...)` 做过这种精细判断，结果漏掉了 status 维度，
// 造成「重新解析后链接从侧栏计数里消失 5 分钟」。宁可多失效。唯一例外是
// metadata CAS：它的 service 已从原子更新得到 TagsChanged，所以由命令自己作出
// 精确决定，避免成功 no-op 白白推进聚合 generation。
//
// # 顺序约束
//
// 挂在鉴权守卫之后，确保未通过身份验证的写请求不会触发无意义的失效。
func InvalidateAggregatesOnWrite(invalidator AggregateInvalidator) gin.HandlerFunc {
	if invalidator == nil {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		c.Next()

		if !isMutatingMethod(c.Request.Method) {
			return
		}
		// 只在写真的成功时失效。4xx/5xx 什么都没改，失效只会白白让下一次读
		// 回源；而且失败请求往往是被限流 / 鉴权挡住的高频重试。
		if status := c.Writer.Status(); status < http.StatusOK || status >= http.StatusMultipleChoices {
			return
		}
		if aggregateInvalidationHandled(c) {
			return
		}
		// A handler may replace c.Request with a derived request, so read the
		// current context after c.Next() rather than capturing it on entry.
		invalidator.Invalidate(c.Request.Context())
	}
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

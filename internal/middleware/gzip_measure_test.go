package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// listItem 复刻 dto.LinkResponse 中列表投影实际会带的字段（含 summary /
// description 这两个长文本字段），用来产出
// 形状真实的列表响应，而不是拿一段人造字符串糊弄压缩比。
type listItem struct {
	ID              string    `json:"id"`
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	Summary         string    `json:"summary"`
	Description     string    `json:"description"`
	Tags            []string  `json:"tags"`
	ContentType     string    `json:"content_type"`
	LibraryKind     string    `json:"library_kind"`
	Status          string    `json:"status"`
	Domain          string    `json:"domain"`
	PathDepth       int       `json:"path_depth"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	FetcherType     string    `json:"fetcher_type"`
	IsLowConfidence bool      `json:"is_low_confidence"`
	HasContent      bool      `json:"has_content"`
	ContentCJKChars int       `json:"content_cjk_chars"`
	ContentWords    int       `json:"content_words"`
}

// 30 条各不相同的条目素材。**逐条不同是这份 fixture 的关键**：上一版 30
// 条内容完全一致，gzip 直接把 29 份重复引用掉，测出 3.4% 的压缩比——那个
// 数字反映的是 fixture 的懒惰，不是真实收益。真实链接库里每条摘要都不一样，
// 跨条目的冗余只剩字段名与 JSON 结构本身。
var measureTitles = []string{
	"分布式系统笔记：一致性模型与共识协议的工程取舍",
	"Rust 所有权模型在嵌入式固件中的实践",
	"为什么我们把单体拆了又合回去",
	"PostgreSQL 查询计划器的十个反直觉行为",
	"从零实现一个 CRDT 文本编辑器",
	"TypeScript 类型体操的收益边界在哪里",
	"eBPF 在生产环境可观测性中的落地路径",
	"重新审视 CAP 定理的常见误读",
	"Kubernetes 调度器扩展点实战",
	"编译器优化如何改变你的基准测试结论",
	"WebAssembly 组件模型初探",
	"如何设计一个不会被滥用的限流器",
	"数据库索引选择性与统计信息的关系",
	"前端构建工具二十年演进史",
	"函数式错误处理在 Go 里的可行形态",
	"深入 TCP 拥塞控制算法的演化",
	"向量数据库选型的六个维度",
	"为什么你的缓存命中率在说谎",
	"Linux 内存回收机制与 OOM 判定",
	"大模型推理服务的批处理调度",
	"从 monorepo 迁移到 polyrepo 的代价",
	"分布式追踪采样策略的取舍",
	"设计一个可演进的 API 版本方案",
	"Git 内部对象模型与打包算法",
	"浏览器渲染管线与布局抖动",
	"消息队列的恰好一次语义究竟意味着什么",
	"用类型系统消灭一整类运行时错误",
	"服务网格的性能开销实测",
	"时间序列数据库的压缩算法比较",
	"重构遗留系统的绞杀者模式实践",
}

var measureSummaries = []string{
	"本文梳理线性一致性、顺序一致性与因果一致性在真实系统中的取舍，结合 Raft 与 Paxos 的实现差异说明多数生产系统的选型逻辑。",
	"记录了把 Rust 引入资源受限固件时遇到的借用检查器摩擦，以及最终采用的若干规避模式与它们的运行时代价。",
	"团队两年内经历拆分与回收两轮架构变更，作者复盘了当初拆分决策依据的失效点，以及合并后保留的哪些边界仍然有效。",
	"整理了十个容易让人误判的计划器行为，每个都配了可复现的最小 SQL 与 EXPLAIN 输出，并解释统计信息在其中扮演的角色。",
	"从操作变换与无冲突复制数据类型的差异讲起，逐步实现一个支持并发编辑的文本结构，讨论墓碑膨胀的处理办法。",
	"讨论复杂类型推导在可读性与维护成本上的临界点，给出几条判断何时应该退回运行时校验的经验规则。",
	"介绍在无侵入前提下采集内核态指标的方案，覆盖工具链选择、验证器限制以及生产灰度中踩过的坑。",
	"澄清分区容忍性并非可选项这一常见误解，重新表述该定理在工程决策中真正约束的是什么。",
	"通过实现一个自定义调度插件，说明各扩展点的调用时机与相互影响，附带调度延迟的实测数据。",
	"展示了编译器的循环外提与内联如何让一段看似严谨的微基准得出完全相反的结论，并给出规避方法。",
	"解析组件模型的接口类型系统，对比它与既有模块方案在跨语言互操作上的实质差别。",
	"从令牌桶与漏桶的语义差异出发，讨论分布式场景下的时钟依赖与突发流量放行策略。",
	"解释为什么高基数列未必是好索引，以及规划器如何用直方图与最常见值列表估算行数。",
	"回顾从脚本拼接到模块打包再到原生 ESM 的路径，分析每次范式转移背后的实际约束。",
	"评估显式错误值与哨兵错误在大型代码库中的可维护性，给出一套折中的包装与断言约定。",
	"从早期算法讲到基于时延的现代方案，说明数据中心与广域网场景下的不同权衡。",
	"提出索引类型、召回质量、写入吞吐、内存占用、运维复杂度与生态成熟度六个评估维度。",
	"指出常见的命中率统计口径忽略了穿透与雪崩，给出更能反映真实收益的度量方式。",
	"梳理页面回收路径上的各类水位线，解释在什么条件下会触发终止进程的判定。",
	"讨论动态批处理与连续批处理的实现差异，给出不同请求长度分布下的吞吐对比。",
	"记录一次逆向迁移的完整过程，量化了构建时间、依赖治理与发布协调三方面的变化。",
	"比较头部采样与尾部采样的信息损失，讨论如何在保留异常链路的同时控制存储成本。",
	"从兼容性契约出发，讨论版本号放在路径、头部还是媒体类型各自的长期维护成本。",
	"拆解松散对象与打包文件的存储结构，说明增量编码与窗口大小如何影响仓库体积。",
	"说明样式计算、布局、绘制与合成各阶段的触发条件，以及哪些属性变更可以跳过布局。",
	"区分传输层重复投递与业务层幂等，论证端到端的恰好一次只能由消费者配合实现。",
	"通过若干重构案例展示如何把非法状态变成不可表达，从而删掉对应的运行时分支。",
	"在统一压测环境下测量数据面代理引入的延迟与资源占用，并分析其随连接数的变化。",
	"对比多种面向浮点时序的有损与无损压缩方案，给出压缩比与查询延迟的权衡曲线。",
	"介绍如何用外观层逐步替换遗留模块，重点是切换期间双写与数据校验的组织方式。",
}

var measureDomains = []string{
	"example.com", "engineering.acme.dev", "notes.hallowed.io", "blog.pinecone.tools",
	"lab.verdant.systems", "writings.kestrel.net",
}

func realisticListPayload(t *testing.T) []byte {
	t.Helper()
	stamp := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	items := make([]listItem, 0, 30)
	for i := 0; i < 30; i++ {
		items = append(items, listItem{
			ID:              fmt.Sprintf("9f2c1b84-4e7a-4d33-9a10-3f6b2c5e%04d", i*137%10000),
			URL:             fmt.Sprintf("https://%s/engineering/blog/2026/%02d/%s", measureDomains[i%len(measureDomains)], i%12+1, strings.ToLower(strings.ReplaceAll(measureTitles[i][:12], " ", "-"))),
			Title:           measureTitles[i],
			Summary:         measureSummaries[i],
			Description:     measureSummaries[(i+7)%len(measureSummaries)][:60],
			Tags:            []string{measureTitles[i][:6], "工程实践", measureDomains[i%len(measureDomains)]},
			ContentType:     "article",
			LibraryKind:     "reading",
			Status:          "done",
			Domain:          measureDomains[i%len(measureDomains)],
			PathDepth:       4 + i%3,
			CreatedAt:       stamp.Add(-time.Duration(i) * time.Hour),
			UpdatedAt:       stamp.Add(-time.Duration(i) * time.Minute),
			FetcherType:     []string{"basic", "github", "arxiv", "jina"}[i%4],
			HasContent:      i%3 != 0,
			ContentCJKChars: 1200 + i*311,
			ContentWords:    80 + i*47,
		})
	}
	body, err := json.Marshal(map[string]any{"items": items, "total": 1284, "page": 1, "limit": 30})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// TestGzipMeasuredRatios 产出本 Phase 验收要求的实测压缩比，并把「压缩后
// ≤ 压缩前 30%」这条门槛钉成回归测试。
//
// 用 -v 跑可以直接读到写进 commit message 的数字。
func TestGzipMeasuredRatios(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	openapi, err := os.ReadFile("../app/assets/openapi.json")
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}

	cases := []struct {
		name    string
		payload []byte
	}{
		{name: "GET /api/links?limit=30", payload: realisticListPayload(t)},
		{name: "GET /openapi.json", payload: openapi},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := gin.New()
			router.Use(Gzip(DefaultGzipMinLength))
			router.GET("/probe", func(c *gin.Context) {
				c.Data(http.StatusOK, "application/json", tc.payload)
			})

			compressed := doGzipGet(router, "/probe", "gzip")
			if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
				t.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			if got := gunzip(t, compressed.Body.Bytes()); got != string(tc.payload) {
				t.Fatalf("round-trip mismatch")
			}

			before := len(tc.payload)
			after := compressed.Body.Len()
			ratio := float64(after) / float64(before)
			t.Logf("%s: %d B -> %d B (%.1f%% of original, saved %d B)",
				tc.name, before, after, ratio*100, before-after)

			if ratio > 0.30 {
				t.Fatalf("compressed to %.1f%% of original, PF1 acceptance requires <= 30%%", ratio*100)
			}
		})
	}
}

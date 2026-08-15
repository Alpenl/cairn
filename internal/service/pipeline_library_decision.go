package service

import "webtag/internal/model"

// finalLibraryInput 是「解析完成后，这条链接归到哪个库」的全部输入。
//
// 抽出本类型的意义在于：这四类输入此前分散在三个地方——analyzer 结果、
// collectionFinalizationRequest 的 RequestedKind/RuleMatch、collectionFinalizer
// 自己的 disableSiteAutoClassification 字段——而决策本身又被拆成三段散在
// Finalize、persist、finalClassificationSource 里。要回答「这条链接为什么进了
// 阅读库」，得同时读五处代码。
//
// 与 ClassifyLibrary 的分工：ClassifyLibrary 是**提交时**的前置分类（基于抓取
// 信号打分，可能返回 Ambiguous 交给 analyzer 裁决）；本函数是**解析完成后**的
// 终态分类（analyzer 已给出结论，这里叠加个人规则与灰度开关）。两者阶段不同、
// 输入不同，刻意保持独立。
type finalLibraryInput struct {
	// AnalyzedKind 是 analyzer 给出的归属结论。
	AnalyzedKind model.LibraryKind
	// Confidence / Reason / Explanation 是 analyzer 对该结论的解释，命中个人
	// 规则时会被规则的说法整体替换。
	Confidence  float32
	Reason      string
	Explanation string
	// Requested / RequestedSource are the persisted capture intent. A concrete
	// kind from user is authoritative and locked; the same kind from auto is a
	// system hard rule and remains unlocked.
	Requested       model.RequestedLibraryKind
	RequestedSource model.RequestedLibraryKindSource
	// Rule 是命中的个人分类规则，nil 表示未命中。
	Rule *ClassificationRuleMatch
	// DisableSiteAutoClassification 是灰度开关：为 true 时，自动判定的 site
	// 结果降级为 reading 落库，但保留 site 预测。
	DisableSiteAutoClassification bool

	// CurrentKind / CurrentlyLocked 是这条链接**当前已落库**的归属与锁定标志。
	//
	// 它们必须参与决策，否则用户的显式选择会被下一次解析静默撤销：站点转阅读
	// （site_conversion_execute）会写入 locked=true 并把 status 置为 pending，
	// 从而自己触发重解析；重解析时 analyzer 若仍判 site，旧逻辑会输出
	// kind=site / source=auto / locked=false——用户刚点的「转为阅读」在几秒内
	// 被系统撤销，锁也一并清掉。
	//
	// locked 的语义是「用户已裁决，自动分类不得改写」。此前全仓只有
	// historical_migration 在 WHERE 里检查它，解析路径完全无视，等于这个锁从
	// 未生效过。
	CurrentKind     model.LibraryKind
	CurrentlyLocked bool
}

// finalLibraryDecision 是终态分类的完整结论。持久化层只消费它，不再参与判断。
type finalLibraryDecision struct {
	// Kind 是实际落库的归属。
	Kind model.LibraryKind
	// PredictedKind 是模型/规则原本的判断。灰度降级时它与 Kind 不同——这正是
	// 「收集 site 预测但暂不写入 site 库」的落地方式。
	PredictedKind model.LibraryKind
	// Source 区分用户显式指定与系统自动判定；Locked 为 true 时后续自动分类
	// 不得改写它。
	Source model.LibraryKindSource
	Locked bool

	Confidence  float32
	Reason      string
	Explanation string

	// AutoSiteDowngraded 记录本次决策是否发生了灰度降级。持久化层据此决定是否
	// 用 SiteName/SiteIntro 兜底标题与摘要——降级后的 reading 链接没有经过
	// reading 侧的标题生成，缺省字段需要从 site 画像补。
	AutoSiteDowngraded bool
}

// decideFinalLibrary 由 analyzer 结论、用户意图、个人规则与灰度开关推出终态归属。
//
// 纯函数：无 IO、无副作用、不读时钟。所有分支都能被表驱动测试穷举，
// 见 pipeline_library_decision_test.go。
//
// 优先级自上而下：
//  0. 该链接已被用户锁定 → 保持既有归属，忽略 analyzer 与个人规则；
//  1. 个人规则命中 → 规则的目标库覆盖 analyzer 结论，置信度记为 1；
//  2. 灰度开关关闭 site 自动分类 → 自动判定的 site 降级为 reading，但
//     PredictedKind 仍记 site；
//  3. 用户显式指定且与终态一致 → Source=user 且 Locked。
//
// 步骤 0 优先于其余全部：locked 的语义就是「用户已裁决」。少了它，站点转阅读
// 会被它自己触发的重解析撤销（转换写 locked=true 并置 status=pending）。
// PredictedKind 仍记录模型的判断，让「用户锁定为 A、模型认为是 B」这个分歧
// 保持可见、可用于后续复核。
//
// 注意步骤 1 与 2 的交互：个人规则判 site、但用户提交时未显式指定（Requested
// 为 auto）、且灰度开关开启时，规则结果同样会被降级。这是既有行为——降级开关
// 的语义是「site 库尚未就绪」，此时无论 site 判断来自模型还是规则都不该写入。
func decideFinalLibrary(in finalLibraryInput) finalLibraryDecision {
	// 上游归属未必合法（analyzer 的截断回退路径会产出空值），统一收敛，
	// 保证本函数的输出可直接落库。理由见 normalizeLibraryKind。
	kind := normalizeLibraryKind(in.AnalyzedKind)
	confidence := in.Confidence
	reason := in.Reason
	explanation := in.Explanation

	requested, requestedSource := normalizeCaptureRequestedLibraryIntent(in.Requested, in.RequestedSource)

	requestedKind := normalizeLibraryKind(model.LibraryKind(requested))
	// A matching persisted user intent is the provenance of the existing lock,
	// not a new decision. Preserve the established user_locked diagnostics. A
	// different concrete intent is a later user choice and continues below so it
	// can supersede the old lock.
	if in.CurrentlyLocked && (requestedSource != model.RequestedLibraryKindSourceUser || requestedKind == normalizeLibraryKind(in.CurrentKind)) {
		return lockedLibraryDecision(normalizeLibraryKind(in.CurrentKind), kind)
	}

	if in.Rule != nil {
		kind = normalizeLibraryKind(in.Rule.TargetKind)
		confidence = 1
		reason = in.Rule.Reason
		explanation = "matched an enabled personal classification rule"
	}

	// A newly committed explicit selection supersedes an older lock. This is
	// what makes the last committed user choice win while preserving the
	// analyzer/rule prediction for review.
	if requestedSource == model.RequestedLibraryKindSourceUser {
		return requestedLibraryDecision(requested, kind, model.LibraryKindSourceUser, true, confidence, reason, explanation)
	}

	// A concrete automatic request is a source hard rule (currently RSS ->
	// reading), not a user selection. It fixes the target without creating a
	// lock and without hiding the analyzer prediction.
	if requested != model.RequestedLibraryKindAuto {
		return requestedLibraryDecision(requested, kind, model.LibraryKindSourceAuto, false, confidence, reason, explanation)
	}

	predicted := kind
	downgraded := in.DisableSiteAutoClassification &&
		kind == model.LibraryKindSite
	if downgraded {
		kind = model.LibraryKindReading
	}

	return finalLibraryDecision{
		Kind:               kind,
		PredictedKind:      predicted,
		Source:             model.LibraryKindSourceAuto,
		Locked:             false,
		Confidence:         confidence,
		Reason:             reason,
		Explanation:        explanation,
		AutoSiteDowngraded: downgraded,
	}
}

func requestedLibraryDecision(
	requested model.RequestedLibraryKind,
	predicted model.LibraryKind,
	source model.LibraryKindSource,
	locked bool,
	confidence float32,
	reason, explanation string,
) finalLibraryDecision {
	kind := model.LibraryKind(requested)
	return finalLibraryDecision{
		Kind:          normalizeLibraryKind(kind),
		PredictedKind: predicted,
		Source:        source,
		Locked:        locked,
		Confidence:    confidence,
		Reason:        reason,
		Explanation:   explanation,
	}
}

// lockedLibraryDecision 构造「用户已锁定」时的结论：归属保持 currentKind 不动，
// Source 记 user、Locked 保持 true。
//
// predictedKind 取本轮 **analyzer** 的判断（analyzedKind），而不是 currentKind——
// 注意短路发生在个人规则之前，所以锁定链接上的规则分歧不会进入 predicted，
// 也就不会出现在复核面。这是刻意取舍：锁定意味着用户已裁决，规则不该再对它
// 表态；若将来要让规则分歧也可见，需把短路移到规则之后并同步更新本注释。
// 二者不一致正是「用户锁成了 A，模型认为是 B」这个分歧本身，把它写进
// predicted_library_kind 才能让 library_review 之类的复核面看见它。若这里也填
// currentKind，分歧就被抹平，锁定链接将永远不会被提示复核。
//
// 置信度记 1、理由固定为 user_locked：这条结论不是模型算出来的，沿用模型的
// confidence 会让「用户裁决」和「模型高置信」在指标上混为一谈。
func lockedLibraryDecision(currentKind, analyzedKind model.LibraryKind) finalLibraryDecision {
	return finalLibraryDecision{
		Kind:          currentKind,
		PredictedKind: analyzedKind,
		Source:        model.LibraryKindSourceUser,
		Locked:        true,
		Confidence:    1,
		Reason:        libraryReasonUserLocked,
		Explanation:   "user locked this collection target; automatic classification left it unchanged",
	}
}

// libraryReasonUserLocked 是锁定链接的固定分类理由。写成常量而非字面量，
// 供指标与测试共同引用。
const libraryReasonUserLocked = "user_locked"

// normalizeLibraryKind 把任何非法归属收敛为 reading。
//
// 上游并不保证归属合法：analyzer 的截断摘要回退路径（analyzer/response.go 的
// truncatedSummaryFallback）走不带 wrapper 的校验，产出的 LibraryKind 为空字符串。
// 决策函数的契约是「输出一个可直接落库的归属」，所以在这里收口，而不是把非法值
// 传下去——CompleteReadingParse 要求 Kind 必须是 reading（parse_state_repository.go
// 的前置校验），DB 侧还有 chk_links_library_kind 约束，空值会让一次本可成功的解析
// 变成失败。
//
// 收敛到 reading 与历史行为一致：收口重构之前，reading 分支把 kind 硬编码为
// reading，空归属因此被静默收进阅读库。这里保持同样的结果，只是把它变成显式规则。
func normalizeLibraryKind(kind model.LibraryKind) model.LibraryKind {
	if kind == model.LibraryKindSite {
		return model.LibraryKindSite
	}
	return model.LibraryKindReading
}

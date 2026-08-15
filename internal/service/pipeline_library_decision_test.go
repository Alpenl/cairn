package service

import (
	"testing"

	"webtag/internal/model"
)

// TestDecideFinalLibraryMatrix 穷举终态归属决策的全部输入组合。
//
// 决策此前散在五处（analyzer 输出、requestedKindForAnalysis、Finalize 的规则
// 覆写、persist 的灰度降级、finalClassificationSource），任何一条「这条链接为
// 什么进了阅读库」的问题都要跨文件追。收口成纯函数后，全部行为可以在一张表里
// 读完——这张表就是分类语义的规格说明。
func TestDecideFinalLibraryMatrix(t *testing.T) {
	t.Parallel()

	siteRule := &ClassificationRuleMatch{TargetKind: model.LibraryKindSite, Reason: "personal_rule_host"}
	readingRule := &ClassificationRuleMatch{TargetKind: model.LibraryKindReading, Reason: "personal_rule_path"}

	for _, tc := range []struct {
		name  string
		in    finalLibraryInput
		want  finalLibraryDecision
		notes string
	}{
		{
			name: "自动判定 reading",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading,
				Confidence:   0.8, Reason: "ai_reading", Explanation: "长文",
				Requested: model.RequestedLibraryKindAuto,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 0.8, Reason: "ai_reading", Explanation: "长文",
			},
		},
		{
			name: "自动判定 site",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindSite,
				Confidence:   0.9, Reason: "ai_site", Explanation: "工具站",
				Requested: model.RequestedLibraryKindAuto,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 0.9, Reason: "ai_site", Explanation: "工具站",
			},
		},
		{
			name: "用户显式指定 reading 且模型一致 → 锁定",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading, Confidence: 0.7, Reason: "ai_reading",
				Requested: model.RequestedLibraryKindReading, RequestedSource: model.RequestedLibraryKindSourceUser,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 0.7, Reason: "ai_reading",
			},
		},
		{
			name: "用户显式指定 site 且模型一致 → 锁定",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindSite, Confidence: 0.7, Reason: "ai_site",
				Requested: model.RequestedLibraryKindSite, RequestedSource: model.RequestedLibraryKindSourceUser,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 0.7, Reason: "ai_site",
			},
		},
		{
			name:  "用户要 site 但模型判 reading → 用户选择控制终态并锁定",
			notes: "请求意图与模型预测是独立事实，显式选择直接控制最终分区",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading, Confidence: 0.6, Reason: "ai_reading",
				Requested: model.RequestedLibraryKindSite, RequestedSource: model.RequestedLibraryKindSourceUser,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 0.6, Reason: "ai_reading",
			},
		},
		{
			name:  "个人规则覆盖模型结论",
			notes: "规则命中后置信度记为 1，理由与解释整体换成规则的说法",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading, Confidence: 0.55, Reason: "ai_reading", Explanation: "看着像文章",
				Requested: model.RequestedLibraryKindAuto,
				Rule:      siteRule,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 1, Reason: "personal_rule_host",
				Explanation: "matched an enabled personal classification rule",
			},
		},
		{
			name: "个人规则判 reading",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindSite, Confidence: 0.95, Reason: "ai_site",
				Requested: model.RequestedLibraryKindAuto,
				Rule:      readingRule,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 1, Reason: "personal_rule_path",
				Explanation: "matched an enabled personal classification rule",
			},
		},
		{
			name:  "灰度关闭 site 自动分类 → 降级为 reading 但保留 site 预测",
			notes: "site 库未就绪时先收集预测，Kind 与 PredictedKind 因此分叉",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindSite, Confidence: 0.88, Reason: "ai_site", Explanation: "工具站",
				Requested:                     model.RequestedLibraryKindAuto,
				DisableSiteAutoClassification: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 0.88, Reason: "ai_site", Explanation: "工具站",
				AutoSiteDowngraded: true,
			},
		},
		{
			name:  "灰度开启但用户显式要 site → 不降级",
			notes: "降级只作用于自动判定；用户显式请求绕过灰度开关并锁定",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindSite, Confidence: 0.9, Reason: "ai_site",
				Requested:                     model.RequestedLibraryKindSite,
				RequestedSource:               model.RequestedLibraryKindSourceUser,
				DisableSiteAutoClassification: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 0.9, Reason: "ai_site",
			},
		},
		{
			name: "灰度开启且模型判 reading → 与降级无关",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading, Confidence: 0.8, Reason: "ai_reading",
				Requested:                     model.RequestedLibraryKindAuto,
				DisableSiteAutoClassification: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 0.8, Reason: "ai_reading",
			},
		},
		{
			name: "规则判 site 叠加灰度降级 → 规则结果同样被降级",
			notes: "既有行为：降级开关语义是「site 库尚未就绪」，" +
				"此时 site 判断无论来自模型还是个人规则都不写入 site 库。" +
				"规则的理由与置信度仍被保留在 reading 行上。",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading, Confidence: 0.5, Reason: "ai_reading",
				Requested:                     model.RequestedLibraryKindAuto,
				Rule:                          siteRule,
				DisableSiteAutoClassification: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 1, Reason: "personal_rule_host",
				Explanation:        "matched an enabled personal classification rule",
				AutoSiteDowngraded: true,
			},
		},
		{
			name:  "规则判 site + 用户显式要 site + 灰度开启 → 不降级并锁定",
			notes: "显式请求同时豁免降级并触发锁定，与规则是否命中无关",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading, Confidence: 0.5, Reason: "ai_reading",
				Requested:                     model.RequestedLibraryKindSite,
				RequestedSource:               model.RequestedLibraryKindSourceUser,
				Rule:                          siteRule,
				DisableSiteAutoClassification: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 1, Reason: "personal_rule_host",
				Explanation: "matched an enabled personal classification rule",
			},
		},
		{
			name:  "RSS reading 硬规则保持自动来源且不锁定",
			notes: "具体请求值与来源分开后，系统硬规则不会伪装成用户选择",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindSite, Confidence: 0.86, Reason: "ai_site",
				Requested: model.RequestedLibraryKindReading, RequestedSource: model.RequestedLibraryKindSourceAuto,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 0.86, Reason: "ai_site",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideFinalLibrary(tc.in)
			if got != tc.want {
				t.Errorf("decideFinalLibrary() 结果不符\n got: %+v\nwant: %+v", got, tc.want)
				if tc.notes != "" {
					t.Logf("语义说明：%s", tc.notes)
				}
			}
		})
	}
}

// TestDecideFinalLibraryIsPure 断言决策函数无副作用：同一输入重复调用结果恒等，
// 且不修改传入的规则对象。
func TestDecideFinalLibraryIsPure(t *testing.T) {
	t.Parallel()

	rule := &ClassificationRuleMatch{TargetKind: model.LibraryKindSite, Reason: "personal_rule_host"}
	in := finalLibraryInput{
		AnalyzedKind: model.LibraryKindReading,
		Confidence:   0.42,
		Reason:       "ai_reading",
		Requested:    model.RequestedLibraryKindAuto,
		Rule:         rule,
	}

	first := decideFinalLibrary(in)
	second := decideFinalLibrary(in)
	if first != second {
		t.Fatalf("同一输入两次调用结果不同：%+v vs %+v", first, second)
	}
	if rule.TargetKind != model.LibraryKindSite || rule.Reason != "personal_rule_host" {
		t.Fatalf("决策函数修改了传入的规则对象：%+v", rule)
	}

	// 幂等断言本身是 got/want 同源的——`return finalLibraryDecision{}` 也能让它
	// 通过。补一条对本例结论的实质断言，让这个测试无法被空实现满足。
	if first.Kind != model.LibraryKindSite || first.Reason != "personal_rule_host" || first.Confidence != 1 {
		t.Fatalf("规则命中的结论不符：%+v", first)
	}
}

// TestDecideFinalLibraryNormalizesIllegalKind 锁住归属兜底。
//
// 回归背景：analyzer 的截断摘要回退路径（analyzer/response.go 的
// truncatedSummaryFallback → validateAnalysisResponse）绕过了给 LibraryKind
// 赋默认值的 wrapper，产出空归属。收口重构之前，persist 的 reading 分支把 kind
// 硬编码为 reading，空值被静默兜住；改为消费决策结论后，空值会原样传给
// CompleteReadingParse，而它要求 Kind 必须是 reading——一次本可成功的解析因此
// 变成失败。决策函数的契约是「输出可直接落库的归属」，兜底属于它的职责。
func TestDecideFinalLibraryNormalizesIllegalKind(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		in       finalLibraryInput
		wantKind model.LibraryKind
	}{
		{
			name: "analyzer 未给出归属",
			in: finalLibraryInput{
				AnalyzedKind: "",
				Requested:    model.RequestedLibraryKindAuto,
			},
			wantKind: model.LibraryKindReading,
		},
		{
			name: "analyzer 给出未知归属",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKind("archive"),
				Requested:    model.RequestedLibraryKindAuto,
			},
			wantKind: model.LibraryKindReading,
		},
		{
			name: "个人规则的目标库非法",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindSite,
				Requested:    model.RequestedLibraryKindAuto,
				Rule:         &ClassificationRuleMatch{TargetKind: model.LibraryKind(""), Reason: "broken_rule"},
			},
			// 规则命中后归属取自规则；规则的目标库非法，同样归一化为 reading。
			wantKind: model.LibraryKindReading,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := decideFinalLibrary(tc.in)
			// 既断言「合法」，也断言具体落点——只查集合成员资格的话，
			// `return LibraryKindSite` 这种实现也能让测试通过。
			if got.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Kind, tc.wantKind)
			}
			if got.PredictedKind != tc.wantKind {
				t.Fatalf("PredictedKind = %q, want %q（非法归属归一化后两者应一致）", got.PredictedKind, tc.wantKind)
			}
		})
	}
}

// TestDecideFinalLibraryEmptyKindLandsInReading 除了「合法」，还锁死具体落点：
// 收敛到 reading，与收口重构前的硬编码行为一致。
func TestDecideFinalLibraryEmptyKindLandsInReading(t *testing.T) {
	t.Parallel()

	got := decideFinalLibrary(finalLibraryInput{
		AnalyzedKind: "",
		Confidence:   0.5,
		Reason:       "ai_unknown",
		Requested:    model.RequestedLibraryKindReading,
	})
	if got.Kind != model.LibraryKindReading {
		t.Fatalf("Kind = %q, want reading", got.Kind)
	}
	// 用户显式请求 reading，兜底后终态一致 → 应判为用户锁定。
	// 注：这个退化只在「收口后、兜底修复前」的窗口里出现过——彼时 kind 为空会落入
	// finalClassificationSource 的 default 分支使 Source 退化为 auto。收口重构之前
	// reading 分支硬编码 kind=reading，Source 本就是 user/true。
	if got.Source != model.LibraryKindSourceUser || !got.Locked {
		t.Fatalf("Source/Locked = %v/%v, want user/true", got.Source, got.Locked)
	}
}

// TestDecideFinalLibraryRespectsUserLock 锁死「用户裁决压过自动分类」。
//
// 回归背景：站点转阅读（site_conversion_execute）写入 locked=true 并把 status
// 置为 pending，从而自己触发重解析。重解析时 analyzer 若仍判 site，旧逻辑输出
// kind=site / source=auto / locked=false——用户刚点的「转为阅读」在几秒内被系统
// 撤销，锁一并清掉。library_kind_locked 此前全仓只有 historical_migration 在
// WHERE 里检查，解析路径完全无视它，等于这个锁从未生效过。
func TestDecideFinalLibraryRespectsUserLock(t *testing.T) {
	t.Parallel()

	siteRule := &ClassificationRuleMatch{TargetKind: model.LibraryKindSite, Reason: "personal_rule_host"}

	for _, tc := range []struct {
		name string
		in   finalLibraryInput
		want finalLibraryDecision
	}{
		{
			name: "锁定为 reading，模型判 site → 保持 reading",
			in: finalLibraryInput{
				AnalyzedKind:    model.LibraryKindSite,
				Confidence:      0.93,
				Reason:          "ai_site",
				Requested:       model.RequestedLibraryKindReading,
				RequestedSource: model.RequestedLibraryKindSourceUser,
				CurrentKind:     model.LibraryKindReading,
				CurrentlyLocked: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 1, Reason: libraryReasonUserLocked,
				Explanation: "user locked this collection target; automatic classification left it unchanged",
			},
		},
		{
			name: "锁定为 site，模型判 reading → 保持 site",
			in: finalLibraryInput{
				AnalyzedKind:    model.LibraryKindReading,
				Requested:       model.RequestedLibraryKindSite,
				RequestedSource: model.RequestedLibraryKindSourceUser,
				CurrentKind:     model.LibraryKindSite,
				CurrentlyLocked: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 1, Reason: libraryReasonUserLocked,
				Explanation: "user locked this collection target; automatic classification left it unchanged",
			},
		},
		{
			name: "锁定压过个人规则——规则也是自动分类的一种",
			in: finalLibraryInput{
				AnalyzedKind:    model.LibraryKindReading,
				Requested:       model.RequestedLibraryKindAuto,
				Rule:            siteRule,
				CurrentKind:     model.LibraryKindReading,
				CurrentlyLocked: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 1, Reason: libraryReasonUserLocked,
				Explanation: "user locked this collection target; automatic classification left it unchanged",
			},
		},
		{
			name: "锁定压过灰度降级——降级只作用于自动判定",
			in: finalLibraryInput{
				AnalyzedKind:                  model.LibraryKindSite,
				Requested:                     model.RequestedLibraryKindAuto,
				CurrentKind:                   model.LibraryKindSite,
				CurrentlyLocked:               true,
				DisableSiteAutoClassification: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 1, Reason: libraryReasonUserLocked,
				Explanation: "user locked this collection target; automatic classification left it unchanged",
			},
		},
		{
			name: "锁定但当前归属非法 → 归一化为 reading，不写出非法值",
			in: finalLibraryInput{
				AnalyzedKind:    model.LibraryKindSite,
				Requested:       model.RequestedLibraryKindAuto,
				CurrentKind:     "",
				CurrentlyLocked: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindReading, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 1, Reason: libraryReasonUserLocked,
				Explanation: "user locked this collection target; automatic classification left it unchanged",
			},
		},
		{
			name: "未锁定 → 走原有自动分类，行为不变",
			in: finalLibraryInput{
				AnalyzedKind:    model.LibraryKindSite,
				Confidence:      0.9,
				Reason:          "ai_site",
				Requested:       model.RequestedLibraryKindAuto,
				CurrentKind:     model.LibraryKindReading,
				CurrentlyLocked: false,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindSite,
				Source: model.LibraryKindSourceAuto, Locked: false,
				Confidence: 0.9, Reason: "ai_site",
			},
		},
		{
			name: "后来显式改选 site 压过旧 reading 锁",
			in: finalLibraryInput{
				AnalyzedKind: model.LibraryKindReading, Confidence: 0.72, Reason: "ai_reading",
				Requested: model.RequestedLibraryKindSite, RequestedSource: model.RequestedLibraryKindSourceUser,
				CurrentKind: model.LibraryKindReading, CurrentlyLocked: true,
			},
			want: finalLibraryDecision{
				Kind: model.LibraryKindSite, PredictedKind: model.LibraryKindReading,
				Source: model.LibraryKindSourceUser, Locked: true,
				Confidence: 0.72, Reason: "ai_reading",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decideFinalLibrary(tc.in); got != tc.want {
				t.Errorf("decideFinalLibrary() 结果不符\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

// TestSiteToReadingConversionSurvivesReparse 复现完整的冲突场景，确认闭环。
//
// 这是 P0 的端到端断言：用户转换 → locked=true + status=pending → 重解析 →
// analyzer 仍判 site。修复前此处会得到 site/auto/false。
func TestSiteToReadingConversionSurvivesReparse(t *testing.T) {
	t.Parallel()

	// 转换后 link 的实际状态（见 convertSiteToReadingSQL）。
	reading := model.LibraryKindReading
	converted := &model.Link{LibraryKind: &reading, LibraryKindLocked: true}

	got := decideFinalLibrary(finalLibraryInput{
		AnalyzedKind:    model.LibraryKindSite, // 内容确实像站点，模型不改口
		Confidence:      0.95,
		Reason:          "ai_site",
		Requested:       requestedKindForLink(parseInputForTest(converted)),
		CurrentKind:     currentLibraryKind(parseInputForTest(converted)),
		CurrentlyLocked: converted.LibraryKindLocked,
	})

	if got.Kind != model.LibraryKindReading {
		t.Fatalf("Kind = %q，用户的「转为阅读」被重解析撤销了", got.Kind)
	}
	if !got.Locked || got.Source != model.LibraryKindSourceUser {
		t.Fatalf("Locked=%v Source=%q，用户锁定未被保留", got.Locked, got.Source)
	}
	if got.PredictedKind != model.LibraryKindSite {
		t.Fatalf("PredictedKind = %q，模型与用户的分歧应保留下来供复核", got.PredictedKind)
	}
}

func TestRequestedKindForLinkDoesNotInferAutomaticIntentFromFinalPartition(t *testing.T) {
	t.Parallel()

	reading := model.LibraryKindReading
	link := parseInputForTest(&model.Link{
		LibraryKind:       &reading,
		LibraryKindLocked: false,
	})

	if got := requestedKindForLink(link); got != model.RequestedLibraryKindAuto {
		t.Fatalf("requestedKindForLink() = %q, want auto for an unlocked automatic result", got)
	}
}

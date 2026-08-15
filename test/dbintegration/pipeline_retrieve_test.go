// pipeline_retrieve_test.go exercises the Phase 7 Retrieve-then-Select
// tagging chain end to end against a real pgvector Postgres. The pure-Go
// unit tests in internal/service cover the gate/route branch logic with
// stubs; this file is the only place the full chain runs against the actual
// schema:
//
//	content embedding → ListNearestConcepts (model-scoped cosine recall) →
//	candidate injection → analyzer (stub) → new-word routing → concept
//	attach (real resolver: alias fast-table for selections, vector gate for
//	the new word).
//
// All vectors are unit vectors so cosine similarity equals the dot product,
// letting each probe sit at an exact known cosine to the seeds.
package dbintegration

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"webtag/internal/concept"
	"webtag/internal/fetcher"
	"webtag/internal/model"
	"webtag/internal/repository"
	"webtag/internal/service"
	"webtag/internal/service/analyzer"
)

const (
	retrieveModel   = "test-embed-model"
	retrieveDims    = 1536
	retrieveColdMin = 30
	retrieveTopK    = 40
)

// retrieveUnitVec returns a 1536-d unit vector at cosine c to e0=[1,0,…]:
// [c, sqrt(1-c^2), 0, …].
func retrieveUnitVec(c float64) []float32 {
	v := make([]float32, retrieveDims)
	v[0] = float32(c)
	v[1] = float32(math.Sqrt(1 - c*c))
	return v
}

// seedRetrieveConcept inserts a concept carrying a vector at cosine c to
// the content probe (e0). Returns the new concept id.
func seedRetrieveConcept(t *testing.T, pool *pgxpool.Pool, name string, cos float64) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO concept (primary_name, display_name, embedding, embedding_model)
		 VALUES ($1, $1, $2, $3) RETURNING id`,
		name, pgvector.NewVector(retrieveUnitVec(cos)), retrieveModel,
	).Scan(&id); err != nil {
		t.Fatalf("seed retrieve concept %q: %v", name, err)
	}
	// Identity alias so selections hit the alias fast-table exactly like
	// production-created concepts do.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO concept_alias (alias, concept_id, source, confidence)
		 VALUES (lower($1), $2, 'identity', 1.0) ON CONFLICT DO NOTHING`,
		name, id,
	); err != nil {
		t.Fatalf("seed identity alias %q: %v", name, err)
	}
	return id
}

// retrieveScriptedEmbedder embeds content (and most surface forms) as the
// probe vector e0, so recall ranks seeds by their seeded cosine to e0. The
// one exception is the exact surface "WeKnora": it embeds onto an ORTHOGONAL
// axis (e2 = [0,0,1,0,…]), which is cosine 0 to every e0-plane seed, so the
// vector gate admits it as a genuinely-new concept rather than auto-merging
// it into RAG. This lets the end-to-end test cover BOTH routing branches in
// one run: a candidate selection (RAG → existing) and a new word (WeKnora →
// vector gate → new concept).
type retrieveScriptedEmbedder struct{}

func (retrieveScriptedEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		if in == "WeKnora" {
			v := make([]float32, retrieveDims)
			v[2] = 1 // orthogonal to every seed in the e0/e1 plane
			out[i] = v
			continue
		}
		out[i] = retrieveUnitVec(1.0) // e0
	}
	return out, nil
}
func (retrieveScriptedEmbedder) Model() string { return retrieveModel }
func (retrieveScriptedEmbedder) Enabled() bool { return true }

type stubFetcher struct{ content fetcher.Content }

func (s stubFetcher) Fetch(context.Context, string) (fetcher.Content, error) { return s.content, nil }

type stubAnalyzer struct {
	gotCandidates []string
	tags          []string
}

func (s *stubAnalyzer) Analyze(_ context.Context, req analyzer.AnalyzeRequest) (analyzer.AnalysisResult, error) {
	s.gotCandidates = append([]string(nil), req.Candidates...)
	return analyzer.AnalysisResult{Summary: "摘要", Tags: s.tags}, nil
}

// TestPipelineRetrieveThenSelectEndToEnd: with a warm词表 (≥ ColdStartMin
// seeded concepts) and a content embedding nearest to the "RAG" seed, the
// pipeline must inject candidates, route the analyzer's chosen "RAG"
// (candidate selection → existing concept) and a new word "WeKnora" (vector
// gate → new concept) onto the link, persisting exactly those two tags.
func TestPipelineRetrieveThenSelectEndToEnd(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()

	// RAG 落在**歧义带**（NewThreshold 0.80 < 0.85 < AutoMergeThreshold 0.92），
	// 这个取值是刻意的，不能随手调高：
	//
	// 「RAG」这个 surface 被 embedder 映射成 e0，与 RAG seed 的余弦就等于这里
	// 的种子余弦。若取 0.99（本测试的原值），即便 alias 快表完全失效，向量门
	// 也会因 0.99 ≥ 0.92 自动合并回同一个 concept——**两条路径殊途同归，测试
	// 分辨不出 alias 快表是否真的命中**。
	//
	// 取 0.85 后，只有 alias 快表命中才会落到 ragID；一旦快表失效，向量门走
	// gateAmbiguous 新建一个 concept（见 internal/concept/gate.go），下面的
	// `attached[ragID]` 断言就会变红。
	//
	// 这不是推断，是变异验证过的：把 resolver_batch.go 里的
	// `r.store.GetConceptIDsByAliases(ctx, lookupSet)` 换成恒返回空 map，
	//   cos 0.99 → 测试仍然 ok（无判别力）
	//   cos 0.85 → 测试变红，报「新建了重复 concept」
	// 注意批量路径查的是 GetConceptIDsByAliases，不是 resolver.go 里那个单条
	// 的 GetConceptIDByAlias——改错后者会得到「变异不生效」的假象。
	//
	// 与召回排序无关：filler 在 0.10–0.135，RAG 仍是无争议的 top-1。
	ragID := seedRetrieveConcept(t, pool, "RAG", 0.85)
	for i := 0; i < retrieveColdMin+5; i++ {
		// Spread the filler seeds across cosines well below RAG so RAG is
		// the unambiguous top-1 and the filler stays out of the new-word's
		// auto-merge band.
		seedRetrieveConcept(t, pool, fillerName(i), 0.10+float64(i)*0.001)
	}

	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	tags := repository.NewPGXTagRepository(pool)
	tree := repository.NewPGXTreeRepository(pool)
	concepts := repository.NewPGXConceptRepository(pool)
	proposals := repository.NewPGXConceptProposalRepository(pool)

	link, job, err := links.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/rag-article",
		SourceKind: "url",
		SourceKey:  "rag-article",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	resolver := concept.NewResolver(concept.ResolverOptions{
		Store:     concepts,
		Embedder:  retrieveScriptedEmbedder{},
		Proposals: proposals,
		Gate:      concept.GateConfig{AutoMergeThreshold: 0.92, NewThreshold: 0.80},
		Cache:     concept.NewAliasCache(),
	})

	an := &stubAnalyzer{tags: []string{"RAG", "WeKnora"}}

	pipeline := service.NewParsePipeline(service.ParsePipelineOptions{
		Links:            links,
		Jobs:             jobs,
		Tags:             tags,
		Tree:             tree,
		SiteCompleter:    links,
		ReadingCompleter: links,
		Fetcher:          stubFetcher{content: fetcher.Content{URL: link.URL, Title: "RAG 检索增强生成", Body: "正文讲 RAG 与 WeKnora", FetcherType: "basic"}},
		Analyzer:         an,
		// One embedder backs both content recall and the resolver's vector
		// gate. It embeds content as e0 (so RAG is the nearest candidate)
		// and the exact surface "WeKnora" onto an orthogonal axis (so the
		// gate admits it as a NEW concept instead of auto-merging into RAG).
		Embedder:          retrieveScriptedEmbedder{},
		ConceptCandidates: concepts,
		ConceptAttacher:   resolver,
		RetrieveTopK:      retrieveTopK,
		ColdStartMin:      retrieveColdMin,
	})

	if err := pipeline.Run(ctx, link.ID, job.ID); err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}

	// 1. Candidates were injected and RAG was among them (top-1 by cosine).
	if len(an.gotCandidates) == 0 {
		t.Fatal("analyzer received no candidates; retrieval did not run")
	}
	if an.gotCandidates[0] != "RAG" {
		t.Errorf("top candidate = %q, want RAG", an.gotCandidates[0])
	}

	// 2. The link persisted to done with the two routed tags.
	got, err := links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != model.LinkStatusDone {
		t.Fatalf("link status = %q, want done", got.Status)
	}
	if len(got.Tags) != 2 {
		t.Fatalf("persisted tags = %#v, want 2 (RAG + WeKnora)", got.Tags)
	}

	// 3+4. 两条路由分支各自落到了正确的 concept 上。
	//
	// 断言必须读 link_concept——它是 attachConcepts **写出来**的东西。早先这里
	// 查的是 `GetConceptIDByAlias(ctx, "rag")`，而那行 alias 是
	// seedRetrieveConcept 在 Run 之前自己插的：断言在 pipeline 跑之前就已经为
	// 真，把 attachConcepts 换成空实现也照样通过。配套的 `count(*) = 2` 同样
	// 不辨身份——「RAG 挂到了 seed」和「新建了一个重复的 RAG 再挂上去」在计数
	// 上完全一样，而后者正是 alias 快表失效时会发生的事。
	rows, err := pool.Query(ctx,
		`SELECT lc.concept_id, c.primary_name
		   FROM link_concept lc JOIN concept c ON c.id = lc.concept_id
		  WHERE lc.link_id = $1`, link.ID)
	if err != nil {
		t.Fatalf("query link_concept: %v", err)
	}
	defer rows.Close()

	attached := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scan link_concept: %v", err)
		}
		attached[id] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate link_concept: %v", err)
	}

	if len(attached) != 2 {
		t.Fatalf("link_concept 挂了 %d 个 concept，want 2：%v", len(attached), attached)
	}

	// 分支一：候选选择 → 复用既有 concept。挂上的必须**就是那个 seed 的 id**，
	// 而不是另一个同名新建的。
	if _, ok := attached[ragID]; !ok {
		t.Errorf("RAG 没有挂到既有 seed %v —— alias 快表未命中，很可能新建了重复 concept。实际挂上的：%v",
			ragID, attached)
	}

	// 分支二：新词 → 向量门 → 新建 concept。它的 id 必须不等于任何 seed。
	var newIDs []uuid.UUID
	for id := range attached {
		if id != ragID {
			newIDs = append(newIDs, id)
		}
	}
	if len(newIDs) != 1 {
		t.Fatalf("除 RAG 外应恰好有 1 个新 concept，实际 %d 个：%v", len(newIDs), attached)
	}
	// 只查 primary_name 即可：newIDs 本身就是按 `id != ragID` 过滤出来的，
	// 再断言 `newIDs[0] != ragID` 恒真、不携带任何信息。
	// （上一版写了那条，属于把过滤条件重复成断言。）
	if got := attached[newIDs[0]]; !strings.EqualFold(got, "WeKnora") {
		t.Errorf("新建 concept 的 primary_name = %q，want WeKnora"+
			"——说明新词没走向量门，或被并进了别的既有 concept", got)
	}
}

// TestPipelineRetrieveColdStartSkipsRecall: below ColdStartMin the pipeline
// must NOT inject candidates (free-generation), even with a live embedder
// and a non-empty词表.
func TestPipelineRetrieveColdStartSkipsRecall(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()

	// Only a handful of concepts — below the threshold.
	for i := 0; i < 5; i++ {
		seedRetrieveConcept(t, pool, fillerName(i), 0.5)
	}

	links := repository.NewPGXLinkRepository(pool)
	jobs := repository.NewPGXJobRepository(pool)
	tags := repository.NewPGXTagRepository(pool)
	tree := repository.NewPGXTreeRepository(pool)
	concepts := repository.NewPGXConceptRepository(pool)

	link, job, err := links.SubmitNew(ctx, repository.CreateLinkParams{
		URL:        "https://example.com/cold",
		SourceKind: "url",
		SourceKey:  "cold",
		Status:     model.LinkStatusPending,
	})
	if err != nil {
		t.Fatalf("SubmitNew: %v", err)
	}

	an := &stubAnalyzer{tags: []string{"x", "y", "z"}}
	pipeline := service.NewParsePipeline(service.ParsePipelineOptions{
		Links:             links,
		Jobs:              jobs,
		Tags:              tags,
		Tree:              tree,
		SiteCompleter:     links,
		ReadingCompleter:  links,
		Fetcher:           stubFetcher{content: fetcher.Content{URL: link.URL, Title: "标题", Body: "正文", FetcherType: "basic"}},
		Analyzer:          an,
		Embedder:          retrieveScriptedEmbedder{},
		ConceptCandidates: concepts,
		RetrieveTopK:      retrieveTopK,
		ColdStartMin:      retrieveColdMin,
	})

	if err := pipeline.Run(ctx, link.ID, job.ID); err != nil {
		t.Fatalf("pipeline.Run: %v", err)
	}
	if len(an.gotCandidates) != 0 {
		t.Errorf("cold start injected candidates %#v, want none", an.gotCandidates)
	}
	got, err := links.GetByID(ctx, link.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Tags) != 3 {
		t.Errorf("persisted tags = %#v, want 3 (free-generation, unrouted)", got.Tags)
	}
}

// TestListNearestConceptsModelScopedTopK directly probes the repo method:
// it returns at most topK candidates, model-scoped, nearest-first.
func TestListNearestConceptsModelScopedTopK(t *testing.T) {
	pool := StartPostgres(t)
	ctx := context.Background()
	concepts := repository.NewPGXConceptRepository(pool)

	// Three same-model concepts at descending cosine + one other-model
	// concept that must be invisible.
	near := seedRetrieveConcept(t, pool, "Near", 0.95)
	seedRetrieveConcept(t, pool, "Mid", 0.70)
	seedRetrieveConcept(t, pool, "Far", 0.40)
	// Other-model neighbour at cos 0.99 — must NOT appear.
	if _, err := pool.Exec(ctx,
		`INSERT INTO concept (primary_name, display_name, embedding, embedding_model)
		 VALUES ('OtherModel', 'OtherModel', $1, 'some-other-model')`,
		pgvector.NewVector(retrieveUnitVec(0.99)),
	); err != nil {
		t.Fatalf("seed other-model concept: %v", err)
	}

	cands, err := concepts.ListNearestConcepts(ctx, retrieveUnitVec(1.0), retrieveModel, 2)
	if err != nil {
		t.Fatalf("ListNearestConcepts: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates = %d, want 2 (topK clamp)", len(cands))
	}
	if cands[0].ConceptID != near || cands[0].Name != "Near" {
		t.Errorf("top candidate = %+v, want Near %v", cands[0], near)
	}
	for _, c := range cands {
		if c.Name == "OtherModel" {
			t.Errorf("other-model concept leaked into candidates: %+v", c)
		}
	}

	// CountConcepts sees all four rows regardless of model.
	n, err := concepts.CountConcepts(ctx)
	if err != nil {
		t.Fatalf("CountConcepts: %v", err)
	}
	if n != 4 {
		t.Errorf("CountConcepts = %d, want 4", n)
	}
}

func fillerName(i int) string {
	return "filler-" + string(rune('a'+(i%26))) + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

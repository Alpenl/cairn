package concept

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// ResolveAndAttachBatch is the bulk entry point the parse pipeline calls.
// A naive per-tag implementation would issue four round-trips to Postgres
// for every LLM-produced surface tag (alias lookup + attach + use_count
// bump + display-name recalc) — 20-32 statements for a typical 5-8 tag
// link. The batch form collapses the cold path to four statements
// regardless of N:
//
//  1. one alias lookup (GetConceptIDsByAliases)
//  2. per-tag Resolve only for the misses — vector-gate creates stay
//     one-at-a-time because each one embeds the surface form and runs
//     its own nearest-neighbour comparison that the singular Resolve
//     path already handles correctly
//  3. one AttachLinkConceptsBatch
//  4. one IncrementUseCounts + one RecalculateDisplayNames
//
// Fail-soft contract: a per-tag Resolve error is logged and the tag
// is skipped, never bubbled up. The returned slice mirrors the input
// order with uuid.Nil for tags that failed to resolve, so callers
// that need traceability can correlate without re-doing the work.
//
//nolint:gocyclo // reason: 这是分四阶段的 batch 编排（cache 查询/缺失收集/persist/recalc），拆函数会把共享状态在子函数间反复传递，反而比一处线性读起来难。
func (r *Resolver) ResolveAndAttachBatch(ctx context.Context, linkID uuid.UUID, expectedMetadataRevision int64, surfaceTags []string) ([]uuid.UUID, error) {
	// A zero revision belongs to a pre-fence legacy parse_jobs row. It has no
	// authority to create derived edges because its actual start revision is
	// unknowable after rollout.
	if linkID == uuid.Nil || expectedMetadataRevision <= 0 || len(surfaceTags) == 0 {
		return nil, nil
	}

	// Step 1: normalise + dedupe, then split into cache hits vs the
	// residual that needs a DB round-trip. We keep the caller's
	// original order in `cleaned` so the returned ID slice aligns
	// 1:1 with the input.
	cleaned := make([]string, len(surfaceTags))
	lookupSet := make([]string, 0, len(surfaceTags))
	seenLookup := make(map[string]struct{}, len(surfaceTags))
	aliasHits := make(map[string]uuid.UUID, len(surfaceTags))
	for i, raw := range surfaceTags {
		t := strings.TrimSpace(raw)
		cleaned[i] = t
		if t == "" {
			continue
		}
		key := strings.ToLower(t)
		if _, ok := seenLookup[key]; ok {
			continue
		}
		seenLookup[key] = struct{}{}
		// Cache hit short-circuits the batch SELECT for this key —
		// every key skipped here is one column-row pair we do not
		// have to round-trip for. The cache is nil-safe so the
		// no-cache wiring continues to land every key in lookupSet.
		if id, ok := r.cache.Get(t); ok {
			aliasHits[key] = id
			continue
		}
		lookupSet = append(lookupSet, key)
	}

	// aliasLookupOK 记录批量别名查询是否成功。失败时（DB 瞬时错误，非 miss）所有
	// 未命中 tag 必须走完整的 per-tag Resolve——它会*重试* store 别名查询，命中已存在
	// 别名则复用概念。若改走 resolveMiss（故意跳过 store 别名查询，只信任本批查询
	// 结果），瞬时故障下会把本应复用的 tag 误判为新词、创建重复概念。故批量查询失败
	// 时禁用下面的批量 embedding 优化，回退到与旧行为等价的 per-tag 路径。
	aliasLookupOK := true
	if len(lookupSet) > 0 {
		dbHits, err := r.store.GetConceptIDsByAliases(ctx, lookupSet)
		if err != nil {
			// A batch-lookup failure is recoverable: each unresolved
			// tag falls through to the per-tag Resolve path below,
			// which itself consults the cache + store one at a time.
			// Logged so operators can see the degradation if it
			// persists.
			aliasLookupOK = false
			r.logf("warn", "batch alias lookup failed; falling back to per-tag resolve",
				"link_id", linkID.String(), "err", err)
		} else {
			for k, id := range dbHits {
				aliasHits[k] = id
				// Back-fill the cache so subsequent links with the
				// same tag skip even the batch SELECT. The key here
				// is already lowercased (the store contract); Put
				// re-normalises so this stays correct either way.
				r.cache.Put(k, id)
			}
		}
	}

	// Step 1b: batch-embed the alias misses in ONE upstream call. Previously
	// each miss embedded its surface form separately inside
	// Resolve→resolveViaGate (N round-trips for N new tags). The
	// nearest-neighbour DB comparisons stay per-tag (each vector is compared
	// independently), but the embedding-provider network call collapses to
	// one. Fail-soft: a batch embed error / disabled embedder leaves embByKey
	// empty and each miss falls back to the original per-tag Resolve below.
	// 仅当批量别名查询成功时启用——失败时 resolveMiss 会跳过 store 别名重试（见
	// aliasLookupOK 注释），故此时 embByKey 留空，全部走 per-tag Resolve。
	embByKey := make(map[string][]float32)
	if aliasLookupOK && r.embedder != nil && r.embedder.Enabled() {
		missKeys := make([]string, 0, len(cleaned))
		missTexts := make([]string, 0, len(cleaned))
		seenMiss := make(map[string]struct{}, len(cleaned))
		for _, tag := range cleaned {
			if tag == "" {
				continue
			}
			key := strings.ToLower(tag)
			if _, ok := aliasHits[key]; ok {
				continue
			}
			if _, dup := seenMiss[key]; dup {
				continue
			}
			seenMiss[key] = struct{}{}
			missKeys = append(missKeys, key)
			missTexts = append(missTexts, tag)
		}
		if len(missTexts) > 0 {
			vectors, err := r.embedder.Embed(ctx, missTexts)
			if err != nil || len(vectors) != len(missTexts) {
				r.logf("warn", "batch tag embedding failed; falling back to per-tag resolve",
					"link_id", linkID.String(), "count", len(missTexts), "err", errString(err))
			} else {
				for i, vec := range vectors {
					if len(vec) > 0 {
						embByKey[missKeys[i]] = vec
					}
				}
			}
		}
	}

	// Step 2: resolve each tag. Hits come from the alias map; misses
	// fall through to the full Resolve (vector gate + local create).
	// conceptIDs is deduped here so Step 4 only bumps use_count once
	// per distinct concept for this run. The repository's batch methods
	// dedupe again as a defence-in-depth measure — both layers are
	// intentional: removing either is safe in isolation but doing so
	// breaks the other's invariant if a future caller bypasses one layer.
	results := make([]uuid.UUID, len(cleaned))
	attachItems := make([]AttachLinkConceptItem, 0, len(cleaned))
	conceptIDs := make([]uuid.UUID, 0, len(cleaned))
	conceptSeen := make(map[uuid.UUID]struct{}, len(cleaned))
	for i, tag := range cleaned {
		if tag == "" {
			continue
		}
		id, ok := aliasHits[strings.ToLower(tag)]
		if !ok {
			// Miss: use the batch-precomputed embedding when available
			// (resolveMiss), else fall back to per-tag Resolve which embeds
			// itself (batch embed disabled / failed / empty vector for this tag).
			var resolved uuid.UUID
			var resolveErr error
			if vec, has := embByKey[strings.ToLower(tag)]; has {
				resolved, resolveErr = r.resolveMiss(ctx, tag, vec)
			} else {
				resolved, resolveErr = r.Resolve(ctx, tag)
			}
			if resolveErr != nil {
				r.logf("warn", "batch resolve fallthrough failed",
					"link_id", linkID.String(),
					"surface", tag, "err", resolveErr)
				continue
			}
			if resolved == uuid.Nil {
				continue
			}
			id = resolved
		}
		results[i] = id
		attachItems = append(attachItems, AttachLinkConceptItem{
			ConceptID:  id,
			SurfaceTag: tag,
		})
		if _, dup := conceptSeen[id]; !dup {
			conceptSeen[id] = struct{}{}
			conceptIDs = append(conceptIDs, id)
		}
	}

	if len(attachItems) == 0 {
		return results, nil
	}

	// Step 3: bulk attach. A failure here means we lose this link's
	// link_concept rows for this run; we still return the resolved IDs
	// so an upstream caller could retry without re-doing the LLM /
	// Wikidata work. Logged at warn for the same reason as the
	// singular path.
	attached, err := r.store.AttachLinkConceptsBatch(ctx, linkID, expectedMetadataRevision, surfaceTags, attachItems)
	if err != nil {
		r.logf("warn", "batch attach link_concept failed",
			"link_id", linkID.String(),
			"count", len(attachItems), "err", err)
		return results, nil
	}
	if !attached {
		// A user CAS changed either the metadata revision or tag tuple while
		// concepts were being resolved. The Link is already authoritative; do
		// not increment counts or run the later replace/detach pass against a
		// newer tuple.
		r.logf("info", "skip stale batch link_concept attach",
			"link_id", linkID.String(), "expected_metadata_revision", expectedMetadataRevision)
		return results, nil
	}

	// Step 4: bookkeeping. Both are best-effort: a failure leaves the
	// UI a little stale (stale use_count ordering, stale display name)
	// but does not invalidate the attach itself. Note that conceptIDs
	// is already deduped above so the same link re-processing the same
	// concept twice in one run only bumps use_count by 1 — but if the
	// whole link is re-parsed later, the bump fires again (ON CONFLICT
	// on attach is row-level idempotent, use_count is not).
	if err := r.store.IncrementUseCounts(ctx, conceptIDs); err != nil {
		r.logf("warn", "batch increment use_counts failed",
			"link_id", linkID.String(),
			"count", len(conceptIDs), "err", err)
	}

	// Step 5: reconcile to "replace" semantics. The attach above only
	// adds rows, so a re-parsed link would otherwise accumulate every
	// concept it ever carried (re-tag a "try proposal" link as "Go" and
	// both linger). Shedding the concepts no longer in this run's set
	// keeps the link's concept surface in sync with its current tags.
	// Best-effort like the rest of Step 4/5: a failure leaves stale rows
	// but never invalidates the attach. recalcIDs folds the detached
	// concepts into the display-name recompute so a concept that just
	// lost its surface tag here refreshes too.
	recalcIDs := conceptIDs
	if removed, err := r.store.DetachLinkConceptsExcept(ctx, linkID, expectedMetadataRevision, surfaceTags, conceptIDs); err != nil {
		r.logf("warn", "batch detach stale link_concepts failed",
			"link_id", linkID.String(),
			"keep", len(conceptIDs), "err", err)
	} else if len(removed) > 0 {
		recalcIDs = append(append(make([]uuid.UUID, 0, len(conceptIDs)+len(removed)), conceptIDs...), removed...)
	}

	if err := r.store.RecalculateDisplayNames(ctx, recalcIDs); err != nil {
		r.logf("warn", "batch recalc display_names failed",
			"link_id", linkID.String(),
			"count", len(recalcIDs), "err", err)
	}
	return results, nil
}

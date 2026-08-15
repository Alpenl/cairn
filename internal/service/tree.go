package service

import (
	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/model"
)

// maxTreeDepth caps the recursion depth in materializeTreeNode. A
// pathological linear parent_id chain (e.g. 5000 rows each pointing at
// the next) would otherwise emit a 5000-deep nested JSON object that
// blows the JSON encoder's stack and is unparseable on the client.
// 32 is the same cap used by the parser for urlmeta.AncestorURLs and well
// above any healthy navigation tree.
const maxTreeDepth = 32

// BuildTree 把扁平链接列表按 ParentID 拼装成多根森林并返回总数。
// 构建过程会主动剔除环：DB 历史脏数据导致的 parent_id 环会被识别成
// 「孤儿根」而非递归崩栈；同时 materializeTreeNode 用 maxTreeDepth 限制
// 渲染深度，让客户端始终能解析返回。
func BuildTree(links []model.Link) ([]dto.TreeNodeResponse, int) {
	nodeMap := make(map[string]*treeNode, len(links))
	parentIDs := make(map[string]string, len(links))
	nodeIDs := make([]string, 0, len(links))
	for _, link := range links {
		node := &treeNode{value: linkToTreeNode(link)}
		nodeMap[node.value.ID] = node
		nodeIDs = append(nodeIDs, node.value.ID)
		if link.ParentID != nil {
			parentIDs[link.ID.String()] = link.ParentID.String()
		}
	}

	// Single O(N) ancestor-marking pass identifies all nodes that
	// belong to a cycle. The previous per-node createsCycle walk was
	// O(N²) worst case in a long parent_id chain — within the LIMIT
	// 5000 cap that's ~12.5M ops on a pathological dataset.
	cyclic := detectCyclicNodes(parentIDs, nodeIDs)

	roots := make([]*treeNode, 0, len(links))
	for _, link := range links {
		node := nodeMap[link.ID.String()]
		if link.ParentID != nil {
			if _, inCycle := cyclic[link.ID.String()]; !inCycle {
				if parent, ok := nodeMap[link.ParentID.String()]; ok {
					parent.children = append(parent.children, node)
					continue
				}
			}
		}
		roots = append(roots, node)
	}

	out := make([]dto.TreeNodeResponse, 0, len(roots))
	for _, root := range roots {
		out = append(out, materializeTreeNode(root, 0))
	}

	return out, len(links)
}

type treeNode struct {
	value    dto.TreeNodeResponse
	children []*treeNode
}

// linkToTreeNode mirrors the shared fields linkToResponse exposes minus the
// error/path metadata that the tree view does not surface.
// Both functions list their fields explicitly because they target distinct
// DTO types — a future generic abstraction is not worth the cost of
// hiding the contract from `find` searches.
func linkToTreeNode(link model.Link) dto.TreeNodeResponse {
	return dto.TreeNodeResponse{
		ID:                  link.ID.String(),
		URL:                 link.URL,
		Title:               link.Title,
		Summary:             link.Summary,
		Description:         link.Description,
		Tags:                append([]string{}, link.Tags...),
		ContentType:         link.ContentType,
		Status:              string(link.Status),
		Domain:              link.Domain,
		PathDepth:           link.PathDepth,
		ParentID:            parentIDPtr(link),
		FetcherType:         link.FetcherType,
		IsLowConfidence:     link.IsLowConfidence,
		LowConfidenceReason: deriveLowConfidenceReason(link),
		CreatedAt:           link.CreatedAt,
		UpdatedAt:           link.UpdatedAt,
		Children:            []dto.TreeNodeResponse{},
	}
}

// materializeTreeNode produces an immutable subtree by copying the
// pre-built dto.TreeNodeResponse value and recursing on children. Tags
// is shared with the source node — linkToTreeNode already gave each
// node its own slice and the tree builder never mutates Tags, only
// Children — so the previous re-clone was redundant overhead per node.
//
// depth is the number of ancestors above the current node; the call
// bails at maxTreeDepth and marks the node Truncated so clients can
// distinguish "actual leaf" from "render cap fired".
func materializeTreeNode(node *treeNode, depth int) dto.TreeNodeResponse {
	cloned := node.value
	if depth >= maxTreeDepth {
		if len(node.children) > 0 {
			cloned.Truncated = true
		}
		cloned.Children = []dto.TreeNodeResponse{}
		return cloned
	}
	cloned.Children = make([]dto.TreeNodeResponse, 0, len(node.children))
	for _, child := range node.children {
		cloned.Children = append(cloned.Children, materializeTreeNode(child, depth+1))
	}
	return cloned
}

// parentIDPtr converts the optional uuid.UUID parent reference into the
// optional string pointer that both LinkResponse and TreeNodeResponse
// expose. Centralising it keeps mappers from each repeating the pattern.
func parentIDPtr(link model.Link) *string {
	return uuidStringPtr(link.ParentID)
}

func uuidStringPtr(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	value := id.String()
	return &value
}

// cycleColor labels each node during three-color DFS. white = not yet
// visited, gray = currently on the walk path (an ancestor of the
// in-progress node), black = fully resolved (either confirmed cyclic
// or confirmed acyclic — once black we never revisit).
type cycleColor int8

const (
	cycleWhite cycleColor = 0
	cycleGray  cycleColor = 1
	cycleBlack cycleColor = 2
)

// detectCyclicNodes returns the set of node IDs that participate in any
// cycle in the parentIDs graph. Three-color DFS along the parent chain
// runs each node at most twice (once gray entering, once black on
// resolution), so the algorithm is O(N) amortized regardless of chain
// length. Any node whose parent chain folds back onto itself, plus
// every node on the cycle interior, end up in the returned set.
func detectCyclicNodes(parentIDs map[string]string, nodeIDs []string) map[string]struct{} {
	color := make(map[string]cycleColor, len(nodeIDs))
	cyclic := make(map[string]struct{})
	for _, start := range nodeIDs {
		if color[start] != cycleWhite {
			continue
		}
		path := make([]string, 0, 8)
		cur := start
		for {
			if cur == "" {
				// Walked off the top of the parent chain — the entire
				// path is acyclic; paint it black and move on.
				for _, p := range path {
					color[p] = cycleBlack
				}
				break
			}
			c := color[cur]
			if c == cycleBlack {
				// We hit a node we've already classified as acyclic;
				// the rest of our path inherits that classification.
				for _, p := range path {
					color[p] = cycleBlack
				}
				break
			}
			if c == cycleGray {
				// Cycle: cur is the entry point. Path entries from cur
				// to the end of `path` are on the cycle; entries before
				// cur are tails that lead into the cycle but aren't part
				// of it. We mark the whole path black so we never
				// revisit, and the cycle interior as cyclic.
				inCycle := false
				for _, p := range path {
					if p == cur {
						inCycle = true
					}
					if inCycle {
						cyclic[p] = struct{}{}
					}
					color[p] = cycleBlack
				}
				break
			}
			color[cur] = cycleGray
			path = append(path, cur)
			cur = parentIDs[cur]
		}
	}
	return cyclic
}

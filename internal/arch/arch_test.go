package arch

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var retiredDurablePortNames = map[string]struct{}{
	"LinkConversionEnqueueHook": {},
	"SubmitEnqueueHook":         {},
	"TranslationScheduleHook":   {},
	"LinkConversionWriter":      {},
	"LinkSubmitter":             {},
	"LinkLifecycleDeleter":      {},
}

var retiredFullLinkLookupMethods = map[string]struct{}{
	"GetByID":             {},
	"GetByURL":            {},
	"GetBySourceKey":      {},
	"GetBySourceKeyOrURL": {},
}

// TestServiceLinkReadsUseNarrowProjections locks the RF9 module boundary.
// Composition code may aggregate all repository capabilities, but a product
// service must own a consumer-shaped port. Otherwise adding one convenient
// GetByID call silently restores the 1.5 MiB capture scan RF9 removed.
func TestServiceLinkReadsUseNarrowProjections(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Join("..", "..")
	serviceRoot := filepath.Join(repoRoot, "internal", "service")
	forbiddenRepositoryTypes := map[string]struct{}{
		"LinkReader":        {},
		"LinkStore":         {},
		"PGXLinkRepository": {},
	}
	var violations []string
	if err := filepath.WalkDir(serviceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		repositoryAliases := map[string]struct{}{}
		modelAliases := map[string]struct{}{}
		for _, imp := range file.Imports {
			pathValue, unquoteErr := strconv.Unquote(imp.Path.Value)
			if unquoteErr != nil {
				continue
			}
			switch pathValue {
			case "webtag/internal/repository":
				alias := "repository"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
				repositoryAliases[alias] = struct{}{}
			case "webtag/internal/model":
				alias := "model"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
				modelAliases[alias] = struct{}{}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.SelectorExpr:
				ident, ok := typed.X.(*ast.Ident)
				if !ok {
					break
				}
				if _, isRepository := repositoryAliases[ident.Name]; !isRepository {
					break
				}
				if _, forbidden := forbiddenRepositoryTypes[typed.Sel.Name]; forbidden {
					violations = append(violations, rel+": depends on repository."+typed.Sel.Name)
				}
			case *ast.InterfaceType:
				for _, field := range typed.Methods.List {
					for _, name := range field.Names {
						if _, retired := retiredFullLinkLookupMethods[name.Name]; retired && methodReturnsModelLink(field.Type, modelAliases) {
							violations = append(violations, rel+": declares legacy full-link method "+name.Name)
						}
					}
				}
			}
			return true
		})
		return nil
	}); err != nil {
		t.Fatalf("walk service sources: %v", err)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("RF9 narrow projection boundary regressed:\n  %s", strings.Join(violations, "\n  "))
	}
}

func methodReturnsModelLink(expr ast.Expr, modelAliases map[string]struct{}) bool {
	function, ok := expr.(*ast.FuncType)
	if !ok || function.Results == nil {
		return false
	}
	for _, result := range function.Results.List {
		resultType := result.Type
		if pointer, ok := resultType.(*ast.StarExpr); ok {
			resultType = pointer.X
		}
		selector, ok := resultType.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Link" {
			continue
		}
		alias, ok := selector.X.(*ast.Ident)
		if ok {
			if _, isModel := modelAliases[alias.Name]; isModel {
				return true
			}
		}
	}
	return false
}

// TestDurableCommandSeamsDoNotExposeTransactions locks the RF8 seam: product
// services use application commands, while pgx transactions remain an
// implementation detail of concrete repository and durablework adapters.
func TestDurableCommandSeamsDoNotExposeTransactions(t *testing.T) {
	t.Parallel()

	repoRoot := filepath.Join("..", "..")
	var violations []string
	checkFile := func(path string, forbidPGXTx bool) {
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			t.Fatalf("relative path for %s: %v", path, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		pgxAliases := map[string]struct{}{}
		for _, imp := range file.Imports {
			pathValue, unquoteErr := strconv.Unquote(imp.Path.Value)
			if unquoteErr != nil || pathValue != "github.com/jackc/pgx/v5" {
				continue
			}
			alias := "pgx"
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			pgxAliases[alias] = struct{}{}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.SelectorExpr:
				if !forbidPGXTx || typed.Sel.Name != "Tx" {
					break
				}
				ident, ok := typed.X.(*ast.Ident)
				if ok {
					if _, isPGX := pgxAliases[ident.Name]; isPGX {
						violations = append(violations, rel+": exposes pgx.Tx")
					}
				}
			case *ast.Ident:
				if _, retired := retiredDurablePortNames[typed.Name]; retired {
					violations = append(violations, rel+": uses retired "+typed.Name)
				}
			}
			return true
		})
	}

	serviceRoot := filepath.Join(repoRoot, "internal", "service")
	if err := filepath.WalkDir(serviceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		checkFile(path, true)
		return nil
	}); err != nil {
		t.Fatalf("walk service sources: %v", err)
	}
	checkFile(filepath.Join(repoRoot, "internal", "repository", "interfaces.go"), true)

	internalRoot := filepath.Join(repoRoot, "internal")
	if err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasPrefix(path, serviceRoot+string(filepath.Separator)) || path == filepath.Join(repoRoot, "internal", "repository", "interfaces.go") {
			return nil
		}
		checkFile(path, false)
		return nil
	}); err != nil {
		t.Fatalf("walk internal sources: %v", err)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("durable command seam regressed:\n  %s", strings.Join(violations, "\n  "))
	}
}

// layer 给每个内部包一个层号。规则只有一条：**低层不得导入高层**。
//
// 层号直接编码进本测试：
//
//	5  app                             composition root，装配一切
//	4  handler / worker / middleware   入口层（HTTP 请求、后台 job、HTTP 横切）
//	3  service                         业务编排
//	2  repository / migrate            持久化及其直接使用者
//	1  database                        连接池 / 迁移
//	0  基础包                           领域模型、工具；彼此之间不设约束
//
// **未列出的包会被完全跳过**（见 checkDirection 的 `if !ok { continue }`），
// 因此漏列一个包等于对它免检。这不是理论风险：middleware 最初就没列，导致门禁
// 对 repository→middleware 完全放行。allInternalPackagesAreLayered 现在强制
// 每个内部包都必须显式定级，杜绝
// 「漏列 = 免检」。
//
// 定级要点：同层之间不设约束，所以「谁不能依赖谁」必须体现为层号差。middleware
// 一度被定为 2（与 repository 同层），导致注入 repository→middleware 时门禁依旧
// 放行——同层被视为合法。它现在是 4，与 handler 同层，repository/service 对它的
// 任何依赖都会被判为倒置。
var layer = map[string]int{
	// 6 层：cmd/* 是进程入口，位于 composition root 之上。每个 main 单独定级
	// 而非合并为一个 "cmd"——它们依赖面差异很大（webtag 只碰 app/config，
	// migrate 直连 database，eval 直连 analyzer），合并会掩盖各自的越界。
	"cmd/webtag":  6,
	"cmd/migrate": 6,
	"cmd/eval":    6,
	// release-manifest 是发布期工具（生成并签名 Release manifest），不随镜像或
	// 归档分发；Core 归档仍然只有 webtag 与 migrate 两个可执行文件。
	"cmd/release-manifest": 6,
	// cairn-updater 是 root 运行的部署 helper，与 webtag 是两个进程、两个用户。
	// 它只依赖 releasetrust（0 层信任根）与 buildinfo；迁移一律交给目标 Release
	// 自带的 migrate 二进制以子进程执行，因此这里刻意不 import internal/migrate
	// ——helper 编译进来的迁移计划永远是"旧那一版"，用它规划升级范围必然漏掉本次
	// 要执行的 step。
	"cmd/cairn-updater": 6,

	"app":     5,
	"handler": 4,
	"worker":  4,
	// eval 是 cmd/eval 专用的离线评测逻辑，依赖 service/analyzer，
	// 因此与其它入口层同级而非工具包。
	"eval": 4,
	// middleware 是 HTTP 入口层的横切能力（鉴权、限流、幂等），与 handler 同层。
	// 关键是它必须高于 repository/service：这两层反过来依赖 HTTP 中间件正是
	// 已修复过的依赖倒置。同层为 4 而非 2——同层之间不设约束，
	// 定成 2 会让 repository→middleware 被当作同层放行。
	"middleware": 4,
	"service":    3,
	"repository": 2,
	// migrate 是 cmd/migrate 的迁移源，直接使用 database，故位于其上而非 0 层。
	"migrate":  2,
	"database": 1,

	// 0 层：领域模型与工具包。显式列出而非依赖默认值，这样新增包时
	// allInternalPackagesAreLayered 会强制作者主动定级。
	"alloc":         0, // 分配容量夹取；零依赖叶子包，任何层都可 import
	"arch":          0,
	"buildinfo":     0,
	"concept":       0,
	"config":        0,
	"contentdoc":    0,
	"dto":           0,
	"embedding":     0,
	"errsafe":       0,
	"feed":          0,
	"fetcher":       0,
	"httperr":       0,
	"jsonx":         0,
	"lifecycle":     0,
	"lockkey":       0,
	"model":         0,
	"notetitle":     0,
	"observability": 0,
	// deploybackup：cairn-updater 的 pg_dump / pg_restore 驱动。抽成包而不是
	// helper 内部方法，是因为「dump 可恢复」这条判断是关于 PostgreSQL 的断言，
	// 只有拿真实容器跑真实客户端才算证明；而 test/dbintegration 那个独立 module
	// 能 import 内部包、不能 import cmd 的 main 包。它同样必须是零依赖叶子（纯
	// stdlib，不含任何数据库驱动）。
	"deploybackup": 0,
	// releasetrust：Release 信任根（canonical manifest、Ed25519 公钥集合与验证
	// 路径）。root-owned 的 cairn-updater helper 直接 import 它，因此必须是零
	// 依赖叶子——它一旦依赖任何应用层包，helper 就会被迫链接整个应用。
	"releasetrust":   0,
	"retry":          0,
	"representation": 0,
	"readertext":     0,
	"security":       0,
	// session：浏览器会话凭证的签名 / 校验原语。只依赖同层 authn
	// 身份原语，是被 middleware 和 handler 共用的叶子。
	"session":       0,
	"siteidentity":  0,
	"summarypolicy": 0,
	"textutil":      0,
	// urlidentity：URL 规范化身份的唯一实现。service 的采集入口、repository 的
	// Inbox 确认、feed 的订阅保存和 migrate 的存量回填都要用它，所以必须是零
	// 依赖叶子——它一旦上移，就会有某一层被迫绕开它自己写一套规则。
	"urlidentity": 0,
}

// skipLayerRules 记录「高层跳过中间层直接依赖更低层」的已知偏差。
//
// 与上面的方向规则不同，跳层本身不违反箭头方向（handler→repository 仍是自上而
// 下），因此不会被 TestNoLowerLayerImportsUpperLayer 捕获。新端点应经 service
// 落到 repository，跳层会让业务规则散进 HTTP 层。
//
// 这里不强制修复现存偏差——那是独立的重构决策——而是把它们钉死成清单：清单与
// 实际不符（新增或减少）都会让测试变红。新增是违规，减少是进展未同步到清单，
// 两者都需要人来处理，因此都报错而非静默通过。
var knownSkipLayer = map[string][]string{
	"handler -> repository": {
		"internal/handler/concept_merges.go",
		"internal/handler/library_rules.go",
	},
	// worker 与 handler 同为入口层，同样受「新逻辑经 service 落到 repository」
	// 的约定约束。这条规则一度缺失，直接后果是 site_payload_cleaner.go 为引用
	// repository.SQLNotReadingPredicate 新增的跳层全绿通过——门禁只钉了 handler，
	// 对 worker 整条线失效。凡是入口层都要登记，不能只登记想起来的那一层。
	"worker -> repository": {
		"internal/worker/feed_scheduler.go",
		"internal/worker/site_payload_cleaner.go",
	},
	// worker 直接持有 pgx 连接跑维护性 SQL（对账、遗留迁移、payload 清理）。
	// 这些是运维动作而非业务用例，走 service 反而要为一次性任务开接口，因此
	// 保留但登记在案。
	"worker -> database": {
		"internal/worker/parse_terminal_reconciler.go",
		"internal/worker/site_payload_cleaner.go",
		"internal/worker/translation_terminal_reconciler.go",
	},
}

type pkgInfo struct {
	ImportPath string
	Imports    []string
	// TestImports / XTestImports 是测试文件的导入。它们同样受分层约束——一个
	// _test.go 里的 repository→middleware 与生产代码里的没有本质区别，且会随
	// 重构泄漏回生产。go list 默认把三者分开，只看 Imports 会漏掉整个测试面。
	TestImports  []string
	XTestImports []string
}

// allImports 汇总生产与测试导入，供方向检查统一处理。
func (p pkgInfo) allImports() []string {
	out := make([]string, 0, len(p.Imports)+len(p.TestImports)+len(p.XTestImports))
	out = append(out, p.Imports...)
	out = append(out, p.TestImports...)
	out = append(out, p.XTestImports...)
	return out
}

// loadPackages 用 go list 读取真实导入图。刻意走 go list 而非静态解析源码：
// 它看到的是构建约束求值后的结果，与编译器一致。
func loadPackages(t *testing.T) []pkgInfo {
	t.Helper()

	// 先把被检查的源文件读一遍，把它们纳入 Go test cache 的输入指纹。
	//
	// 这不是多余动作：导入图是通过 exec 子进程（go list）获取的，而子进程读了
	// 什么文件不在缓存指纹里。没有这一步，改动其它包的 import 不会让本测试的
	// 缓存失效——`go test ./...`（无 -count=1）会在一棵有倒置的树上直接复用旧的
	// 绿结果。CI 用了 -count=1 所以幸免，但 make test / make race / make verify
	// 没有，本地 PR 前门禁会静默放行。
	fingerprintSources(t)

	// ./... 从仓库根跑，因此同时覆盖 internal/ 与 cmd/。
	// The import graph does not depend on VCS metadata. Disabling VCS stamping
	// also keeps parallel architecture checks from racing over Git's index.
	cmd := exec.Command("go", "list", "-buildvcs=false", "-json", "./...")
	cmd.Dir = "../.."
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// 带上 stderr：分层倒置若同时构成导入循环，go list 会在这里就失败，
		// 而循环信息只出现在 stderr。吞掉它会让排查从「读一行报错」变成
		// 「手动复现命令」。
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}

	var pkgs []pkgInfo
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkgInfo
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		pkgs = append(pkgs, p)
	}
	if len(pkgs) == 0 {
		t.Fatal("go list 返回空包列表")
	}
	return pkgs
}

// internalName 把包路径归一化为层表的 key：
//
//	webtag/internal/service/analyzer → "service"     子包与父包同层
//	webtag/cmd/webtag                → "cmd/webtag"  每个 main 单独定级
//
// 非本仓包返回空串。
//
// cmd 一度不在此列（只认 internal/ 前缀），于是整层零检查——给 cmd/webtag 加一条
// 直连 repository 的导入，门禁全绿。那正是前几轮反复修的「漏列 = 免检」，只是
// 上移了一层。
func internalName(importPath string) string {
	if rest, ok := strings.CutPrefix(importPath, "webtag/internal/"); ok {
		if idx := strings.Index(rest, "/"); idx >= 0 {
			rest = rest[:idx]
		}
		return rest
	}
	if rest, ok := strings.CutPrefix(importPath, "webtag/cmd/"); ok {
		if idx := strings.Index(rest, "/"); idx >= 0 {
			rest = rest[:idx]
		}
		return "cmd/" + rest
	}
	return ""
}

// TestNoLowerLayerImportsUpperLayer 保证分层依赖箭头严格单向。
//
// 本仓库修复过的三处真实倒置，现均在本测试覆盖范围内（经反向注入逐条验证）：
//   - internal/service → internal/handler（service 返回 handler 定义的响应类型）
//   - internal/repository → internal/middleware（持久化层取租户身份）
//   - internal/service → internal/middleware（同上，随租户包抽离一并解决）
//
// 后两条一度不在覆盖范围内：middleware 未在 layer 表中定级，未定级的包会被
// 静默跳过。TestAllInternalPackagesAreLayered 现在杜绝了这种「漏列即免检」。
func TestNoLowerLayerImportsUpperLayer(t *testing.T) {
	t.Parallel()

	var violations []string
	for _, pkg := range loadPackages(t) {
		from := internalName(pkg.ImportPath)
		if from == "" {
			continue
		}
		fromLayer, ok := layer[from]
		if !ok {
			continue
		}
		for _, imp := range pkg.allImports() {
			to := internalName(imp)
			if to == "" || to == from {
				continue
			}
			toLayer, ok := layer[to]
			if !ok {
				continue
			}
			if fromLayer < toLayer {
				violations = append(violations,
					pkg.ImportPath+" (层 "+strconv.Itoa(fromLayer)+") 导入了 "+imp+" (层 "+strconv.Itoa(toLayer)+")")
			}
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("检测到 %d 处分层倒置——低层包不得导入高层包：\n  %s\n\n"+
			"修法通常是把被共享的类型下沉到基础包（如 internal/dto、internal/model、"+
			"internal/model），而不是放宽本规则。",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestAllInternalPackagesAreLayered 保证 layer 表覆盖每一个内部包。
//
// 方向检查对未定级的包直接 continue，所以漏列一个包 = 对它免检。这条元测试把
// 「漏列」从静默免检变成显式失败：新增内部包时必须主动定级，而不是默认获得豁免。
//
// 它的来历：middleware 最初不在表中，门禁因此对 repository→middleware 完全放行，
// 而那正是一次已修复的分层倒置——门禁号称守住的东西，恰恰守不住。
func TestAllInternalPackagesAreLayered(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	var missing []string
	for _, pkg := range loadPackages(t) {
		name := internalName(pkg.ImportPath)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if _, ok := layer[name]; !ok {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("以下包未在 layer 表中定级，方向检查会静默跳过它们：\n  %s\n\n"+
			"请在 arch_test.go 的 layer 表中为每个包显式指定层号（叶子工具包用 0）。"+
			"漏列等于免检——这正是 middleware 曾让 repository→middleware 蒙混过关的原因。",
			strings.Join(missing, "\n  "))
	}
}

// skipLayerExempt 是**不视为跳层**的包对。
//
// app 是 composition root：它的职责就是把各层拼起来，直连任何层都是本分，不算
// 偏差。cmd/* 同理——每个 main 只做「读环境、装配、阻塞」，为一次性任务经
// service 中转反而是过度设计。
//
// 写成显式豁免而非靠遗漏：否则「没登记」既可能表示「刻意放行」，也可能表示
// 「忘了」，两者不该长得一样。豁免只给进程/装配入口，业务层一律走 knownSkipLayer。
var skipLayerExempt = map[string]bool{
	"app -> repository": true,
	"app -> database":   true,
	"app -> migrate":    true,
	"app -> service":    true,

	// cmd/eval 是离线评测入口，直接驱动 analyzer 与 eval 打分逻辑。
	"cmd/eval -> eval":    true,
	"cmd/eval -> service": true,
	// cmd/migrate 是迁移执行器，直连迁移源与连接池。
	"cmd/migrate -> database": true,
	"cmd/migrate -> migrate":  true,
	// TODO 投影 backfill 要解析 Markdown 清单，无法写成 SQL step，只能是 Go。
	// 它必须在服务开始只读投影之前跑完，因此挂在部署期的 migrate 入口上，与
	// 其他 step 一样是「跑完即退出」的一次性任务。
	"cmd/migrate -> repository": true,
	// cmd/release-manifest 在发布期读取本次发布编译进来的迁移计划，把精确的
	// schema target 与 online-update 分类写进签名 manifest。它必须直读迁移源：
	// 经 service 中转会让「manifest 里的目标」与「二进制真正会执行的目标」变成
	// 两处可漂移的事实。
	"cmd/release-manifest -> migrate": true,
}

// TestEverySkipLayerPairIsRegistered 保证 knownSkipLayer 覆盖每一个真实存在的
// 跳层包对。
//
// 这条元测试的必要性与 TestAllInternalPackagesAreLayered 完全同构：
// TestSkipLayerImportsDoNotGrow 只遍历 knownSkipLayer 的 key，未登记的包对等于
// 零检查。区别在于 layer 表的「漏列即免检」已被元测试堵住，跳层规则却一直靠人工
// 列举——第一版只登记了 handler → repository，于是 worker → repository 新增时
// 全绿通过；补上 worker → repository 之后，worker → database、
// middleware → repository 等依然裸奔。
//
// 靠人工列举的清单会一直漏，所以这里改成自动枚举：凡 layer 差 ≥ 2 且目标层非
// 基础包的真实导入，要么登记进 knownSkipLayer，要么写进 skipLayerExempt，二选一。
//
// 口径提醒：本测试用 allImports()（含测试导入），而 filesImporting 只看生产
// 文件。因此若某个跳层包对**只**出现在 _test.go 里，本测试会要求登记，而
// TestSkipLayerImportsDoNotGrow 扫不到任何文件——此时把该规则登记成空
// []string{} 即可，两边都满足。这不是矛盾：测试代码直连仓储是正常的，不该进
// 「哪些生产文件绕过了 service」的清单，但它同样需要被看见一次。
func TestEverySkipLayerPairIsRegistered(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	var unregistered []string
	for _, pkg := range loadPackages(t) {
		from := internalName(pkg.ImportPath)
		fromLayer, ok := layer[from]
		if !ok {
			continue
		}
		for _, imp := range pkg.allImports() {
			to := internalName(imp)
			if to == "" || to == from {
				continue
			}
			toLayer, ok := layer[to]
			// toLayer == 0 的是叶子工具包，人人可用，跳层概念不适用。
			if !ok || toLayer == 0 {
				continue
			}
			// 只看自上而下且跨过了至少一整层的依赖。
			if fromLayer-toLayer < 2 {
				continue
			}
			rule := from + " -> " + to
			if seen[rule] || skipLayerExempt[rule] {
				continue
			}
			seen[rule] = true
			if _, ok := knownSkipLayer[rule]; !ok {
				unregistered = append(unregistered, rule)
			}
		}
	}

	if len(unregistered) > 0 {
		sort.Strings(unregistered)
		t.Fatalf("以下跳层包对既未登记进 knownSkipLayer，也不在 skipLayerExempt 中：\n  %s\n\n"+
			"跳层会让业务规则散进入口层。要么把涉及的文件登记进 knownSkipLayer 并写明原因，"+
			"要么在 skipLayerExempt 中显式豁免（如 composition root）。"+
			"「没登记」不应既表示刻意放行、又表示忘了登记。",
			strings.Join(unregistered, "\n  "))
	}
}

// TestSkipLayerImportsDoNotGrow 锁定已知的跳层偏差清单。
//
// 清单收缩（修好了）会提示更新清单；清单扩张（新增跳层）直接失败。
func TestSkipLayerImportsDoNotGrow(t *testing.T) {
	t.Parallel()

	for rule, want := range knownSkipLayer {
		parts := strings.Split(rule, " -> ")
		if len(parts) != 2 {
			t.Fatalf("规则格式错误: %q", rule)
		}
		fromPkg, toPkg := parts[0], parts[1]

		got := filesImporting(t, "internal/"+fromPkg, "webtag/internal/"+toPkg)
		sort.Strings(got)
		wantSorted := append([]string(nil), want...)
		sort.Strings(wantSorted)

		if strings.Join(got, "\n") == strings.Join(wantSorted, "\n") {
			continue
		}

		added, removed := diffStrings(wantSorted, got)
		if len(added) > 0 {
			t.Errorf("%s 出现新的跳层依赖：\n  %s\n\n"+
				"新端点应经 service 落到 repository。"+
				"确有理由跳层时，把文件加进 knownSkipLayer 并写明原因。",
				rule, strings.Join(added, "\n  "))
		}
		if len(removed) > 0 {
			t.Errorf("%s 的跳层依赖已减少：\n  %s\n\n请从 knownSkipLayer 中移除这些条目，锁住这份进展。",
				rule, strings.Join(removed, "\n  "))
		}
	}
}

// filesImporting 返回 dir 下（非测试）导入 target 的文件相对路径。
//
// 用 go/parser 而非 exec grep：只解析 import 块，不会被注释或字符串里出现的
// 包路径误伤，也不依赖环境里 grep 的具体实现与参数方言。
//
// 口径说明：这里**排除** _test.go，与方向检查（allImports 显式纳入测试导入）
// 不同。跳层清单钉的是「生产代码里哪些文件绕过了 service」，测试为了构造场景
// 直连仓储是正常的，纳进来只会让清单充满噪音。方向倒置则不同——测试里的倒置
// 同样会随重构泄漏回生产，所以那边必须查。
func filesImporting(t *testing.T, dir, target string) []string {
	t.Helper()

	absDir := filepath.Join("../..", dir)
	fset := token.NewFileSet()
	var files []string

	// 递归下降到子包。只读顶层目录会留出一块免检区：对**已登记**的包对，在其子包
	// 里新增跳层导入时，EverySkipLayerPairIsRegistered 因规则已登记而放行，
	// SkipLayerImportsDoNotGrow 又扫不到文件、认为清单没变，两道门一起失效。
	err := filepath.WalkDir(absDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			// 跳过 Go 工具链本就忽略的目录：testdata 里可能放语法不完整的夹具，
			// 解析失败会让本测试误报。
			if name == "testdata" || strings.HasPrefix(name, "_") || (name != "." && strings.HasPrefix(name, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, imp := range file.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if p == target {
				rel, rerr := filepath.Rel("../..", path)
				if rerr != nil {
					return rerr
				}
				files = append(files, filepath.ToSlash(rel))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", absDir, err)
	}
	return files
}

func diffStrings(want, got []string) (added, removed []string) {
	inWant := make(map[string]bool, len(want))
	for _, w := range want {
		inWant[w] = true
	}
	inGot := make(map[string]bool, len(got))
	for _, g := range got {
		inGot[g] = true
		if !inWant[g] {
			added = append(added, g)
		}
	}
	for _, w := range want {
		if !inGot[w] {
			removed = append(removed, w)
		}
	}
	return added, removed
}

// fingerprintSources 读取 internal/ 与 cmd/ 下所有 .go 文件，只为让 Go test cache
// 把它们纳入本测试的输入指纹。
//
// 内容本身不做任何解析——导入图仍由 go list 提供。这里要的只是「文件变了，缓存
// 就失效」这个副作用；否则经 exec 获取的导入图对缓存完全透明，一棵有倒置的树会
// 复用之前的绿结果。
func fingerprintSources(t *testing.T) {
	t.Helper()

	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			if _, err := os.ReadFile(path); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			t.Fatalf("fingerprint %s: %v", root, err)
		}
	}
}

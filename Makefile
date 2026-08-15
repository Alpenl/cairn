.PHONY: help build build-full reader-build reader-clean run migrate migrate-fresh test test-integration test-dbintegration test-dbintegration-required test-no-skip arch-contract ci-contracts version-test core-legal-check core-release-test vet tools lint actionlint fmt tidy modules-verify vuln race fuzz-smoke bench frontend-verify docker-build db-migrate schema-dump schema-check container-smoke gate verify clean

GO ?= go
BIN_DIR ?= bin
BIN ?= $(BIN_DIR)/webtag
READER_EMBED_DIR ?= internal/app/assets/reader
PNPM ?= corepack pnpm@10.13.1
SCHEMA_PG_IMAGE ?= pgvector/pgvector:pg16
TOOLS_BIN_DIR ?= $(BIN_DIR)/tools
FUZZ_TIME ?= 2s
VERSION ?= $(shell sh scripts/version.sh)
COMMIT ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GOLANGCI_LINT_VERSION ?= v2.12.0
GOVULNCHECK_VERSION ?= v1.3.0
ACTIONLINT_VERSION ?= v1.7.7
# actionlint 只有在 PATH 上能找到 shellcheck 时才检查 run: 块里的 shell。
# 本地缺它 = 本地绿、CI 红：SC2129 与 SC2012 都是这么漏到 CI 才暴露的。
# 固定版本下载到 bin/tools，让本地与 runner 检查同一套规则。
SHELLCHECK_VERSION ?= v0.10.0
YTDLP_BASE_URL ?= https://github.com/yt-dlp/yt-dlp/releases/download
GOLANGCI_LINT := $(TOOLS_BIN_DIR)/golangci-lint
GOVULNCHECK := $(TOOLS_BIN_DIR)/govulncheck
ACTIONLINT := $(TOOLS_BIN_DIR)/actionlint
SHELLCHECK := $(TOOLS_BIN_DIR)/shellcheck
GOLANGCI_LINT_STAMP := $(TOOLS_BIN_DIR)/golangci-lint.$(GOLANGCI_LINT_VERSION).stamp
GOVULNCHECK_STAMP := $(TOOLS_BIN_DIR)/govulncheck.$(GOVULNCHECK_VERSION).stamp
ACTIONLINT_STAMP := $(TOOLS_BIN_DIR)/actionlint.$(ACTIONLINT_VERSION).stamp
SHELLCHECK_STAMP := $(TOOLS_BIN_DIR)/shellcheck.$(SHELLCHECK_VERSION).stamp
define ACTION_PIN_CHECK
import re, glob, collections, sys
pins = collections.defaultdict(set)
for path in glob.glob('.github/workflows/*.yml'):
    text = open(path, encoding='utf-8').read()
    for action, sha, ver in re.findall(r'uses:\s+([\w./-]+)@([a-f0-9]{40})\s+#\s+(v[\d.]+)', text):
        pins[(action, ver)].add(sha)
bad = {k: v for k, v in pins.items() if len(v) > 1}
for (action, ver), shas in sorted(bad.items()):
    print(f'action pin mismatch: {action} {ver} -> {sorted(shas)}', file=sys.stderr)
if bad:
    sys.exit(1)
print(f'action pins: {len(pins)} action@version combinations are self-consistent')
endef
export ACTION_PIN_CHECK

LDFLAGS := -s -w -X webtag/internal/buildinfo.Version=$(VERSION) -X webtag/internal/buildinfo.Commit=$(COMMIT) -X webtag/internal/buildinfo.BuildTime=$(BUILD_TIME)

# `make` / `make help` prints every target that carries a `## description`
# trailing comment. Keep the comments short — they have to fit one line.
.DEFAULT_GOAL := help

help: ## 显示所有 make target 的说明
	@awk 'BEGIN {FS = ":.*?## "; printf "Usage:\n  make <target>\n\nTargets:\n"} \
		/^[a-zA-Z0-9_-]+:.*?## / { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## 静态构建 webtag 到 bin/webtag（不重建 Reader，用当前已注入的产物）
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -tags=nomsgpack,sonic -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/webtag

build-full: reader-build build ## 先构建 Reader 再编译，产出带前端的完整二进制

# Reader 产物必须在 go build 之前落到 $(READER_EMBED_DIR)：internal/app/reader.go
# 用 //go:embed 在编译期把它读进二进制，编译之后再拷是没有用的。
# 目录整体 gitignore（只留 .gitkeep），所以先清空再拷，避免上一次构建的
# 陈旧哈希文件永久堆积在里面被一起嵌进去。
# 只在 Reader 依赖或共享 workspace link 缺失时从根 lockfile 安装。Reader 与
# @webtag/api 必须来自同一冻结图；只检查 Reader 自己的 node_modules 会漏掉共享包。
reader-build: ## 构建 Reader 前端并把产物注入 internal/app/assets/reader
	@test -d reader/node_modules && test -e reader/node_modules/@webtag/api || $(PNPM) install --frozen-lockfile
	$(PNPM) --filter webtag-reader build
	find $(READER_EMBED_DIR) -mindepth 1 ! -name .gitkeep -delete
	cp -R reader/dist/. $(READER_EMBED_DIR)/

reader-clean: ## 清空已注入的 Reader 产物（回到「尚未构建」形态）
	find $(READER_EMBED_DIR) -mindepth 1 ! -name .gitkeep -delete

run: ## 直接以 `go run` 启动 webtag 服务
	$(GO) run ./cmd/webtag

migrate: ## 应用数据库迁移（cmd/migrate）
	$(GO) run ./cmd/migrate

migrate-fresh: ## 为已知空库显式运行单安装迁移计划
	MIGRATION_TARGET=fresh $(GO) run ./cmd/migrate

test: ## 跑全量单元 + 轻量集成测试
	$(GO) test ./...

version-test: ## 在隔离 Git 仓库中验证发布版本推导规则
	bash scripts/version.test.sh

core-legal-check: ## 重建并校验 Core 法律材料与冻结生产依赖闭包一致
	$(PNPM) install --frozen-lockfile
	node scripts/core-legal.mjs check

core-release-test: core-legal-check ## 验证法律材料、draft 与 digest promotion 合同
	$(GO) test ./scripts
	bash scripts/core-legal.test.sh
	bash scripts/core-release-build.test.sh
	bash scripts/core-release-verify.test.sh
	bash scripts/core-release-promote.test.sh

# Live fetcher integration tests — hits real ArXiv / GitHub / DuckDuckGo /
# Jina / YouTube. Network required. yt-dlp resolved via WEBTAG_TEST_YTDLP_BIN
# (falls back to `yt-dlp` on PATH). Set WEBTAG_TEST_GITHUB_TOKEN to avoid
# anonymous rate limiting. Slow on purpose; not part of `make verify`.
test-integration: ## 对真实上游跑 fetcher 集成测试（需要网络）
	$(GO) test -tags=integration -timeout=180s -v ./internal/fetcher/...

# Repository DB integration tests — boot a real Postgres container via
# testcontainers-go, run the production migration set, and exercise the
# pg-specific behaviours pgxmock cannot model (real ON CONFLICT, FK
# cascades, text encoding round-trip, migration drift). Requires a
# working Docker daemon. It is part of `make verify`, but not the fast
# offline `make gate`.
#
# The suite lives in an independent Go module under test/dbintegration/
# so testcontainers-go's ~40 transitive Docker / logrus dependencies
# never enter the production go.mod. The `go test -C <dir>` form
# (Go 1.20+) runs as if cwd were the sub-module without touching the
# parent shell's cwd.
# The dbintegration tag exposes a narrow production-config bridge used only by
# the real pgxpool shutdown fault test; normal application builds omit it.
test-dbintegration: ## 用 testcontainers-go 跑真实 Postgres 集成测试（需要 Docker）
	$(GO) test -C test/dbintegration -tags=dbintegration -count=1 -timeout=300s -v ./...

test-dbintegration-required: ## 必须成功启动真实 Postgres，禁止把基础设施失败记成 skip
	WEBTAG_DBINTEGRATION_REQUIRED=true $(GO) test -C test/dbintegration -tags=dbintegration -count=1 -timeout=300s -v ./...
	bash scripts/migrate-dbintegration.sh

vet: ## go vet 全量包（含 test/dbintegration 独立 module）
	$(GO) vet ./...
# test/dbintegration 是独立 module，根 module 的 `go vet ./...` 完全不覆盖它
# （`go list ./...` 在那里返回 0 个包）。此前该 module 唯一的门禁是需要 Docker
# 的 test-dbintegration，于是编译级漂移只能等到重测试才暴露——而那条 gate 曾
# 长期被 `| tee` 吞掉退出码。这一行是秒级的，放在 vet 里保证 PR 就能拦下。
	$(GO) vet -C test/dbintegration -tags=dbintegration ./...

tools: $(GOLANGCI_LINT_STAMP) $(GOVULNCHECK_STAMP) $(ACTIONLINT_STAMP) $(SHELLCHECK_STAMP) ## 安装项目本地质量工具到 bin/tools/

$(TOOLS_BIN_DIR):
	mkdir -p $(TOOLS_BIN_DIR)

$(GOLANGCI_LINT_STAMP): | $(TOOLS_BIN_DIR)
	rm -f $(GOLANGCI_LINT) $(TOOLS_BIN_DIR)/golangci-lint.*.stamp
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	touch $@

$(GOVULNCHECK_STAMP): | $(TOOLS_BIN_DIR)
	rm -f $(GOVULNCHECK) $(TOOLS_BIN_DIR)/govulncheck.*.stamp
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) $(GO) install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	touch $@

$(ACTIONLINT_STAMP): | $(TOOLS_BIN_DIR)
	rm -f $(ACTIONLINT) $(TOOLS_BIN_DIR)/actionlint.*.stamp
	GOBIN=$(abspath $(TOOLS_BIN_DIR)) $(GO) install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)
	touch $@

$(SHELLCHECK_STAMP): | $(TOOLS_BIN_DIR)
	rm -f $(SHELLCHECK) $(TOOLS_BIN_DIR)/shellcheck.*.stamp
	@set -eu; \
	arch=$$(uname -m); \
	case "$$arch" in x86_64) asset=x86_64 ;; aarch64|arm64) asset=aarch64 ;; \
		*) echo "shellcheck: 不支持的架构 $$arch，跳过安装" >&2; touch $@; exit 0 ;; esac; \
	tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	url="https://github.com/koalaman/shellcheck/releases/download/$(SHELLCHECK_VERSION)/shellcheck-$(SHELLCHECK_VERSION).linux.$$asset.tar.xz"; \
	curl -fsSL "$$url" -o "$$tmp/sc.tar.xz"; \
	tar -xJf "$$tmp/sc.tar.xz" -C "$$tmp"; \
	install -m 0755 "$$tmp/shellcheck-$(SHELLCHECK_VERSION)/shellcheck" $(SHELLCHECK)
	touch $@

lint: $(GOLANGCI_LINT_STAMP) ## 跑项目内固定版本的 golangci-lint
	$(GOLANGCI_LINT) run --timeout=5m

actionlint: $(ACTIONLINT_STAMP) $(SHELLCHECK_STAMP) ## 校验所有 GitHub Actions workflow 语法与表达式
	PATH="$(abspath $(TOOLS_BIN_DIR)):$$PATH" $(ACTIONLINT)
	@# 同一个 action@版本 在不同 workflow 里必须钉同一个 SHA。actionlint 只校验
	@# 语法，不核对 SHA 与版本注释是否对应——release-extension 曾把
	@# docker/setup-qemu-action 的 SHA 贴到 pnpm/action-setup 上，注释还写着 v6.0.10，
	@# 一路过了 lint，直到 GitHub 拒绝调度才暴露。
	@python3 -c "$$ACTION_PIN_CHECK"
	@awk '\
		/^[[:space:]]*uses:[[:space:]]+/ { \
			action = $$0; \
			sub(/^[[:space:]]*uses:[[:space:]]+/, "", action); \
			sub(/[[:space:]#].*$$/, "", action); \
			if (action ~ /^\.\//) next; \
			ref = action; \
			sub(/^.*@/, "", ref); \
			if (action !~ /@/ || length(ref) != 40 || ref ~ /[^0-9a-f]/) { \
				print FILENAME ":" FNR ": action is not pinned to a full commit SHA: " $$0 > "/dev/stderr"; \
				bad = 1; \
			} \
		} \
		END { exit bad }' .github/workflows/*.yml

fmt: ## 用 gofmt 格式化所有 Go 源码（不动 vendor/）
	@find . -path ./vendor -prune -o -name '*.go' -type f -print | xargs $(GO) fmt

tidy: ## 整理 go.mod 并重建 vendor/
	$(GO) mod tidy
	$(GO) mod vendor

modules-verify: ## 检查两个 Go module 无 tidy 漂移且依赖校验和完整
	$(GO) mod tidy -diff
	$(GO) -C test/dbintegration mod tidy -diff
	$(GO) mod verify

vuln: $(GOVULNCHECK_STAMP) ## 用项目内固定版本的 govulncheck 扫描已知漏洞
	$(GOVULNCHECK) ./...

race: ## 跑 -race 检测竞态
	$(GO) test -race ./...

fuzz-smoke: ## 对 analyzer 解析器做 2 秒短 fuzz（FUZZ_TIME 可覆盖）
	$(GO) test ./internal/service/analyzer -run=^$$ -fuzz=FuzzParseAnalysisResponse -fuzztime=$(FUZZ_TIME)
	$(GO) test ./internal/service/analyzer -run=^$$ -fuzz=FuzzDecodeAnalyzerMessageContent -fuzztime=$(FUZZ_TIME)

frontend-verify: ## 安装冻结 workspace 并运行共享契约、Reader、Extension 全门禁
	$(PNPM) install --frozen-lockfile
	$(PNPM) verify

# 热路径 benchmark：concept resolver / analyzer parser / link DTO 映射 /
# light fetcher 解析。开发者本地按需跑，留 baseline 用 benchstat 对比，
# 不接入 CI（跑 PR 太重，benchstat 也需要稳定环境才有意义）。
# 用法：保存 `-count` 输出作为 baseline，再用 benchstat 比较同环境结果。
bench: ## 跑热路径 benchmark（concept / analyzer / service / fetcher 各热路径）
	$(GO) test -bench=. -benchmem -count=3 -run='^$$' \
		./internal/concept/... \
		./internal/service/analyzer/... \
		./internal/service/ \
		./internal/fetcher/...

docker-build: ## 构建 slim + full 两种容器镜像
	docker build \
		--target=slim \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t webtag:$(VERSION)-slim \
		-t webtag:slim .
	docker build \
		--target=full \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg YTDLP_BASE_URL=$(YTDLP_BASE_URL) \
		-t webtag:$(VERSION) \
		-t webtag:latest \
		-t webtag:full .

db-migrate: ## 在临时 Postgres 容器里跑迁移冒烟
	sh scripts/db_migrate_smoke.sh

# 快速看当前 schema 全貌：起一次性 postgres、跑迁移、pg_dump 到
# internal/migrate/schema.sql。schema.sql 是生成产物，不是 source of truth，
# 每次新增/修改 migration step 后跑一次并把 diff 一起 commit。
schema-dump: ## 重新生成 internal/migrate/schema.sql（需要 Docker）
	@./scripts/db-dump-schema.sh

# Rebuild the generated snapshot into an untracked temporary file and compare
# bytes. This must never use the tracked snapshot as the dump destination:
# a failed or interrupted dump must not make the working tree look clean.
schema-check: ## 机械检查迁移后的真实 schema 与 tracked snapshot 一致（需要 Docker）
	@tmp=$$(mktemp); \
	cleanup() { rm -f "$$tmp"; }; \
	trap cleanup EXIT; \
	PG_IMAGE="$${PG_IMAGE:-$(SCHEMA_PG_IMAGE)}" OUT_FILE="$$tmp" ./scripts/db-dump-schema.sh; \
	if ! cmp -s "$$tmp" internal/migrate/schema.sql; then \
		echo "schema drift detected: internal/migrate/schema.sql is stale" >&2; \
		diff -u internal/migrate/schema.sql "$$tmp" || true; \
		exit 1; \
	fi; \
	echo "schema-check: internal/migrate/schema.sql matches generated schema"

container-smoke: ## 启动完整镜像跑端到端容器冒烟
	VERSION=$(VERSION) COMMIT=$(COMMIT) sh scripts/container_smoke.sh

# test-no-skip：任何在门禁上被 skip 的 Go 测试都判红。
#
# 一条恒 skip 的测试不是通过的测试。`if !ReaderBuilt() { t.Skip }` 会让未注入
# Reader 产物的门禁永远跳过测试，却仍被记成「已覆盖」；本 target 把它变成机械
# 判据。
#
# 需要**有意** skip 的（如 Docker 不可用的 dbintegration）不在本 target 范围内
# ——它跑的是根 module，dbintegration 是独立 module。
#
# 两个已知边界，写在这里而不是让人自己发现：
#
#  1. `go test` 跑不干净（编译错误、测试红）时本 target 直接上抛它的退出码。
#     初版把 go test 的退出码丢在管道里、又用 2>/dev/null 吞掉编译错误，于是
#     「测试全红」会被报成「没有 skip，通过」——一个跑不起来就报通过的门禁，
#     比没有门禁更糟。
#  2. 它只认 t.Skip。**条件 return 穿得过去**：
#     `if !readerBuilt() { …; return }` 这种写法在门禁上同样从不执行另一半，
#     但不产生 skip 事件。这类只有变异测试能抓，且变异点不能由写代码的人挑。
test-no-skip: ## 断言没有测试在门禁上被 skip
	@out=$$(mktemp); \
	$(GO) test ./... -json >"$$out" 2>&1; \
	rc=$$?; \
	if [ $$rc -ne 0 ]; then \
		echo "go test 未能干净跑完（退出码 $$rc）：无法判断 skip 情况。" >&2; \
		echo "先修好测试本身——一个『跑不起来就报通过』的门禁比没有门禁更糟。" >&2; \
		tail -20 "$$out" >&2; rm -f "$$out"; exit $$rc; \
	fi; \
	skipped=$$(grep '"Action":"skip"' "$$out" \
		| grep -o '"Test":"[^"]*"' \
		| sed 's/"Test":"//;s/"//' | sort -u); \
	rm -f "$$out"; \
	if [ -n "$$skipped" ]; then \
		echo "以下测试在门禁上被 skip：" >&2; \
		echo "$$skipped" | sed 's/^/  /' >&2; \
		echo "恒 skip 的测试不是通过的测试。要么让它在门禁上能跑，要么删掉——不要记成「已覆盖」。" >&2; \
		exit 1; \
	fi

# Architecture contracts can consult subprocesses (go list and git ls-files),
# whose inputs are outside Go's test-cache fingerprint. Force a fresh run in
# the gate so a staged forbidden path or changed import graph cannot reuse a
# previously green result.
arch-contract: ## 不使用 Go test cache 运行架构与 tracked-tree 合同
	$(GO) test -count=1 ./internal/arch

ci-contracts: ## 离线验证 CI 诊断与 main ruleset 策略工具
	node --test scripts/ci-run-diagnose.test.mjs scripts/validate-main-ruleset.test.mjs

# gate 是快速、离线的 Go 与发布版本门禁；verify 才是全仓门禁。
#
# 它把每次 Go 改动都必须执行的 vet/lint/test/no-skip 固定下来；此前靠人挑命令时
# 漏掉的正好是 lint，而 unused 检查器正是「函数存在、没人读」的机械探测器。
#
# 不含需要 Docker / 网络的项（docker-build、db-migrate、container-smoke、
# required dbintegration、前端审计）——那些在 verify 里。真实外部网站的
# test-integration 仍是人工诊断项，不属于确定性的 PR 门禁。
gate: vet lint test test-no-skip arch-contract ci-contracts version-test ## 快速离线门禁：Go 检查 + 版本推导

verify: schema-check gate modules-verify vuln race fuzz-smoke actionlint frontend-verify core-release-test test-dbintegration-required build docker-build db-migrate container-smoke ## 本地 PR 前的全仓聚合门禁

clean: ## 清理构建产物（bin/ 与根目录残留二进制）
	rm -rf $(BIN_DIR)
	rm -f ./webtag

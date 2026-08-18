# ---------------------------------------------------------------------------
# reader-builder — 构建 Reader 前端（reader/，Vite + React）。
#
# 产物在下一阶段被 //go:embed 收进 Go 二进制，所以必须先于 go build 完成。
# 前端构建与架构无关，固定跑在构建机原生架构上（--platform=$BUILDPLATFORM），
# 多架构构建时不会被 QEMU 拖慢，也不会为每个架构重复跑一遍 Node。
#
# 依赖安装单独一层（只 COPY workspace manifest + lockfile）：改前端源码不会让 pnpm
# install 层失效，增量构建省掉整个装包过程。
# ---------------------------------------------------------------------------
ARG SOURCE_DATE_EPOCH
FROM --platform=$BUILDPLATFORM node:22.22.2-alpine AS reader-builder

WORKDIR /workspace

# corepack 按根 package.json 的 packageManager 字段取到固定版 pnpm，
# 不受构建机本地 pnpm 版本影响。
RUN corepack enable

COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
COPY reader/package.json ./reader/package.json
COPY packages/webtag-api/package.json ./packages/webtag-api/package.json
RUN pnpm install --frozen-lockfile --filter webtag-reader...

COPY packages/webtag-api/ ./packages/webtag-api/
COPY reader/ ./reader/
RUN pnpm --filter webtag-reader build

# --platform=$BUILDPLATFORM：builder 永远跑在构建机原生架构上，目标架构
# 经 GOARCH=$TARGETARCH 纯交叉编译产出（CGO 已关）——多架构构建时避免
# 整个 Go 编译在 QEMU 模拟下慢一个数量级；runtime 阶段仍按目标平台运行
# （仅 apk add 等轻量步骤走模拟）。
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS builder

WORKDIR /src

ARG VERSION=0.0.0
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG RELEASE_BUILD=false
ARG SOURCE_STATE=unknown
# 必须是裸 ARG（不带默认值）：TARGETARCH 是 BuildKit 预定义参数，写
# `=amd64` 默认值会覆盖注入、让 arm64 构建静默产出 amd64 二进制。
# 裸 ARG 在 buildx 下解析为目标架构，普通 docker build 下解析为宿主
# 架构——两条路径都正确，无需手动兜底。
ARG TARGETARCH

RUN if [ "${RELEASE_BUILD}" = "true" ]; then \
		printf '%s\n' "${VERSION}" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' || { echo "formal VERSION must be X.Y.Z" >&2; exit 1; }; \
		case "${VERSION}" in 0.0.0|*unknown*|*dirty*|*+*) echo "invalid formal VERSION: ${VERSION}" >&2; exit 1 ;; esac; \
		case "${COMMIT}" in *[!0-9a-f]*|'') echo "invalid formal COMMIT: ${COMMIT}" >&2; exit 1 ;; esac; \
		[ "${#COMMIT}" -eq 40 ] || { echo "formal COMMIT must be a full 40-character revision" >&2; exit 1; }; \
		printf '%s\n' "${BUILD_TIME}" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})$' || { echo "invalid formal BUILD_TIME" >&2; exit 1; }; \
		[ "${SOURCE_STATE}" = "clean" ] || { echo "formal SOURCE_STATE must be clean" >&2; exit 1; }; \
	fi

COPY go.mod go.sum ./
COPY vendor ./vendor

COPY cmd ./cmd
COPY internal ./internal

# Reader 产物注入 //go:embed 的挂载点。必须排在 `COPY internal` 之后——反过来
# 会被整目录覆盖掉，产出一个「Reader 尚未构建」的镜像，而且不报任何错。
# 目标目录已由上一行带来 .gitkeep，这里是往里合并，不是替换。
COPY --from=reader-builder /workspace/reader/dist ./internal/app/assets/reader

# webtag 主服务 + migrate 迁移工具一起出二进制：服务器部署用
# `docker run --entrypoint /app/migrate <image> up` 跑迁移，
# 不再依赖源码环境（scripts/db_migrate_smoke.sh 的 go run 路径仅限开发机）。
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -mod=vendor -trimpath -tags=nomsgpack,sonic \
	-ldflags "-s -w -X webtag/internal/buildinfo.Version=${VERSION} -X webtag/internal/buildinfo.Commit=${COMMIT} -X webtag/internal/buildinfo.BuildTime=${BUILD_TIME}" \
	-o /out/webtag ./cmd/webtag \
	&& CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -mod=vendor -trimpath \
	-ldflags "-s -w -X webtag/internal/buildinfo.Version=${VERSION} -X webtag/internal/buildinfo.Commit=${COMMIT} -X webtag/internal/buildinfo.BuildTime=${BUILD_TIME}" \
	-o /out/migrate ./cmd/migrate

# ---------------------------------------------------------------------------
# slim — the published runtime. yt-dlp is not bundled and is not part of
# Core release images. `docker build .` (no --target) lands here.
# ---------------------------------------------------------------------------
FROM alpine:3.24 AS slim

ARG VERSION=0.0.0
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
ARG SOURCE_STATE=unknown

LABEL org.opencontainers.image.title="Cairn Core" \
	org.opencontainers.image.version="${VERSION}" \
	org.opencontainers.image.revision="${COMMIT}" \
	org.opencontainers.image.created="${BUILD_TIME}" \
	org.opencontainers.image.licenses="MIT" \
	io.cairn.source-state="${SOURCE_STATE}"

RUN apk add --no-cache ca-certificates \
	&& addgroup -S -g 65532 webtag \
	&& adduser -S -u 65532 -G webtag webtag

WORKDIR /app

ENV LISTEN_ADDR=:8000

# GOMEMLIMIT is left empty by default; ops should inject a soft memory ceiling
# matching the container's cgroup quota, e.g.
#   docker run -e GOMEMLIMIT=400MiB ...
# to let the Go runtime trigger GC before the OOM killer fires.
ENV GOMEMLIMIT=

COPY --from=builder --chown=webtag:webtag /out/webtag /app/webtag
COPY --from=builder --chown=webtag:webtag /out/migrate /app/migrate
COPY --chown=webtag:webtag legal/core/common/ /usr/share/licenses/cairn/

RUN test -r /usr/share/licenses/cairn/CAIRN_LICENSE.txt \
	&& test -r /usr/share/licenses/cairn/OPENCC_LICENSE.txt \
	&& test -r /usr/share/licenses/cairn/OPENCC_SOURCE.txt \
	&& test -r /usr/share/licenses/cairn/GO_WEBTAG_THIRD_PARTY.txt \
	&& test -r /usr/share/licenses/cairn/GO_MIGRATE_THIRD_PARTY.txt \
	&& test -r /usr/share/licenses/cairn/READER_THIRD_PARTY.txt \
	&& test -r /usr/share/licenses/cairn/DISTRIBUTION_BOUNDARY.txt

EXPOSE 8000

USER webtag

# HEALTHCHECK 仅用于纯 docker / docker-compose / Swarm / Nomad 场景；
# Kubernetes 部署忽略 Dockerfile HEALTHCHECK，请改用 livenessProbe /
# 容器编排时可把同一命令用于 readiness probe。busybox wget 自带于
# alpine 基础镜像，无需 apk add。--start-period=20s 给 BuildRuntime
# （migrate + legacy River job migration）足够启动时间，避免误判 unhealthy。
HEALTHCHECK --interval=30s --timeout=3s --start-period=20s --retries=3 \
	CMD wget -qO- http://127.0.0.1:8000/health >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/webtag"]

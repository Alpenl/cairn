#!/usr/bin/env sh
set -eu

VERSION="${VERSION:-$(sh scripts/version.sh)}"
COMMIT="${COMMIT:-}"

pg_container="$(docker run -d --name webtag-app-smoke-pg -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=webtag -p 5432 postgres:16)"

cleanup() {
	docker rm -f webtag-app-smoke >/dev/null 2>&1 || true
	docker rm -f "$pg_container" >/dev/null 2>&1 || true
}

trap cleanup EXIT INT TERM

# 失败时把两个容器的状态与日志一次性打出来。容器可能已经退出，
# docker ps 看不到，但 docker inspect / logs 仍然可用（前提是没有 --rm）。
dump_smoke_diagnostics() {
	echo "--- docker ps -a ---" >&2
	docker ps -a >&2 || true
	for c in webtag-app-smoke webtag-app-smoke-pg; do
		echo "--- inspect ${c} ---" >&2
		docker inspect --format '{{.State.Status}} exit={{.State.ExitCode}} err={{.State.Error}}' "$c" >&2 || true
		echo "--- logs ${c} ---" >&2
		docker logs "$c" >&2 || true
	done
}

pg_port="$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$pg_container")"

attempt=0
until docker run --rm \
	--add-host=host.docker.internal:host-gateway \
	postgres:16 \
	pg_isready -h host.docker.internal -p "$pg_port" -U postgres -d webtag >/dev/null 2>&1; do
	attempt=$((attempt + 1))
	if [ "$attempt" -ge 30 ]; then
		docker logs "$pg_container" >&2 || true
		echo "PostgreSQL did not become reachable through its published port" >&2
		exit 1
	fi
	sleep 1
done

DATABASE_URL="postgres://postgres:postgres@127.0.0.1:${pg_port}/webtag?sslmode=disable" make migrate-fresh

app_container="$(docker run -d \
	--name webtag-app-smoke \
	--add-host=host.docker.internal:host-gateway \
	-p 127.0.0.1::8000 \
	-e DATABASE_URL="postgres://postgres:postgres@host.docker.internal:${pg_port}/webtag?sslmode=disable" \
	-e AI_BASE_URL=https://example.com/v1 \
	-e AI_API_KEY=smoke-key \
	-e AI_MODEL=smoke-model \
	-e EXTENSION_API_TOKEN=smoke-installation-token \
	-e SESSION_SIGNING_KEY=smoke-session-signing-key-0123456789abcdef \
	-e CURSOR_SIGNING_KEY=smoke-cursor-signing-key-0123456789abcdef \
	webtag:${VERSION})"
app_port="$(docker inspect --format '{{(index (index .NetworkSettings.Ports "8000/tcp") 0).HostPort}}' "$app_container" 2>/dev/null || true)"
if [ -z "$app_port" ]; then
	dump_smoke_diagnostics
	echo "application container did not expose port 8000" >&2
	exit 1
fi

wait_for_http() {
	path="$1"
	expected="$2"
	attempt=0
	while [ "$attempt" -lt 30 ]; do
		if code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${app_port}${path}" || true)" && [ "$code" = "$expected" ]; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done

	dump_smoke_diagnostics
	echo "expected HTTP ${expected} from ${path}" >&2
	exit 1
}

wait_for_http /health 200
wait_for_http /ready 200

health_body="$(curl -fsS "http://127.0.0.1:${app_port}/health")"
if ! printf '%s' "$health_body" | grep -Fq "\"version\":\"${VERSION}\""; then
	dump_smoke_diagnostics
	echo "expected /health version to equal '${VERSION}'" >&2
	printf 'actual: %s\n' "$health_body" >&2
	exit 1
fi
if [ -n "$COMMIT" ] && ! printf '%s' "$health_body" | grep -Fq "\"commit\":\"${COMMIT}\""; then
	dump_smoke_diagnostics
	echo "expected /health commit to equal '${COMMIT}'" >&2
	printf 'actual: %s\n' "$health_body" >&2
	exit 1
fi

# Reader 必须真的在镜像里。这几条是「发布出去的是不是产品本体」的唯一自动
# 判据：Dockerfile 的 reader-builder 阶段一旦漏接、或 COPY 顺序被调换成先注入
# 后覆盖，镜像会静默退化成一页「Reader 尚未构建」，而 /health 照样 200。
assert_status() {
	path="$1"
	expected="$2"
	code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${app_port}${path}" || true)"
	if [ "$code" != "$expected" ]; then
		dump_smoke_diagnostics
		echo "expected HTTP ${expected} from ${path}, got ${code}" >&2
		exit 1
	fi
}

assert_body_contains() {
	path="$1"
	needle="$2"
	body="$(curl -s "http://127.0.0.1:${app_port}${path}" || true)"
	if ! printf '%s' "$body" | grep -q "$needle"; then
		dump_smoke_diagnostics
		echo "expected ${path} body to contain '${needle}'" >&2
		printf 'actual (first 400 chars): %s\n' "$(printf '%s' "$body" | head -c 400)" >&2
		exit 1
	fi
}

# 站点根送到 Reader，而不是任何一页调试 UI。
assert_status / 302
assert_status /reader 301
assert_status /reader/ 200
# 产物缺席时 /reader/ 返回 503 说明页，上一条就会红；这条再确认返回的确实是
# Reader 入口（挂载点 + 带 /reader/ 前缀的 Vite 产物），而不是别的 HTML。
assert_body_contains /reader/ '<div id="root">'
assert_body_contains /reader/ '/reader/assets/'
# HTML 出现资源前缀还不够：生产 handler 接错 FS 时入口仍是 200，真正的脚本却
# 全部 404。解析入口引用的第一个 JS，并沿浏览器实际使用的 URL 请求一次。
reader_html="$(curl -fsS "http://127.0.0.1:${app_port}/reader/")"
reader_js_path="$(printf '%s' "$reader_html" | grep -oE "/reader/assets/[^\"'[:space:]<>]+\\.js" | head -n 1)"
if [ -z "$reader_js_path" ]; then
	dump_smoke_diagnostics
	echo "expected /reader/ to reference a /reader/assets/*.js file" >&2
	printf 'actual (first 400 chars): %s\n' "$(printf '%s' "$reader_html" | head -c 400)" >&2
	exit 1
fi
assert_status "$reader_js_path" 200
# SPA 深链刷新不能 404。
assert_status /reader/some/deep/view 200

# 公开 API 默认 fail-closed。这条曾经是 200：MODE=single（默认）+ 未配
# EXTENSION_API_TOKEN + 零把 api key = 对所有人敞开，端口一暴露就能读走全库。
# 冒烟容器正是这个配置，所以它是这条默认值最直接的证人。
assert_status /api/links 401

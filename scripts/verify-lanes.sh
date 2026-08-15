#!/bin/sh

# Run independently attributable local verification lanes. This script does
# not install dependencies or regenerate tracked artifacts; prepare the
# relevant toolchain first, then opt into only the lanes needed for a change.
set -eu

repository=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repository"

go_targets=${GO_TARGETS:-"./internal/app/... ./internal/handler/... ./internal/service/..."}

usage() {
	cat <<'EOF'
Usage: scripts/verify-lanes.sh <lane> [<lane> ...]

Lanes (run in the order provided; each lane stops at its first failure):
  openapi          Go OpenAPI/router and OpenAPI/DTO consistency tests
  webtag-api       generated TypeScript drift check and package typecheck
  schema           migration/schema snapshot drift check (requires Docker)
  go-targeted      targeted Go tests (override packages with GO_TARGETS)
  go-race          targeted Go race tests (override packages with GO_TARGETS)
  reader           Reader lint, typecheck, unit, build, and browser gates
  extension        Extension lint, build verification, typecheck, and tests
  mobile-static    shared fixture, Android/iOS source, and wire checks
  android-jvm      Android JVM tests, lint, and Android-test compilation
  actionlint       workflow syntax, expressions, and full-SHA pin checks
  docker-dry-run   BuildKit Dockerfile validation without building an image

Examples:
  scripts/verify-lanes.sh openapi webtag-api
  GO_TARGETS='./internal/app/... ./internal/service/...' \
    scripts/verify-lanes.sh go-targeted go-race
EOF
}

run_lane() {
	lane=$1
	shift
	printf '\n==> %s\n' "$lane"
	"$@"
}

run_pnpm() {
	corepack pnpm@10.13.1 "$@"
}

run_go_targets() {
	mode=$1
	# Intentional word splitting lets GO_TARGETS carry a package list.
	# shellcheck disable=SC2086
	case "$mode" in
		test) go test -count=1 $go_targets ;;
		race) go test -race -count=1 $go_targets ;;
	esac
}

run_mobile_static() {
	python3 mobile/shared/fixtures/validate.py
	python3 mobile/shared/fixtures/compare.py
	python3 scripts/mobile-x1-check.py
	python3 scripts/mobile-wire-smoke.py
}

run_android_jvm() {
	./mobile/android/gradlew -p mobile/android --no-daemon \
		--dependency-verification strict \
		testDebugUnitTest lintDebug compileDebugAndroidTestKotlin
}

run_actionlint() {
	make actionlint
}

run_docker_check() {
	docker buildx build --check .
}

[ "$#" -gt 0 ] || {
	usage
	exit 2
}

for lane in "$@"; do
	case "$lane" in
		openapi)
			run_lane "$lane" go test ./internal/app -run OpenAPI -count=1
			;;
		webtag-api)
			run_lane "$lane:generated-drift" run_pnpm --filter @webtag/api api:check
			run_lane "$lane:typecheck" run_pnpm --filter @webtag/api typecheck
			;;
		schema)
			run_lane "$lane" make schema-check
			;;
		go-targeted)
			run_lane "$lane" run_go_targets test
			;;
		go-race)
			run_lane "$lane" run_go_targets race
			;;
		reader)
			run_lane "$lane" run_pnpm verify:reader
			;;
		extension)
			run_lane "$lane" run_pnpm verify:extension
			;;
		mobile-static)
			run_lane "$lane" run_mobile_static
			;;
		android-jvm)
			run_lane "$lane" run_android_jvm
			;;
		actionlint)
			run_lane "$lane" run_actionlint
			;;
		docker-dry-run)
			run_lane "$lane" run_docker_check
			;;
		-h|--help|help)
			usage
			;;
		*)
			printf 'unknown verification lane: %s\n' "$lane" >&2
			usage >&2
			exit 2
			;;
	esac
done

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The harness stands up a whole fake host: real directories with real
// permissions, real executables for systemctl, pg_dump and pg_restore, a real
// HTTP server answering /health and /ready, and a real HTTP server serving
// release assets.
//
// Nothing here is a mock returning a canned struct. Every fault the tests
// inject is the same fault the production code would meet — a program that
// exits non-zero, a dump file that is zero bytes, a service that reports the
// wrong commit — because the failures worth defending against in a deployment
// helper are exactly the ones a mock cannot express.

const previousCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type host struct {
	t          *testing.T
	root       string
	control    string
	binDir     string
	config     Config
	fixture    *releaseFixture
	assets     *assetServer
	store      *Store
	server     *Server
	healthSrv  *httptest.Server
	deployToke string
}

const testDeployToken = "deploy-token-that-is-long-enough-for-the-floor"

func newHost(t *testing.T) *host {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Fatalf("the deployment helper targets Linux hosts; this suite needs one")
	}
	root := t.TempDir()
	control := filepath.Join(root, "control")
	binDir := filepath.Join(root, "bin")
	for _, dir := range []string{control, binDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}

	host := &host{t: t, root: root, control: control, binDir: binDir, deployToke: testDeployToken}
	host.fixture = newReleaseFixture(t, control)
	host.assets = newAssetServer(t, host.fixture)
	host.writeControl("service.state", "active")
	host.writeControl("health.commit", previousCommit)
	host.writeDefaultMigrationReports()
	host.startHealthServer()
	host.writeFakeTools()
	host.buildConfig()
	host.enforceLayout()
	host.buildServer()
	return host
}

func (host *host) writeControl(name, content string) {
	host.t.Helper()
	if err := os.WriteFile(filepath.Join(host.control, name), []byte(content), 0o644); err != nil {
		host.t.Fatalf("write control file %s: %v", name, err)
	}
}

func (host *host) removeControl(name string) {
	host.t.Helper()
	if err := os.Remove(filepath.Join(host.control, name)); err != nil && !os.IsNotExist(err) {
		host.t.Fatalf("remove control file %s: %v", name, err)
	}
}

func (host *host) readControl(name string) string {
	host.t.Helper()
	data, err := os.ReadFile(filepath.Join(host.control, name))
	if err != nil {
		return ""
	}
	return string(data)
}

// writeDefaultMigrationReports installs the happy-path plan and apply reports.
func (host *host) writeDefaultMigrationReports() {
	host.t.Helper()
	host.writeControl("plan.json", mustJSON(host.t, MigrationReport{
		SchemaVersion: MigrationReportSchemaVersion,
		Tool:          "cairn-migrate",
		OK:            true,
		Mode:          "target",
		Target:        fixtureSchema,
		OnlineUpdate: &OnlineUpdatePlan{
			Target:   fixtureSchema,
			Pending:  []string{fixtureSchema},
			Allowed:  true,
			Blockers: []OnlineUpdateBlocker{},
		},
	}))
	host.writeControl("apply.json", mustJSON(host.t, host.successfulApplyReport()))
}

func (host *host) successfulApplyReport() MigrationReport {
	return MigrationReport{
		SchemaVersion: MigrationReportSchemaVersion,
		Tool:          "cairn-migrate",
		OK:            true,
		Mode:          "target",
		Target:        fixtureSchema,
		StartVersion:  "previousstep2026010101",
		EndVersion:    fixtureSchema,
		Applied:       []string{fixtureSchema},
		Ledgers: &LedgerReconciliation{
			OK:       true,
			Schema:   SchemaLedgerState{Present: true, Target: fixtureSchema, Head: fixtureSchema, AtTarget: true},
			River:    RiverLedgerState{Present: true, Target: fixtureRiver, Head: fixtureRiver, AtTarget: true},
			Problems: []LedgerProblem{},
		},
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode fixture JSON: %v", err)
	}
	return string(data)
}

// startHealthServer answers /health and /ready from the control directory, so
// stopping the fake service really does make the application unreachable.
func (host *host) startHealthServer() {
	host.t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(writer http.ResponseWriter, _ *http.Request) {
		if strings.TrimSpace(host.readControl("service.state")) != "active" {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(writer, `{"status":"ok","version":%q,"commit":%q,"build_time":%q}`,
			fixtureVersion, strings.TrimSpace(host.readControl("health.commit")), fixtureBuildTime)
	})
	mux.HandleFunc("/ready", func(writer http.ResponseWriter, _ *http.Request) {
		if strings.TrimSpace(host.readControl("service.state")) != "active" || host.readControl("not_ready") != "" {
			writer.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(writer, `{"status":"degraded","ready":false,"failed":["database"]}`)
			return
		}
		fmt.Fprint(writer, `{"status":"ok","ready":true}`)
	})
	host.healthSrv = httptest.NewServer(mux)
	host.t.Cleanup(host.healthSrv.Close)
}

// writeFakeTools installs executable stand-ins for systemctl, pg_dump and
// pg_restore.
func (host *host) writeFakeTools() {
	host.t.Helper()
	coreCurrent := filepath.Join(host.root, "opt", "webtag", "releases", "current")

	host.writeScript("systemctl", fmt.Sprintf(`#!/bin/sh
CONTROL=%q
echo "$1 $2" >> "$CONTROL/service.log"
case "$1" in
  stop)
    echo inactive > "$CONTROL/service.state"
    ;;
  start)
    if [ -f "$CONTROL/start.exit" ]; then exit "$(cat "$CONTROL/start.exit")"; fi
    if [ -f "$CONTROL/force.commit" ]; then
      cp "$CONTROL/force.commit" "$CONTROL/health.commit"
    elif [ -x %q/webtag ]; then
      %q/webtag --version | sed -n 's/^commit: //p' > "$CONTROL/health.commit"
    fi
    echo active > "$CONTROL/service.state"
    ;;
  is-active)
    if [ -f "$CONTROL/stuck_active" ]; then echo active; exit 0; fi
    cat "$CONTROL/service.state"
    ;;
esac
exit 0
`, host.control, coreCurrent, coreCurrent))

	host.writeScript("pg_dump", fmt.Sprintf(`#!/bin/sh
CONTROL=%q
if [ "$1" = "--version" ]; then echo "pg_dump (PostgreSQL) 16.4"; exit 0; fi
echo "$@" >> "$CONTROL/pgdump.log"
OUT=""
for arg in "$@"; do
  case "$arg" in --file=*) OUT="${arg#--file=}";; esac
done
if [ -f "$CONTROL/pgdump.exit" ]; then
  echo "pg_dump: error: connection to server failed" >&2
  exit "$(cat "$CONTROL/pgdump.exit")"
fi
if [ -f "$CONTROL/pgdump.empty" ]; then
  : > "$OUT"
elif [ -f "$CONTROL/pgdump.corrupt" ]; then
  printf 'not-an-archive' > "$OUT"
else
  printf 'PGDMP\000fake custom-format dump' > "$OUT"
fi
exit 0
`, host.control))

	host.writeScript("pg_restore", fmt.Sprintf(`#!/bin/sh
CONTROL=%q
if [ "$1" = "--version" ]; then echo "pg_restore (PostgreSQL) 16.4"; exit 0; fi
FILE="$2"
echo "$@" >> "$CONTROL/pgrestore.log"
if [ ! -s "$FILE" ]; then
  echo "pg_restore: error: input file is too short (read 0, expected 5)" >&2
  exit 1
fi
if ! head -c 5 "$FILE" | grep -q PGDMP; then
  echo "pg_restore: error: did not find magic string in file header" >&2
  exit 1
fi
echo ";     dbname: webtag"
echo "215; 1259 16388 TABLE public links webtag"
exit 0
`, host.control))
}

func (host *host) writeScript(name, body string) {
	host.t.Helper()
	path := filepath.Join(host.binDir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // test fixture executable
		host.t.Fatalf("write %s: %v", name, err)
	}
}

// buildConfig points every path at the temporary host and sets the expected
// owner to the running user, so the permission model is exercised for real
// without the suite needing root.
func (host *host) buildConfig() {
	host.t.Helper()
	current, err := user.Current()
	if err != nil {
		host.t.Fatalf("read current user: %v", err)
	}
	coreDir := filepath.Join(host.root, "opt", "webtag")
	readerDir := filepath.Join(host.root, "var", "www", "reader")
	stateDir := filepath.Join(host.root, "var", "lib", "cairn-updater")
	helperEnv := filepath.Join(host.root, "etc", "cairn-updater.env")

	// stateDir is deliberately not pre-created: the helper owns it and must
	// create it at 0700 itself.
	for _, dir := range []string{coreDir, readerDir, filepath.Dir(helperEnv)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			host.t.Fatalf("create %s: %v", dir, err)
		}
	}
	writeFile(host.t, filepath.Join(coreDir, ".env"), "DATABASE_URL=postgres://example\n", 0o640)
	writeFile(host.t, helperEnv, "DEPLOY_AUTH_TOKEN=x\n", 0o600)

	uid := 0
	fmt.Sscanf(current.Uid, "%d", &uid)
	host.config = Config{
		Repo:        fixtureRepo,
		DeployToken: host.deployToke,
		DatabaseURL: "postgres://webtag:secret@127.0.0.1:5433/webtag?sslmode=disable",
		SocketPath:  filepath.Join(host.root, "run", "cairn-updater.sock"),
		SocketGroup: current.Gid,
		StateDir:    stateDir,
		CoreDir:     coreDir,
		ReaderDir:   readerDir,
		HelperEnv:   helperEnv,
		ServiceUnit: "webtag.service",
		// "nobody" stands in for the webtag account: it exists on every Linux
		// host and is not a member of the group the socket is created in, which
		// is exactly the fencing the model requires of the real service user.
		ServiceUser:  "nobody",
		CoreEnvGroup: current.Gid,
		HealthBase:   host.healthSrv.URL,
		Systemctl:    filepath.Join(host.binDir, "systemctl"),
		PgDump:       filepath.Join(host.binDir, "pg_dump"),
		PgRestore:    filepath.Join(host.binDir, "pg_restore"),
		MinFreeBytes: 1 << 20,
		OwnerUID:     uid,
	}
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

func (host *host) enforceLayout() {
	host.t.Helper()
	if err := BuildLayout(host.config).Enforce(); err != nil {
		host.t.Fatalf("the harness layout does not satisfy the permission model: %v", err)
	}
}

func (host *host) buildServer() {
	host.t.Helper()
	store, err := NewStore(host.config.JobsDir(), time.Now)
	if err != nil {
		host.t.Fatalf("open job store: %v", err)
	}
	host.store = store
	host.server = host.newServer(store)
}

func (host *host) newServer(store *Store) *Server {
	trust := testTrust{publicKey: host.fixture.PublicKey}
	downloader := host.assets.downloader(host.t)
	commands := execRunner{}
	health := NewHealthClient(host.config.HealthBase)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	server := &Server{
		config:     host.config,
		auth:       newAuthenticator(host.config.DeployToken),
		store:      store,
		health:     health,
		downloader: downloader,
		trust:      trust,
		logger:     logger,
		now:        time.Now,
		hostOS:     "linux",
		hostArch:   runtime.GOARCH,
	}
	server.newRunner = func() *Runner {
		return &Runner{
			config:      host.config,
			store:       store,
			trust:       trust,
			downloader:  downloader,
			commands:    commands,
			service:     NewServiceControl(commands, host.config.Systemctl, host.config.ServiceUnit),
			health:      health,
			hostOS:      "linux",
			hostArch:    runtime.GOARCH,
			now:         time.Now,
			readyBudget: 2 * time.Second,
			readyPoll:   50 * time.Millisecond,
		}
	}
	return server
}

// runUpdate submits a job through the HTTP surface and waits for it to finish.
func (host *host) runUpdate(target string) *Job {
	host.t.Helper()
	response := host.request(http.MethodPost, "/api/deploy/system/jobs", `{"target":"`+target+`"}`, host.deployToke)
	if response.Code != http.StatusAccepted {
		host.t.Fatalf("submit %s: status %d body %s", target, response.Code, response.Body.String())
	}
	var submitted SubmitJobResponse
	decodeBody(host.t, response, &submitted)
	host.server.Wait()
	job, err := host.store.Get(submitted.JobID)
	if err != nil {
		host.t.Fatalf("read finished job: %v", err)
	}
	return job
}

func (host *host) request(method, path, body, token string) *httptest.ResponseRecorder {
	host.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	recorder := httptest.NewRecorder()
	host.server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("decode response %s: %v", recorder.Body.String(), err)
	}
}

// serviceLog is the ordered list of systemctl verbs the update issued. It is
// how the tests prove that a refusal happened before the service was stopped.
func (host *host) serviceLog() []string {
	host.t.Helper()
	raw := host.readControl("service.log")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(raw), "\n")
}

func (host *host) serviceWasStopped() bool {
	for _, line := range host.serviceLog() {
		if strings.HasPrefix(line, "stop") {
			return true
		}
	}
	return false
}

func (host *host) migrationRan() bool {
	return strings.TrimSpace(host.readControl("apply.log")) != ""
}

func hostArchForTest() string { return runtime.GOARCH }

func nowForTest() time.Time { return time.Now() }

func jsonBody(value any) (string, error) {
	data, err := json.Marshal(value)
	return string(data), err
}

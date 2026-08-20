package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"webtag/internal/releasetrust"
)

func TestVersionReportsTheHelperAndTheRunningCore(t *testing.T) {
	host := newHost(t)

	response := host.request(http.MethodGet, "/api/deploy/system/version", "", testDeployToken)
	if response.Code != http.StatusOK {
		t.Fatalf("version answered %d", response.Code)
	}
	var payload VersionResponse
	decodeBody(t, response, &payload)

	if payload.SchemaVersion != APISchemaVersion {
		t.Fatalf("expected schema version %d, got %d", APISchemaVersion, payload.SchemaVersion)
	}
	if payload.Helper.Protocol != releasetrust.HelperProtocol {
		t.Fatalf("expected helper protocol %d, got %d", releasetrust.HelperProtocol, payload.Helper.Protocol)
	}
	if payload.Repo != fixtureRepo {
		t.Fatalf("expected repo %s, got %s", fixtureRepo, payload.Repo)
	}
	if payload.InstallMode != InstallModeSystemdRelease {
		t.Fatalf("expected install mode %s, got %s", InstallModeSystemdRelease, payload.InstallMode)
	}
	if !payload.Eligible {
		t.Fatal("a systemd-release installation must be eligible")
	}
	if !payload.Current.Reachable || payload.Current.Commit != previousCommit {
		t.Fatalf("expected the running commit %s, got %+v", previousCommit, payload.Current)
	}
}

// TestVersionStillAnswersWhileTheCoreIsStopped is the reason this endpoint
// lives in the helper rather than the application.
func TestVersionStillAnswersWhileTheCoreIsStopped(t *testing.T) {
	host := newHost(t)
	host.writeControl("service.state", "inactive")

	response := host.request(http.MethodGet, "/api/deploy/system/version", "", testDeployToken)
	if response.Code != http.StatusOK {
		t.Fatalf("version must answer while the Core is stopped, got %d", response.Code)
	}
	var payload VersionResponse
	decodeBody(t, response, &payload)
	if payload.Current.Reachable {
		t.Fatal("a stopped Core must be reported as unreachable rather than guessed at")
	}
	if payload.Current.Error == "" {
		t.Fatal("an unreachable Core must explain itself")
	}
	if payload.Helper.Protocol == 0 {
		t.Fatal("the helper must still describe itself when the Core is down")
	}
}

func TestCheckUpdatesReturnsAnExactVerifiedCandidate(t *testing.T) {
	host := newHost(t)

	response := host.request(http.MethodGet, "/api/deploy/system/check-updates", "", testDeployToken)
	if response.Code != http.StatusOK {
		t.Fatalf("check-updates answered %d", response.Code)
	}
	var payload CheckUpdatesResponse
	decodeBody(t, response, &payload)

	if payload.Candidate == nil {
		t.Fatalf("expected a candidate, got discovery error %q", payload.DiscoveryError)
	}
	candidate := payload.Candidate
	if candidate.Tag != fixtureTag || candidate.Commit != fixtureCommit {
		t.Fatalf("expected %s/%s, got %s/%s", fixtureTag, fixtureCommit, candidate.Tag, candidate.Commit)
	}
	if candidate.ManifestSHA256 != digestHex(host.fixture.ManifestBytes) {
		t.Fatal("the manifest digest must be the digest of the exact bytes that verified")
	}
	if candidate.SchemaTarget != fixtureSchema || candidate.RiverLedgerTarget != fixtureRiver {
		t.Fatalf("the migration targets must come from the signed manifest, got %+v", candidate)
	}
	if candidate.CoreSHA256 != host.fixture.Manifest.Core[0].SHA256 {
		t.Fatal("the Core digest must come from the signed manifest")
	}
	if !payload.CanUpdate {
		t.Fatalf("expected an updatable candidate, disabled because %q", payload.DisabledReason)
	}
	if candidate.SignatureKeyID == "" {
		t.Fatal("the candidate must name the key that signed it")
	}
}

func TestCheckUpdatesDegradesToReadOnlyWhenDiscoveryFails(t *testing.T) {
	host := newHost(t)
	host.assets.failAsset = releasetrust.ManifestFileName

	response := host.request(http.MethodGet, "/api/deploy/system/check-updates?force=true", "", testDeployToken)
	if response.Code != http.StatusOK {
		t.Fatalf("a discovery failure must not fail the endpoint, got %d", response.Code)
	}
	var payload CheckUpdatesResponse
	decodeBody(t, response, &payload)
	if payload.Candidate != nil {
		t.Fatal("a manifest that could not be fetched must not produce a candidate")
	}
	if payload.CanUpdate {
		t.Fatal("an undiscoverable release must not be updatable")
	}
	if payload.DiscoveryError == "" || payload.DisabledReason == "" {
		t.Fatal("a degraded check must say why")
	}
	// The running version is still reported, which is what keeps the settings
	// page useful when GitHub is unreachable.
	if !payload.Current.Reachable || payload.Current.Commit != previousCommit {
		t.Fatalf("the running version must still be reported, got %+v", payload.Current)
	}
}

func TestCheckUpdatesRefusesANonFormalDiscoveryAnswer(t *testing.T) {
	host := newHost(t)
	// A repository that answered with a branch or a prerelease must not turn
	// into an install candidate.
	for _, latest := range []string{"main", "v1.2.3-rc1", "latest", "v1.2"} {
		host.assets.releases = []releaseListing{{TagName: latest}}
		response := host.request(http.MethodGet, "/api/deploy/system/check-updates?force=true", "", testDeployToken)
		var payload CheckUpdatesResponse
		decodeBody(t, response, &payload)
		if payload.Candidate != nil {
			t.Fatalf("discovery answer %q became a candidate", latest)
		}
		if payload.CanUpdate {
			t.Fatalf("discovery answer %q was updatable", latest)
		}
	}
}

func TestCheckUpdatesFailsClosedBeforeDiscoveryWithoutAFormalCurrentIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*host)
	}{
		{
			name: "Core unreachable",
			mutate: func(host *host) {
				host.writeControl("service.state", "inactive")
			},
		},
		{
			name: "placeholder version",
			mutate: func(host *host) {
				host.writeControl("health.version", "0.0.0")
			},
		},
		{
			name: "non-release version",
			mutate: func(host *host) {
				host.writeControl("health.version", "dev")
			},
		},
		{
			name: "non-release commit",
			mutate: func(host *host) {
				host.writeControl("health.commit", "unknown")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			host := newHost(t)
			test.mutate(host)
			before := len(host.assets.requests)

			response := host.request(http.MethodGet, "/api/deploy/system/check-updates?force=true", "", testDeployToken)
			var payload CheckUpdatesResponse
			decodeBody(t, response, &payload)

			if payload.Candidate != nil || payload.CanUpdate || payload.UpdateAvailable {
				t.Fatalf("an unconfirmed current identity produced an update: %+v", payload)
			}
			if payload.DiscoveryError == "" || payload.DisabledReason == "" {
				t.Fatalf("an unconfirmed current identity must explain the refusal: %+v", payload)
			}
			if len(host.assets.requests) != before {
				t.Fatal("GitHub must not be queried before the current release series is confirmed")
			}
		})
	}
}

func TestCheckUpdatesDoesNotOfferTheAlreadyInstalledRelease(t *testing.T) {
	host := newHost(t)
	host.writeControl("health.version", fixtureVersion)
	host.writeControl("health.commit", fixtureCommit)

	response := host.request(http.MethodGet, "/api/deploy/system/check-updates?force=true", "", testDeployToken)
	var payload CheckUpdatesResponse
	decodeBody(t, response, &payload)

	if payload.Candidate == nil || payload.Candidate.Tag != fixtureTag {
		t.Fatalf("the signed current release should remain visible, got %+v", payload)
	}
	if payload.UpdateAvailable || payload.CanUpdate {
		t.Fatalf("the installed release was offered as an update: %+v", payload)
	}
	if payload.DisabledReason != "" || payload.DiscoveryError != "" {
		t.Fatalf("being current is not an error: %+v", payload)
	}
}

func TestCheckUpdatesCacheIsBoundToTheCurrentCoreIdentity(t *testing.T) {
	host := newHost(t)
	first := host.request(http.MethodGet, "/api/deploy/system/check-updates", "", testDeployToken)
	var payload CheckUpdatesResponse
	decodeBody(t, first, &payload)
	if payload.Candidate == nil {
		t.Fatalf("first discovery failed: %+v", payload)
	}
	before := len(host.assets.requests)

	host.writeControl("health.version", "1.2.1")
	second := host.request(http.MethodGet, "/api/deploy/system/check-updates", "", testDeployToken)
	decodeBody(t, second, &payload)
	if payload.Cached {
		t.Fatal("a result selected for a different running Core identity was reused")
	}
	if len(host.assets.requests) == before {
		t.Fatal("changing the running Core identity must trigger fresh discovery")
	}
}

func TestCheckUpdatesIsCachedUntilForced(t *testing.T) {
	host := newHost(t)
	first := host.request(http.MethodGet, "/api/deploy/system/check-updates", "", testDeployToken)
	var payload CheckUpdatesResponse
	decodeBody(t, first, &payload)
	if payload.Cached {
		t.Fatal("the first check cannot be cached")
	}
	before := len(host.assets.requests)

	second := host.request(http.MethodGet, "/api/deploy/system/check-updates", "", testDeployToken)
	decodeBody(t, second, &payload)
	if !payload.Cached {
		t.Fatal("the second check must be served from cache")
	}
	if len(host.assets.requests) != before {
		t.Fatal("a cached check must not touch the release host")
	}

	third := host.request(http.MethodGet, "/api/deploy/system/check-updates?force=true", "", testDeployToken)
	decodeBody(t, third, &payload)
	if payload.Cached {
		t.Fatal("force=true must bypass the cache")
	}
	if len(host.assets.requests) == before {
		t.Fatal("a forced check must reach the release host")
	}
}

func TestAnUnknownJobIsNotFound(t *testing.T) {
	host := newHost(t)
	for _, id := range []string{
		"0123456789abcdef0123456789abcdef",
		"not-a-job-id",
		"..%2f..%2fetc%2fpasswd",
		strings.Repeat("a", 200),
	} {
		response := host.request(http.MethodGet, "/api/deploy/system/jobs/"+id, "", testDeployToken)
		if response.Code != http.StatusNotFound {
			t.Fatalf("job id %q was answered %d, expected 404", id, response.Code)
		}
	}
}

// TestJobStatusIsReadableWhileTheCoreIsStoppedAndTheJobIsMidFlight is the
// central availability claim of the whole design.
func TestJobStatusIsReadableWhileTheCoreIsStoppedAndTheJobIsMidFlight(t *testing.T) {
	host := newHost(t)
	host.writeControl("slow", "3")

	submitted := host.submit(fixtureTag)

	// Wait until the maintenance window is actually open.
	waitFor(t, 5*time.Second, func() bool {
		return strings.TrimSpace(host.readControl("service.state")) == "inactive"
	})

	// The Core is down. The status endpoint must still answer, and it must
	// answer about a job that is still moving.
	response := host.request(http.MethodGet, "/api/deploy/system/jobs/"+submitted.JobID, "", testDeployToken)
	if response.Code != http.StatusOK {
		t.Fatalf("job status answered %d while the Core was stopped", response.Code)
	}
	var status JobResponse
	decodeBody(t, response, &status)
	if status.State != JobRunning {
		t.Fatalf("expected a running job mid-window, got %s", status.State)
	}
	if status.Target != fixtureTag || status.TargetCommit != fixtureCommit {
		t.Fatalf("the status must name the exact target, got %+v", status)
	}
	if len(status.Order) != len(Phases) {
		t.Fatal("the status must publish the phase order the UI renders")
	}

	// The record is on disk, so a helper restart could still answer it.
	reopened, err := NewStore(host.config.JobsDir(), time.Now)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if _, err := reopened.Load(submitted.JobID); err != nil {
		t.Fatalf("the job record must be readable from disk mid-flight: %v", err)
	}

	host.server.Wait()
	final := host.request(http.MethodGet, "/api/deploy/system/jobs/"+submitted.JobID, "", testDeployToken)
	decodeBody(t, final, &status)
	if status.State != JobSucceeded {
		t.Fatalf("expected the job to finish, got %s (%+v)", status.State, status.Hold)
	}
}

// TestConcurrentSubmissionsOfTheSameTargetShareOneJob covers the
// double-clicking operator and the retrying browser at once.
func TestConcurrentSubmissionsOfTheSameTargetShareOneJob(t *testing.T) {
	host := newHost(t)
	host.writeControl("slow", "2")

	const attempts = 8
	ids := make([]string, attempts)
	deduplicated := make([]bool, attempts)
	var group sync.WaitGroup
	start := make(chan struct{})
	for index := range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			response := host.request(http.MethodPost, "/api/deploy/system/jobs",
				`{"target":"`+fixtureTag+`"}`, testDeployToken)
			if response.Code != http.StatusAccepted {
				t.Errorf("submission %d answered %d", index, response.Code)
				return
			}
			var submitted SubmitJobResponse
			if err := json.Unmarshal(response.Body.Bytes(), &submitted); err != nil {
				t.Errorf("decode submission %d: %v", index, err)
				return
			}
			ids[index] = submitted.JobID
			deduplicated[index] = submitted.Deduplicated
		}()
	}
	close(start)
	group.Wait()

	for index, id := range ids {
		if id != ids[0] {
			t.Fatalf("submission %d produced job %s, expected the shared job %s", index, id, ids[0])
		}
	}
	joined := 0
	for _, wasDeduplicated := range deduplicated {
		if wasDeduplicated {
			joined++
		}
	}
	if joined != attempts-1 {
		t.Fatalf("expected %d submissions to report joining an existing job, got %d", attempts-1, joined)
	}

	host.server.Wait()
	// Exactly one migration ran, not eight.
	if runs := strings.Count(strings.TrimSpace(host.readControl("apply.log")), "\n") + 1; runs != 1 {
		t.Fatalf("expected exactly one migration, the apply log shows %d", runs)
	}
}

func TestADifferentTargetIsRefusedWhileAJobIsRunning(t *testing.T) {
	host := newHost(t)
	host.writeControl("slow", "2")

	host.submit(fixtureTag)
	response := host.request(http.MethodPost, "/api/deploy/system/jobs", `{"target":"v9.9.9"}`, testDeployToken)
	if response.Code != http.StatusConflict {
		t.Fatalf("a second target was answered %d, expected 409", response.Code)
	}
	var failure ErrorResponse
	decodeBody(t, response, &failure)
	if failure.Error.Code != CodeOperationInProgress {
		t.Fatalf("expected %q, got %q", CodeOperationInProgress, failure.Error.Code)
	}
	host.server.Wait()
}

func TestAHeldJobBlocksFurtherUpdatesUntilAHumanResolvesIt(t *testing.T) {
	host := newHost(t)
	host.writeControl("pgdump.empty", "yes")

	held := host.runUpdate(fixtureTag)
	if held.State != JobHold {
		t.Fatalf("expected a hold, got %s", held.State)
	}

	response := host.request(http.MethodPost, "/api/deploy/system/jobs", `{"target":"`+fixtureTag+`"}`, testDeployToken)
	if response.Code != http.StatusConflict {
		t.Fatalf("a submission after a hold was answered %d, expected 409", response.Code)
	}
	// check-updates says the same thing, so the page disables the button rather
	// than letting the operator discover the conflict by clicking it.
	check := host.request(http.MethodGet, "/api/deploy/system/check-updates?force=true", "", testDeployToken)
	var payload CheckUpdatesResponse
	decodeBody(t, check, &payload)
	if payload.CanUpdate {
		t.Fatal("an unresolved hold must disable further updates")
	}
	if !strings.Contains(payload.DisabledReason, "holding") {
		t.Fatalf("the page must be told why, got %q", payload.DisabledReason)
	}
}

// TestAClientDisconnectDoesNotCancelTheUpdate runs the API over a real HTTP
// server so the client can genuinely go away mid-update.
func TestAClientDisconnectDoesNotCancelTheUpdate(t *testing.T) {
	host := newHost(t)
	host.writeControl("slow", "2")

	apiServer := httptest.NewServer(host.server.Handler())
	defer apiServer.Close()

	submitCtx, cancelSubmit := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(submitCtx, http.MethodPost,
		apiServer.URL+"/api/deploy/system/jobs", strings.NewReader(`{"target":"`+fixtureTag+`"}`))
	if err != nil {
		t.Fatalf("build submit request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testDeployToken)
	response, err := apiServer.Client().Do(request)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	var submitted SubmitJobResponse
	if err := json.NewDecoder(response.Body).Decode(&submitted); err != nil {
		t.Fatalf("decode submission: %v", err)
	}
	_ = response.Body.Close()

	// The operator's browser goes away entirely: the request context is
	// cancelled and the connection is closed while the update is mid-flight.
	cancelSubmit()
	apiServer.Client().CloseIdleConnections()

	host.server.Wait()

	job, err := host.store.Get(submitted.JobID)
	if err != nil {
		t.Fatalf("read job after disconnect: %v", err)
	}
	if job.State != JobSucceeded {
		t.Fatalf("the update must survive the client going away, got %s (%+v)", job.State, job.Hold)
	}
	if !host.migrationRan() {
		t.Fatal("the migration must have run despite the disconnect")
	}
}

func (host *host) submit(target string) SubmitJobResponse {
	host.t.Helper()
	response := host.request(http.MethodPost, "/api/deploy/system/jobs", `{"target":"`+target+`"}`, testDeployToken)
	if response.Code != http.StatusAccepted {
		host.t.Fatalf("submit %s answered %d: %s", target, response.Code, response.Body.String())
	}
	var submitted SubmitJobResponse
	decodeBody(host.t, response, &submitted)
	return submitted
}

func waitFor(t *testing.T, budget time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s", budget)
}

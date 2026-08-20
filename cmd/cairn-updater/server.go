package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"webtag/internal/buildinfo"
	"webtag/internal/releasetrust"
)

// Route paths. They are fixed strings because Caddy proxies exactly this prefix
// to the helper socket; a route that could move would be a route Caddy might
// stop protecting.
const (
	RouteVersion      = "GET /api/deploy/system/version"
	RouteCheckUpdates = "GET /api/deploy/system/check-updates"
	RouteSubmitJob    = "POST /api/deploy/system/jobs"
	RouteJobStatus    = "GET /api/deploy/system/jobs/{id}"
)

// maxRequestBytes bounds a submission body. The only body any route accepts is
// a one-field object.
const maxRequestBytes int64 = 4 << 10

// checkCacheTTL is how long a discovery result is reused. GitHub rate-limits
// unauthenticated API calls, and a settings page left open would otherwise poll
// it continuously.
const checkCacheTTL = 5 * time.Minute

// Server is the HTTP surface of the helper.
type Server struct {
	config     Config
	auth       *authenticator
	store      *Store
	health     *HealthClient
	downloader *Downloader
	trust      ReleaseTrust
	newRunner  func() *Runner
	logger     *slog.Logger
	now        func() time.Time
	hostOS     string
	hostArch   string

	cacheMu   sync.Mutex
	cached    *CheckUpdatesResponse
	cachedAt  time.Time
	launchers sync.WaitGroup
}

// Handler builds the routed, authenticated handler.
//
// Every route goes through requireToken. There is no health endpoint, no
// unauthenticated status route, and no exemption for a local socket peer:
// "arrived on the Unix socket" proves the request came through Caddy, and Caddy
// is a reverse proxy for the public internet.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteVersion, server.handleVersion)
	mux.HandleFunc(RouteCheckUpdates, server.handleCheckUpdates)
	mux.HandleFunc(RouteSubmitJob, server.handleSubmitJob)
	mux.HandleFunc(RouteJobStatus, server.handleJobStatus)
	return server.requireToken(mux)
}

func (server *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !server.auth.authorize(request) {
			// The realm is announced so a browser client knows the scheme, but
			// nothing about why the credential failed is disclosed.
			writer.Header().Set("WWW-Authenticate", `Bearer realm="cairn-deploy"`)
			server.writeError(writer, http.StatusUnauthorized, CodeUnauthorized,
				"this endpoint requires the deployment bearer token")
			return
		}
		// Deployment answers are never cacheable. A proxy or browser that
		// replayed a stale job status during a maintenance window would be
		// showing an operator a state the host left minutes ago.
		writer.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(writer, request)
	})
}

func (server *Server) handleVersion(writer http.ResponseWriter, request *http.Request) {
	response := VersionResponse{
		SchemaVersion: APISchemaVersion,
		Helper: HelperIdentity{
			Protocol:  releasetrust.HelperProtocol,
			Version:   buildinfo.VersionValue(),
			Commit:    buildinfo.CommitValue(),
			BuildTime: buildinfo.BuildTimeValue(),
		},
		Repo:        server.config.Repo,
		InstallMode: InstallModeSystemdRelease,
		Eligible:    true,
		Current:     server.currentIdentity(request.Context()),
	}
	if active := server.store.Active(); active != nil {
		response.ActiveJobID = active.ID
	}
	server.writeJSON(writer, http.StatusOK, response)
}

// currentIdentity reads /health, tolerating a stopped Core.
//
// A failure here is data, not an error. The settings page has to keep showing
// the installed version while the Core is stopped mid-update, which is exactly
// when /health is unreachable.
func (server *Server) currentIdentity(ctx context.Context) CoreIdentity {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	report, err := server.health.Health(probeCtx)
	if err != nil {
		return CoreIdentity{Reachable: false, Error: err.Error()}
	}
	return CoreIdentity{
		Reachable: true,
		Version:   report.Version,
		Commit:    report.Commit,
		BuildTime: report.BuildTime,
	}
}

func (server *Server) handleCheckUpdates(writer http.ResponseWriter, request *http.Request) {
	force := strings.EqualFold(strings.TrimSpace(request.URL.Query().Get("force")), "true")
	current := server.currentIdentity(request.Context())
	if !force {
		if cached := server.cachedCheck(current); cached != nil {
			server.writeJSON(writer, http.StatusOK, *cached)
			return
		}
	}
	response := server.check(request.Context(), current)
	server.storeCheck(response)
	server.writeJSON(writer, http.StatusOK, response)
}

func (server *Server) cachedCheck(current CoreIdentity) *CheckUpdatesResponse {
	server.cacheMu.Lock()
	defer server.cacheMu.Unlock()
	if server.cached == nil || server.now().Sub(server.cachedAt) > checkCacheTTL {
		return nil
	}
	if server.cached.Current != current {
		return nil
	}
	snapshot := *server.cached
	snapshot.Cached = true
	return &snapshot
}

func (server *Server) storeCheck(response CheckUpdatesResponse) {
	server.cacheMu.Lock()
	defer server.cacheMu.Unlock()
	stored := response
	server.cached = &stored
	server.cachedAt = server.now()
}

// check discovers the highest patch in the current series and verifies it.
//
// Discovery and verification are one step on purpose. Returning a candidate the
// helper has not verified would put a tag and a commit on the confirmation
// screen that nothing had vouched for, and the operator's confirmation is
// supposed to be a confirmation of verified facts.
func (server *Server) check(ctx context.Context, current CoreIdentity) CheckUpdatesResponse {
	response := CheckUpdatesResponse{
		SchemaVersion: APISchemaVersion,
		CheckedAt:     server.now(),
		Current:       current,
	}
	currentRelease, err := currentReleaseFromIdentity(current)
	if err != nil {
		response.DiscoveryError = err.Error()
		response.DisabledReason = server.activeJobDisabledReason()
		if response.DisabledReason == "" {
			response.DisabledReason = "the running Core release series could not be confirmed"
		}
		return response
	}

	discoverCtx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()

	tag, err := server.downloader.LatestTag(discoverCtx, currentRelease.tag)
	if err != nil {
		response.DiscoveryError = err.Error()
		response.DisabledReason = "the newest Core patch in the current release series could not be discovered"
		return response
	}
	signed, err := server.downloader.FetchSignedManifest(discoverCtx, tag)
	if err != nil {
		response.DiscoveryError = err.Error()
		response.DisabledReason = "the signed manifest for " + tag + " could not be fetched"
		return response
	}
	verified, err := server.trust.VerifyRelease(releasetrust.VerifyRequest{
		ManifestBytes:  signed.ManifestBytes,
		SignatureBytes: signed.SignatureBytes,
		ExpectedRepo:   server.config.Repo,
		ExpectedTag:    tag,
		HostOS:         server.hostOS,
		HostArch:       server.hostArch,
		HelperProtocol: releasetrust.HelperProtocol,
	})
	if err != nil {
		response.DiscoveryError = err.Error()
		response.DisabledReason = "the newest Core patch did not verify against the compiled-in trust root"
		return response
	}
	candidate := newCandidate(verified, digestHex(signed.ManifestBytes))
	response.Candidate = &candidate
	candidateRelease, err := parseReleaseTag(tag)
	if err != nil {
		response.DiscoveryError = err.Error()
		response.DisabledReason = "the verified candidate version could not be compared with the running Core"
		response.Candidate = nil
		return response
	}
	if candidateRelease.patch < currentRelease.patch {
		response.DiscoveryError = fmt.Sprintf(
			"the running Core %s is newer than the highest discoverable release %s in its patch series",
			currentRelease.tag, candidateRelease.tag,
		)
		response.DisabledReason = "the running Core identity could not be reconciled with published releases"
		response.Candidate = nil
		return response
	}
	if candidateRelease.patch == currentRelease.patch {
		if verified.Manifest.Commit != current.Commit {
			response.DisabledReason = "the running Core commit does not match the signed release for its reported version"
		}
		return response
	}
	response.UpdateAvailable = true
	response.CanUpdate, response.DisabledReason = server.updatability(verified)
	return response
}

func currentReleaseFromIdentity(identity CoreIdentity) (releaseVersion, error) {
	if !identity.Reachable {
		if identity.Error != "" {
			return releaseVersion{}, fmt.Errorf("the running Core is unreachable: %s", identity.Error)
		}
		return releaseVersion{}, errors.New("the running Core is unreachable")
	}
	version, err := releaseTagFromCoreVersion(identity.Version)
	if err != nil {
		return releaseVersion{}, err
	}
	if !fullCommitPattern.MatchString(identity.Commit) {
		return releaseVersion{}, fmt.Errorf("running Core commit %q is not a full lowercase Git commit", identity.Commit)
	}
	if _, err := time.Parse(time.RFC3339, identity.BuildTime); err != nil {
		return releaseVersion{}, fmt.Errorf("running Core build time %q is not RFC3339: %w", identity.BuildTime, err)
	}
	return version, nil
}

func (server *Server) updatability(verified *releasetrust.VerifiedRelease) (bool, string) {
	if !verified.Manifest.OnlineUpdateCompatible {
		return false, verified.Manifest.OnlineUpdateReason
	}
	if reason := server.activeJobDisabledReason(); reason != "" {
		return false, reason
	}
	return true, ""
}

func (server *Server) activeJobDisabledReason() string {
	if active := server.store.Active(); active != nil && !active.terminal() {
		return "another update is already in progress"
	} else if active != nil && active.State == JobHold {
		return "a previous update is holding and must be resolved by hand first"
	}
	return ""
}

func newCandidate(verified *releasetrust.VerifiedRelease, manifestDigest string) Candidate {
	manifest := verified.Manifest
	return Candidate{
		Tag:                    manifest.Tag,
		Version:                manifest.Version,
		Commit:                 manifest.Commit,
		BuildTime:              manifest.BuildTime,
		ManifestSHA256:         manifestDigest,
		SignatureKeyID:         verified.Key.KeyID,
		SchemaTarget:           manifest.SchemaTarget,
		RiverLedgerTarget:      manifest.RiverLedgerTarget,
		MinimumHelperProtocol:  manifest.MinimumHelperProtocol,
		CoreArchive:            verified.Core.Archive,
		CoreSHA256:             verified.Core.SHA256,
		CoreSizeBytes:          verified.Core.SizeBytes,
		ReaderArchive:          manifest.Reader.Archive,
		ReaderSHA256:           manifest.Reader.SHA256,
		ReaderSizeBytes:        manifest.Reader.SizeBytes,
		OnlineUpdateCompatible: manifest.OnlineUpdateCompatible,
		OnlineUpdateReason:     manifest.OnlineUpdateReason,
		RollbackCompatible:     manifest.RollbackCompatible,
		RollbackReason:         manifest.RollbackReason,
	}
}

func (server *Server) handleSubmitJob(writer http.ResponseWriter, request *http.Request) {
	var body SubmitJobRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, maxRequestBytes))
	// Unknown fields are rejected rather than ignored. A body carrying a field
	// this helper does not implement is a client expecting behaviour it will
	// not get, and silently dropping "url" or "allow_manual" is the worst of
	// the available answers.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		server.writeError(writer, http.StatusBadRequest, CodeInvalidRequest,
			"the request body must be a JSON object with a single target field")
		return
	}
	target := strings.TrimSpace(body.Target)
	if !IsFormalTag(target) {
		server.writeError(writer, http.StatusBadRequest, CodeInvalidTarget,
			"target must be an exact formal release tag such as v1.2.3; channels, branches, prereleases and URLs are not accepted")
		return
	}

	job, deduplicated, err := server.store.Submit(target)
	if errors.Is(err, ErrOperationInProgress) {
		server.writeError(writer, http.StatusConflict, CodeOperationInProgress, err.Error())
		return
	}
	if err != nil {
		server.logger.Error("submit update job", "target", target, "error", err)
		server.writeError(writer, http.StatusInternalServerError, CodeInternal, "the update job could not be recorded")
		return
	}
	if !deduplicated {
		server.launch(job.ID)
	}
	server.writeJSON(writer, http.StatusAccepted, SubmitJobResponse{
		SchemaVersion: APISchemaVersion,
		JobID:         job.ID,
		Target:        job.Target,
		State:         job.State,
		Phase:         job.Phase,
		Deduplicated:  deduplicated,
	})
}

// launch starts the state machine detached from the request.
//
// The context is context.Background(), not the request's. A deployment must not
// be cancellable by closing a browser tab: the dangerous window is the one
// between stopping the service and proving the new one is ready, and a client
// disconnect landing inside it would abandon the installation mid-migration.
// The operation lock and the persisted job record are what a returning client
// reconnects to.
func (server *Server) launch(jobID string) {
	runner := server.newRunner()
	server.launchers.Add(1)
	go func() {
		defer server.launchers.Done()
		runner.Execute(context.Background(), jobID)
	}()
}

// Wait blocks until every launched job has finished. It exists so shutdown does
// not race a running update and so tests can join deterministically.
func (server *Server) Wait() { server.launchers.Wait() }

func (server *Server) handleJobStatus(writer http.ResponseWriter, request *http.Request) {
	job, err := server.store.Get(request.PathValue("id"))
	if errors.Is(err, ErrJobNotFound) {
		server.writeError(writer, http.StatusNotFound, CodeNotFound, "no such job")
		return
	}
	if err != nil {
		server.logger.Error("read update job", "error", err)
		server.writeError(writer, http.StatusInternalServerError, CodeInternal, "the job record could not be read")
		return
	}
	server.writeJSON(writer, http.StatusOK, newJobResponse(job))
}

func (server *Server) writeJSON(writer http.ResponseWriter, status int, payload any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		server.logger.Error("write response", "error", err)
	}
}

func (server *Server) writeError(writer http.ResponseWriter, status int, code, message string) {
	server.writeJSON(writer, status, ErrorResponse{Error: APIError{Code: code, Message: message}})
}

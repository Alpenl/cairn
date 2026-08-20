package scripts

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func loadWorkflow(t *testing.T, name string) map[string]any {
	t.Helper()
	content, err := os.ReadFile("../.github/workflows/" + name)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(content, &workflow); err != nil {
		t.Fatalf("parse workflow: %v", err)
	}
	return workflow
}

func object(t *testing.T, value any, context string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", context, value)
	}
	return result
}

func array(t *testing.T, value any, context string) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, want array", context, value)
	}
	return result
}

func needs(t *testing.T, job map[string]any) map[string]bool {
	t.Helper()
	result := map[string]bool{}
	switch value := job["needs"].(type) {
	case string:
		result[value] = true
	case []any:
		for _, item := range value {
			name, ok := item.(string)
			if !ok {
				t.Fatalf("needs item is %T, want string", item)
			}
			result[name] = true
		}
	default:
		t.Fatalf("needs is %T, want string or array", value)
	}
	return result
}

func requireNeeds(t *testing.T, jobs map[string]any, jobName string, expected ...string) {
	t.Helper()
	job := object(t, jobs[jobName], "job "+jobName)
	actual := needs(t, job)
	for _, dependency := range expected {
		if !actual[dependency] {
			t.Errorf("job %s does not need %s", jobName, dependency)
		}
	}
}

func findStep(t *testing.T, job map[string]any, id string) map[string]any {
	t.Helper()
	for _, raw := range array(t, job["steps"], "steps") {
		step := object(t, raw, "step")
		if step["id"] == id {
			return step
		}
	}
	t.Fatalf("step %s not found", id)
	return nil
}

func findStepByName(t *testing.T, job map[string]any, name string) (int, map[string]any) {
	t.Helper()
	for index, raw := range array(t, job["steps"], "steps") {
		step := object(t, raw, "step")
		if step["name"] == name {
			return index, step
		}
	}
	t.Fatalf("step %q not found", name)
	return -1, nil
}

func TestCoreReleaseValidationPinsTheApprovedSeries(t *testing.T) {
	workflow := loadWorkflow(t, "release-core.yml")
	jobs := object(t, workflow["jobs"], "jobs")
	validate := object(t, jobs["validate"], "validate")
	_, release := findStepByName(t, validate, "Validate stable Core release")
	run, _ := release["run"].(string)

	for _, fragment := range []string{"scripts/core-release-series.sh", "release_major_minor"} {
		if !strings.Contains(run, fragment) {
			t.Errorf("Core release validation does not enforce %q", fragment)
		}
	}
}

func TestCoreReleasePromotionDependsOnExactDigestGates(t *testing.T) {
	workflow := loadWorkflow(t, "release-core.yml")
	jobs := object(t, workflow["jobs"], "jobs")

	requireNeeds(t, jobs, "candidate", "go", "lint", "reader", "database", "codeql", "trivy-source")
	requireNeeds(t, jobs, "trivy-images", "candidate")
	requireNeeds(t, jobs, "prepare-draft", "candidate", "trivy-images")
	requireNeeds(t, jobs, "promote-versions", "candidate", "prepare-draft")
	requireNeeds(t, jobs, "publish-release", "promote-versions")
	requireNeeds(t, jobs, "promote-channels", "publish-release")

	candidate := object(t, jobs["candidate"], "candidate")
	outputs := object(t, candidate["outputs"], "candidate outputs")
	for _, name := range []string{
		"slim_index_digest", "slim_amd64_digest", "slim_arm64_digest",
	} {
		if outputs[name] == nil {
			t.Errorf("candidate does not output %s", name)
		}
	}
	if outputs["full_index_digest"] != nil {
		t.Error("candidate still outputs a full image digest")
	}

	for _, id := range []string{"slim"} {
		step := findStep(t, candidate, id)
		with := object(t, step["with"], id+" with")
		if _, tagged := with["tags"]; tagged {
			t.Errorf("candidate step %s publishes a tag", id)
		}
		output, _ := with["outputs"].(string)
		if !strings.Contains(output, "push-by-digest=true") || !strings.Contains(output, "name-canonical=true") {
			t.Errorf("candidate step %s is not pushed only by canonical digest: %q", id, output)
		}
	}

	imageGate := object(t, jobs["trivy-images"], "trivy-images")
	gateInputs := object(t, imageGate["with"], "trivy-images inputs")
	for _, name := range []string{
		"slim_index_digest", "slim_amd64_digest", "slim_arm64_digest",
	} {
		value, _ := gateInputs[name].(string)
		if !strings.Contains(value, "needs.candidate.outputs."+name) {
			t.Errorf("image gate %s is not sourced from candidate output: %q", name, value)
		}
	}
	if _, ok := gateInputs["full_index_digest"]; ok {
		t.Error("image gate still accepts a full image digest")
	}
}

func TestCoreCandidateExecutesBothArchiveArchitecturesAndChecksLegalBytes(t *testing.T) {
	workflow := loadWorkflow(t, "release-core.yml")
	jobs := object(t, workflow["jobs"], "jobs")
	candidate := object(t, jobs["candidate"], "candidate")

	qemuIndex, _ := findStepByName(t, candidate, "Set up QEMU for cross-architecture archive verification")
	archiveIndex, archive := findStepByName(t, candidate, "Build and verify release archives")
	if qemuIndex >= archiveIndex {
		t.Fatal("QEMU must be registered before both release archives execute --version")
	}
	archiveEnv := object(t, archive["env"], "archive env")
	if archiveEnv["CORE_RELEASE_EXECUTE"] != "true" {
		t.Fatalf("archive verification execution mode = %v, want true", archiveEnv["CORE_RELEASE_EXECUTE"])
	}

	_, images := findStepByName(t, candidate, "Verify every final child image")
	run, _ := images["run"].(string)
	for _, material := range []string{
		"CAIRN_LICENSE.txt", "OPENCC_LICENSE.txt", "OPENCC_SOURCE.txt",
		"GO_WEBTAG_THIRD_PARTY.txt", "GO_MIGRATE_THIRD_PARTY.txt",
		"READER_THIRD_PARTY.txt", "DISTRIBUTION_BOUNDARY.txt",
	} {
		if !strings.Contains(run, material) {
			t.Errorf("final image verification omits %s", material)
		}
	}
	for _, absent := range []string{
		"test ! -e /usr/local/bin/yt-dlp",
		"test ! -e /usr/share/licenses/cairn/YT_DLP_LICENSE.txt",
		"test ! -e /usr/share/licenses/cairn/YT_DLP_SOURCE.txt",
	} {
		if !strings.Contains(run, absent) {
			t.Errorf("final image verification does not forbid %s", absent)
		}
	}
	if !strings.Contains(run, "sha256sum") {
		t.Error("final image verification checks legal presence but not bytes")
	}
}

func TestCoreDraftSealsRollbackAndSecurityEvidenceBeforePromotion(t *testing.T) {
	workflow := loadWorkflow(t, "release-core.yml")
	jobs := object(t, workflow["jobs"], "jobs")
	prepare := object(t, jobs["prepare-draft"], "prepare-draft")
	prepareEnv := object(t, prepare["env"], "prepare-draft env")
	for _, name := range []string{
		"SLIM_INDEX_DIGEST", "SLIM_AMD64_DIGEST", "SLIM_ARM64_DIGEST",
	} {
		value, _ := prepareEnv[name].(string)
		if !strings.Contains(value, "needs.candidate.outputs.") {
			t.Errorf("draft coordinate %s is not sourced from candidate output: %q", name, value)
		}
	}
	if _, ok := prepareEnv["FULL_INDEX_DIGEST"]; ok {
		t.Error("draft still carries a full image digest")
	}

	rollbackIndex, _ := findStepByName(t, prepare, "Seal pre-promotion channel digests")
	securityIndex, security := findStepByName(t, prepare, "Seal security evidence and checksums")
	draftIndex, _ := findStepByName(t, prepare, "Prepare complete draft Release idempotently")
	if rollbackIndex >= securityIndex || securityIndex >= draftIndex {
		t.Fatalf("draft order rollback=%d security=%d draft=%d", rollbackIndex, securityIndex, draftIndex)
	}
	securityRun, _ := security["run"].(string)
	if !strings.Contains(securityRun, "CHANNEL-ROLLBACK.json") || !strings.Contains(securityRun, "SHA256SUMS") {
		t.Error("draft checksums do not bind the pre-promotion channel record")
	}

	promoteChannels := object(t, jobs["promote-channels"], "promote-channels")
	for _, raw := range array(t, promoteChannels["steps"], "promote-channels steps") {
		step := object(t, raw, "promote-channels step")
		if step["name"] == "Record pre-promotion channel digests" {
			t.Error("channel rollback evidence is still added after the Release becomes public")
		}
	}
}

func TestTrivyReleaseMatrixCoversEveryChildAndProducesSBOM(t *testing.T) {
	workflow := loadWorkflow(t, "trivy.yml")
	jobs := object(t, workflow["jobs"], "jobs")
	imageScan := object(t, jobs["image-scan"], "image-scan")
	imageEnv := object(t, imageScan["env"], "image-scan env")
	platformEnv, _ := imageEnv["TRIVY_PLATFORM"].(string)
	if !strings.Contains(platformEnv, "matrix.platform") {
		t.Errorf("Trivy platform is not explicitly bound to the scan matrix: %q", platformEnv)
	}
	strategy := object(t, imageScan["strategy"], "image-scan strategy")
	matrix := object(t, strategy["matrix"], "image-scan matrix")
	entries := array(t, matrix["include"], "image-scan includes")
	if len(entries) != 2 {
		t.Fatalf("image-scan matrix has %d entries, want 2", len(entries))
	}

	seen := map[string]bool{}
	for _, raw := range entries {
		entry := object(t, raw, "image-scan entry")
		variant, _ := entry["variant"].(string)
		arch, _ := entry["arch"].(string)
		child, _ := entry["child_digest"].(string)
		key := variant + "/" + arch
		seen[key] = true
		if !strings.Contains(child, "inputs."+variant+"_"+arch+"_digest") {
			t.Errorf("%s child digest does not use its workflow input: %q", key, child)
		}
	}
	for _, key := range []string{"slim/amd64", "slim/arm64"} {
		if !seen[key] {
			t.Errorf("image-scan matrix omits %s", key)
		}
	}
	if seen["full/amd64"] || seen["full/arm64"] {
		t.Error("image-scan matrix still covers a full image")
	}

	trivyActions := 0
	hasSBOM := false
	hasEvidenceUpload := false
	hasManifestProof := false
	for _, raw := range array(t, imageScan["steps"], "image-scan steps") {
		step := object(t, raw, "image-scan step")
		if step["name"] == "Prove child digest belongs to candidate index" {
			run, _ := step["run"].(string)
			hasManifestProof = strings.Contains(run, "imagetools inspect") &&
				strings.Contains(run, "actual_child") && strings.Contains(run, "CHILD_DIGEST") &&
				strings.Contains(run, "index-manifest.json") && strings.Contains(run, "child-image.json")
		}
		uses, _ := step["uses"].(string)
		if strings.HasPrefix(uses, "aquasecurity/trivy-action@") {
			trivyActions++
			with := object(t, step["with"], "trivy action inputs")
			if with["format"] == "cyclonedx" {
				hasSBOM = true
			}
		}
		if strings.HasPrefix(uses, "actions/upload-artifact@") {
			hasEvidenceUpload = true
		}
	}
	if trivyActions != 2 || !hasSBOM || !hasEvidenceUpload || !hasManifestProof {
		t.Fatalf("image gate actions: trivy=%d sbom=%v evidence=%v manifest-proof=%v",
			trivyActions, hasSBOM, hasEvidenceUpload, hasManifestProof)
	}
}

// The signed release manifest is the trust root the cairn-updater helper links
// against (issue #41). Everything about how the Release produces it has to be
// pinned here, because none of it can be observed offline any other way:
// where in the job it runs, that it is not optional, and that the secret does
// not leak into the rest of the workflow.
func TestCoreReleaseSignsTheManifestBeforeSealingChecksums(t *testing.T) {
	workflow := loadWorkflow(t, "release-core.yml")
	jobs := object(t, workflow["jobs"], "jobs")
	prepare := object(t, jobs["prepare-draft"], "prepare-draft")

	goIndex, _ := findStepByName(t, prepare, "Setup Go for the signed release manifest")
	signIndex, sign := findStepByName(t, prepare, "Sign the canonical release manifest")
	sealIndex, seal := findStepByName(t, prepare, "Seal security evidence and checksums")
	verifyIndex, verify := findStepByName(t, prepare, "Verify the signed release manifest the way the helper will")
	draftIndex, _ := findStepByName(t, prepare, "Prepare complete draft Release idempotently")

	if goIndex >= signIndex {
		t.Fatalf("Go toolchain is set up at %d, after the signing step at %d", goIndex, signIndex)
	}
	if signIndex >= sealIndex {
		t.Fatalf("the manifest is signed at %d, after checksums are sealed at %d", signIndex, sealIndex)
	}
	if sealIndex >= verifyIndex || verifyIndex >= draftIndex {
		t.Fatalf("verification order seal=%d verify=%d draft=%d", sealIndex, verifyIndex, draftIndex)
	}

	// Fail closed: no condition, no tolerated failure. A release that cannot be
	// signed must not reach the draft stage at all.
	if _, conditional := sign["if"]; conditional {
		t.Error("the manifest signing step is conditional: a release could reach draft unsigned")
	}
	if _, tolerated := sign["continue-on-error"]; tolerated {
		t.Error("the manifest signing step tolerates failure")
	}
	signEnv := object(t, sign["env"], "signing step env")
	secret, _ := signEnv["CAIRN_RELEASE_SIGNING_KEY"].(string)
	if !strings.Contains(secret, "secrets.CAIRN_RELEASE_SIGNING_KEY") {
		t.Errorf("the signing key does not come from a repository secret: %q", secret)
	}

	// Verification is the helper's own code path and must need no secret.
	if verifyEnv, ok := verify["env"].(map[string]any); ok {
		if _, leaked := verifyEnv["CAIRN_RELEASE_SIGNING_KEY"]; leaked {
			t.Error("manifest verification is handed the signing secret; it must verify with the public trust root")
		}
	}

	sealRun, _ := seal["run"].(string)
	for _, asset := range []string{"cairn-release-manifest.json", "cairn-release-manifest.json.sig"} {
		if !strings.Contains(sealRun, asset) {
			t.Errorf("SHA256SUMS does not cover %s", asset)
		}
	}
}

// The signing secret must exist in exactly one place in the whole workflow.
// Every extra reference is another chance for it to be echoed, written to a
// file, or handed to a step that does not need it.
func TestCoreReleaseSigningSecretIsReferencedExactlyOnce(t *testing.T) {
	content, err := os.ReadFile("../.github/workflows/release-core.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	if count := strings.Count(string(content), "secrets.CAIRN_RELEASE_SIGNING_KEY"); count != 1 {
		t.Fatalf("the signing secret is referenced %d times, want exactly 1", count)
	}
	entries, err := os.ReadDir("../.github/workflows")
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "release-core.yml" {
			continue
		}
		other, err := os.ReadFile("../.github/workflows/" + entry.Name())
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if strings.Contains(string(other), "CAIRN_RELEASE_SIGNING_KEY") {
			t.Errorf("%s references the release signing key", entry.Name())
		}
	}
}

// The signed manifest and its detached signature are part of the strict Release
// asset set. If they were merely produced but not required, a Release could be
// published without a trust root and the helper would have nothing to check.
func TestSignedManifestIsPartOfTheStrictReleaseAssetSet(t *testing.T) {
	content, err := os.ReadFile("core-release-promote.sh")
	if err != nil {
		t.Fatalf("read promote script: %v", err)
	}
	for _, asset := range []string{"cairn-release-manifest.json", "cairn-release-manifest.json.sig"} {
		if !strings.Contains(string(content), asset) {
			t.Errorf("the strict Release asset set omits %s", asset)
		}
	}
}

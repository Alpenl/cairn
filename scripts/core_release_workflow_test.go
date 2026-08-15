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
		"full_index_digest", "full_amd64_digest", "full_arm64_digest",
		"slim_index_digest", "slim_amd64_digest", "slim_arm64_digest",
	} {
		if outputs[name] == nil {
			t.Errorf("candidate does not output %s", name)
		}
	}

	for _, id := range []string{"full", "slim"} {
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
		"full_index_digest", "full_amd64_digest", "full_arm64_digest",
		"slim_index_digest", "slim_amd64_digest", "slim_arm64_digest",
	} {
		value, _ := gateInputs[name].(string)
		if !strings.Contains(value, "needs.candidate.outputs."+name) {
			t.Errorf("image gate %s is not sourced from candidate output: %q", name, value)
		}
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
		"YT_DLP_LICENSE.txt", "YT_DLP_SOURCE.txt",
	} {
		if !strings.Contains(run, material) {
			t.Errorf("final image verification omits %s", material)
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
		"FULL_INDEX_DIGEST", "FULL_AMD64_DIGEST", "FULL_ARM64_DIGEST",
		"SLIM_INDEX_DIGEST", "SLIM_AMD64_DIGEST", "SLIM_ARM64_DIGEST",
	} {
		value, _ := prepareEnv[name].(string)
		if !strings.Contains(value, "needs.candidate.outputs.") {
			t.Errorf("draft coordinate %s is not sourced from candidate output: %q", name, value)
		}
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
	if len(entries) != 4 {
		t.Fatalf("image-scan matrix has %d entries, want 4", len(entries))
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
	for _, key := range []string{"full/amd64", "full/arm64", "slim/amd64", "slim/arm64"} {
		if !seen[key] {
			t.Errorf("image-scan matrix omits %s", key)
		}
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

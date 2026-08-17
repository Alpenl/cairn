package releasetrust

import (
	"bytes"
	"strings"
	"testing"
)

func TestManifestRoundTripsThroughItsCanonicalBytes(t *testing.T) {
	release := newTestRelease(t)

	parsed, err := ParseManifest(release.ManifestBytes)
	if err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	reencoded, err := parsed.Canonical()
	if err != nil {
		t.Fatalf("re-canonicalise manifest: %v", err)
	}
	if !bytes.Equal(reencoded, release.ManifestBytes) {
		t.Fatal("parsing and re-encoding the manifest changed the signed bytes")
	}
}

// The manifest is a closed schema. An extra member is a rejection, not
// something a helper silently ignores: "ignore what you do not understand" is
// how a downgrade hint gets smuggled past a checker that only reads the fields
// it already knew about.
func TestManifestRejectsUnknownMembers(t *testing.T) {
	release := newTestRelease(t)

	tampered := strings.Replace(string(release.ManifestBytes),
		"  \"artifact_kind\":", "  \"admin_override\": true,\n  \"artifact_kind\":", 1)
	if _, err := ParseManifest([]byte(tampered)); err == nil {
		t.Fatal("a manifest carrying an unknown member was accepted")
	} else if !strings.Contains(err.Error(), "unknown member") {
		t.Fatalf("error %q does not name the unknown member", err)
	}
}

func TestManifestRejectsNonCanonicalBytes(t *testing.T) {
	release := newTestRelease(t)

	compacted := strings.ReplaceAll(string(release.ManifestBytes), "\n  ", "\n")
	if _, err := ParseManifest([]byte(compacted)); err == nil {
		t.Fatal("a re-indented manifest was accepted")
	}
}

//nolint:gocyclo // A table of independent schema rules; each entry is one rule.
func TestManifestValidationRules(t *testing.T) {
	release := newTestRelease(t)
	base := release.Manifest

	for name, testCase := range map[string]struct {
		mutate func(*Manifest)
		want   string
	}{
		"unsupported schema version": {
			func(m *Manifest) { m.SchemaVersion = 2 }, "schema_version",
		},
		"wrong artifact kind": {
			func(m *Manifest) { m.ArtifactKind = "cairn-reader-manifest" }, "artifact_kind",
		},
		"repository is not owner/name": {
			func(m *Manifest) { m.Repo = "cairn" }, "owner/name",
		},
		"pre-release tag": {
			func(m *Manifest) { m.Tag = "v1.2.3-rc1" }, "formal vX.Y.Z",
		},
		"version disagrees with tag": {
			func(m *Manifest) { m.Version = "1.2.4" }, "does not match tag",
		},
		"development placeholder version": {
			func(m *Manifest) { m.Tag, m.Version = "v0.0.0", "0.0.0" }, "development placeholder",
		},
		"short commit": {
			func(m *Manifest) { m.Commit = "0123456789ab" }, "full lowercase Git revision",
		},
		"build time is not RFC3339": {
			func(m *Manifest) { m.BuildTime = "yesterday" }, "RFC3339",
		},
		"helper protocol floor below one": {
			func(m *Manifest) { m.MinimumHelperProtocol = 0 }, "minimum_helper_protocol",
		},
		"schema target is not a step id": {
			func(m *Manifest) { m.SchemaTarget = "HEAD~1" }, "schema_target",
		},
		"river ledger target below one": {
			func(m *Manifest) { m.RiverLedgerTarget = 0 }, "river_ledger_target",
		},
		"online update decision without a reason": {
			func(m *Manifest) { m.OnlineUpdateReason = "  " }, "online_update_reason",
		},
		"rollback decision without a reason": {
			func(m *Manifest) { m.RollbackReason = "" }, "rollback_reason",
		},
		"empty matrix": {
			func(m *Manifest) { m.Core, m.Platforms = nil, nil }, "matrix is empty",
		},
		"platforms disagree with the matrix": {
			func(m *Manifest) { m.Platforms = []string{"linux/amd64"} }, "do not match the core matrix",
		},
		"unsorted matrix": {
			func(m *Manifest) {
				m.Core = []CoreArtifact{m.Core[1], m.Core[0]}
				m.Platforms = []string{m.Core[0].Platform(), m.Core[1].Platform()}
			}, "not sorted",
		},
		"archive name does not encode the version": {
			func(m *Manifest) { m.Core[0].Archive = "cairn.tar.gz" }, "archive",
		},
		"package root outside the archive name": {
			func(m *Manifest) { m.Core[0].PackageRoot = "cairn" }, "package_root",
		},
		"provenance outside the package root": {
			func(m *Manifest) { m.Core[0].ProvenancePath = "BUILD-PROVENANCE.json" }, "provenance_path",
		},
		"a third executable": {
			func(m *Manifest) {
				m.Core[0].Executables["debug-shell"] = m.Core[0].Executables["webtag"]
			}, "want exactly",
		},
		"executable outside the package root": {
			func(m *Manifest) {
				executable := m.Core[0].Executables["webtag"]
				executable.Path = "/usr/local/bin/webtag"
				m.Core[0].Executables["webtag"] = executable
			}, "inside the package root",
		},
		"executable identity from another build": {
			func(m *Manifest) {
				executable := m.Core[0].Executables["migrate"]
				executable.Identity.Commit = strings.Repeat("f", 40)
				m.Core[0].Executables["migrate"] = executable
			}, "identity does not match",
		},
		"version output does not match the identity": {
			func(m *Manifest) {
				executable := m.Core[0].Executables["migrate"]
				executable.Identity.VersionOutput = "cairn 1.2.3"
				m.Core[0].Executables["migrate"] = executable
			}, "version_output",
		},
		"reader archive from another version": {
			func(m *Manifest) { m.Reader.Archive = "cairn-reader-9.9.9.tar.gz" }, "reader archive",
		},
		"reader built from another commit": {
			func(m *Manifest) { m.Reader.Commit = strings.Repeat("a", 40) }, "reader commit",
		},
		"reader missing a deployment shape": {
			func(m *Manifest) { m.Reader.Builds = m.Reader.Builds[:1] }, "want exactly",
		},
		"reader build served from the wrong base path": {
			func(m *Manifest) { m.Reader.Builds[1].BasePath = "/reader/" }, "base_path",
		},
	} {
		manifest := cloneManifest(base)
		testCase.mutate(&manifest)
		err := manifest.Validate()
		if err == nil {
			t.Errorf("%s: manifest was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("%s: error %q does not mention %q", name, err, testCase.want)
		}
	}
}

func cloneManifest(source Manifest) Manifest {
	clone := source
	clone.Platforms = append([]string(nil), source.Platforms...)
	clone.Core = make([]CoreArtifact, 0, len(source.Core))
	for _, artifact := range source.Core {
		copied := artifact
		copied.Executables = map[string]Executable{}
		for name, executable := range artifact.Executables {
			copied.Executables[name] = executable
		}
		clone.Core = append(clone.Core, copied)
	}
	clone.Reader.Builds = append([]ReaderBuild(nil), source.Reader.Builds...)
	return clone
}

// CoreFor is how the helper answers "does this release run on this host".
func TestCoreForSelectsTheHostArchitecture(t *testing.T) {
	release := newTestRelease(t)

	artifact, err := release.Manifest.CoreFor("linux", "arm64")
	if err != nil {
		t.Fatalf("linux/arm64: %v", err)
	}
	if artifact.Arch != "arm64" {
		t.Fatalf("selected %s, want arm64", artifact.Arch)
	}
	if _, err := release.Manifest.CoreFor("linux", "riscv64"); err == nil {
		t.Fatal("an architecture outside the matrix was accepted")
	}
}

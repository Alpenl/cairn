package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"webtag/internal/buildinfo"
	"webtag/internal/releasetrust"
)

func TestVersionDoesNotRequireConfiguration(t *testing.T) {
	// --version has to work before anything is loaded: it is the first thing an
	// operator runs on a host whose configuration is the thing being debugged.
	restore := func(version, commit, buildTime string) func() {
		previous := [3]string{buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime}
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = version, commit, buildTime
		return func() {
			buildinfo.Version, buildinfo.Commit, buildinfo.BuildTime = previous[0], previous[1], previous[2]
		}
	}
	t.Cleanup(restore("1.2.3", "abcdef", "2026-08-14T01:02:03Z"))

	var out bytes.Buffer
	if err := execute([]string{"--version"}, &out); err != nil {
		t.Fatalf("--version: %v", err)
	}
	want := "cairn 1.2.3\ncommit: abcdef\nbuilt: 2026-08-14T01:02:03Z\n"
	if out.String() != want {
		t.Fatalf("expected %q, got %q", want, out.String())
	}
}

func TestTheCommandLineTakesNoTargetOrURL(t *testing.T) {
	// Every argument shape that would represent an install instruction has to
	// be an unknown subcommand. An update is requested through the socket API
	// with an exact tag, and there is no second way in.
	for _, args := range [][]string{
		{"v1.2.3"},
		{"install", "v1.2.3"},
		{"update", "--url", "https://evil.example/x.tar.gz"},
		{"serve", "--repo", "attacker/cairn"},
		{"selfcheck", "/opt/webtag"},
	} {
		var out bytes.Buffer
		if err := execute(args, &out); err == nil {
			t.Fatalf("arguments %v were accepted", args)
		}
	}
}

func TestHelpIsAvailableWithoutConfiguration(t *testing.T) {
	var out bytes.Buffer
	if err := execute([]string{"help"}, &out); err != nil {
		t.Fatalf("help: %v", err)
	}
	if !strings.Contains(out.String(), "exact vX.Y.Z tag") {
		t.Fatalf("the usage must state the target contract, got %q", out.String())
	}
}

func TestNoSubcommandIsAnError(t *testing.T) {
	var out bytes.Buffer
	if err := execute(nil, &out); err == nil {
		t.Fatal("an empty argument list was accepted")
	}
}

// TestProductionTrustIsTheRealPackage pins the wiring the fixture's testTrust
// deliberately stands in for. Without it, a refactor that pointed production at
// a lenient verifier would leave every test green.
func TestProductionTrustIsTheRealPackage(t *testing.T) {
	var trust ReleaseTrust = productionTrust{}

	// An empty request must fail exactly the way releasetrust fails it, which
	// proves the call reaches the real implementation rather than a stub.
	if _, err := trust.VerifyRelease(releasetrust.VerifyRequest{}); err == nil {
		t.Fatal("the production verifier accepted an empty request")
	}
	if _, err := trust.VerifyCoreArchive([]byte("not an archive"), releasetrust.CoreArtifact{
		Archive: "x.tar.gz", SHA256: strings.Repeat("0", 64), SizeBytes: 14,
	}); err == nil {
		t.Fatal("the production verifier accepted a bogus archive")
	}
	if err := trust.VerifyExecutableIdentity("webtag", []byte("cairn 9.9.9"), releasetrust.CoreArtifact{
		Executables: map[string]releasetrust.Executable{
			"webtag": {Identity: releasetrust.Identity{VersionOutput: "cairn 1.2.3"}},
		},
	}); err == nil {
		t.Fatal("the production verifier accepted a mismatched identity")
	}
	if err := trust.VerifyCoreProvenance([]byte(`{}`), releasetrust.Manifest{}, releasetrust.CoreArtifact{}); err == nil {
		t.Fatal("the production verifier accepted empty provenance")
	}
	if _, err := trust.VerifyReaderArchive([]byte("nope"), releasetrust.ReaderArtifact{}); err == nil {
		t.Fatal("the production verifier accepted a bogus Reader archive")
	}
}

// TestTheSelfCheckSubcommandReportsTheLayout drives the operator-facing entry
// point end to end through the environment, which is how the installer will
// verify a freshly provisioned host.
func TestTheSelfCheckSubcommandReportsTheLayout(t *testing.T) {
	host := newHost(t)
	environment := map[string]string{
		envDeployToken:  testDeployToken,
		envDatabaseURL:  host.config.DatabaseURL,
		envSocketPath:   host.config.SocketPath,
		envSocketGroup:  host.config.SocketGroup,
		envStateDir:     host.config.StateDir,
		envCoreDir:      host.config.CoreDir,
		envReaderDir:    host.config.ReaderDir,
		envHelperEnv:    host.config.HelperEnv,
		envCoreEnvGroup: host.config.CoreEnvGroup,
		envServiceUser:  host.config.ServiceUser,
		envOwnerUID:     itoa(host.config.OwnerUID),
	}
	for name, value := range environment {
		t.Setenv(name, value)
	}

	var out bytes.Buffer
	if err := runSelfCheck(nil, &out); err != nil {
		t.Fatalf("selfcheck on a correct host failed: %v", err)
	}
	if !strings.Contains(out.String(), "satisfies its permission model") {
		t.Fatalf("selfcheck said %q", out.String())
	}

	// And it fails, loudly and specifically, on a host that drifted.
	if err := os.Chmod(host.config.ReleasesDir(), 0o777); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	out.Reset()
	err := runSelfCheck(nil, &out)
	if err == nil {
		t.Fatal("selfcheck passed on a world-writable releases directory")
	}
	if !strings.Contains(err.Error(), filepath.Base(host.config.ReleasesDir())) {
		t.Fatalf("selfcheck must name the offending path, got %v", err)
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	negative := value < 0
	if negative {
		value = -value
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	if negative {
		return "-" + digits
	}
	return digits
}

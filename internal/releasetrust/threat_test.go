package releasetrust

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The threat tests issue #41 stage 0 requires. Each one builds the exact
// corruption it names and then asserts which check rejected it — a test that
// only asserted "an error happened" would keep passing if the wrong check
// fired, or if the only thing that noticed was an unrelated shape rule.

// Threat 1: the attacker replaces a Release asset *and* SHA256SUMS.
//
// This is the reason the manifest exists. Both files live in the same GitHub
// Release, so whoever can swap one can swap the other, and a helper that
// trusted SHA256SUMS would see a perfectly self-consistent Release. The signed
// manifest is what breaks the tie, and the attacker cannot forge it without a
// key from the compiled-in trust root.
func TestTamperedArchiveWithMatchingChecksumsIsCaughtOnlyByTheSignedManifest(t *testing.T) {
	release := newTestRelease(t)

	// The attacker rebuilds linux/amd64 with a trojaned webtag and recomputes
	// SHA256SUMS so the Release stays internally consistent.
	packageRoot, files := coreArchiveFiles("amd64")
	for index, file := range files {
		if file.path == packageRoot+"/webtag" {
			files[index].data = []byte("ELF-webtag-amd64-with-backdoor")
		}
	}
	trojaned := buildTarGz(t, files)
	archiveName := packageRoot + ".tar.gz"
	release.CoreArchives[archiveName] = trojaned
	tamperedSums := release.checksums()

	// SHA256SUMS is happy: it parses, and every digest matches the bytes on
	// disk. A checksum-rooted helper would install the backdoor here.
	checksums, err := ParseChecksums(tamperedSums)
	if err != nil {
		t.Fatalf("the tampered checksum list does not even parse: %v", err)
	}
	if checksums[archiveName] != sha256Hex(trojaned) {
		t.Fatal("the tampered checksum list does not describe the tampered archive; the test is not testing the threat")
	}

	// The signed manifest is not happy.
	artifact, err := release.Manifest.CoreFor("linux", "amd64")
	if err != nil {
		t.Fatalf("select platform: %v", err)
	}
	_, err = VerifyCoreArchive(trojaned, artifact)
	if err == nil {
		t.Fatal("the trojaned archive passed manifest verification")
	}
	if !strings.Contains(err.Error(), "signed manifest") {
		t.Fatalf("rejection %q is not a signed-manifest check", err)
	}
	// Isolate the digest check from the size check: even when the attacker
	// pads the archive back to the original length, the hash still decides.
	digestOnly := VerifyArchiveBytes(trojaned, artifact.SHA256, int64(len(trojaned)))
	if digestOnly == nil {
		t.Fatal("the trojaned archive matched the signed digest")
	}
	if !strings.Contains(digestOnly.Error(), "does not match the signed manifest digest") {
		t.Fatalf("rejection %q is not the manifest digest check", digestOnly)
	}
	if err := CrossCheckChecksums(release.Manifest, checksums); err == nil {
		t.Fatal("SHA256SUMS and the signed manifest were reported as agreeing")
	}

	// The signature over the untouched manifest still verifies, so the helper
	// knows the manifest is authentic and the archive is not.
	if _, err := VerifyRelease(release.request()); err != nil {
		t.Fatalf("the untouched manifest stopped verifying: %v", err)
	}

	// The attacker's remaining move is to re-sign a manifest that describes the
	// trojan. Without a trusted key that fails at signature verification.
	attackerSeed := make([]byte, ed25519.SeedSize)
	for index := range attackerSeed {
		attackerSeed[index] = 0x5A
	}
	attackerKey := ed25519.NewKeyFromSeed(attackerSeed)
	forgedManifest := cloneManifest(release.Manifest)
	forgedManifest.Core[0].SHA256 = sha256Hex(trojaned)
	forgedManifest.Core[0].SizeBytes = int64(len(trojaned))
	forgedBytes, err := forgedManifest.Canonical()
	if err != nil {
		t.Fatalf("canonicalise forged manifest: %v", err)
	}
	if _, err := SignManifest(forgedBytes, attackerKey); err == nil {
		t.Fatal("an untrusted key produced an acceptable release signature")
	}
	forgedSignature := Signature{
		SchemaVersion:  SignatureSchemaVersion,
		ArtifactKind:   SignatureArtifactKind,
		Algorithm:      SignatureAlgorithm,
		KeyID:          release.KeyID,
		ManifestSHA256: sha256Hex(forgedBytes),
		Signature:      ed25519.Sign(attackerKey, forgedBytes),
	}
	forgedSignatureBytes, err := forgedSignature.Canonical()
	if err != nil {
		t.Fatalf("canonicalise forged signature: %v", err)
	}
	request := release.request()
	request.ManifestBytes = forgedBytes
	request.SignatureBytes = forgedSignatureBytes
	if _, err := VerifyRelease(request); err == nil {
		t.Fatal("a manifest signed by an untrusted key was accepted")
	} else if !strings.Contains(err.Error(), "does not verify under trusted key") {
		t.Fatalf("rejection %q is not the signature check", err)
	}
}

// Threat 2: wrong signature, wrong repository, wrong ref, wrong architecture.
func TestVerificationRejectsWrongSignatureRepositoryRefAndArchitecture(t *testing.T) {
	release := newTestRelease(t)

	if _, err := VerifyRelease(release.request()); err != nil {
		t.Fatalf("the healthy release did not verify: %v", err)
	}

	t.Run("wrong signature", func(t *testing.T) {
		// A signature over a different manifest, presented with this one.
		other := cloneManifest(release.Manifest)
		other.RollbackReason = "a different reason produces a different document"
		otherBytes, err := other.Canonical()
		if err != nil {
			t.Fatalf("canonicalise: %v", err)
		}
		otherSignature, err := SignManifest(otherBytes, release.SigningKey)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		otherSignatureBytes, err := otherSignature.Canonical()
		if err != nil {
			t.Fatalf("canonicalise signature: %v", err)
		}
		request := release.request()
		request.SignatureBytes = otherSignatureBytes
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("a signature over another manifest was accepted")
		}

		// A single flipped bit in the signature itself.
		flipped := release.Signature
		flipped.Signature = append([]byte(nil), flipped.Signature...)
		flipped.Signature[0] ^= 0x01
		flippedBytes, err := flipped.Canonical()
		if err != nil {
			t.Fatalf("canonicalise flipped signature: %v", err)
		}
		request = release.request()
		request.SignatureBytes = flippedBytes
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("a corrupted signature was accepted")
		} else if !strings.Contains(err.Error(), "does not verify under trusted key") {
			t.Fatalf("rejection %q is not the signature check", err)
		}

		// A signature naming a key that is not compiled in.
		renamed := release.Signature
		renamed.KeyID = "cairn-release-2099z"
		renamedBytes, err := renamed.Canonical()
		if err != nil {
			t.Fatalf("canonicalise renamed signature: %v", err)
		}
		request = release.request()
		request.SignatureBytes = renamedBytes
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("a signature naming an unknown key was accepted")
		} else if !strings.Contains(err.Error(), "not a trusted key") {
			t.Fatalf("rejection %q is not the trust root lookup", err)
		}
	})

	t.Run("wrong repository", func(t *testing.T) {
		// A manifest correctly signed, but for another repository.
		foreign := cloneManifest(release.Manifest)
		foreign.Repo = "attacker/cairn"
		foreignBytes, err := foreign.Canonical()
		if err != nil {
			t.Fatalf("canonicalise: %v", err)
		}
		foreignSignature, err := SignManifest(foreignBytes, release.SigningKey)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		foreignSignatureBytes, err := foreignSignature.Canonical()
		if err != nil {
			t.Fatalf("canonicalise signature: %v", err)
		}
		request := release.request()
		request.ManifestBytes = foreignBytes
		request.SignatureBytes = foreignSignatureBytes
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("a manifest for another repository was accepted")
		} else if !strings.Contains(err.Error(), "only installs") {
			t.Fatalf("rejection %q is not the repository check", err)
		}

		// The helper must also refuse to verify without knowing what it wants.
		request = release.request()
		request.ExpectedRepo = ""
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("verification without an expected repository was accepted")
		}
	})

	t.Run("wrong ref", func(t *testing.T) {
		request := release.request()
		request.ExpectedTag = "v1.2.4"
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("a manifest for another tag was accepted")
		} else if !strings.Contains(err.Error(), "the authorised target is") {
			t.Fatalf("rejection %q is not the exact-target check", err)
		}

		for _, ref := range []string{"latest", "main", "v1.2", "v1.2.3-rc1", "V1.2.3", ""} {
			request := release.request()
			request.ExpectedTag = ref
			if _, err := VerifyRelease(request); err == nil {
				t.Errorf("informal ref %q was accepted as an execution target", ref)
			}
		}
	})

	t.Run("wrong architecture", func(t *testing.T) {
		request := release.request()
		request.HostArch = "riscv64"
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("a host outside the release matrix was accepted")
		} else if !strings.Contains(err.Error(), "does not ship") {
			t.Fatalf("rejection %q is not the matrix check", err)
		}

		request = release.request()
		request.HostOS = "darwin"
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("a non-linux host was accepted")
		}

		// Selecting the wrong entry must not verify either: the amd64 archive
		// does not satisfy the arm64 matrix entry.
		arm64, err := release.Manifest.CoreFor("linux", "arm64")
		if err != nil {
			t.Fatalf("select arm64: %v", err)
		}
		if _, err := VerifyCoreArchive(release.CoreArchives["cairn_1.2.3_linux_amd64.tar.gz"], arm64); err == nil {
			t.Fatal("the amd64 archive satisfied the arm64 matrix entry")
		}
	})

	t.Run("helper protocol below the release floor", func(t *testing.T) {
		request := release.request()
		request.HelperProtocol = HelperProtocol - 1
		if _, err := VerifyRelease(request); err == nil {
			t.Fatal("an outdated helper was allowed to install the release")
		} else if !strings.Contains(err.Error(), "upgrade the helper manually") {
			t.Fatalf("rejection %q does not point at the manual helper upgrade", err)
		}
	})
}

// Threat 3: a checksum list that names the same asset twice.
//
// Two entries mean two answers. A reader that keeps the first and a reader that
// keeps the last disagree about what was verified, and the attacker chooses
// which reader is right by choosing which entry to add.
func TestDuplicateChecksumEntriesAreRejected(t *testing.T) {
	release := newTestRelease(t)

	healthy := release.checksums()
	if _, err := ParseChecksums(healthy); err != nil {
		t.Fatalf("the healthy checksum list was rejected: %v", err)
	}

	archiveName := "cairn_1.2.3_linux_amd64.tar.gz"
	duplicate := append(append([]byte(nil), healthy...),
		[]byte(fmt.Sprintf("%s  %s\n", sha256Hex([]byte("attacker payload")), archiveName))...)
	_, err := ParseChecksums(duplicate)
	if err == nil {
		t.Fatal("a checksum list naming the same asset twice was accepted")
	}
	if !strings.Contains(err.Error(), "twice") || !strings.Contains(err.Error(), archiveName) {
		t.Fatalf("rejection %q does not name the duplicated asset", err)
	}

	// A duplicate that repeats the *same* digest is still ambiguous input and
	// is still rejected: "harmless duplicate" is a judgement the parser should
	// not be making.
	repeated := append(append([]byte(nil), healthy...),
		[]byte(fmt.Sprintf("%s  %s\n", sha256Hex(release.CoreArchives[archiveName]), archiveName))...)
	if _, err := ParseChecksums(repeated); err == nil {
		t.Fatal("a repeated identical checksum entry was accepted")
	}

	for name, list := range map[string]string{
		"uppercase digest": strings.ToUpper(sha256Hex([]byte("x"))) + "  a.tar.gz\n",
		"truncated digest": "deadbeef  a.tar.gz\n",
		"path traversal":   sha256Hex([]byte("x")) + "  ../../etc/passwd\n",
		"no separator":     sha256Hex([]byte("x")) + "\n",
		"empty list":       "\n\n",
	} {
		if _, err := ParseChecksums([]byte(list)); err == nil {
			t.Errorf("%s checksum list was accepted", name)
		}
	}
}

// Threat 4: the package carries an executable beyond webtag and migrate.
//
// The helper unpacks as root and the switch is atomic, so anything executable
// in the tree becomes something that can be launched on the production host.
// Two declared programs means exactly two executable files, and the check runs
// even on a correctly signed archive — a signature proves who built it, not
// that what they built is what the contract allows.
func TestExtraExecutableInsideThePackageIsRejected(t *testing.T) {
	release := newTestRelease(t)

	packageRoot, files := coreArchiveFiles("amd64")
	files = append(files,
		archiveFile{path: packageRoot + "/tools/", mode: 0o755},
		archiveFile{path: packageRoot + "/tools/debug-shell", data: []byte("ELF-shell"), mode: 0o755})
	withExtra := buildTarGz(t, files)

	// Re-sign so the manifest genuinely describes these bytes: the point is
	// that the packaging rule is enforced independently of the signature.
	manifest := cloneManifest(release.Manifest)
	manifest.Core[0].SHA256 = sha256Hex(withExtra)
	manifest.Core[0].SizeBytes = int64(len(withExtra))
	release.sign(t, manifest)

	verified, err := VerifyRelease(release.request())
	if err != nil {
		t.Fatalf("the re-signed manifest did not verify: %v", err)
	}
	_, err = VerifyCoreArchive(withExtra, verified.Core)
	if err == nil {
		t.Fatal("a package carrying a third executable was accepted")
	}
	if !strings.Contains(err.Error(), "debug-shell") || !strings.Contains(err.Error(), "carries executables") {
		t.Fatalf("rejection %q does not name the undeclared executable", err)
	}

	// A non-executable extra file is not by itself a finding; the rule is about
	// executables, and stating it this way keeps the test from passing for the
	// wrong reason.
	_, benign := coreArchiveFiles("amd64")
	benign = append(benign, archiveFile{path: packageRoot + "/README-not-tracked", data: []byte("notes"), mode: 0o644})
	withBenign := buildTarGz(t, benign)
	benignManifest := cloneManifest(release.Manifest)
	benignManifest.Core[0].SHA256 = sha256Hex(withBenign)
	benignManifest.Core[0].SizeBytes = int64(len(withBenign))
	release.sign(t, benignManifest)
	benignVerified, err := VerifyRelease(release.request())
	if err != nil {
		t.Fatalf("verify benign release: %v", err)
	}
	if _, err := VerifyCoreArchive(withBenign, benignVerified.Core); err != nil {
		t.Fatalf("a package with a non-executable extra file was rejected: %v", err)
	}

	// The static Reader tree must carry no executable at all.
	readerFiles := append(readerArchiveFiles(),
		archiveFile{path: "root/postinstall.sh", data: []byte("#!/bin/sh\n"), mode: 0o755})
	readerWithExecutable := buildTarGz(t, readerFiles)
	if err := VerifyReaderArchiveContents(mustInspect(t, readerWithExecutable), release.Manifest.Reader); err == nil {
		t.Fatal("an executable inside the static Reader tree was accepted")
	}
}

// Threat 5: an archive that escapes its package root or is not a plain file.
// This is adjacent to threat 4 and belongs to the same "unpack as root" family.
func TestArchiveEntriesThatEscapeOrAreNotPlainFilesAreRejected(t *testing.T) {
	release := newTestRelease(t)
	artifact, err := release.Manifest.CoreFor("linux", "amd64")
	if err != nil {
		t.Fatalf("select platform: %v", err)
	}

	packageRoot, _ := coreArchiveFiles("amd64")
	for name, entry := range map[string]archiveFile{
		"parent escape":   {path: packageRoot + "/../evil", data: []byte("x"), mode: 0o644},
		"absolute path":   {path: "/etc/cron.d/evil", data: []byte("x"), mode: 0o644},
		"outside root":    {path: "elsewhere/evil", data: []byte("x"), mode: 0o644},
		"embedded dotdot": {path: packageRoot + "/legal/../../evil", data: []byte("x"), mode: 0o644},
	} {
		_, files := coreArchiveFiles("amd64")
		archive := buildTarGz(t, append(files, entry))
		contents, inspectErr := InspectArchive(newReader(archive))
		if inspectErr != nil {
			continue // rejected during inspection, which is the earliest possible point
		}
		if err := VerifyCoreArchiveContents(contents, artifact); err == nil {
			t.Errorf("%s entry was accepted", name)
		}
	}
}

// Threat 6: the download is redirected off the release domain.
//
// GitHub really does redirect asset downloads to its object store, so blanket
// "no redirects" is not an option. The policy therefore checks every hop, with
// exact host equality: a suffix match would accept evil-githubusercontent.com
// and a substring match would accept github.com.attacker.net.
func TestDownloadRedirectsOffTheAllowListAreRejected(t *testing.T) {
	t.Parallel()

	assetURL, err := AssetURL(testRepo, testTag, ManifestFileName)
	if err != nil {
		t.Fatalf("build asset URL: %v", err)
	}
	if assetURL.String() != "https://github.com/Alpenl/cairn/releases/download/v1.2.3/"+ManifestFileName {
		t.Fatalf("asset URL drifted: %s", assetURL)
	}

	original := &http.Request{URL: assetURL}
	for name, target := range map[string]string{
		"github object store": "https://objects.githubusercontent.com/blob/1",
		"release assets host": "https://release-assets.githubusercontent.com/blob/1",
	} {
		if err := CheckDownloadRedirect(&http.Request{URL: mustParseURL(t, target)}, []*http.Request{original}); err != nil {
			t.Errorf("%s redirect was rejected: %v", name, err)
		}
	}

	for name, target := range map[string]string{
		"foreign domain":       "https://evil.example.com/cairn.tar.gz",
		"lookalike suffix":     "https://evil-githubusercontent.com/blob/1",
		"lookalike subdomain":  "https://github.com.attacker.net/blob/1",
		"plaintext downgrade":  "http://github.com/Alpenl/cairn/releases/download/v1.2.3/x",
		"embedded credentials": "https://user:pass@github.com/Alpenl/cairn/releases/download/v1.2.3/x",
		"file scheme":          "file:///etc/passwd",
	} {
		err := CheckDownloadRedirect(&http.Request{URL: mustParseURL(t, target)}, []*http.Request{original})
		if err == nil {
			t.Errorf("%s redirect was accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "redirected off the allow-list") {
			t.Errorf("%s rejection %q is not the redirect policy", name, err)
		}
	}

	chain := make([]*http.Request, MaxDownloadRedirects)
	for index := range chain {
		chain[index] = original
	}
	if err := CheckDownloadRedirect(&http.Request{URL: assetURL}, chain); err == nil {
		t.Fatal("an unbounded redirect chain was accepted")
	}

	for name, asset := range map[string]string{
		"path traversal": "../../../etc/passwd",
		"nested path":    "assets/cairn.tar.gz",
		"query string":   "cairn.tar.gz?token=x",
		"fragment":       "cairn.tar.gz#x",
		"empty":          "",
	} {
		if _, err := AssetURL(testRepo, testTag, asset); err == nil {
			t.Errorf("%s asset name was accepted", name)
		}
	}
	for _, ref := range []string{"latest", "main", "v1.2.3-rc1"} {
		if _, err := AssetURL(testRepo, ref, ManifestFileName); err == nil {
			t.Errorf("informal ref %q produced a download URL", ref)
		}
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return parsed
}

func mustInspect(t *testing.T, archive []byte) *ArchiveContents {
	t.Helper()
	contents, err := InspectArchive(newReader(archive))
	if err != nil {
		t.Fatalf("inspect archive: %v", err)
	}
	return contents
}

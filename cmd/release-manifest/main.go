// Command release-manifest produces and checks the signed canonical release
// manifest the cairn-updater helper trusts (issue #41 stage 0).
//
// It is release tooling, not a shipped program: the Core archives still carry
// exactly two executables, webtag and migrate. This command runs on the release
// runner, reads the archives that were already built and verified, binds them
// to the migration targets compiled into the same commit, and signs the result
// with the Ed25519 key held in the CAIRN_RELEASE_SIGNING_KEY secret.
//
// Without that secret it fails. A release that cannot be signed is not
// published unsigned and is not published with a "signature pending" marker;
// there is no state in which the helper has to decide what an unsigned manifest
// means.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"webtag/internal/releasetrust"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "generate":
		err = runGenerate(os.Args[2:])
	case "preview":
		err = runPreview(os.Args[2:], os.Stdout)
	case "verify":
		err = runVerify(os.Args[2:])
	case "keygen":
		err = runKeygen(os.Args[2:], os.Stdout)
	case "-h", "--help", "help":
		fmt.Println(usage)
		return
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "release-manifest: %v\n", err)
		os.Exit(1)
	}
}

const usage = `Usage:
  release-manifest generate --dist DIR --repo OWNER/NAME --tag vX.Y.Z --commit SHA --build-time RFC3339
                            [--previous-schema-target ID --previous-river-target N] [--out DIR]
  release-manifest preview  (same flags as generate, minus --out)
  release-manifest verify --dist DIR --repo OWNER/NAME --tag vX.Y.Z [--host-arch ARCH]
  release-manifest keygen

generate signs the manifest with the Ed25519 seed in ` + releasetrust.SigningKeyEnv + `.
It fails when that variable is unset: releases fail closed rather than ship unsigned.

preview writes the same canonical bytes to stdout and needs no secret. It never
creates a file and never produces a signature, so it cannot be mistaken for a
release artifact; it exists so the canonical encoding can be checked offline.

verify re-reads the produced manifest, signature, and archives the way the
helper will, and needs no secret.

keygen prints a new key pair for rotation. The seed goes to a secret store, the
public key goes into internal/releasetrust/keys.go alongside the outgoing key.`

// manifestFlags is the input surface shared by generate and preview. Keeping it
// in one place means the previewed bytes are produced by exactly the same path
// as the signed ones; two flag sets would eventually diverge and the preview
// would stop proving anything about the release.
type manifestFlags struct {
	set    *flag.FlagSet
	inputs *manifestInputs
	out    *string
}

func newManifestFlags(name string, withOutput bool) manifestFlags {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	inputs := &manifestInputs{}
	set.StringVar(&inputs.DistDir, "dist", "dist", "directory holding the built release archives")
	set.StringVar(&inputs.Repo, "repo", "", "owner/name of the release repository")
	set.StringVar(&inputs.Tag, "tag", "", "the exact formal release tag, vX.Y.Z")
	set.StringVar(&inputs.Commit, "commit", "", "the full 40 character release commit")
	set.StringVar(&inputs.BuildTime, "build-time", "", "the RFC3339 release build time")
	set.StringVar(&inputs.PreviousTarget, "previous-schema-target", "",
		"the previous release's terminal migration step id, if known")
	set.IntVar(&inputs.PreviousRiverTarget, "previous-river-target", 0,
		"the previous release's River ledger target, if known")
	flags := manifestFlags{set: set, inputs: inputs}
	if withOutput {
		flags.out = set.String("out", "",
			"directory to write the manifest and signature into (default: --dist)")
	}
	return flags
}

func runPreview(args []string, output io.Writer) error {
	flags := newManifestFlags("preview", false)
	if err := flags.set.Parse(args); err != nil {
		return err
	}
	manifest, err := buildManifest(*flags.inputs)
	if err != nil {
		return err
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		return err
	}
	_, err = output.Write(canonical)
	return err
}

func runGenerate(args []string) error {
	flags := newManifestFlags("generate", true)
	if err := flags.set.Parse(args); err != nil {
		return err
	}
	outputDir := *flags.out
	if outputDir == "" {
		outputDir = flags.inputs.DistDir
	}

	manifest, err := buildManifest(*flags.inputs)
	if err != nil {
		return err
	}
	canonical, err := manifest.Canonical()
	if err != nil {
		return err
	}

	seed, present := os.LookupEnv(releasetrust.SigningKeyEnv)
	if !present {
		return fmt.Errorf("%s is not set: a release manifest is signed or it is not produced",
			releasetrust.SigningKeyEnv)
	}
	privateKey, err := releasetrust.DecodeSigningSeed(seed)
	if err != nil {
		return err
	}
	signature, err := releasetrust.SignManifest(canonical, privateKey)
	if err != nil {
		return err
	}
	signatureBytes, err := signature.Canonical()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	manifestPath := filepath.Join(outputDir, releasetrust.ManifestFileName)
	signaturePath := filepath.Join(outputDir, releasetrust.SignatureFileName)
	if err := os.WriteFile(manifestPath, canonical, 0o644); err != nil { //nolint:gosec // Release assets are world readable by design.
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.WriteFile(signaturePath, signatureBytes, 0o644); err != nil { //nolint:gosec // Release assets are world readable by design.
		return fmt.Errorf("write signature: %w", err)
	}
	fmt.Printf("signed %s for %s with key %s\n", releasetrust.ManifestFileName, manifest.Tag, signature.KeyID)
	fmt.Printf("  schema target        %s\n", manifest.SchemaTarget)
	fmt.Printf("  river ledger target  %d\n", manifest.RiverLedgerTarget)
	fmt.Printf("  online update        %t (%s)\n", manifest.OnlineUpdateCompatible, manifest.OnlineUpdateReason)
	fmt.Printf("  rollback             %t (%s)\n", manifest.RollbackCompatible, manifest.RollbackReason)
	fmt.Printf("  platforms            %v\n", manifest.Platforms)
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	dist := flags.String("dist", "dist", "directory holding the release assets")
	repo := flags.String("repo", "", "owner/name the helper would accept")
	tag := flags.String("tag", "", "the exact formal release tag the helper was authorised for")
	hostArch := flags.String("host-arch", "", "architecture to verify for (default: every platform in the matrix)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return verifyRelease(*dist, *repo, *tag, *hostArch, os.Stdout)
}

func runKeygen(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("keygen takes no arguments")
	}
	return generateKeyPair(output)
}

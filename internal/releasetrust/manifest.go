package releasetrust

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// ManifestArtifactKind tags the signed document so a signature made over
	// some other canonical Cairn artifact can never be replayed as a release
	// manifest.
	ManifestArtifactKind = "cairn-release-manifest"

	// ManifestSchemaVersion is the only manifest schema this package accepts.
	// A helper that encounters a different value must stop rather than guess.
	ManifestSchemaVersion = 1

	// ManifestFileName and SignatureFileName are the fixed Release asset names.
	// The helper never learns an asset URL from the network; it derives both
	// from the exact tag and these constants.
	ManifestFileName  = "cairn-release-manifest.json"
	SignatureFileName = "cairn-release-manifest.json.sig"

	// HelperProtocol is the deployment protocol this code speaks. The helper
	// compares it against the manifest floor and refuses to act when the
	// release requires something newer than the installed helper.
	HelperProtocol = 1

	// ReleaseOS is the only operating system Core releases target.
	ReleaseOS = "linux"
)

// ExecutableNames are the two programs a Core archive must ship, and the only
// two it may ship. They stay separate binaries on purpose: the updater does not
// get a `webtag migrate` subcommand.
var ExecutableNames = []string{"migrate", "webtag"}

// ReaderBuildNames are the two Reader deployment shapes carried by one Core
// release. root/ serves reader.alpenl.com; embedded/ is the /reader/ smoke
// build compiled into the backend binary.
var ReaderBuildNames = []string{"embedded", "root"}

var (
	repoPattern         = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)
	formalTagPattern    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	commitPattern       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Pattern       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	migrationIDPattern  = regexp.MustCompile(`^[0-9a-z]+$`)
	architecturePattern = regexp.MustCompile(`^[a-z0-9]+$`)
	readerReleasePrefix = "reader-"
)

// Identity is what an executable reports for `--version`. version_output is
// stored verbatim so the helper compares process output byte for byte instead
// of re-deriving a format string it might get subtly wrong.
type Identity struct {
	Version       string
	Commit        string
	BuildTime     string
	VersionOutput string
}

// ExpectedVersionOutput is the identity string every Core executable prints.
// It mirrors internal/buildinfo.Identity, duplicated here so the helper does
// not have to link the application to check it.
func ExpectedVersionOutput(version, commit, buildTime string) string {
	return fmt.Sprintf("cairn %s\ncommit: %s\nbuilt: %s", version, commit, buildTime)
}

// Executable is one program inside a Core archive.
type Executable struct {
	Path     string
	SHA256   string
	Identity Identity
}

// CoreArtifact is one OS/arch entry of the release matrix.
type CoreArtifact struct {
	OS               string
	Arch             string
	Archive          string
	SHA256           string
	SizeBytes        int64
	PackageRoot      string
	ProvenancePath   string
	ProvenanceSHA256 string
	Executables      map[string]Executable
}

// Platform is the "os/arch" key used by the matrix.
func (artifact CoreArtifact) Platform() string {
	return artifact.OS + "/" + artifact.Arch
}

// ReaderBuild is one deployment shape inside the Reader archive.
type ReaderBuild struct {
	Name       string
	BasePath   string
	Directory  string
	FileCount  int64
	TotalBytes int64
}

// ReaderArtifact is the standalone Reader archive that ships with the same Core
// commit. It has no independent version line; it is a Core sub-artifact that
// can be switched atomically on its own.
type ReaderArtifact struct {
	Archive          string
	SHA256           string
	SizeBytes        int64
	ReleaseID        string
	Commit           string
	ProvenancePath   string
	ProvenanceSHA256 string
	Builds           []ReaderBuild
}

// Manifest is the signed release contract between the Release workflow and the
// cairn-updater helper.
type Manifest struct {
	SchemaVersion          int
	ArtifactKind           string
	Repo                   string
	Tag                    string
	Version                string
	Commit                 string
	BuildTime              string
	MinimumHelperProtocol  int
	SchemaTarget           string
	RiverLedgerTarget      int
	OnlineUpdateCompatible bool
	OnlineUpdateReason     string
	RollbackCompatible     bool
	RollbackReason         string
	Platforms              []string
	Core                   []CoreArtifact
	Reader                 ReaderArtifact
}

// CanonicalValue renders the manifest as a canonical value tree.
func (manifest Manifest) CanonicalValue() CanonicalValue {
	core := make(CanonicalArray, 0, len(manifest.Core))
	for _, artifact := range manifest.Core {
		executables := make(CanonicalObject, 0, len(artifact.Executables))
		for name, executable := range artifact.Executables {
			executables = append(executables, CanonicalField{Key: name, Value: CanonicalObject{
				{Key: "identity", Value: CanonicalObject{
					{Key: "build_time", Value: CanonicalString(executable.Identity.BuildTime)},
					{Key: "commit", Value: CanonicalString(executable.Identity.Commit)},
					{Key: "version", Value: CanonicalString(executable.Identity.Version)},
					{Key: "version_output", Value: CanonicalString(executable.Identity.VersionOutput)},
				}},
				{Key: "path", Value: CanonicalString(executable.Path)},
				{Key: "sha256", Value: CanonicalString(executable.SHA256)},
			}})
		}
		core = append(core, CanonicalObject{
			{Key: "arch", Value: CanonicalString(artifact.Arch)},
			{Key: "archive", Value: CanonicalString(artifact.Archive)},
			{Key: "executables", Value: executables},
			{Key: "os", Value: CanonicalString(artifact.OS)},
			{Key: "package_root", Value: CanonicalString(artifact.PackageRoot)},
			{Key: "provenance_path", Value: CanonicalString(artifact.ProvenancePath)},
			{Key: "provenance_sha256", Value: CanonicalString(artifact.ProvenanceSHA256)},
			{Key: "sha256", Value: CanonicalString(artifact.SHA256)},
			{Key: "size_bytes", Value: CanonicalInt(artifact.SizeBytes)},
		})
	}

	builds := make(CanonicalArray, 0, len(manifest.Reader.Builds))
	for _, build := range manifest.Reader.Builds {
		builds = append(builds, CanonicalObject{
			{Key: "base_path", Value: CanonicalString(build.BasePath)},
			{Key: "directory", Value: CanonicalString(build.Directory)},
			{Key: "file_count", Value: CanonicalInt(build.FileCount)},
			{Key: "name", Value: CanonicalString(build.Name)},
			{Key: "total_bytes", Value: CanonicalInt(build.TotalBytes)},
		})
	}

	platforms := make(CanonicalArray, 0, len(manifest.Platforms))
	for _, platform := range manifest.Platforms {
		platforms = append(platforms, CanonicalString(platform))
	}

	return CanonicalObject{
		{Key: "artifact_kind", Value: CanonicalString(manifest.ArtifactKind)},
		{Key: "build_time", Value: CanonicalString(manifest.BuildTime)},
		{Key: "commit", Value: CanonicalString(manifest.Commit)},
		{Key: "core", Value: core},
		{Key: "minimum_helper_protocol", Value: CanonicalInt(int64(manifest.MinimumHelperProtocol))},
		{Key: "online_update_compatible", Value: CanonicalBool(manifest.OnlineUpdateCompatible)},
		{Key: "online_update_reason", Value: CanonicalString(manifest.OnlineUpdateReason)},
		{Key: "platforms", Value: platforms},
		{Key: "reader", Value: CanonicalObject{
			{Key: "archive", Value: CanonicalString(manifest.Reader.Archive)},
			{Key: "builds", Value: builds},
			{Key: "commit", Value: CanonicalString(manifest.Reader.Commit)},
			{Key: "provenance_path", Value: CanonicalString(manifest.Reader.ProvenancePath)},
			{Key: "provenance_sha256", Value: CanonicalString(manifest.Reader.ProvenanceSHA256)},
			{Key: "release_id", Value: CanonicalString(manifest.Reader.ReleaseID)},
			{Key: "sha256", Value: CanonicalString(manifest.Reader.SHA256)},
			{Key: "size_bytes", Value: CanonicalInt(manifest.Reader.SizeBytes)},
		}},
		{Key: "repo", Value: CanonicalString(manifest.Repo)},
		{Key: "river_ledger_target", Value: CanonicalInt(int64(manifest.RiverLedgerTarget))},
		{Key: "rollback_compatible", Value: CanonicalBool(manifest.RollbackCompatible)},
		{Key: "rollback_reason", Value: CanonicalString(manifest.RollbackReason)},
		{Key: "schema_target", Value: CanonicalString(manifest.SchemaTarget)},
		{Key: "schema_version", Value: CanonicalInt(int64(manifest.SchemaVersion))},
		{Key: "tag", Value: CanonicalString(manifest.Tag)},
		{Key: "version", Value: CanonicalString(manifest.Version)},
	}
}

// Canonical validates the manifest and renders its signed byte representation.
// Validation runs first so an invalid manifest can never be signed.
func (manifest Manifest) Canonical() ([]byte, error) {
	if err := manifest.Validate(); err != nil {
		return nil, err
	}
	return EncodeCanonical(manifest.CanonicalValue())
}

// ParseManifest reads a manifest from its canonical bytes. It fails unless the
// input is already canonical, carries no unknown members, and satisfies every
// internal consistency rule.
func ParseManifest(data []byte) (Manifest, error) {
	if err := RequireCanonical(data); err != nil {
		return Manifest{}, fmt.Errorf("release manifest: %w", err)
	}
	value, err := DecodeCanonical(data)
	if err != nil {
		return Manifest{}, fmt.Errorf("release manifest: %w", err)
	}
	root, err := asObject(value, "manifest")
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := manifestFromObject(root)
	if err != nil {
		return Manifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

//nolint:gocyclo // A schema reader is inherently one branch per member; splitting it only hides the field list.
func manifestFromObject(root CanonicalObject) (Manifest, error) {
	var manifest Manifest
	var err error
	fields := newFieldReader(root, "manifest")
	manifest.SchemaVersion, err = fields.intField("schema_version")
	if err != nil {
		return manifest, err
	}
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return manifest, fmt.Errorf("manifest schema_version %d is not supported (want %d)",
			manifest.SchemaVersion, ManifestSchemaVersion)
	}
	if manifest.ArtifactKind, err = fields.stringField("artifact_kind"); err != nil {
		return manifest, err
	}
	if manifest.Repo, err = fields.stringField("repo"); err != nil {
		return manifest, err
	}
	if manifest.Tag, err = fields.stringField("tag"); err != nil {
		return manifest, err
	}
	if manifest.Version, err = fields.stringField("version"); err != nil {
		return manifest, err
	}
	if manifest.Commit, err = fields.stringField("commit"); err != nil {
		return manifest, err
	}
	if manifest.BuildTime, err = fields.stringField("build_time"); err != nil {
		return manifest, err
	}
	if manifest.MinimumHelperProtocol, err = fields.intField("minimum_helper_protocol"); err != nil {
		return manifest, err
	}
	if manifest.SchemaTarget, err = fields.stringField("schema_target"); err != nil {
		return manifest, err
	}
	if manifest.RiverLedgerTarget, err = fields.intField("river_ledger_target"); err != nil {
		return manifest, err
	}
	if manifest.OnlineUpdateCompatible, err = fields.boolField("online_update_compatible"); err != nil {
		return manifest, err
	}
	if manifest.OnlineUpdateReason, err = fields.stringField("online_update_reason"); err != nil {
		return manifest, err
	}
	if manifest.RollbackCompatible, err = fields.boolField("rollback_compatible"); err != nil {
		return manifest, err
	}
	if manifest.RollbackReason, err = fields.stringField("rollback_reason"); err != nil {
		return manifest, err
	}
	if manifest.Platforms, err = fields.stringArrayField("platforms"); err != nil {
		return manifest, err
	}
	coreEntries, err := fields.arrayField("core")
	if err != nil {
		return manifest, err
	}
	for index, entry := range coreEntries {
		artifact, parseErr := coreArtifactFromValue(entry, fmt.Sprintf("core[%d]", index))
		if parseErr != nil {
			return manifest, parseErr
		}
		manifest.Core = append(manifest.Core, artifact)
	}
	readerObject, err := fields.objectField("reader")
	if err != nil {
		return manifest, err
	}
	if manifest.Reader, err = readerArtifactFromObject(readerObject); err != nil {
		return manifest, err
	}
	return manifest, fields.requireExhausted()
}

func coreArtifactFromValue(value CanonicalValue, context string) (CoreArtifact, error) {
	var artifact CoreArtifact
	object, err := asObject(value, context)
	if err != nil {
		return artifact, err
	}
	fields := newFieldReader(object, context)
	if artifact.OS, err = fields.stringField("os"); err != nil {
		return artifact, err
	}
	if artifact.Arch, err = fields.stringField("arch"); err != nil {
		return artifact, err
	}
	if artifact.Archive, err = fields.stringField("archive"); err != nil {
		return artifact, err
	}
	if artifact.SHA256, err = fields.stringField("sha256"); err != nil {
		return artifact, err
	}
	if artifact.SizeBytes, err = fields.int64Field("size_bytes"); err != nil {
		return artifact, err
	}
	if artifact.PackageRoot, err = fields.stringField("package_root"); err != nil {
		return artifact, err
	}
	if artifact.ProvenancePath, err = fields.stringField("provenance_path"); err != nil {
		return artifact, err
	}
	if artifact.ProvenanceSHA256, err = fields.stringField("provenance_sha256"); err != nil {
		return artifact, err
	}
	executables, err := fields.objectField("executables")
	if err != nil {
		return artifact, err
	}
	artifact.Executables = map[string]Executable{}
	for _, field := range executables {
		executable, execErr := executableFromValue(field.Value, context+".executables."+field.Key)
		if execErr != nil {
			return artifact, execErr
		}
		artifact.Executables[field.Key] = executable
	}
	return artifact, fields.requireExhausted()
}

func executableFromValue(value CanonicalValue, context string) (Executable, error) {
	var executable Executable
	object, err := asObject(value, context)
	if err != nil {
		return executable, err
	}
	fields := newFieldReader(object, context)
	if executable.Path, err = fields.stringField("path"); err != nil {
		return executable, err
	}
	if executable.SHA256, err = fields.stringField("sha256"); err != nil {
		return executable, err
	}
	identityObject, err := fields.objectField("identity")
	if err != nil {
		return executable, err
	}
	identityFields := newFieldReader(identityObject, context+".identity")
	if executable.Identity.Version, err = identityFields.stringField("version"); err != nil {
		return executable, err
	}
	if executable.Identity.Commit, err = identityFields.stringField("commit"); err != nil {
		return executable, err
	}
	if executable.Identity.BuildTime, err = identityFields.stringField("build_time"); err != nil {
		return executable, err
	}
	if executable.Identity.VersionOutput, err = identityFields.stringField("version_output"); err != nil {
		return executable, err
	}
	if err := identityFields.requireExhausted(); err != nil {
		return executable, err
	}
	return executable, fields.requireExhausted()
}

func readerArtifactFromObject(object CanonicalObject) (ReaderArtifact, error) {
	var reader ReaderArtifact
	var err error
	fields := newFieldReader(object, "reader")
	if reader.Archive, err = fields.stringField("archive"); err != nil {
		return reader, err
	}
	if reader.SHA256, err = fields.stringField("sha256"); err != nil {
		return reader, err
	}
	if reader.SizeBytes, err = fields.int64Field("size_bytes"); err != nil {
		return reader, err
	}
	if reader.ReleaseID, err = fields.stringField("release_id"); err != nil {
		return reader, err
	}
	if reader.Commit, err = fields.stringField("commit"); err != nil {
		return reader, err
	}
	if reader.ProvenancePath, err = fields.stringField("provenance_path"); err != nil {
		return reader, err
	}
	if reader.ProvenanceSHA256, err = fields.stringField("provenance_sha256"); err != nil {
		return reader, err
	}
	buildEntries, err := fields.arrayField("builds")
	if err != nil {
		return reader, err
	}
	for index, entry := range buildEntries {
		build, buildErr := readerBuildFromValue(entry, fmt.Sprintf("reader.builds[%d]", index))
		if buildErr != nil {
			return reader, buildErr
		}
		reader.Builds = append(reader.Builds, build)
	}
	return reader, fields.requireExhausted()
}

func readerBuildFromValue(value CanonicalValue, context string) (ReaderBuild, error) {
	var build ReaderBuild
	object, err := asObject(value, context)
	if err != nil {
		return build, err
	}
	fields := newFieldReader(object, context)
	if build.Name, err = fields.stringField("name"); err != nil {
		return build, err
	}
	if build.BasePath, err = fields.stringField("base_path"); err != nil {
		return build, err
	}
	if build.Directory, err = fields.stringField("directory"); err != nil {
		return build, err
	}
	if build.FileCount, err = fields.int64Field("file_count"); err != nil {
		return build, err
	}
	if build.TotalBytes, err = fields.int64Field("total_bytes"); err != nil {
		return build, err
	}
	return build, fields.requireExhausted()
}

// Validate enforces every rule the helper is allowed to rely on. It is called
// on the way in (ParseManifest) and on the way out (Canonical), so a manifest
// can neither be produced nor accepted in an inconsistent state.
//
//nolint:gocyclo // Every branch is one named contract rule; collapsing them would hide the contract.
func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != ManifestSchemaVersion {
		return fmt.Errorf("manifest schema_version %d is not supported (want %d)",
			manifest.SchemaVersion, ManifestSchemaVersion)
	}
	if manifest.ArtifactKind != ManifestArtifactKind {
		return fmt.Errorf("manifest artifact_kind %q is not %q", manifest.ArtifactKind, ManifestArtifactKind)
	}
	if !repoPattern.MatchString(manifest.Repo) {
		return fmt.Errorf("manifest repo %q is not owner/name", manifest.Repo)
	}
	if !formalTagPattern.MatchString(manifest.Tag) {
		return fmt.Errorf("manifest tag %q is not a formal vX.Y.Z release tag", manifest.Tag)
	}
	if manifest.Version != strings.TrimPrefix(manifest.Tag, "v") {
		return fmt.Errorf("manifest version %q does not match tag %q", manifest.Version, manifest.Tag)
	}
	if manifest.Version == "0.0.0" {
		return errors.New("manifest version 0.0.0 is a development placeholder")
	}
	if !commitPattern.MatchString(manifest.Commit) {
		return fmt.Errorf("manifest commit %q is not a full lowercase Git revision", manifest.Commit)
	}
	if _, err := time.Parse(time.RFC3339, manifest.BuildTime); err != nil {
		return fmt.Errorf("manifest build_time %q is not RFC3339: %w", manifest.BuildTime, err)
	}
	if manifest.MinimumHelperProtocol < 1 {
		return fmt.Errorf("manifest minimum_helper_protocol %d must be at least 1", manifest.MinimumHelperProtocol)
	}
	if !migrationIDPattern.MatchString(manifest.SchemaTarget) {
		return fmt.Errorf("manifest schema_target %q is not a migration step id", manifest.SchemaTarget)
	}
	if manifest.RiverLedgerTarget < 1 {
		return fmt.Errorf("manifest river_ledger_target %d must be at least 1", manifest.RiverLedgerTarget)
	}
	if strings.TrimSpace(manifest.OnlineUpdateReason) == "" {
		return errors.New("manifest online_update_reason must explain the classification")
	}
	if strings.TrimSpace(manifest.RollbackReason) == "" {
		return errors.New("manifest rollback_reason must explain the classification")
	}
	if err := manifest.validateMatrix(); err != nil {
		return err
	}
	return manifest.validateReader()
}

//nolint:gocyclo // Same reason as Validate: one branch per matrix rule.
func (manifest Manifest) validateMatrix() error {
	if len(manifest.Core) == 0 {
		return errors.New("manifest core matrix is empty")
	}
	platforms := make([]string, 0, len(manifest.Core))
	for index, artifact := range manifest.Core {
		if artifact.OS != ReleaseOS {
			return fmt.Errorf("core[%d] os %q is not %q", index, artifact.OS, ReleaseOS)
		}
		if !architecturePattern.MatchString(artifact.Arch) {
			return fmt.Errorf("core[%d] arch %q is not a plain architecture name", index, artifact.Arch)
		}
		expectedRoot := fmt.Sprintf("cairn_%s_%s_%s", manifest.Version, artifact.OS, artifact.Arch)
		if artifact.PackageRoot != expectedRoot {
			return fmt.Errorf("core[%d] package_root %q is not %q", index, artifact.PackageRoot, expectedRoot)
		}
		if artifact.Archive != expectedRoot+".tar.gz" {
			return fmt.Errorf("core[%d] archive %q is not %q", index, artifact.Archive, expectedRoot+".tar.gz")
		}
		if !sha256Pattern.MatchString(artifact.SHA256) {
			return fmt.Errorf("core[%d] sha256 %q is not a lowercase hex digest", index, artifact.SHA256)
		}
		if artifact.SizeBytes <= 0 {
			return fmt.Errorf("core[%d] size_bytes %d must be positive", index, artifact.SizeBytes)
		}
		if artifact.ProvenancePath != expectedRoot+"/BUILD-PROVENANCE.json" {
			return fmt.Errorf("core[%d] provenance_path %q is not inside the package root", index, artifact.ProvenancePath)
		}
		if !sha256Pattern.MatchString(artifact.ProvenanceSHA256) {
			return fmt.Errorf("core[%d] provenance_sha256 %q is not a lowercase hex digest", index, artifact.ProvenanceSHA256)
		}
		if err := manifest.validateExecutables(index, artifact, expectedRoot); err != nil {
			return err
		}
		platforms = append(platforms, artifact.Platform())
	}
	if !slices.IsSorted(platforms) {
		return errors.New("manifest core matrix is not sorted by os/arch")
	}
	if len(slices.Compact(slices.Clone(platforms))) != len(platforms) {
		return errors.New("manifest core matrix repeats an os/arch")
	}
	if !slices.Equal(manifest.Platforms, platforms) {
		return fmt.Errorf("manifest platforms %v do not match the core matrix %v", manifest.Platforms, platforms)
	}
	return nil
}

func (manifest Manifest) validateExecutables(index int, artifact CoreArtifact, packageRoot string) error {
	names := make([]string, 0, len(artifact.Executables))
	for name := range artifact.Executables {
		names = append(names, name)
	}
	slices.Sort(names)
	if !slices.Equal(names, ExecutableNames) {
		return fmt.Errorf("core[%d] ships executables %v, want exactly %v", index, names, ExecutableNames)
	}
	expectedOutput := ExpectedVersionOutput(manifest.Version, manifest.Commit, manifest.BuildTime)
	for _, name := range names {
		executable := artifact.Executables[name]
		if executable.Path != packageRoot+"/"+name {
			return fmt.Errorf("core[%d] executable %s path %q is not inside the package root", index, name, executable.Path)
		}
		if !sha256Pattern.MatchString(executable.SHA256) {
			return fmt.Errorf("core[%d] executable %s sha256 %q is not a lowercase hex digest", index, name, executable.SHA256)
		}
		if executable.Identity.Version != manifest.Version ||
			executable.Identity.Commit != manifest.Commit ||
			executable.Identity.BuildTime != manifest.BuildTime {
			return fmt.Errorf("core[%d] executable %s identity does not match the release identity", index, name)
		}
		if executable.Identity.VersionOutput != expectedOutput {
			return fmt.Errorf("core[%d] executable %s version_output does not match the release identity", index, name)
		}
	}
	return nil
}

func (manifest Manifest) validateReader() error {
	reader := manifest.Reader
	expectedArchive := fmt.Sprintf("cairn-reader-%s.tar.gz", manifest.Version)
	if reader.Archive != expectedArchive {
		return fmt.Errorf("reader archive %q is not %q", reader.Archive, expectedArchive)
	}
	if !sha256Pattern.MatchString(reader.SHA256) {
		return fmt.Errorf("reader sha256 %q is not a lowercase hex digest", reader.SHA256)
	}
	if reader.SizeBytes <= 0 {
		return fmt.Errorf("reader size_bytes %d must be positive", reader.SizeBytes)
	}
	if reader.ReleaseID != readerReleasePrefix+manifest.Version {
		return fmt.Errorf("reader release_id %q is not %q", reader.ReleaseID, readerReleasePrefix+manifest.Version)
	}
	if reader.Commit != manifest.Commit {
		return fmt.Errorf("reader commit %q does not match the release commit", reader.Commit)
	}
	if reader.ProvenancePath != ReaderProvenanceFileName {
		return fmt.Errorf("reader provenance_path %q is not %q", reader.ProvenancePath, ReaderProvenanceFileName)
	}
	if !sha256Pattern.MatchString(reader.ProvenanceSHA256) {
		return fmt.Errorf("reader provenance_sha256 %q is not a lowercase hex digest", reader.ProvenanceSHA256)
	}
	names := make([]string, 0, len(reader.Builds))
	for _, build := range reader.Builds {
		if build.Directory != build.Name {
			return fmt.Errorf("reader build %q directory %q must equal its name", build.Name, build.Directory)
		}
		expectedBase, known := readerBasePaths[build.Name]
		if !known {
			return fmt.Errorf("reader build %q is not a known deployment shape", build.Name)
		}
		if build.BasePath != expectedBase {
			return fmt.Errorf("reader build %q base_path %q is not %q", build.Name, build.BasePath, expectedBase)
		}
		if build.FileCount <= 0 || build.TotalBytes <= 0 {
			return fmt.Errorf("reader build %q provenance is empty", build.Name)
		}
		names = append(names, build.Name)
	}
	if !slices.Equal(names, ReaderBuildNames) {
		return fmt.Errorf("reader ships builds %v, want exactly %v", names, ReaderBuildNames)
	}
	return nil
}

// ReaderProvenanceFileName is the per-file provenance document produced by
// scripts/reader-vnext-release.sh at the root of the Reader archive.
const ReaderProvenanceFileName = "reader-vnext-release-manifest.json"

var readerBasePaths = map[string]string{
	"embedded": "/reader/",
	"root":     "/",
}

// CoreFor returns the matrix entry for an operating system and architecture.
func (manifest Manifest) CoreFor(operatingSystem, architecture string) (CoreArtifact, error) {
	for _, artifact := range manifest.Core {
		if artifact.OS == operatingSystem && artifact.Arch == architecture {
			return artifact, nil
		}
	}
	return CoreArtifact{}, fmt.Errorf("release %s does not ship %s/%s (matrix: %v)",
		manifest.Tag, operatingSystem, architecture, manifest.Platforms)
}

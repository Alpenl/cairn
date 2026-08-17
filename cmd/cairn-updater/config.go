package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Repository is the only repository this helper will ever install from. It is a
// compile-time constant rather than configuration on purpose: a settable
// repository is an arbitrary-source install one environment file away, and the
// signed manifest's repo field would then be checked against attacker input.
const Repository = "Alpenl/cairn"

// InstallModeSystemdRelease is the only install mode this helper can act on. A
// Docker or source install reports its own mode and is read-only from the page.
const InstallModeSystemdRelease = "systemd-release"

// Environment variable names. They are read directly rather than through
// internal/config because the helper must not link the application's
// configuration surface — it has no business knowing about API keys or CORS.
const (
	//nolint:gosec // This is the name of an environment variable, not a credential.
	envDeployToken  = "DEPLOY_AUTH_TOKEN"
	envDatabaseURL  = "DATABASE_URL"
	envSocketPath   = "CAIRN_UPDATER_SOCKET"
	envSocketGroup  = "CAIRN_UPDATER_SOCKET_GROUP"
	envStateDir     = "CAIRN_UPDATER_STATE_DIR"
	envCoreDir      = "CAIRN_UPDATER_CORE_DIR"
	envReaderDir    = "CAIRN_UPDATER_READER_DIR"
	envHelperEnv    = "CAIRN_UPDATER_ENV_FILE"
	envServiceUnit  = "CAIRN_UPDATER_SERVICE"
	envServiceUser  = "CAIRN_UPDATER_SERVICE_USER"
	envCoreEnvGroup = "CAIRN_UPDATER_CORE_ENV_GROUP"
	envHealthBase   = "CAIRN_UPDATER_HEALTH_URL"
	envSystemctl    = "CAIRN_UPDATER_SYSTEMCTL"
	envPgDump       = "CAIRN_UPDATER_PG_DUMP"
	envPgRestore    = "CAIRN_UPDATER_PG_RESTORE"
	envMinFreeBytes = "CAIRN_UPDATER_MIN_FREE_BYTES"
	envOwnerUID     = "CAIRN_UPDATER_EXPECT_UID"
)

// Defaults for the production layout described in issue #41.
const (
	defaultSocketPath  = "/run/cairn-updater.sock"
	defaultSocketGroup = "caddy"
	defaultStateDir    = "/var/lib/cairn-updater"
	defaultCoreDir     = "/opt/webtag"
	defaultReaderDir   = "/var/www/reader"
	defaultHelperEnv   = "/etc/cairn-updater.env"
	defaultServiceUnit = "webtag.service"
	defaultServiceUser = "webtag"
	defaultHealthBase  = "http://127.0.0.1:8732"
	defaultSystemctl   = "systemctl"
	defaultPgDump      = "pg_dump"
	defaultPgRestore   = "pg_restore"
)

// defaultMinFreeBytes is the free space the staging filesystem must still have
// after the download. Two Core archives plus a custom-format dump of a small
// installation fit comfortably; the point of the floor is to fail in preflight
// rather than halfway through writing a dump nobody can restore.
const defaultMinFreeBytes int64 = 2 << 30

// Config is the fully resolved runtime configuration. Every field is settable
// from the environment except Repo, which is the compiled-in constant.
type Config struct {
	Repo         string
	DeployToken  string
	DatabaseURL  string
	SocketPath   string
	SocketGroup  string
	StateDir     string
	CoreDir      string
	ReaderDir    string
	HelperEnv    string
	ServiceUnit  string
	ServiceUser  string
	CoreEnvGroup string
	HealthBase   string
	Systemctl    string
	PgDump       string
	PgRestore    string
	MinFreeBytes int64
	// OwnerUID is the uid every deployment path must belong to. It is
	// configurable only so the permission self-check can be exercised by a
	// test that is not running as root; production leaves it at 0.
	OwnerUID int
}

// LoadConfig resolves configuration from an environment lookup function.
//
// It is fail-closed on the two values that decide whether the helper is safe to
// run at all: an empty DEPLOY_AUTH_TOKEN and an empty DATABASE_URL both refuse
// to start. There is deliberately no development exemption for the token. The
// application's ADMIN_AUTH_TOKEN has one, and that exemption is precisely why
// it cannot be promoted to deployment authority: an empty-token dev default
// that ever reaches a host with a public listener is a root-owned remote
// install primitive.
func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	get := func(name, fallback string) string {
		if raw, ok := lookup(name); ok {
			if trimmed := strings.TrimSpace(raw); trimmed != "" {
				return trimmed
			}
		}
		return fallback
	}

	config := Config{
		Repo:         Repository,
		DeployToken:  get(envDeployToken, ""),
		DatabaseURL:  get(envDatabaseURL, ""),
		SocketPath:   get(envSocketPath, defaultSocketPath),
		SocketGroup:  get(envSocketGroup, defaultSocketGroup),
		StateDir:     get(envStateDir, defaultStateDir),
		CoreDir:      get(envCoreDir, defaultCoreDir),
		ReaderDir:    get(envReaderDir, defaultReaderDir),
		HelperEnv:    get(envHelperEnv, defaultHelperEnv),
		ServiceUnit:  get(envServiceUnit, defaultServiceUnit),
		ServiceUser:  get(envServiceUser, defaultServiceUser),
		CoreEnvGroup: get(envCoreEnvGroup, defaultServiceUser),
		HealthBase:   strings.TrimRight(get(envHealthBase, defaultHealthBase), "/"),
		Systemctl:    get(envSystemctl, defaultSystemctl),
		PgDump:       get(envPgDump, defaultPgDump),
		PgRestore:    get(envPgRestore, defaultPgRestore),
	}

	minFree, err := parseBytes(get(envMinFreeBytes, ""), defaultMinFreeBytes)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", envMinFreeBytes, err)
	}
	config.MinFreeBytes = minFree

	uid, err := parseInt(get(envOwnerUID, ""), 0)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", envOwnerUID, err)
	}
	config.OwnerUID = uid

	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// LoadConfigFromEnv is the production entry point.
func LoadConfigFromEnv() (Config, error) {
	return LoadConfig(os.LookupEnv)
}

// Validate rejects a configuration the helper must not run with.
func (config Config) Validate() error {
	if config.DeployToken == "" {
		return fmt.Errorf("%s is empty: the deployment helper is fail-closed and has no development exemption", envDeployToken)
	}
	if len(config.DeployToken) < MinimumDeployTokenLength {
		return fmt.Errorf("%s is shorter than %d characters: a guessable deployment token is a root-owned install primitive",
			envDeployToken, MinimumDeployTokenLength)
	}
	if config.DatabaseURL == "" {
		return fmt.Errorf("%s is empty: the helper cannot dump or migrate without a host database URL", envDatabaseURL)
	}
	parsed, err := url.Parse(config.HealthBase)
	if err != nil {
		return fmt.Errorf("%s %q is not a URL: %w", envHealthBase, config.HealthBase, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s %q must be an http or https URL", envHealthBase, config.HealthBase)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s %q has no host", envHealthBase, config.HealthBase)
	}
	for name, value := range map[string]string{
		envSocketPath: config.SocketPath,
		envStateDir:   config.StateDir,
		envCoreDir:    config.CoreDir,
		envReaderDir:  config.ReaderDir,
		envHelperEnv:  config.HelperEnv,
	} {
		if !strings.HasPrefix(value, "/") {
			return fmt.Errorf("%s %q must be an absolute path", name, value)
		}
	}
	if config.MinFreeBytes <= 0 {
		return errors.New(envMinFreeBytes + " must be positive")
	}
	return nil
}

// ReleasesDir is where verified Core release trees are unpacked.
func (config Config) ReleasesDir() string { return config.CoreDir + "/releases" }

// BackupsDir holds the pre-migration custom-format dumps.
func (config Config) BackupsDir() string { return config.CoreDir + "/backups" }

// CoreCurrent is the symlink systemd's ExecStart resolves through.
func (config Config) CoreCurrent() string { return config.ReleasesDir() + "/current" }

// CoreEnvFile is the application's environment file. The helper only checks its
// ownership; it never reads it, because the application's secrets are none of
// the helper's business.
func (config Config) CoreEnvFile() string { return config.CoreDir + "/.env" }

// ReaderReleasesDir holds unpacked root-path Reader builds.
func (config Config) ReaderReleasesDir() string { return config.ReaderDir + "/releases" }

// ReaderCurrent is the symlink Caddy serves the root-domain Reader from.
func (config Config) ReaderCurrent() string { return config.ReaderDir + "/current" }

// CoreIncoming is where a Core release is unpacked before it is renamed into
// place. It sits inside the releases directory on purpose: the final step of an
// unpack must be a rename, and a rename is only atomic within one filesystem.
// The dot prefix keeps it out of any listing that enumerates installed
// releases, and nothing ever resolves through it.
func (config Config) CoreIncoming(tag string) string {
	return config.ReleasesDir() + "/.incoming-" + tag
}

// ReaderIncoming is the Reader equivalent of CoreIncoming.
func (config Config) ReaderIncoming(tag string) string {
	return config.ReaderReleasesDir() + "/.incoming-" + tag
}

// CoreRelease is the installed tree for one tag.
func (config Config) CoreRelease(tag string) string { return config.ReleasesDir() + "/" + tag }

// ReaderRelease is the installed Reader tree for one tag.
func (config Config) ReaderRelease(tag string) string { return config.ReaderReleasesDir() + "/" + tag }

// JobsDir holds the persisted job records.
func (config Config) JobsDir() string { return config.StateDir + "/jobs" }

func parseBytes(raw string, fallback int64) (int64, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a byte count: %w", raw, err)
	}
	return value, nil
}

func parseInt(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%q is not an integer: %w", raw, err)
	}
	return value, nil
}

// Timeouts used across the state machine. They are constants rather than
// configuration because an operator who can stretch the migration timeout can
// also stretch the write-stop window, and that is a decision for a release, not
// for an environment file.
const (
	// DownloadTimeout bounds one asset download.
	DownloadTimeout = 10 * time.Minute
	// ServiceStopTimeout bounds the quiesce step.
	ServiceStopTimeout = 2 * time.Minute
	// BackupTimeout bounds pg_dump.
	BackupTimeout = 30 * time.Minute
	// PlanTimeout bounds the read-only eligibility question. It is far shorter
	// than MigrateTimeout because a plan applies nothing: an hour spent here
	// would be an hour of an operator watching a page that cannot yet say
	// whether the update is even permitted.
	PlanTimeout = 5 * time.Minute
	// MigrateTimeout bounds the migration subprocess.
	MigrateTimeout = 60 * time.Minute
	// ReadyTimeout is how long the new binary has to answer /ready with the
	// target commit before the job holds.
	ReadyTimeout = 3 * time.Minute
	// ReadyPollInterval is the gap between /health probes.
	ReadyPollInterval = 2 * time.Second
	// IdentityTimeout bounds a `--version` probe of a staged binary.
	IdentityTimeout = 30 * time.Second
)

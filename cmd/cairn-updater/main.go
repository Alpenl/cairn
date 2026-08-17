package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"webtag/internal/buildinfo"
)

func main() {
	if err := execute(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "cairn-updater: %v\n", err)
		os.Exit(1)
	}
}

const usage = `Usage:
  cairn-updater serve      run the deployment helper on its Unix socket
  cairn-updater selfcheck  verify the deployment layout and exit
  cairn-updater --version  print the build identity

Configuration comes from the environment; see /etc/cairn-updater.env.
The helper takes no target, URL or path on the command line: an update is
requested through the socket API and is always an exact vX.Y.Z tag.`

func execute(args []string, stdout io.Writer) error {
	if handled, err := buildinfo.PrintVersion(args, stdout); handled {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("a subcommand is required\n\n%s", usage)
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "selfcheck":
		return runSelfCheck(args[1:], stdout)
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(stdout, usage)
		return err
	default:
		return fmt.Errorf("unknown subcommand %q\n\n%s", args[0], usage)
	}
}

func runSelfCheck(args []string, stdout io.Writer) error {
	if len(args) != 0 {
		return errors.New("selfcheck takes no arguments")
	}
	config, err := LoadConfigFromEnv()
	if err != nil {
		return err
	}
	if err := BuildLayout(config).Enforce(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "the deployment layout under %s and %s satisfies its permission model\n",
		config.CoreDir, config.ReaderDir)
	return err
}

// runServe is the process lifecycle.
//
// The order is load, self-check, then listen. A helper that opened its socket
// before proving the permission model would be accepting deployment requests
// for a host it has already lost the ability to protect, and an operator would
// have no way to tell the difference from the page.
func runServe(args []string) error {
	if len(args) != 0 {
		return errors.New("serve takes no arguments")
	}
	config, err := LoadConfigFromEnv()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := BuildLayout(config).Enforce(); err != nil {
		return err
	}
	return serve(config, logger)
}

func serve(config Config, logger *slog.Logger) error {
	store, err := NewStore(config.JobsDir(), time.Now)
	if err != nil {
		return err
	}
	server := NewServer(config, store, logger, time.Now)

	listener, err := ListenOnSocket(config)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()

	httpServer := &http.Server{
		Handler: server.Handler(),
		// The read timeout is generous because the only body is tiny but the
		// socket peer is a reverse proxy that may be slow under load; the write
		// timeout is absent because a status response is small and a deadline
		// that fired mid-write would corrupt exactly the document an operator
		// is trying to read during an incident.
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          nil,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() { errs <- httpServer.Serve(listener) }()

	logger.Info("cairn-updater listening",
		"socket", config.SocketPath, "repo", config.Repo, "unit", config.ServiceUnit)

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve on %s: %w", config.SocketPath, err)
	case <-ctx.Done():
	}

	// Shutdown stops accepting and drains in-flight requests, then the helper
	// waits for any running update. A deployment must not be truncated by a
	// systemd restart: the job that is halfway through a migration owns the
	// operation lock and the only safe thing to do is let it reach its own
	// terminal state.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("shutdown did not drain cleanly", "error", err)
	}
	server.Wait()
	return nil
}

// NewServer wires the production server.
func NewServer(config Config, store *Store, logger *slog.Logger, now func() time.Time) *Server {
	health := NewHealthClient(config.HealthBase)
	downloader := NewDownloader(config.Repo)
	commands := execRunner{}
	trust := productionTrust{}

	server := &Server{
		config:     config,
		auth:       newAuthenticator(config.DeployToken),
		store:      store,
		health:     health,
		downloader: downloader,
		trust:      trust,
		logger:     logger,
		now:        now,
		hostOS:     runtime.GOOS,
		hostArch:   runtime.GOARCH,
	}
	server.newRunner = func() *Runner {
		return &Runner{
			config:      config,
			store:       store,
			trust:       trust,
			downloader:  downloader,
			commands:    commands,
			service:     NewServiceControl(commands, config.Systemctl, config.ServiceUnit),
			health:      health,
			hostOS:      runtime.GOOS,
			hostArch:    runtime.GOARCH,
			now:         now,
			readyBudget: ReadyTimeout,
			readyPoll:   ReadyPollInterval,
		}
	}
	return server
}

// socketMode is the only mode the deployment socket may carry: owner and group
// read/write, nothing for anyone else. The group is Caddy's, so the reverse
// proxy can connect and the application user — which is in neither — cannot.
const socketMode fs.FileMode = 0o660

// ListenOnSocket creates the Unix socket with the ownership the model requires.
//
// The umask is narrowed around the bind rather than relying on the chmod that
// follows it. Between bind and chmod the socket exists on disk, and a process
// that connected in that window under a permissive umask would have connected
// to the deployment API. The window is small; it is also completely avoidable.
func ListenOnSocket(config Config) (net.Listener, error) {
	if err := removeStaleSocket(config.SocketPath); err != nil {
		return nil, err
	}
	previousUmask := syscall.Umask(0o177)
	listener, err := net.Listen("unix", config.SocketPath)
	syscall.Umask(previousUmask)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", config.SocketPath, err)
	}
	if err := applySocketOwnership(config); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func applySocketOwnership(config Config) error {
	gid, err := lookupGroupID(config.SocketGroup)
	if err != nil {
		return fmt.Errorf("resolve socket group %q: %w", config.SocketGroup, err)
	}
	if err := os.Chown(config.SocketPath, config.OwnerUID, gid); err != nil {
		return fmt.Errorf("set ownership of %s to uid %d group %q: %w",
			config.SocketPath, config.OwnerUID, config.SocketGroup, err)
	}
	if err := os.Chmod(config.SocketPath, socketMode); err != nil {
		return fmt.Errorf("set mode of %s: %w", config.SocketPath, err)
	}
	info, err := os.Stat(config.SocketPath)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", config.SocketPath, err)
	}
	if info.Mode().Perm() != socketMode {
		return fmt.Errorf("%s came out mode %04o, the model requires %04o",
			config.SocketPath, info.Mode().Perm(), socketMode)
	}
	return nil
}

// removeStaleSocket clears a socket left behind by a killed process.
//
// Only a socket is removed. If the path is a regular file, a directory, or a
// symlink, something other than a previous helper put it there and unlinking it
// would be doing that thing's work for it.
func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket; refusing to remove it", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	return nil
}

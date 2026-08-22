package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServerRunStartsLifecycleAndStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	lifecycle := &fakeLifecycle{}
	server := NewServer(ServerOptions{
		Addr: listener.Addr().String(),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		Lifecycle:       lifecycle,
		Listen:          func(string, string) (net.Listener, error) { return listener, nil },
		ShutdownTimeout: time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Run(ctx)
	}()

	waitForHTTPServer(t, "http://"+listener.Addr().String())
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server shutdown")
	}

	if lifecycle.started != 1 {
		t.Fatalf("lifecycle started = %d, want 1", lifecycle.started)
	}
	if lifecycle.closed != 1 {
		t.Fatalf("lifecycle closed = %d, want 1", lifecycle.closed)
	}
}

func TestServerRunClosesLifecycleWhenListenFails(t *testing.T) {
	t.Parallel()

	lifecycle := &fakeLifecycle{}
	listenErr := errors.New("listen failed")
	server := NewServer(ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         http.NewServeMux(),
		Lifecycle:       lifecycle,
		Listen:          func(string, string) (net.Listener, error) { return nil, listenErr },
		ShutdownTimeout: time.Second,
	})

	err := server.Run(context.Background())
	if !errors.Is(err, listenErr) {
		t.Fatalf("Run() error = %v, want %v", err, listenErr)
	}

	if lifecycle.started != 1 {
		t.Fatalf("lifecycle started = %d, want 1", lifecycle.started)
	}
	if lifecycle.closed != 1 {
		t.Fatalf("lifecycle closed = %d, want 1", lifecycle.closed)
	}
}

func TestServerRunClosesLifecycleWhenStartFails(t *testing.T) {
	t.Parallel()

	startErr := errors.New("start failed")
	lifecycle := &fakeLifecycle{startErr: startErr}
	server := NewServer(ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         http.NewServeMux(),
		Lifecycle:       lifecycle,
		ShutdownTimeout: time.Second,
	})

	err := server.Run(context.Background())
	if !errors.Is(err, startErr) {
		t.Fatalf("Run() error = %v, want %v", err, startErr)
	}
	if lifecycle.started != 1 {
		t.Fatalf("lifecycle started = %d, want 1", lifecycle.started)
	}
	if lifecycle.closed != 1 {
		t.Fatalf("lifecycle closed = %d, want 1 after start failure", lifecycle.closed)
	}
}

func TestNewServerAppliesDefaultHTTPTimeouts(t *testing.T) {
	t.Parallel()

	server := NewServer(ServerOptions{
		Addr:    "127.0.0.1:0",
		Handler: http.NewServeMux(),
	})

	if server.httpServer.ReadHeaderTimeout != defaultReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %v, want %v", server.httpServer.ReadHeaderTimeout, defaultReadHeaderTimeout)
	}
	if server.httpServer.ReadTimeout != defaultReadTimeout {
		t.Fatalf("ReadTimeout = %v, want %v", server.httpServer.ReadTimeout, defaultReadTimeout)
	}
	if server.httpServer.WriteTimeout != defaultWriteTimeout {
		t.Fatalf("WriteTimeout = %v, want %v", server.httpServer.WriteTimeout, defaultWriteTimeout)
	}
	if server.httpServer.IdleTimeout != defaultIdleTimeout {
		t.Fatalf("IdleTimeout = %v, want %v", server.httpServer.IdleTimeout, defaultIdleTimeout)
	}
}

func TestNewServerUsesConfiguredHTTPTimeouts(t *testing.T) {
	t.Parallel()

	server := NewServer(ServerOptions{
		Addr:              "127.0.0.1:0",
		Handler:           http.NewServeMux(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      4 * time.Second,
		IdleTimeout:       5 * time.Second,
	})

	if server.httpServer.ReadHeaderTimeout != 2*time.Second {
		t.Fatalf("ReadHeaderTimeout = %v, want 2s", server.httpServer.ReadHeaderTimeout)
	}
	if server.httpServer.ReadTimeout != 3*time.Second {
		t.Fatalf("ReadTimeout = %v, want 3s", server.httpServer.ReadTimeout)
	}
	if server.httpServer.WriteTimeout != 4*time.Second {
		t.Fatalf("WriteTimeout = %v, want 4s", server.httpServer.WriteTimeout)
	}
	if server.httpServer.IdleTimeout != 5*time.Second {
		t.Fatalf("IdleTimeout = %v, want 5s", server.httpServer.IdleTimeout)
	}
}

type fakeLifecycle struct {
	started  int
	closed   int
	startErr error
}

func (f *fakeLifecycle) Start(context.Context) error {
	f.started++
	return f.startErr
}

func (f *fakeLifecycle) Close(context.Context) error {
	f.closed++
	return nil
}

func waitForHTTPServer(t *testing.T, baseURL string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server at %s did not start in time", baseURL)
}

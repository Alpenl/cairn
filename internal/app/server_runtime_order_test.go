package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

type serverRuntimeOrderContextKey struct{}

type serverRuntimeOrderBackground struct {
	startCtx    chan context.Context
	stopEntered chan struct{}
	stopOnce    sync.Once
	cancel      context.CancelFunc
}

type serverRuntimeOrderListener struct {
	conn net.Conn
	err  error

	mu          sync.Mutex
	accepted    bool
	returnError chan struct{}
	closeOnce   sync.Once
	closed      chan struct{}
}

func (l *serverRuntimeOrderListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.accepted {
		l.accepted = true
		conn := l.conn
		l.mu.Unlock()
		return conn, nil
	}
	l.mu.Unlock()

	select {
	case <-l.returnError:
		return nil, l.err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *serverRuntimeOrderListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (*serverRuntimeOrderListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
}

func (b *serverRuntimeOrderBackground) Start(ctx context.Context) error {
	ownerCtx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.startCtx <- ownerCtx
	return nil
}

func (b *serverRuntimeOrderBackground) Stop(context.Context) error {
	b.stopOnce.Do(func() {
		b.cancel()
		close(b.stopEntered)
	})
	return nil
}

func TestServerStopsHTTPAdmissionBeforeRuntimeBackgrounds(t *testing.T) {
	t.Parallel()

	background := &serverRuntimeOrderBackground{
		startCtx:    make(chan context.Context, 1),
		stopEntered: make(chan struct{}),
	}
	resources := newRuntimeResources(
		[]namedRuntimeBackground{{name: "order probe", background: background}},
		nil,
	)
	closeEntered := make(chan struct{})
	var closeOnce sync.Once
	runtime := &Runtime{
		start: resources.Start,
		close: func(ctx context.Context) error {
			closeOnce.Do(func() { close(closeEntered) })
			return resources.Close(ctx)
		},
	}

	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseHandler) }) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(handlerEntered)
		<-releaseHandler
		w.WriteHeader(http.StatusNoContent)
	})

	listenerReady := make(chan net.Listener, 1)
	server := NewServer(ServerOptions{
		Addr:            "127.0.0.1:0",
		Handler:         handler,
		Lifecycle:       runtime,
		ShutdownTimeout: 2 * time.Second,
		Listen: func(network, address string) (net.Listener, error) {
			listener, err := net.Listen(network, address)
			if err == nil {
				listenerReady <- listener
			}
			return listener, err
		},
	})

	signalCtx, cancelSignal := context.WithCancel(context.WithValue(
		context.Background(),
		serverRuntimeOrderContextKey{},
		"runtime-value",
	))
	defer cancelSignal()
	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(signalCtx) }()

	var ownerCtx context.Context
	select {
	case ownerCtx = <-background.startCtx:
	case <-time.After(time.Second):
		t.Fatal("runtime background did not start")
	}
	if got := ownerCtx.Value(serverRuntimeOrderContextKey{}); got != "runtime-value" {
		t.Fatalf("runtime owner context value = %v, want runtime-value", got)
	}
	listener := <-listenerReady

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	requestErr := make(chan error, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			requestErr <- err
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			requestErr <- fmt.Errorf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
			return
		}
		requestErr <- nil
	}()

	select {
	case <-handlerEntered:
	case <-time.After(time.Second):
		t.Fatal("in-flight handler did not enter")
	}
	cancelSignal()
	waitForListenerClosed(t, listener.Addr().String())

	if err := ownerCtx.Err(); err != nil {
		t.Fatalf("runtime owner canceled before HTTP drain: %v", err)
	}
	select {
	case <-closeEntered:
		t.Fatal("Runtime.Close entered before in-flight HTTP handler completed")
	default:
	}
	select {
	case <-background.stopEntered:
		t.Fatal("background Stop entered before in-flight HTTP handler completed")
	default:
	}

	releaseOnce.Do(func() { close(releaseHandler) })
	if err := <-requestErr; err != nil {
		t.Fatalf("in-flight request error = %v", err)
	}
	if err := <-runErr; err != nil {
		t.Fatalf("Server.Run() error = %v", err)
	}
	select {
	case <-closeEntered:
	default:
		t.Fatal("Runtime.Close was not entered after HTTP drain")
	}
	select {
	case <-background.stopEntered:
	default:
		t.Fatal("background Stop was not entered after HTTP drain")
	}
	if err := ownerCtx.Err(); err != context.Canceled {
		t.Fatalf("runtime owner context after Stop = %v, want context.Canceled", err)
	}
}

func TestServerDrainsActiveHandlersBeforeRuntimeCloseWhenServeFails(t *testing.T) {
	t.Parallel()

	serveErr := errors.New("accept failed")
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	listener := &serverRuntimeOrderListener{
		conn:        serverConn,
		err:         serveErr,
		returnError: make(chan struct{}),
		closed:      make(chan struct{}),
	}

	closeEntered := make(chan struct{})
	var closeOnce sync.Once
	runtime := &Runtime{
		close: func(context.Context) error {
			closeOnce.Do(func() { close(closeEntered) })
			return nil
		},
	}

	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseHandler) }) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(handlerEntered)
		<-releaseHandler
		w.WriteHeader(http.StatusNoContent)
	})

	server := NewServer(ServerOptions{
		Addr:            "pipe",
		Handler:         handler,
		Lifecycle:       runtime,
		Listen:          func(string, string) (net.Listener, error) { return listener, nil },
		ShutdownTimeout: time.Second,
	})
	shutdownCalled := make(chan struct{})
	server.httpServer.RegisterOnShutdown(func() { close(shutdownCalled) })

	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(context.Background()) }()
	requestErr := make(chan error, 1)
	go func() { requestErr <- serverRuntimeOrderRequest(clientConn) }()

	select {
	case <-handlerEntered:
	case <-time.After(time.Second):
		t.Fatal("in-flight handler did not enter")
	}
	close(listener.returnError)

	select {
	case <-shutdownCalled:
	case <-closeEntered:
		t.Fatal("Runtime.Close entered before the active handler drained after Serve failed")
	case err := <-runErr:
		t.Fatalf("Server.Run() returned before the active handler drained: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Server did not begin HTTP shutdown after Serve failed")
	}
	select {
	case <-closeEntered:
		t.Fatal("Runtime.Close entered before the active handler drained after Serve failed")
	default:
	}

	releaseOnce.Do(func() { close(releaseHandler) })
	if err := <-requestErr; err != nil {
		t.Fatalf("in-flight request error = %v", err)
	}
	if err := <-runErr; !errors.Is(err, serveErr) {
		t.Fatalf("Server.Run() error = %v, want %v", err, serveErr)
	}
	select {
	case <-closeEntered:
	default:
		t.Fatal("Runtime.Close was not entered after the active handler drained")
	}
}

func TestServerRetainsRuntimeWhenHTTPDrainDeadlineExpires(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	listener := &serverRuntimeOrderListener{
		conn:        serverConn,
		err:         errors.New("unused accept failure"),
		returnError: make(chan struct{}),
		closed:      make(chan struct{}),
	}

	closeEntered := make(chan struct{})
	var closeOnce sync.Once
	runtime := &Runtime{
		close: func(context.Context) error {
			closeOnce.Do(func() { close(closeEntered) })
			return nil
		},
	}

	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseHandler) }) })
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(handlerEntered)
		<-releaseHandler
		w.WriteHeader(http.StatusNoContent)
	})

	server := NewServer(ServerOptions{
		Addr:            "pipe",
		Handler:         handler,
		Lifecycle:       runtime,
		Listen:          func(string, string) (net.Listener, error) { return listener, nil },
		ShutdownTimeout: 100 * time.Millisecond,
	})
	shutdownCalled := make(chan struct{})
	var shutdownOnce sync.Once
	server.httpServer.RegisterOnShutdown(func() {
		shutdownOnce.Do(func() { close(shutdownCalled) })
	})

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runErr := make(chan error, 1)
	go func() { runErr <- server.Run(runCtx) }()
	requestErr := make(chan error, 1)
	go func() { requestErr <- serverRuntimeOrderRequest(clientConn) }()

	select {
	case <-handlerEntered:
	case <-time.After(time.Second):
		t.Fatal("in-flight handler did not enter")
	}
	cancelRun()
	select {
	case <-shutdownCalled:
	case <-time.After(time.Second):
		t.Fatal("Server did not begin HTTP shutdown after caller cancellation")
	}

	var err error
	select {
	case err = <-runErr:
	case <-time.After(time.Second):
		t.Fatal("Server.Run() did not return after the HTTP drain deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Server.Run() error = %v, want context deadline exceeded", err)
	}
	select {
	case <-closeEntered:
		t.Fatal("Runtime.Close entered even though the active handler missed the HTTP drain deadline")
	default:
	}

	releaseOnce.Do(func() { close(releaseHandler) })
	if err := <-requestErr; err != nil {
		t.Fatalf("in-flight request error = %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() after handler drain error = %v", err)
	}
	select {
	case <-closeEntered:
	default:
		t.Fatal("Runtime.Close was not entered after the retained handler drained")
	}
}

func serverRuntimeOrderRequest(conn net.Conn) error {
	if _, err := fmt.Fprint(conn, "GET / HTTP/1.1\r\nHost: example.test\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	return nil
}

func waitForListenerClosed(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("HTTP listener remained open after shutdown signal")
		}
		time.Sleep(time.Millisecond)
	}
}

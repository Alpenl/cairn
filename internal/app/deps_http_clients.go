package app

import (
	"context"
	"sync"

	"webtag/internal/fetcher"
)

type idleConnectionCloser interface {
	CloseIdleConnections()
}

type runtimeHTTPClientFactory func(fetcher.HTTPClientOptions) *fetcher.HTTPClient

// runtimeHTTPClientOwner registers every composition-root HTTP transport before
// it can escape into a service. It closes idle pools only after request and
// background users have stopped.
type runtimeHTTPClientOwner struct {
	mu        sync.Mutex
	newClient runtimeHTTPClientFactory
	clients   []idleConnectionCloser
	stopped   bool
}

func newRuntimeHTTPClientOwner() *runtimeHTTPClientOwner {
	return &runtimeHTTPClientOwner{newClient: fetcher.NewHTTPClientWithOptions}
}

func (o *runtimeHTTPClientOwner) New(options fetcher.HTTPClientOptions) *fetcher.HTTPClient {
	if o == nil {
		panic("app: nil Runtime HTTP client owner")
	}
	factory := o.newClient
	if factory == nil {
		factory = fetcher.NewHTTPClientWithOptions
	}
	client := factory(options)
	if client == nil {
		panic("app: Runtime HTTP client factory returned nil")
	}
	o.register(client)
	return client
}

func (o *runtimeHTTPClientOwner) register(client idleConnectionCloser) {
	if client == nil {
		return
	}
	if o == nil {
		panic("app: nil Runtime HTTP client owner")
	}
	o.mu.Lock()
	if o.stopped {
		o.mu.Unlock()
		client.CloseIdleConnections()
		panic("app: register HTTP client after Runtime HTTP client owner stopped")
	}
	o.clients = append(o.clients, client)
	o.mu.Unlock()
}

func (o *runtimeHTTPClientOwner) Start(_ context.Context) error {
	if o == nil {
		return nil
	}
	return nil
}

func (o *runtimeHTTPClientOwner) Stop(_ context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	if o.stopped {
		o.mu.Unlock()
		return nil
	}
	o.stopped = true
	clients := o.clients
	o.clients = nil
	o.mu.Unlock()
	for index := len(clients) - 1; index >= 0; index-- {
		clients[index].CloseIdleConnections()
	}
	return nil
}

var _ runtimeBackground = (*runtimeHTTPClientOwner)(nil)

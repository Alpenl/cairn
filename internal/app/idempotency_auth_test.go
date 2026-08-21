package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"webtag/internal/dto"
	"webtag/internal/handler"
	"webtag/internal/middleware"
)

type routerIdempotencyStore struct {
	mu      sync.Mutex
	entries map[string]*middleware.StoredResponse
}

func newRouterIdempotencyStore() *routerIdempotencyStore {
	return &routerIdempotencyStore{entries: make(map[string]*middleware.StoredResponse)}
}

func (s *routerIdempotencyStore) Acquire(_ context.Context, key, ownerToken string, now, expiresAt time.Time) (bool, *middleware.StoredResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.entries[key]; existing != nil {
		if !existing.InFlight && !existing.ExpiresAt.After(now) {
			existing.Claim.OwnerToken = ownerToken
			existing.Claim.Generation++
			existing.Status = 0
			existing.Body = nil
			existing.ContentType = ""
			existing.InFlight = true
			existing.ExpiresAt = expiresAt
			copy := *existing
			return true, &copy, nil
		}
		copy := *existing
		copy.Body = append([]byte(nil), existing.Body...)
		return false, &copy, nil
	}
	entry := &middleware.StoredResponse{
		Claim:    middleware.IdempotencyClaim{OwnerToken: ownerToken, Generation: 1},
		InFlight: true, ExpiresAt: expiresAt,
	}
	s.entries[key] = entry
	copy := *entry
	return true, &copy, nil
}

func (s *routerIdempotencyStore) Get(_ context.Context, key string) (*middleware.StoredResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil {
		return nil, nil
	}
	copy := *entry
	copy.Body = append([]byte(nil), entry.Body...)
	return &copy, nil
}

func (s *routerIdempotencyStore) Store(_ context.Context, key string, claim middleware.IdempotencyClaim, status int, body []byte, contentType string, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil || entry.Claim != claim || !entry.InFlight {
		return context.Canceled
	}
	entry.Status = status
	entry.Body = append([]byte(nil), body...)
	entry.ContentType = contentType
	entry.InFlight = false
	entry.ExpiresAt = expiresAt
	return nil
}

func (s *routerIdempotencyStore) Delete(_ context.Context, key string, claim middleware.IdempotencyClaim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[key]
	if entry == nil || entry.Claim != claim || !entry.InFlight {
		return context.Canceled
	}
	delete(s.entries, key)
	return nil
}

func (s *routerIdempotencyStore) PurgeExpired(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	for key, entry := range s.entries {
		if !entry.InFlight && !entry.ExpiresAt.After(now) {
			delete(s.entries, key)
			deleted++
		}
	}
	return deleted, nil
}

func newStartedRouterIdempotencyCache(t *testing.T) *middleware.PGIdempotencyCache {
	t.Helper()
	cache, err := middleware.NewPGIdempotencyCache(middleware.PGIdempotencyOptions{
		Store: newRouterIdempotencyStore(),
		TTL:   time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPGIdempotencyCache() error = %v", err)
	}
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := cache.Stop(stopCtx); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})
	return cache
}

func TestIdempotencyReplayCannotBypassPublicAPIAuthentication(t *testing.T) {
	t.Parallel()

	cache := newStartedRouterIdempotencyCache(t)
	router := NewRouterWithDependencies(smokeDeps(), nil, nil, RouterOptions{
		ExtensionAPIToken:    "extension-secret",
		InstallationIdentity: sessionSecurityVersions{},
		IdempotencyCache:     cache,
	})

	newRequest := func(authenticated bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/links", strings.NewReader(`{"url":"https://example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(middleware.IdempotencyHeader, "shared-replay-key")
		if authenticated {
			req.Header.Set("Authorization", "Bearer extension-secret")
		}
		return req
	}

	authorized := httptest.NewRecorder()
	router.ServeHTTP(authorized, newRequest(true))
	if authorized.Code != http.StatusAccepted {
		t.Fatalf("authorized status = %d, want 202; body=%q", authorized.Code, authorized.Body.String())
	}

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, newRequest(false))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated retry status = %d, want 401; cached authenticated response bypassed the guard", unauthorized.Code)
	}
	if unauthorized.Header().Get("Idempotent-Replay") != "" {
		t.Fatal("unauthenticated request was served from the authenticated idempotency cache")
	}
}

type routerTodoIdempotencyProbe struct {
	handler.ReaderService
	createCalls int
	patchCalls  int
	deleteCalls int
}

func (p *routerTodoIdempotencyProbe) CreateTodo(context.Context, dto.ReaderTodoCreateRequest) (dto.ReaderTodoResponse, error) {
	p.createCalls++
	return dto.ReaderTodoResponse{
		ID:         "00000000-0000-0000-0000-000000000001",
		Text:       "离线创建",
		OriginKind: "standalone",
	}, nil
}

func (p *routerTodoIdempotencyProbe) PatchTodo(context.Context, string, dto.ReaderTodoPatchRequest) (dto.ReaderTodoResponse, error) {
	p.patchCalls++
	return dto.ReaderTodoResponse{
		ID:         "00000000-0000-0000-0000-000000000001",
		Text:       "离线创建",
		Done:       true,
		OriginKind: "standalone",
	}, nil
}

func (p *routerTodoIdempotencyProbe) DeleteTodo(context.Context, string) error {
	p.deleteCalls++
	return nil
}

func TestTodoWritesReplayThroughProductionIdempotencyChain(t *testing.T) {
	t.Parallel()

	probe := &routerTodoIdempotencyProbe{}
	deps := smokeDeps()
	deps.Reader = probe
	router := NewRouterWithDependencies(deps, nil, nil, RouterOptions{
		ExtensionAPIToken:    "extension-secret",
		InstallationIdentity: sessionSecurityVersions{},
		IdempotencyCache:     newStartedRouterIdempotencyCache(t),
	})

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		calls      func() int
	}{
		{
			name:       "create",
			method:     http.MethodPost,
			path:       "/api/todos",
			body:       `{"text":"离线创建"}`,
			wantStatus: http.StatusCreated,
			calls:      func() int { return probe.createCalls },
		},
		{
			name:       "patch",
			method:     http.MethodPatch,
			path:       "/api/todos/00000000-0000-0000-0000-000000000001",
			body:       `{"done":true}`,
			wantStatus: http.StatusOK,
			calls:      func() int { return probe.patchCalls },
		},
		{
			name:       "delete",
			method:     http.MethodDelete,
			path:       "/api/todos/00000000-0000-0000-0000-000000000001",
			wantStatus: http.StatusNoContent,
			calls:      func() int { return probe.deleteCalls },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			send := func() *httptest.ResponseRecorder {
				request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
				request.Header.Set("Authorization", "Bearer extension-secret")
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set(middleware.IdempotencyHeader, "todo-"+tc.name+"-operation")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				return response
			}

			first := send()
			if first.Code != tc.wantStatus {
				t.Fatalf("first status = %d, want %d; body=%q", first.Code, tc.wantStatus, first.Body.String())
			}
			second := send()
			if second.Code != tc.wantStatus {
				t.Fatalf("replay status = %d, want %d; body=%q", second.Code, tc.wantStatus, second.Body.String())
			}
			if second.Body.String() != first.Body.String() {
				t.Fatalf("replay body = %q, want %q", second.Body.String(), first.Body.String())
			}
			if second.Header().Get("Idempotent-Replay") != "true" {
				t.Fatal("replayed TODO write is missing Idempotent-Replay: true")
			}
			if got := tc.calls(); got != 1 {
				t.Fatalf("TODO service calls = %d, want 1", got)
			}
		})
	}
}

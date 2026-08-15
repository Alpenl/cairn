package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"webtag/internal/dto"
	"webtag/internal/httperr"
	"webtag/internal/model"
	"webtag/internal/repository"
)

type aiContextCountingStore struct {
	repository.ReaderVNextStore
	contextReads int
	lastContext  context.Context
	contextValue *model.ReaderAIContext
	err          error
}

func (s *aiContextCountingStore) GetAIContext(ctx context.Context, _ uuid.UUID) (*model.ReaderAIContext, error) {
	s.contextReads++
	s.lastContext = ctx
	if s.err != nil {
		return nil, s.err
	}
	if s.contextValue != nil {
		return s.contextValue, nil
	}
	return &model.ReaderAIContext{}, nil
}

func TestCompleteAIWhenDisabledDoesNotReadLinkContext(t *testing.T) {
	store := &aiContextCountingStore{}
	service := NewReaderVNextService(store, nil)

	response, err := service.CompleteAI(context.Background(), dto.ReaderAIRequest{
		Prompt: "summarize this",
		Scope:  "general",
		LinkID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("CompleteAI() error = %v", err)
	}
	if response.Enabled {
		t.Fatal("disabled AI response unexpectedly enabled")
	}
	if store.contextReads != 0 {
		t.Fatalf("GetAIContext() calls = %d, want 0 when AI is disabled", store.contextReads)
	}
}

type readerAIStub struct {
	calls        int
	answer       string
	model        string
	err          error
	available    *bool
	lastContext  context.Context
	lastPrompt   string
	lastScope    string
	beforeReturn func(context.Context)
}

func (s *readerAIStub) Available() bool {
	return s.available == nil || *s.available
}

func (s *readerAIStub) Complete(ctx context.Context, prompt, scope string) (string, string, error) {
	s.calls++
	s.lastContext = ctx
	s.lastPrompt = prompt
	s.lastScope = scope
	if s.beforeReturn != nil {
		s.beforeReturn(ctx)
	}
	return s.answer, s.model, s.err
}

func TestCompleteAICallsProviderAndReturnsAnswer(t *testing.T) {
	ai := &readerAIStub{answer: "answer", model: "reader-model"}
	svc := NewReaderVNextService(&aiContextCountingStore{}, ai)

	response, err := svc.CompleteAI(context.Background(), dto.ReaderAIRequest{Prompt: "question"})
	if err != nil {
		t.Fatalf("CompleteAI() error = %v", err)
	}
	if !response.Enabled || response.Answer != "answer" {
		t.Fatalf("response = %#v, want enabled answer", response)
	}
	if ai.calls != 1 {
		t.Fatalf("AI calls = %d, want 1", ai.calls)
	}
}

func TestCompleteAIReturnsProviderFailure(t *testing.T) {
	ai := &readerAIStub{err: errors.New("provider unavailable")}
	svc := NewReaderVNextService(&aiContextCountingStore{}, ai)

	if _, err := svc.CompleteAI(context.Background(), dto.ReaderAIRequest{Prompt: "question"}); err == nil {
		t.Fatal("CompleteAI() error = nil, want provider error")
	}
}

func TestCompleteAIValidatesScopeBeforeReadingContextOrCallingProvider(t *testing.T) {
	for _, test := range []struct {
		name    string
		request dto.ReaderAIRequest
		code    string
	}{
		{name: "unknown scope", request: dto.ReaderAIRequest{Prompt: "question", Scope: "admin"}, code: "ai_scope_invalid"},
		{name: "selection without text", request: dto.ReaderAIRequest{Prompt: "question", Scope: "selection", LinkID: uuid.NewString()}, code: "ai_selection_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &aiContextCountingStore{}
			ai := &readerAIStub{answer: "should not run"}
			svc := NewReaderVNextService(store, ai)

			_, err := svc.CompleteAI(context.Background(), test.request)
			if err == nil {
				t.Fatal("CompleteAI() error = nil, want validation error")
			}
			assertReaderAIHTTPError(t, err, http.StatusUnprocessableEntity, test.code)
			if store.contextReads != 0 || ai.calls != 0 {
				t.Fatalf("validation touched context/provider: reads=%d calls=%d", store.contextReads, ai.calls)
			}
		})
	}
}

func assertReaderAIHTTPError(t *testing.T, err error, status int, code string) {
	t.Helper()
	carrier, ok := httperr.As(err)
	if !ok {
		t.Fatalf("error %T = %v does not expose an HTTP carrier", err, err)
	}
	if carrier.HTTPStatus() != status {
		t.Fatalf("HTTP status = %d, want %d", carrier.HTTPStatus(), status)
	}
	coder, ok := carrier.(httperr.ErrorCoder)
	if !ok || coder.HTTPErrorCode() != code {
		got := "<missing>"
		if ok {
			got = coder.HTTPErrorCode()
		}
		t.Fatalf("HTTP error code = %q, want %q", got, code)
	}
}

func TestCompleteAIProviderCancellationMapsError(t *testing.T) {
	ai := &readerAIStub{err: context.Canceled}
	svc := NewReaderVNextService(&aiContextCountingStore{}, ai)

	_, err := svc.CompleteAI(context.Background(), dto.ReaderAIRequest{Prompt: "question"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("CompleteAI() error = %v, want wrapped context.Canceled", err)
	}
	assertReaderAIHTTPError(t, err, readerAIClientClosedRequest, "ai_request_canceled")
	if ai.lastContext == nil {
		t.Fatal("provider did not receive a request context")
	}
}

func TestCompleteAITimeoutMapsError(t *testing.T) {
	ai := &readerAIStub{err: context.DeadlineExceeded}
	svc := NewReaderVNextService(&aiContextCountingStore{}, ai)

	_, err := svc.CompleteAI(context.Background(), dto.ReaderAIRequest{Prompt: "question"})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CompleteAI() error = %v, want wrapped context.DeadlineExceeded", err)
	}
	assertReaderAIHTTPError(t, err, http.StatusGatewayTimeout, "ai_timeout")
}

func TestCompleteAICallerCancellationAfterProviderReturnsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ai := &readerAIStub{
		answer: "late answer",
		beforeReturn: func(context.Context) {
			cancel()
		},
	}
	svc := NewReaderVNextService(&aiContextCountingStore{}, ai)

	_, err := svc.CompleteAI(ctx, dto.ReaderAIRequest{Prompt: "question"})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("CompleteAI() error = %v, want context.Canceled after provider", err)
	}
	assertReaderAIHTTPError(t, err, readerAIClientClosedRequest, "ai_request_canceled")
}

func TestCompleteAIWhitespaceResponseIsNotSuccess(t *testing.T) {
	ai := &readerAIStub{answer: " \n\t "}
	svc := NewReaderVNextService(&aiContextCountingStore{}, ai)

	response, err := svc.CompleteAI(context.Background(), dto.ReaderAIRequest{Prompt: "question"})
	if err == nil {
		t.Fatal("CompleteAI() error = nil, want empty-response error")
	}
	if response.Enabled {
		t.Fatalf("response = %#v, want disabled zero response on provider empty answer", response)
	}
	assertReaderAIHTTPError(t, err, http.StatusBadGateway, "ai_empty_response")
}

func TestCompleteAIUnavailableProviderDoesNotReadTenantContext(t *testing.T) {
	available := false
	ai := &readerAIStub{available: &available}
	store := &aiContextCountingStore{contextValue: &model.ReaderAIContext{Content: "private"}}
	svc := NewReaderVNextService(store, ai)

	response, err := svc.CompleteAI(context.Background(), dto.ReaderAIRequest{Prompt: "question", LinkID: uuid.NewString()})
	if err != nil {
		t.Fatalf("CompleteAI() error = %v", err)
	}
	if response.Enabled {
		t.Fatalf("response = %#v, want capability-off response", response)
	}
	if store.contextReads != 0 || ai.calls != 0 {
		t.Fatalf("disabled provider touched context/provider: reads=%d calls=%d", store.contextReads, ai.calls)
	}
}

func TestCompleteAIPropagatesContextToStoreAndProvider(t *testing.T) {
	linkID := uuid.New()
	ai := &readerAIStub{answer: "library answer", model: "reader-model"}
	store := &aiContextCountingStore{contextValue: &model.ReaderAIContext{LinkID: linkID, Content: "library content"}}
	svc := NewReaderVNextService(store, ai)

	ctx := context.Background()
	response, err := svc.CompleteAI(ctx, dto.ReaderAIRequest{
		Prompt: "question",
		LinkID: linkID.String(),
	})
	if err != nil {
		t.Fatalf("CompleteAI() error = %v", err)
	}
	if !response.Enabled || response.Answer != "library answer" {
		t.Fatalf("response = %#v, want enabled library answer", response)
	}
	for name, ctx := range map[string]context.Context{"store": store.lastContext, "provider": ai.lastContext} {
		if ctx == nil {
			t.Fatalf("%s did not receive the request context", name)
		}
	}
	if ai.lastScope != "general" || !strings.Contains(ai.lastPrompt, "library content") {
		t.Fatalf("provider request = scope %q prompt %q, want installation library context", ai.lastScope, ai.lastPrompt)
	}
}

func TestCompleteAIErrorDoesNotExposePromptOrProviderPayload(t *testing.T) {
	promptSecret := "prompt-secret-should-not-leak"
	providerSecret := "provider-response-secret-should-not-leak"
	ai := &readerAIStub{err: errors.New("upstream rejected: " + providerSecret)}
	svc := NewReaderVNextService(&aiContextCountingStore{}, ai)

	_, err := svc.CompleteAI(context.Background(), dto.ReaderAIRequest{Prompt: promptSecret})
	if err == nil {
		t.Fatal("CompleteAI() error = nil, want provider error")
	}
	if strings.Contains(err.Error(), promptSecret) || strings.Contains(err.Error(), providerSecret) {
		t.Fatalf("error = %q contains prompt/provider payload", err.Error())
	}
	carrier, ok := httperr.As(err)
	if !ok || strings.Contains(carrier.HTTPMessage(), promptSecret) || strings.Contains(carrier.HTTPMessage(), providerSecret) {
		t.Fatalf("HTTP error message = %q contains sensitive payload", carrier.HTTPMessage())
	}
}

func TestReaderAIContextTextHasOneTotalBudget(t *testing.T) {
	contextText := readerAIContextText(model.ReaderAIContext{
		Content:  strings.Repeat("内容", 7000),
		Summary:  strings.Repeat("摘要", 2000),
		Tags:     []string{strings.Repeat("标签", 1000)},
		Thoughts: []model.ReaderAIThoughtContext{{Body: strings.Repeat("想法", 1000)}},
	})
	if got := len([]rune(contextText)); got > readerAIMaxContextRunes {
		t.Fatalf("context runes = %d, want <= %d", got, readerAIMaxContextRunes)
	}
	if !strings.HasSuffix(contextText, readerAIContextTruncatedMarker) {
		t.Fatalf("context = %q, want truncation marker", contextText[len(contextText)-min(len(contextText), 80):])
	}
}

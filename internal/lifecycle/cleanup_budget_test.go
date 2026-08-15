package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

type cleanupBudgetTestContextKey struct{}

func TestCleanupBudgetIsLazyDetachedAndShared(t *testing.T) {
	t.Parallel()

	parent, cancelParent := context.WithCancel(context.WithValue(
		context.Background(),
		cleanupBudgetTestContextKey{},
		"rf7",
	))
	budgetCtx, budget := WithCleanupBudget(parent, time.Second)
	t.Cleanup(budget.Cancel)
	if _, hasDeadline := budgetCtx.Deadline(); hasDeadline {
		t.Fatal("WithCleanupBudget started its deadline before cleanup began")
	}
	cancelParent()

	first, cancelFirst := CleanupContext(budgetCtx, 5*time.Second)
	defer cancelFirst()
	if err := first.Err(); err != nil {
		t.Fatalf("detached cleanup context error = %v", err)
	}
	if got := first.Value(cleanupBudgetTestContextKey{}); got != "rf7" {
		t.Fatalf("cleanup context value = %v, want rf7", got)
	}
	firstDeadline, ok := first.Deadline()
	if !ok {
		t.Fatal("first cleanup context has no deadline")
	}

	second, cancelSecond := CleanupContext(budgetCtx, 10*time.Second)
	defer cancelSecond()
	secondDeadline, ok := second.Deadline()
	if !ok {
		t.Fatal("second cleanup context has no deadline")
	}
	if !firstDeadline.Equal(secondDeadline) {
		t.Fatalf("cleanup deadlines differ: first=%v second=%v", firstDeadline, secondDeadline)
	}
}

func TestCleanupBudgetCancelBeforeActivationIsTerminal(t *testing.T) {
	t.Parallel()

	budgetCtx, budget := WithCleanupBudget(context.Background(), time.Second)
	budget.Cancel()
	cleanupCtx, cancel := CleanupContext(budgetCtx, time.Second)
	defer cancel()
	if !errors.Is(cleanupCtx.Err(), context.Canceled) {
		t.Fatalf("cleanup context error = %v, want context.Canceled", cleanupCtx.Err())
	}
}

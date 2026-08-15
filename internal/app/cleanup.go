package app

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type cleanupAction struct {
	name string
	run  func(context.Context) error
}

// cleanupStack records ownership in acquisition/start order and releases it in
// reverse order. Run always attempts every action and joins named failures.
type cleanupStack struct {
	actions []cleanupAction
}

func (s *cleanupStack) Add(name string, action func(context.Context) error) {
	if s == nil || action == nil {
		return
	}
	s.actions = append(s.actions, cleanupAction{name: name, run: action})
}

func (s *cleanupStack) Run(ctx context.Context) error {
	if s == nil {
		return nil
	}

	actions := s.actions
	s.actions = nil
	var cleanupErr error
	for index := len(actions) - 1; index >= 0; index-- {
		action := actions[index]
		if err := action.run(ctx); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup %s: %w", action.name, err))
		}
	}
	return cleanupErr
}

func (s *cleanupStack) RunDetached(parent context.Context, timeout time.Duration) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	return s.Run(cleanupCtx)
}

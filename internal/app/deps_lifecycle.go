package app

import (
	"context"
	"fmt"
	"time"
)

type runtimeBackground interface {
	Start(context.Context) error
	Stop(context.Context) error
}

type namedRuntimeBackground struct {
	name       string
	background runtimeBackground
}

// runtimeResources owns the process-lifetime resources assembled by
// BuildRuntime. HTTP shutdown prevents new handlers before Close runs, so the
// only required order is to stop backgrounds in reverse construction order and
// close PostgreSQL last.
type runtimeResources struct {
	backgrounds []namedRuntimeBackground
	persistence *persistenceLayer
}

func newRuntimeResources(backgrounds []namedRuntimeBackground, persistence *persistenceLayer) *runtimeResources {
	return &runtimeResources{backgrounds: backgrounds, persistence: persistence}
}

func (r *runtimeResources) Start(ctx context.Context) error {
	if r == nil {
		return nil
	}
	for _, entry := range r.backgrounds {
		if entry.background == nil {
			continue
		}
		if err := entry.background.Start(ctx); err != nil {
			return fmt.Errorf("start %s: %w", entry.name, err)
		}
	}
	return nil
}

func (r *runtimeResources) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	var cleanup cleanupStack
	if r.persistence != nil {
		cleanup.Add(runtimeBuildPersistenceLayer, r.persistence.Close)
	}
	for _, entry := range r.backgrounds {
		if entry.background != nil {
			cleanup.Add(entry.name, entry.background.Stop)
		}
	}
	return cleanup.Run(ctx)
}

func durationMS(ms int) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"webtag/internal/observability"
)

// runtimeOwnedResource is the ownership seam used by production acquisition.
// Each adapter stores one cleanup target; its identity and cleanup action are
// therefore derived from the same object instead of being paired by callers.
type runtimeOwnedResource interface {
	identity() any
	cleanupAction() func(context.Context) error
}

type runtimeAcquiredResource struct {
	owned runtimeOwnedResource

	// resource and cleanup keep the zero-value acquisition helper useful in
	// focused cleanup-stack tests. Production acquisitions reject this split
	// representation and must use one of the bound constructors below.
	resource any
	cleanup  func(context.Context) error
}

type runtimeBackgroundOwnership struct {
	background runtimeManagedBackground
}

func (o runtimeBackgroundOwnership) identity() any {
	return o.background
}

func (o runtimeBackgroundOwnership) cleanupAction() func(context.Context) error {
	if isNilRuntimeResource(o.background) {
		return nil
	}
	return o.background.Stop
}

type runtimeTracerOwnership struct {
	shutdown observability.TracerShutdown
}

func (o runtimeTracerOwnership) identity() any {
	return o.shutdown
}

func (o runtimeTracerOwnership) cleanupAction() func(context.Context) error {
	if o.shutdown == nil {
		return nil
	}
	return o.shutdown
}

type runtimePersistenceOwnership struct {
	layer *persistenceLayer
}

func (o runtimePersistenceOwnership) identity() any {
	return o.layer
}

func (o runtimePersistenceOwnership) cleanupAction() func(context.Context) error {
	if o.layer == nil {
		return nil
	}
	return o.layer.cleanupBuildFailure
}

func newRuntimeBackgroundAcquiredResource(background runtimeManagedBackground) runtimeAcquiredResource {
	if isNilRuntimeResource(background) {
		return runtimeAcquiredResource{}
	}
	return runtimeAcquiredResource{owned: runtimeBackgroundOwnership{background: background}}
}

func newRuntimeTracerAcquiredResource(shutdown observability.TracerShutdown) runtimeAcquiredResource {
	if shutdown == nil {
		return runtimeAcquiredResource{}
	}
	return runtimeAcquiredResource{owned: runtimeTracerOwnership{shutdown: shutdown}}
}

func newRuntimePersistenceAcquiredResource(layer *persistenceLayer) runtimeAcquiredResource {
	if layer == nil {
		return runtimeAcquiredResource{}
	}
	return runtimeAcquiredResource{owned: runtimePersistenceOwnership{layer: layer}}
}

func (r runtimeAcquiredResource) resolveOwnership() (
	resource any,
	cleanup func(context.Context) error,
	bound bool,
) {
	if r.owned != nil {
		return r.owned.identity(), r.owned.cleanupAction(), true
	}
	return r.resource, r.cleanup, false
}

type runtimeResourceAcquire func() (runtimeAcquiredResource, error)

// runtimeBuildAcquisitionHooks is the composition-root fault seam. The
// post-acquire hook runs only after construction succeeded and cleanup was
// registered, so an injected failure exercises ownership of the target
// resource as well as every earlier acquisition.
type runtimeBuildAcquisitionHooks struct {
	afterAcquire         func(string, any) error
	beforeCleanup        func(string, any)
	beforeRouterTransfer func(*runtimeRouterDependencies)
	beforeStartTransfer  func(*runtimeStartResources)
}

// runtimeBuildAcquisitions makes successful construction and cleanup
// ownership one operation. A constructor cannot report success without its
// cleanup being placed on the build-failure stack.
type runtimeBuildAcquisitions struct {
	owned                 cleanupStack
	hooks                 runtimeBuildAcquisitionHooks
	inventory             []string
	checkpoint            int
	resources             map[string]any
	requireBoundOwnership bool
}

func newProductionRuntimeBuildAcquisitions(hooks runtimeBuildAcquisitionHooks) runtimeBuildAcquisitions {
	return runtimeBuildAcquisitions{
		hooks:                 hooks,
		inventory:             append([]string(nil), productionRuntimeBuildInventory[:]...),
		resources:             make(map[string]any, len(productionRuntimeBuildInventory)),
		requireBoundOwnership: true,
	}
}

func (a *runtimeBuildAcquisitions) Acquire(name string, acquire runtimeResourceAcquire) error {
	if acquire == nil {
		return fmt.Errorf("acquire %s: nil constructor", name)
	}
	if a == nil {
		return fmt.Errorf("acquire %s: nil ownership inventory", name)
	}
	if err := a.advanceInventory(name); err != nil {
		return err
	}
	acquired, err := acquire()
	resource, cleanup, bound := acquired.resolveOwnership()
	cleanup = a.observeCleanup(name, resource, cleanup)
	a.owned.Add(name, cleanup)
	if ownershipErr := a.boundOwnershipError(name, resource, cleanup, bound, err); ownershipErr != nil {
		return ownershipErr
	}
	if err != nil {
		// A constructor may discover an error after acquiring a partially
		// usable resource. Preserve any cleanup it returned so the deferred
		// failure stack still owns that resource.
		return fmt.Errorf("acquire %s: %w", name, err)
	}
	a.rememberResource(name, resource)
	if a.hooks.afterAcquire != nil {
		if hookErr := a.hooks.afterAcquire(name, resource); hookErr != nil {
			return fmt.Errorf("acquire %s: %w", name, hookErr)
		}
	}
	return nil
}

func (a *runtimeBuildAcquisitions) advanceInventory(name string) error {
	if len(a.inventory) == 0 {
		return nil
	}
	if a.checkpoint >= len(a.inventory) {
		return fmt.Errorf("runtime build inventory has unexpected checkpoint %q", name)
	}
	want := a.inventory[a.checkpoint]
	if name != want {
		return fmt.Errorf(
			"runtime build inventory checkpoint %d = %q, want %q",
			a.checkpoint,
			name,
			want,
		)
	}
	a.checkpoint++
	return nil
}

func (a *runtimeBuildAcquisitions) observeCleanup(
	name string,
	resource any,
	cleanup func(context.Context) error,
) func(context.Context) error {
	if cleanup == nil || a.hooks.beforeCleanup == nil {
		return cleanup
	}
	return func(ctx context.Context) error {
		a.hooks.beforeCleanup(name, resource)
		return cleanup(ctx)
	}
}

func (a *runtimeBuildAcquisitions) boundOwnershipError(
	name string,
	resource any,
	cleanup func(context.Context) error,
	bound bool,
	acquireErr error,
) error {
	if !a.requireBoundOwnership || bound || (resource == nil && cleanup == nil) {
		return nil
	}
	ownershipErr := fmt.Errorf("acquire %s: production resource must use bound ownership", name)
	if acquireErr == nil {
		return ownershipErr
	}
	return errors.Join(fmt.Errorf("acquire %s: %w", name, acquireErr), ownershipErr)
}

func (a *runtimeBuildAcquisitions) rememberResource(name string, resource any) {
	if a.resources == nil {
		a.resources = make(map[string]any)
	}
	a.resources[name] = resource
}

// BindStartResources verifies the ownership handoff before any background can
// start. Labels and dynamic types are insufficient here: every Start target
// must be the exact instance registered by the matching acquisition.
func (a *runtimeBuildAcquisitions) BindStartResources(resources *runtimeStartResources) error {
	if a == nil || resources == nil {
		return errors.New("bind runtime start resources: nil ownership inventory")
	}
	if a.hooks.beforeStartTransfer != nil {
		a.hooks.beforeStartTransfer(resources)
	}
	for _, binding := range resources.bindings() {
		acquired, ok := a.resources[binding.name]
		if !ok {
			return fmt.Errorf("bind runtime start resource %q: no matching acquisition", binding.name)
		}
		if !sameRuntimeResourceInstance(acquired, binding.background) {
			return fmt.Errorf(
				"bind runtime start resource %q: start target is a different instance from acquired resource",
				binding.name,
			)
		}
	}
	return nil
}

// BindRouterDependencies verifies the earlier handoff into the HTTP router.
// These request-path dependencies must be the same instances that acquisition
// cleanup and Runtime.Start own; a fresh non-nil substitute would otherwise
// evade nil checks while never being started or stopped.
func (a *runtimeBuildAcquisitions) BindRouterDependencies(
	dependencies runtimeRouterDependencies,
) (*runtimeRouterBinding, error) {
	if a == nil {
		return nil, errors.New("bind runtime router dependencies: nil ownership inventory")
	}
	if a.hooks.beforeRouterTransfer != nil {
		a.hooks.beforeRouterTransfer(&dependencies)
	}
	if err := a.validateRouterDependencies(dependencies); err != nil {
		return nil, err
	}
	return &runtimeRouterBinding{inventory: a, dependencies: dependencies}, nil
}

func (a *runtimeBuildAcquisitions) validateRouterDependencies(
	dependencies runtimeRouterDependencies,
) error {
	if a == nil {
		return errors.New("validate runtime router dependencies: nil ownership inventory")
	}
	bindings := []struct {
		name     string
		resource any
	}{
		{name: runtimeBuildIdempotencyCache, resource: dependencies.idempotencyCache},
	}
	for _, binding := range bindings {
		acquired, ok := a.resources[binding.name]
		if !ok {
			return fmt.Errorf(
				"bind runtime router dependency %q: no matching acquisition",
				binding.name,
			)
		}
		if !sameRuntimeResourceInstance(acquired, binding.resource) {
			return fmt.Errorf(
				"bind runtime router dependency %q: router target is a different instance from acquired resource",
				binding.name,
			)
		}
	}
	return nil
}

func sameRuntimeResourceInstance(left, right any) bool {
	leftNil := isNilRuntimeResource(left)
	rightNil := isNilRuntimeResource(right)
	if leftNil || rightNil {
		return leftNil && rightNil
	}
	leftValue := reflect.ValueOf(left)
	rightValue := reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	switch leftValue.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return leftValue.Pointer() == rightValue.Pointer()
	default:
		if !leftValue.Comparable() {
			return false
		}
		return leftValue.Interface() == rightValue.Interface()
	}
}

func isNilRuntimeResource(resource any) bool {
	if resource == nil {
		return true
	}
	value := reflect.ValueOf(resource)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// CleanupFailure releases every successfully acquired resource under one
// detached budget and preserves both the primary and every cleanup error.
func (a *runtimeBuildAcquisitions) CleanupFailure(
	parent context.Context,
	timeout time.Duration,
	primary error,
) (finalErr, cleanupErr error) {
	if primary == nil {
		if len(a.inventory) > 0 && a.checkpoint != len(a.inventory) {
			primary = fmt.Errorf(
				"runtime build inventory incomplete: reached %d of %d checkpoints",
				a.checkpoint,
				len(a.inventory),
			)
		} else {
			// Build succeeded: ownership moves to runtimeLifecycle. Explicitly
			// disarm the deferred build-failure stack.
			a.owned.actions = nil
			return nil, nil
		}
	}
	cleanupErr = a.owned.RunDetached(parent, timeout)
	if cleanupErr == nil {
		return primary, nil
	}
	return errors.Join(primary, fmt.Errorf("runtime build cleanup: %w", cleanupErr)), cleanupErr
}

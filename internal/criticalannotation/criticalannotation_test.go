/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package criticalannotation

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// recordingUpdater is a managed.CriticalAnnotationUpdater test double that
// records the state of the context it was called with -- captured DURING
// the call, before WrapUpdater's deferred cancel of its fresh context runs
// -- so tests can assert on the deadline/cancellation state the real write
// would have observed, rather than only on whether it returned an error.
type recordingUpdater struct {
	called      bool
	gotErr      error // ctx.Err() observed during the call
	gotDeadline bool  // whether ctx.Deadline() reported one, observed during the call
	err         error
}

func (u *recordingUpdater) UpdateCriticalAnnotations(ctx context.Context, _ client.Object) error {
	u.called = true
	u.gotErr = ctx.Err()
	_, u.gotDeadline = ctx.Deadline()
	return u.err
}

// TestWrapUpdaterClearsPendingAnnotationAfterCreateExhaustsContext models
// the Create()-fails path this package exists to fix: the reconcile
// context is already past its deadline by the time the critical-annotation
// write runs, exactly as it is immediately after a Create() call that
// consumed the entire external-operation budget on a failing SSH command.
// Without the fix, the wrapped updater would receive that same expired
// context and its own client.Update call would fail instantly with
// context.DeadlineExceeded, leaving the pending-create marker dangling.
func TestWrapUpdaterClearsPendingAnnotationAfterCreateExhaustsContext(t *testing.T) {
	// Simulate the parent context Create() just exhausted: give it a
	// deadline in the past, as context.WithTimeout would produce once the
	// timeout has elapsed.
	exhausted, cancel := context.WithTimeout(context.Background(), -1*time.Second)
	defer cancel()

	if exhausted.Err() == nil {
		t.Fatal("test setup invariant broken: parent context must already be expired")
	}

	next := &recordingUpdater{}
	wrapped := WrapUpdater(next)

	if err := wrapped.UpdateCriticalAnnotations(exhausted, nil); err != nil {
		t.Fatalf("UpdateCriticalAnnotations() with an exhausted parent context returned an error, want nil (annotation write must still succeed): %v", err)
	}

	if !next.called {
		t.Fatal("wrapped updater was never called")
	}
	if next.gotErr != nil {
		t.Fatalf("wrapped updater received a context that was already done (%v) -- the exhausted parent's deadline/cancellation leaked through instead of being detached", next.gotErr)
	}
	if !next.gotDeadline {
		t.Error("wrapped updater received a context with no deadline at all -- it should carry its own fresh, bounded timeout")
	}
}

// TestWrapUpdaterPropagatesUnderlyingError proves the decorator is
// transparent to a genuine failure from the wrapped updater (e.g. a real
// API server error, not a context problem) -- it must not swallow it.
func TestWrapUpdaterPropagatesUnderlyingError(t *testing.T) {
	want := errors.New("boom")
	next := &recordingUpdater{err: want}
	wrapped := WrapUpdater(next)

	err := wrapped.UpdateCriticalAnnotations(context.Background(), nil)
	if !errors.Is(err, want) {
		t.Fatalf("UpdateCriticalAnnotations() error = %v, want it to wrap/equal %v", err, want)
	}
}

// TestWrapUpdaterDetachesFromCancellation proves the fresh context is not
// cancelled merely because the caller's context was cancelled -- only a
// deadline exceeding the wrapper's own fresh budget should end it.
func TestWrapUpdaterDetachesFromCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the call, same shape as an already-cancelled reconcile context

	next := &recordingUpdater{}
	wrapped := WrapUpdater(next)

	if err := wrapped.UpdateCriticalAnnotations(ctx, nil); err != nil {
		t.Fatalf("UpdateCriticalAnnotations() with a cancelled parent context returned an error, want nil: %v", err)
	}
	if next.gotErr != nil {
		t.Fatalf("wrapped updater received a context that was already cancelled: %v", next.gotErr)
	}
}

// Compile-time assertion that WrapUpdater's return value satisfies the
// interface managed.WithCriticalAnnotationUpdater expects.
var _ managed.CriticalAnnotationUpdater = WrapUpdater(nil)

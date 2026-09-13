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

// Package criticalannotation gives the reconciler's critical-annotation
// write -- the call that clears the pending-create marker once Create
// returns -- a context budget of its own, instead of reusing whatever is
// left of the context Create() just spent.
//
// The reconciler persists that annotation using a context derived from the
// SAME per-reconcile budget passed to Create: a fixed grace period beyond
// Create's own deadline, not a fresh one. Every controller in this provider
// spends that entire budget on Create's blocking SSH install/join call.
// Whether that call succeeds, fails fast, or times out, by the time it
// returns there is little or no budget left for the annotation write that
// follows -- and a write that fails with context.DeadlineExceeded leaves
// the pending-create marker dangling with no success/failure sibling,
// wedging the resource on the create-ambiguity guard forever, with no
// further retries: the reconciler treats that state as unsafe to act on
// again.
//
// WrapUpdater detaches the write from the exhausted parent context
// (dropping its cancellation and deadline, but keeping its values, e.g.
// for tracing) and gives it a fresh, bounded budget of its own, so the
// annotation is always cleared -- on the Create-error path as well as the
// success path.
package criticalannotation

import (
	"context"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// budget is the fresh timeout given to the detached critical-annotation
// write. It only ever has to cover a handful of Kubernetes API calls (a Get
// plus an Update, retried a few times on conflict) -- never an external SSH
// call -- so it is sized generously against that, not against the
// controller's own external-operation timeout.
const budget = 30 * time.Second

// WrapUpdater decorates next so every call runs under a context detached
// from the caller's cancellation/deadline, with its own fresh timeout. Use
// it to wrap managed.NewRetryingCriticalAnnotationUpdater (or any other
// managed.CriticalAnnotationUpdater) via managed.WithCriticalAnnotationUpdater.
func WrapUpdater(next managed.CriticalAnnotationUpdater) managed.CriticalAnnotationUpdater {
	return managed.CriticalAnnotationUpdateFn(func(ctx context.Context, o client.Object) error {
		fresh, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
		defer cancel()
		return next.UpdateCriticalAnnotations(fresh, o)
	})
}

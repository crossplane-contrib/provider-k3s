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

// Package driftdetection implements provider-agnostic, per-field drift
// ownership for Crossplane managed resources.
//
// It is a decorator over crossplane-runtime's TypedExternalConnector and
// contains NO per-resource logic: managed resources are inspected through
// fieldpath's unstructured projection, so one implementation serves every MR
// type.
//
// # Semantics
//
//		spec:
//		  driftDetection:
//		    mode: enabled          # enabled (default) | warn | disabled
//		    ignore:
//		      - paths:
//		          - forProvider.k3sVersion
//
//	  - enabled  — detect and correct drift. Paths under ignore are owned by
//	    someone else: seeded from spec on create, then neither
//	    compared nor written from spec.
//	  - warn     — detect drift, report it on the DriftDetected condition, do
//	    not correct it. The adoption mode: observe what actually
//	    drifts before deciding what to hand over.
//	  - disabled — neither detect nor correct.
//
// # Why an ignored field is substituted rather than skipped
//
// Reconfiguring k3s is a whole-command operation: both the server install
// script and the agent/server join script are re-run with the FULL set of
// flags every time, so a field simply omitted from the command line is
// reset to the script's own default rather than left alone. An ignored path
// is therefore substituted: the value observed on the external resource is
// written over the spec value before the resource is compared and before
// the install/join command is built. The field then compares equal by
// construction, and the command line carries the value its real owner set.
//
// This relies on every generated managed resource's observation type
// mirroring every configurable field under the same name: forProvider.x
// always has a counterpart at status.atProvider.x. Pointer-vs-value
// differences between the two are absorbed by the unstructured projection.
//
// # Ignore-path eligibility
//
// Cluster and Node are both addressed by spec.forProvider.host: this
// provider carries no external-name, so host is the only field either
// resource's identity is derived from, and it is denylisted below. Port
// addresses the same SSH connection target as host and is declared
// immutable on both resources for the same reason, but it is not itself an
// identity-participating field in the fieldpath sense — it names no object
// on the remote side — so it is left off the denylist and instead falls out
// of the ordinary write-only check below whenever it has no
// status.atProvider counterpart.
//
// Because host is the whole of the identity-participating class on this
// provider, and because a path is also mechanically rejected the moment it
// names a field the resource's own Observation type does not mirror (the
// write-only class — nothing to substitute), the declaration below is a
// denylist rather than an allowlist: it is the smaller of the two here, and
// it stays correct as the observation mirror is widened over time.
//
// # Freshness and race safety
//
// crossplane-runtime constructs one external client per reconcile (Connect at
// the top of Reconcile, Disconnect deferred to its end) and calls Observe
// strictly before Update on the same in-memory object. This decorator therefore
// captures the observed values of ignored paths into per-reconcile state during
// Observe, and Update consumes that snapshot instead of re-reading
// status.atProvider.
//
// That ordering is what makes the substitution safe:
//
//   - The values written on update are from this reconcile's observation, not a
//     previous one. Re-reading status.atProvider in Update would read whatever
//     the object happens to carry, which for any code path that does not
//     repopulate it is last poll's value.
//   - Update fails closed if no observation was captured. Falling back to the
//     spec value would revert the field's real owner, which is the exact
//     outcome the ignore list exists to prevent.
//   - Substitution always happens on a deep copy, so the reconciler's managed
//     resource — and any cache it was decoded from — is never mutated, and
//     there is no interaction with ResourceLateInitialized persistence.
//
// A lost-update window remains between the observation and the write: if the
// external owner changes the field in that interval, the update writes the
// value observed a moment earlier and the owner reasserts on its next pass. It
// cannot be closed by a read-modify-write client; APIs offering optimistic
// concurrency (ETag/If-Match) close it in the inner client. The window only
// opens during updates that were going to happen anyway — drift confined to
// ignored paths never triggers an update.
package driftdetection

import (
	"context"
	"reflect"
	"strings"
	"sync"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
)

// Mode selects the drift detection behaviour for a managed resource.
type Mode string

const (
	// ModeEnabled detects and corrects drift. Default when unset, so adding
	// the field to an API is not a behaviour change for existing resources.
	ModeEnabled Mode = "enabled"
	// ModeWarn detects drift and reports it, but does not correct it.
	ModeWarn Mode = "warn"
	// ModeDisabled neither detects nor corrects drift.
	ModeDisabled Mode = "disabled"
)

// TypeDriftDetected is set on resources whose drift is observed but not
// corrected — either because mode is warn, or because every drifting field is
// under ignore.
const TypeDriftDetected xpv2.ConditionType = "DriftDetected"

// Condition reasons for TypeDriftDetected.
const (
	ReasonInSync     xpv2.ConditionReason = "InSync"
	ReasonDrifted    xpv2.ConditionReason = "Drifted"
	ReasonIgnored    xpv2.ConditionReason = "DriftIgnored"
	ReasonUnobserved xpv2.ConditionReason = "IgnoredPathUnobserved"
)

const (
	pathMode        = "spec.driftDetection.mode"
	pathIgnorePaths = "spec.driftDetection.ignore[*].paths[*]"

	prefixSpec        = "spec."
	prefixForProvider = "forProvider."
	prefixAtProvider  = "status.atProvider."
)

const (
	errPave      = "cannot project managed resource for drift detection"
	errUnpave    = "cannot rebuild managed resource after path substitution"
	errSnapshot  = "cannot read observed value for ignored path"
	errApply     = "cannot substitute observed value for ignored path"
	errBadPath   = "invalid drift detection ignore path"
	errNoObserve = "refusing to update with ignored paths before an observation was captured: " +
		"updating from spec would revert the field's external owner"
	errIneligible = "ineligible drift detection ignore path"
)

// identityDenylist names forProvider field segments that a managed
// resource's own external identity is derived from, provider-wide. A path
// whose final segment appears here is rejected outright, whether or not the
// field actually exists on the resource in question — see the eligibility
// discussion in the package doc comment.
var identityDenylist = map[string]bool{
	// Cluster and Node: the SSH target this provider addresses the remote
	// machine by, and — with no external-name implemented anywhere in this
	// provider — the only field either resource's identity is derived from.
	// Substituting an observed value here would re-target the SSH session
	// entirely, not retarget a value on the same host.
	"host": true,
}

// Config is the resolved drift detection configuration for one resource.
type Config struct {
	Mode        Mode
	IgnorePaths []string
}

// Active reports whether the configuration changes any behaviour. A resource
// with no driftDetection block reconciles exactly as it did before.
func (c Config) Active() bool { return c.Mode != ModeEnabled || len(c.IgnorePaths) > 0 }

// ReadConfig extracts the drift detection configuration from any managed
// resource without knowing its type. A resource whose schema has no
// driftDetection block yields the inert default.
//
// Every ignore path is validated against the eligibility denylist before it
// is accepted. A single ineligible path fails the whole configuration: there
// is no partial application and no "ignore the rest of the list" fallback,
// because a config half-applied from a rejected request is as unreviewable as
// one silently accepted in full.
func ReadConfig(mg resource.Managed) (Config, error) { //nolint:gocyclo // sequential validation steps (mode, wildcard expansion, per-path normalise+eligibility); each branch is a single early return, not nested control flow.
	cfg := Config{Mode: ModeEnabled}

	p, err := fieldpath.PaveObject(mg)
	if err != nil {
		return cfg, errors.Wrap(err, errPave)
	}

	switch m, err := p.GetString(pathMode); {
	case fieldpath.IsNotFound(err):
	case err != nil:
		return cfg, errors.Wrap(err, errPave)
	case m != "":
		cfg.Mode = Mode(m)
	}

	expanded, err := p.ExpandWildcards(pathIgnorePaths)
	switch {
	case fieldpath.IsNotFound(err):
		expanded = nil
	case err != nil:
		return cfg, errors.Wrap(err, errPave)
	}
	for _, e := range expanded {
		s, err := p.GetString(e)
		if err != nil {
			return cfg, errors.Wrap(err, errPave)
		}
		n, err := normalizePath(s)
		if err != nil {
			return cfg, err
		}
		if err := checkEligible(mg, n); err != nil {
			return cfg, err
		}
		cfg.IgnorePaths = append(cfg.IgnorePaths, n)
	}

	return cfg, nil
}

// checkEligible rejects any path whose final segment is an
// identity-participating field, any path naming a field that does not exist
// on this resource's own configurable fields, and any path naming a field
// with no counterpart in the resource's own Observation type (write-only —
// there is nothing to substitute). It is a hard rejection in every case —
// never a skip — because a well-formed path that silently does nothing is
// indistinguishable, from the operator's side, from one that is doing
// exactly what was asked.
func checkEligible(mg resource.Managed, path string) error {
	return checkEligibleType(reflect.TypeOf(mg), path)
}

// checkEligibleType is checkEligible's type-only core, split out so the
// mechanical write-only/existence checks can be unit-tested against
// synthetic types without needing a full resource.Managed implementation.
func checkEligibleType(t reflect.Type, path string) error {
	segs := strings.Split(path, ".")
	last := segs[len(segs)-1]
	if identityDenylist[last] {
		return errors.Errorf("%s %q: %q is an identity-participating field and may never be ignored", errIneligible, path, last)
	}

	if fieldType(t, append([]string{"spec"}, segs...)) == nil {
		return errors.Errorf("%s %q: field does not exist on this resource", errIneligible, path)
	}

	atSegs := append([]string{"status", "atProvider"}, strings.Split(strings.TrimPrefix(path, prefixForProvider), ".")...)
	if fieldType(t, atSegs) == nil {
		return errors.Errorf("%s %q: field has no counterpart in status.atProvider and cannot be substituted (write-only)", errIneligible, path)
	}
	return nil
}

// fieldType walks segs — a sequence of JSON field names rooted at t — through
// t's own Go type, matched by JSON tag name rather than by any value
// currently held. Matching on the type rather than on a paved value is what
// lets an omitempty field holding its zero value still be recognised as
// present: a paved projection of the value would drop it from the map
// indistinguishably from a field that does not exist at all. Returns nil if
// any segment does not resolve.
func fieldType(t reflect.Type, segs []string) reflect.Type {
	for _, seg := range segs {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return nil
		}
		next, ok := structFieldByJSONName(t, seg)
		if !ok {
			return nil
		}
		t = next
	}
	return t
}

// structFieldByJSONName searches t's direct fields for one whose JSON tag
// name equals name, descending into anonymous (embedded/inlined) struct
// fields when a direct match is not found — mirroring how encoding/json
// promotes their fields to the parent object.
func structFieldByJSONName(t reflect.Type, name string) (reflect.Type, bool) {
	var embedded []reflect.StructField

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		jsonName := strings.Split(tag, ",")[0]
		if jsonName == "-" {
			continue
		}
		if jsonName == name {
			return f.Type, true
		}
		if jsonName == "" && f.Anonymous {
			embedded = append(embedded, f)
		}
	}
	for _, f := range embedded {
		et := f.Type
		for et.Kind() == reflect.Pointer {
			et = et.Elem()
		}
		if et.Kind() != reflect.Struct {
			continue
		}
		if ft, ok := structFieldByJSONName(et, name); ok {
			return ft, true
		}
	}
	return nil, false
}

// normalizePath accepts either fieldpath grammar (forProvider.a.b — the grammar
// Composition patches use) or JSON Pointer (/forProvider/a/b, the
// Flux/provider-helm spelling) and returns the fieldpath form.
//
// Indices and wildcards are rejected. Positional identity in an API collection
// is not stable: aligning spec[i] with observed[i] silently writes one
// element's value onto another when the server reorders or resizes the list.
// Whole-list ownership (forProvider.dnsServers) is supported and correct.
func normalizePath(s string) (string, error) {
	p := strings.TrimSpace(s)
	if p == "" {
		return "", errors.New(errBadPath + ": empty")
	}

	if strings.HasPrefix(p, "/") {
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		for i, part := range parts {
			part = strings.ReplaceAll(part, "~1", "/") // RFC 6901
			parts[i] = strings.ReplaceAll(part, "~0", "~")
		}
		p = strings.Join(parts, ".")
	}

	if strings.ContainsAny(p, "[]*") {
		return "", errors.Errorf("%s %q: list indices and wildcards are not supported; ignore the whole list instead", errBadPath, s)
	}
	if !strings.HasPrefix(p, prefixForProvider) {
		return "", errors.Errorf("%s %q: must start with %q", errBadPath, s, prefixForProvider)
	}
	if _, err := fieldpath.Parse(p); err != nil {
		return "", errors.Wrap(err, errBadPath+": "+s)
	}
	return p, nil
}

// observation is the per-reconcile record of what the external resource
// reported for each ignored path.
type observation struct {
	// values holds the observed value for every ignored path the external
	// resource reported. A path absent from this map was not reported.
	values map[string]any
	// unobserved lists ignored paths the external resource did not report.
	// Their seed value stands: there is nothing to carry forward.
	unobserved []string
}

// snapshot reads the status.atProvider counterpart of each ignored path from
// mg. It MUST be called immediately after the wrapped client's Observe, while
// status.atProvider holds this reconcile's response.
func snapshot(mg resource.Managed, paths []string) (*observation, error) {
	obs := &observation{values: make(map[string]any, len(paths))}
	if len(paths) == 0 {
		return obs, nil
	}

	p, err := fieldpath.PaveObject(mg)
	if err != nil {
		return nil, errors.Wrap(err, errPave)
	}

	for _, path := range paths {
		at := prefixAtProvider + strings.TrimPrefix(path, prefixForProvider)
		v, err := p.GetValue(at)
		switch {
		case fieldpath.IsNotFound(err):
			obs.unobserved = append(obs.unobserved, path)
		case err != nil:
			return nil, errors.Wrap(err, errSnapshot)
		default:
			obs.values[path] = v
		}
	}
	return obs, nil
}

// apply returns a deep copy of mg with every snapshotted path set to its
// observed value. mg itself is never modified.
func apply[T resource.Managed](mg T, obs *observation) (T, error) {
	var zero T

	shadow, ok := mg.DeepCopyObject().(T)
	if !ok {
		return zero, errors.New(errApply)
	}
	if obs == nil || len(obs.values) == 0 {
		return shadow, nil
	}

	p, err := fieldpath.PaveObject(shadow)
	if err != nil {
		return zero, errors.Wrap(err, errPave)
	}
	for path, v := range obs.values {
		if err := p.SetValue(prefixSpec+path, v); err != nil {
			return zero, errors.Wrap(err, errApply)
		}
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(p.UnstructuredContent(), shadow); err != nil {
		return zero, errors.Wrap(err, errUnpave)
	}
	return shadow, nil
}

// WrapConnector decorates a TypedExternalConnector so every client it produces
// honours spec.driftDetection. This is the single wiring point per controller;
// there is no per-resource logic anywhere in this package.
func WrapConnector[T resource.Managed](c managed.TypedExternalConnector[T]) managed.TypedExternalConnector[T] {
	return managed.TypedExternalConnectorFn[T](func(ctx context.Context, mg T) (managed.TypedExternalClient[T], error) {
		inner, err := c.Connect(ctx, mg)
		if err != nil {
			return nil, err
		}
		return WrapClient(inner), nil
	})
}

// WrapClient decorates a single TypedExternalClient. The returned client holds
// per-reconcile observation state and must not outlive one reconcile — which is
// what WrapConnector guarantees, since crossplane-runtime connects once per
// reconcile and disconnects at its end.
func WrapClient[T resource.Managed](inner managed.TypedExternalClient[T]) managed.TypedExternalClient[T] {
	return &external[T]{inner: inner}
}

type external[T resource.Managed] struct {
	inner managed.TypedExternalClient[T]

	// mu guards the observation captured by Observe and consumed by Update.
	// The reconciler is single-threaded per resource and this client is
	// per-reconcile, so the lock is uncontended; it is here so that a
	// mis-wiring which shares a client cannot corrupt the snapshot silently.
	mu  sync.Mutex
	obs *observation
}

// Observe delegates to the wrapped client, captures the observed values of any
// ignored paths, then reinterprets the up-to-dateness verdict.
func (e *external[T]) Observe(ctx context.Context, mg T) (managed.ExternalObservation, error) { //nolint:gocyclo // the mode switch (enabled/warn/disabled) and the ignore-path re-observe path are sequential, not nested; splitting them would scatter one decision across multiple functions.
	cfg, err := ReadConfig(mg)
	if err != nil {
		// A malformed or ineligible configuration must not be read as
		// "ignore nothing": that would write spec values over a field the
		// user believed was handed off.
		return managed.ExternalObservation{}, err
	}

	obs, err := e.inner.Observe(ctx, mg)
	if err != nil || !obs.ResourceExists {
		return obs, err
	}

	// status.atProvider now holds THIS reconcile's response. Capture the
	// ignored paths before anything else can overwrite it.
	captured, err := snapshot(mg, cfg.IgnorePaths)
	if err != nil {
		return obs, err
	}
	e.mu.Lock()
	e.obs = captured
	e.mu.Unlock()

	switch cfg.Mode { //nolint:exhaustive // ModeEnabled and any unrecognised value fall through unchanged below.
	case ModeDisabled:
		obs.ResourceUpToDate = true
		return obs, nil

	case ModeWarn:
		if !obs.ResourceUpToDate {
			setCondition(mg, ReasonDrifted, "drift detected; not corrected (driftDetection.mode: warn)")
			obs.ResourceUpToDate = true
		} else {
			setCondition(mg, ReasonInSync, "no drift detected")
		}
		return obs, nil
	}

	// ModeEnabled, and any unrecognised value — which defaults to today's
	// behaviour rather than to silently not reconciling.
	if len(cfg.IgnorePaths) == 0 {
		return obs, nil
	}
	if len(captured.unobserved) > 0 {
		setCondition(mg, ReasonUnobserved,
			"external resource did not report ignored path(s): "+strings.Join(captured.unobserved, ", ")+
				"; the spec value stands for these")
	}
	if obs.ResourceUpToDate {
		if len(captured.unobserved) == 0 {
			setCondition(mg, ReasonInSync, "no drift detected")
		}
		return obs, nil
	}

	// Something differs. Re-ask with the ignored paths pinned to the values
	// their real owner has set. Costs one extra read, and only on a resource
	// that both configures ignore paths and is currently reporting drift — a
	// steady-state resource pays nothing.
	shadow, err := apply(mg, captured)
	if err != nil {
		return obs, err
	}
	shadowObs, err := e.inner.Observe(ctx, shadow)
	if err != nil {
		return obs, err
	}

	if shadowObs.ResourceUpToDate {
		setCondition(mg, ReasonIgnored, "drift confined to ignored paths: "+strings.Join(cfg.IgnorePaths, ", "))
	} else {
		setCondition(mg, ReasonDrifted, "drift outside ignored paths; correcting")
	}
	obs.ResourceUpToDate = shadowObs.ResourceUpToDate

	return obs, nil
}

// Create passes through unmodified: spec values for ignored paths are the seed,
// and seeding is why they were set.
func (e *external[T]) Create(ctx context.Context, mg T) (managed.ExternalCreation, error) {
	return e.inner.Create(ctx, mg)
}

// Update substitutes the observed values captured during this reconcile's
// Observe before the wrapped client builds its payload, so a whole-object
// write carries the external owner's value rather than reverting it.
func (e *external[T]) Update(ctx context.Context, mg T) (managed.ExternalUpdate, error) {
	cfg, err := ReadConfig(mg)
	if err != nil {
		return managed.ExternalUpdate{}, err
	}
	if cfg.Mode != ModeEnabled || len(cfg.IgnorePaths) == 0 {
		return e.inner.Update(ctx, mg)
	}

	e.mu.Lock()
	captured := e.obs
	e.mu.Unlock()

	// Deliberately NOT a fallback to reading status.atProvider here. If no
	// observation was captured in this reconcile, any value we could read is
	// from an earlier one, and the spec value would revert the owner outright.
	if captured == nil {
		return managed.ExternalUpdate{}, errors.New(errNoObserve)
	}

	shadow, err := apply(mg, captured)
	if err != nil {
		// Fail closed for the same reason.
		return managed.ExternalUpdate{}, err
	}

	u, err := e.inner.Update(ctx, shadow)

	// Annotations are the one mutation Update may make that the runtime can
	// act on; carry them back from the shadow.
	mg.SetAnnotations(shadow.GetAnnotations())

	return u, err
}

// Delete passes through: field ownership has no bearing on deletion.
func (e *external[T]) Delete(ctx context.Context, mg T) (managed.ExternalDelete, error) {
	return e.inner.Delete(ctx, mg)
}

// Disconnect drops the captured observation and passes through.
func (e *external[T]) Disconnect(ctx context.Context) error {
	e.mu.Lock()
	e.obs = nil
	e.mu.Unlock()
	return e.inner.Disconnect(ctx)
}

func setCondition(mg resource.Managed, reason xpv2.ConditionReason, msg string) {
	status := corev1.ConditionTrue
	if reason == ReasonInSync {
		status = corev1.ConditionFalse
	}
	mg.SetConditions(xpv2.Condition{
		Type:               TypeDriftDetected,
		Status:             status,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            msg,
	})
}

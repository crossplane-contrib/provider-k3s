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

package driftdetection

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/cluster/v1alpha1"
	dd "github.com/crossplane-contrib/provider-k3s/apis/common/driftdetection"
)

// testHost is the fixed SSH target used across this suite. It never varies
// within a test, since host is the identity-participating field these
// tests are not exercising.
const testHost = "fixed.example.com"

// fakeRemote stands in for a real controller talking to the k3s host over
// SSH.
//
// Its update is a whole-object replace: k3s.InstallCommand rebuilds the
// FULL install command line from InstallParams every time it runs, exactly
// like a whole-object PUT. A payload built from spec alone reverts whatever
// the external owner set, so modelling the worst case here keeps the
// write-path assertions meaningful.
type fakeRemote struct {
	state    v1alpha1.ClusterParameters
	observes int
	updates  int
}

func (r *fakeRemote) client() managed.TypedExternalClient[*v1alpha1.Cluster] {
	return managed.TypedExternalClientFns[*v1alpha1.Cluster]{
		ObserveFn: func(_ context.Context, mg *v1alpha1.Cluster) (managed.ExternalObservation, error) {
			r.observes++
			mg.Status.AtProvider = v1alpha1.ClusterObservation{
				Ready:      true,
				K3sVersion: r.state.K3sVersion,
			}
			// Mirrors a real controller: only the field under test
			// (k3sVersion, the one this suite ignores) is compared.
			return managed.ExternalObservation{
				ResourceExists:   true,
				ResourceUpToDate: r.state.K3sVersion == mg.Spec.ForProvider.K3sVersion,
			}, nil
		},
		UpdateFn: func(_ context.Context, mg *v1alpha1.Cluster) (managed.ExternalUpdate, error) {
			r.updates++
			r.state = mg.Spec.ForProvider
			return managed.ExternalUpdate{}, nil
		},
		CreateFn: func(_ context.Context, mg *v1alpha1.Cluster) (managed.ExternalCreation, error) {
			r.state = mg.Spec.ForProvider
			return managed.ExternalCreation{}, nil
		},
		DeleteFn: func(_ context.Context, _ *v1alpha1.Cluster) (managed.ExternalDelete, error) {
			return managed.ExternalDelete{}, nil
		},
		DisconnectFn: func(_ context.Context) error { return nil },
	}
}

func mr(cfg *dd.DriftDetection, params v1alpha1.ClusterParameters) *v1alpha1.Cluster {
	cr := &v1alpha1.Cluster{}
	cr.Spec.DriftDetection = cfg
	cr.Spec.ForProvider = params
	return cr
}

func config(mode dd.Mode, paths ...string) *dd.DriftDetection {
	c := &dd.DriftDetection{Mode: mode}
	if len(paths) > 0 {
		c.Ignore = []dd.IgnoreRule{{Paths: paths}}
	}
	return c
}

// params returns ClusterParameters with a fixed host and extraArgs, and the
// given k3sVersion -- the field most tests vary, mirroring the reference
// implementation's params(label).
func params(k3sVersion string) v1alpha1.ClusterParameters {
	return v1alpha1.ClusterParameters{
		Host:       testHost,
		K3sVersion: k3sVersion,
		ExtraArgs:  "--node-label foo=bar",
	}
}

func driftReason(cr *v1alpha1.Cluster) (corev1.ConditionStatus, string, bool) {
	for _, c := range cr.Status.Conditions {
		if c.Type == TypeDriftDetected {
			return c.Status, string(c.Reason), true
		}
	}
	return "", "", false
}

const ignoreK3sVersion = "forProvider.k3sVersion"

// ─── behaviour ───────────────────────────────────────────────────────────────

// With no driftDetection block the wrapper is transparent. Adding the field to
// an API must not change how existing resources reconcile.
func TestNoConfigIsTransparent(t *testing.T) {
	r := &fakeRemote{state: params("v1.28.2+k3s1")}
	cr := mr(nil, params("v1.29.0+k3s1"))

	obs, err := WrapClient(r.client()).Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.ResourceUpToDate {
		t.Error("want drift reported when nothing is ignored")
	}
	if r.observes != 1 {
		t.Errorf("want 1 inner Observe, got %d", r.observes)
	}
}

func TestIgnoredPathSuppressesDrift(t *testing.T) {
	r := &fakeRemote{state: params("v1.28.2+k3s1")}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))

	obs, err := WrapClient(r.client()).Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceUpToDate {
		t.Error("want up to date: the only difference is on an ignored path")
	}
	if cr.Spec.ForProvider.K3sVersion != "v1.0.0-seed" {
		t.Errorf("user spec was mutated: got %q", cr.Spec.ForProvider.K3sVersion)
	}
	if got := cr.Status.AtProvider.K3sVersion; got != "v1.28.2+k3s1" {
		t.Errorf("atProvider must report the truth, got %q", got)
	}
	if st, reason, ok := driftReason(cr); !ok || st != corev1.ConditionTrue || reason != string(ReasonIgnored) {
		t.Errorf("want DriftDetected=True/DriftIgnored, got %v/%v (present=%v)", st, reason, ok)
	}
}

// The write path: correcting drift must not revert an ignored field.
func TestUpdateCarriesObservedValueForIgnoredPath(t *testing.T) {
	r := &fakeRemote{state: v1alpha1.ClusterParameters{
		Host:       testHost,
		K3sVersion: "v1.28.2+k3s1", // owned externally
		ExtraArgs:  "--node-label previous=value",
	}}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed")) // extraArgs differs: genuine drift on a mutable field

	e := WrapClient(r.client())
	ctx := context.Background()
	if _, err := e.Observe(ctx, cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if _, err := e.Update(ctx, cr); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if r.state.ExtraArgs != "--node-label foo=bar" {
		t.Errorf("owned field not reconciled: want '--node-label foo=bar', got %q", r.state.ExtraArgs)
	}
	if r.state.K3sVersion != "v1.28.2+k3s1" {
		t.Errorf("ignored field reverted by the update: want v1.28.2+k3s1, got %q "+
			"(the failure a compare-only ignore produces under a whole-object write)", r.state.K3sVersion)
	}
	if cr.Spec.ForProvider.K3sVersion != "v1.0.0-seed" {
		t.Errorf("user spec was mutated: got %q", cr.Spec.ForProvider.K3sVersion)
	}
}

func TestCreateSendsSeedValue(t *testing.T) {
	r := &fakeRemote{}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))

	if _, err := WrapClient(r.client()).Create(context.Background(), cr); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if r.state.K3sVersion != "v1.0.0-seed" {
		t.Errorf("want seed sent on create, got %q", r.state.K3sVersion)
	}
}

func TestWarnReportsWithoutCorrecting(t *testing.T) {
	r := &fakeRemote{state: params("v1.28.2+k3s1")}
	cr := mr(config(dd.ModeWarn), params("v1.0.0-seed"))

	obs, err := WrapClient(r.client()).Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceUpToDate {
		t.Error("warn must not trigger an update")
	}
	if st, reason, ok := driftReason(cr); !ok || st != corev1.ConditionTrue || reason != string(ReasonDrifted) {
		t.Errorf("want DriftDetected=True/Drifted, got %v/%v (present=%v)", st, reason, ok)
	}
	if r.observes != 1 {
		t.Errorf("warn must not cost a second read, got %d", r.observes)
	}
}

func TestDisabledSuppressesEverything(t *testing.T) {
	r := &fakeRemote{state: params("v1.28.2+k3s1")}
	cr := mr(config(dd.ModeDisabled), params("v1.0.0-seed"))

	obs, err := WrapClient(r.client()).Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceUpToDate {
		t.Error("disabled must never report drift")
	}
	if _, _, ok := driftReason(cr); ok {
		t.Error("disabled must not set a drift condition")
	}
}

func TestNoSecondReadWhenInSync(t *testing.T) {
	r := &fakeRemote{state: params("v1.28.2+k3s1")}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.28.2+k3s1"))

	if _, err := WrapClient(r.client()).Observe(context.Background(), cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if r.observes != 1 {
		t.Errorf("want 1 inner Observe when in sync, got %d", r.observes)
	}
}

// ─── freshness and race safety ───────────────────────────────────────────────

// Update must consume the observation captured during Observe, never re-read
// status.atProvider. Re-reading would pick up whatever the object carries at
// that moment, which for any path that does not repopulate it is a previous
// reconcile's value.
func TestUpdateUsesSnapshotNotStatusReread(t *testing.T) {
	r := &fakeRemote{state: v1alpha1.ClusterParameters{
		Host:       testHost,
		K3sVersion: "observed-now",
		ExtraArgs:  "--node-label previous=value",
	}}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))

	e := WrapClient(r.client())
	ctx := context.Background()
	if _, err := e.Observe(ctx, cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// Simulate anything that rewrites status between Observe and Update:
	// a stale cache decode, a status patch, a second controller.
	cr.Status.AtProvider.K3sVersion = "STALE-OR-TAMPERED"

	if _, err := e.Update(ctx, cr); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if r.state.K3sVersion != "observed-now" {
		t.Errorf("Update read status.atProvider instead of the captured snapshot: got %q", r.state.K3sVersion)
	}
}

// Update without a preceding Observe must fail closed. Falling back to the spec
// value would revert the external owner outright.
func TestUpdateFailsClosedWithoutObserve(t *testing.T) {
	r := &fakeRemote{state: params("v1.28.2+k3s1")}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))

	if _, err := WrapClient(r.client()).Update(context.Background(), cr); err == nil {
		t.Error("want Update to fail closed when no observation was captured")
	}
	if r.updates != 0 {
		t.Errorf("want no write issued, got %d", r.updates)
	}
	if r.state.K3sVersion != "v1.28.2+k3s1" {
		t.Errorf("external owner's value was overwritten: got %q", r.state.K3sVersion)
	}
}

// Disconnect ends the per-reconcile lifetime; a client reused afterwards must
// not apply a stale snapshot.
func TestDisconnectClearsSnapshot(t *testing.T) {
	r := &fakeRemote{state: params("observed")}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))

	e := WrapClient(r.client())
	ctx := context.Background()
	if _, err := e.Observe(ctx, cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if err := e.Disconnect(ctx); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if _, err := e.Update(ctx, cr); err == nil {
		t.Error("want Update to fail closed after Disconnect dropped the snapshot")
	}
}

// The reconciler's managed resource must never be mutated by substitution: it
// may be shared with a cache, and a mutated spec would be persisted by the
// ResourceLateInitialized path.
func TestInputResourceIsNeverMutated(t *testing.T) {
	r := &fakeRemote{state: v1alpha1.ClusterParameters{
		Host:       testHost,
		K3sVersion: "upstream",
		ExtraArgs:  "--node-label previous=value",
	}}
	cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))
	before := cr.Spec.DeepCopy()

	e := WrapClient(r.client())
	ctx := context.Background()
	if _, err := e.Observe(ctx, cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if _, err := e.Update(ctx, cr); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if diff := cmp.Diff(before, cr.Spec.DeepCopy()); diff != "" {
		t.Errorf("spec was mutated (-want +got):\n%s", diff)
	}
}

// The production shape: one client per reconcile, many reconciles in flight for
// different resources. Run under -race.
func TestConcurrentReconcilesAreIndependent(t *testing.T) {
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := "owner-" + string(rune('a'+i%26))
			r := &fakeRemote{state: v1alpha1.ClusterParameters{
				Host:       testHost,
				K3sVersion: owner,
				ExtraArgs:  "--node-label previous=value",
			}}
			cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))

			e := WrapClient(r.client()) // one wrapper per reconcile, as WrapConnector does
			ctx := context.Background()
			if _, err := e.Observe(ctx, cr); err != nil {
				errs <- err
				return
			}
			if _, err := e.Update(ctx, cr); err != nil {
				errs <- err
				return
			}
			if r.state.K3sVersion != owner {
				t.Errorf("snapshot crossed between reconciles: want %q, got %q", owner, r.state.K3sVersion)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent reconcile: %v", err)
	}
}

// A mis-wiring that shares one client across reconciles must still be free of
// data races, even though the snapshot semantics would then be wrong. Run under
// -race.
func TestSharedClientIsRaceFree(t *testing.T) {
	r := &fakeRemote{state: params("upstream")}
	var mu sync.Mutex
	inner := managed.TypedExternalClientFns[*v1alpha1.Cluster]{
		ObserveFn: func(ctx context.Context, mg *v1alpha1.Cluster) (managed.ExternalObservation, error) {
			mu.Lock()
			defer mu.Unlock()
			return r.client().Observe(ctx, mg)
		},
		UpdateFn: func(ctx context.Context, mg *v1alpha1.Cluster) (managed.ExternalUpdate, error) {
			mu.Lock()
			defer mu.Unlock()
			return r.client().Update(ctx, mg)
		},
		DisconnectFn: func(_ context.Context) error { return nil },
	}
	e := WrapClient[*v1alpha1.Cluster](inner)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cr := mr(config(dd.ModeEnabled, ignoreK3sVersion), params("v1.0.0-seed"))
			ctx := context.Background()
			_, _ = e.Observe(ctx, cr)
			_, _ = e.Update(ctx, cr)
		}()
	}
	wg.Wait()
}

// ─── configuration ───────────────────────────────────────────────────────────

func TestReadConfigIsTypeAgnostic(t *testing.T) {
	cfg, err := ReadConfig(&v1alpha1.Cluster{})
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if cfg.Mode != ModeEnabled || len(cfg.IgnorePaths) != 0 || cfg.Active() {
		t.Errorf("want inert default config, got %+v", cfg)
	}
}

// A driftDetection block with a mode set but zero ignore paths must not be
// mistaken for "no ignore paths configured, therefore reject" -- the two are
// different, and only the latter is an error.
func TestReadConfigDriftDetectionWithoutIgnorePaths(t *testing.T) {
	for _, mode := range []dd.Mode{dd.ModeEnabled, dd.ModeWarn, dd.ModeDisabled} {
		cr := mr(&dd.DriftDetection{Mode: mode}, params("v1.0.0-seed"))
		cfg, err := ReadConfig(cr)
		if err != nil {
			t.Fatalf("ReadConfig(mode=%v): %v", mode, err)
		}
		if cfg.Mode != Mode(mode) || len(cfg.IgnorePaths) != 0 {
			t.Errorf("ReadConfig(mode=%v) = %+v, want mode=%v with no ignore paths", mode, cfg, mode)
		}
	}
}

// ─── eligibility ───────────────────────────────────────────────────────────

// forProvider.host is the field both Cluster and Node's external identity is
// derived from -- this provider carries no external-name, so host (the SSH
// target) is the sole identity. It must be refused regardless of whether the
// field exists on the resource asking, because the denylist matches by name,
// not by resource.
func TestEligibilityRejectsIdentityFields(t *testing.T) {
	cr := mr(config(dd.ModeEnabled, "forProvider.host"), params("v1.0.0-seed"))
	if _, err := ReadConfig(cr); err == nil {
		t.Error("ReadConfig(forProvider.host): want rejection of an identity-participating field")
	}
}

// A path that survives the identity denylist but does not exist on this
// resource's own configurable fields at all must still be rejected -- a
// silent no-op is indistinguishable, from the operator's side, from a path
// that is doing exactly what was asked.
func TestEligibilityRejectsFieldAbsentFromResource(t *testing.T) {
	cr := mr(config(dd.ModeEnabled, "forProvider.bogus"), params("v1.0.0-seed"))
	if _, err := ReadConfig(cr); err == nil {
		t.Error("ReadConfig: want rejection of a field absent from the resource")
	}
}

// A path that exists on forProvider but has no counterpart in
// status.atProvider is write-only -- there is nothing to substitute -- and
// must be rejected. extraArgs is real on this provider today: the
// observation mirror is thin (Cluster reports only ready and k3sVersion), so
// most forProvider fields fall into this class until the mirror is widened.
func TestEligibilityRejectsWriteOnlyFieldOnRealResource(t *testing.T) {
	cr := mr(config(dd.ModeEnabled, "forProvider.extraArgs"), params("v1.0.0-seed"))
	if _, err := ReadConfig(cr); err == nil {
		t.Error("ReadConfig(forProvider.extraArgs): want rejection of a write-only field")
	}
}

// A path present in forProvider AND mirrored in status.atProvider is
// eligible. k3sVersion is the one field this provider currently mirrors.
func TestEligibilityAcceptsFieldWithObservationCounterpart(t *testing.T) {
	cr := mr(config(dd.ModeEnabled, "forProvider.k3sVersion"), params("v1.0.0-seed"))
	cfg, err := ReadConfig(cr)
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if len(cfg.IgnorePaths) != 1 {
		t.Errorf("want the eligible path accepted, got %v", cfg.IgnorePaths)
	}
}

// One ineligible path in a list fails the entire configuration -- there is no
// partial application. Proven at the consumer (Observe), not just at
// ReadConfig: the eligible k3sVersion path in the same list must never be
// applied either, because Observe bails out before making any inner call.
func TestEligibilityRejectionIsTotal(t *testing.T) {
	r := &fakeRemote{state: params("owned-externally")}
	cr := mr(config(dd.ModeEnabled, "forProvider.k3sVersion", "forProvider.host"), params("v1.0.0-seed"))

	obs, err := WrapClient(r.client()).Observe(context.Background(), cr)
	if err == nil {
		t.Fatalf("Observe: want the whole config rejected, got %+v", obs)
	}
	if r.observes != 0 {
		t.Errorf("want no inner Observe when the config is rejected outright (proves no partial "+
			"application of the eligible path), got %d", r.observes)
	}
}

// checkEligibleType is the mechanical core of the eligibility gate: whether a
// path has a same-named counterpart in the resource's own Observation type.
// This is tested against a synthetic type in addition to the real-resource
// case above, to isolate the mechanism from any one resource's current
// mirror shape.
type fakeParams struct {
	Mirrored  string `json:"mirrored"`
	WriteOnly string `json:"writeOnly"`
}

type fakeObservation struct {
	Mirrored string `json:"mirrored"`
}

type fakeSpec struct {
	ForProvider fakeParams `json:"forProvider"`
}

type fakeStatus struct {
	AtProvider fakeObservation `json:"atProvider"`
}

type fakeMR struct {
	Spec   fakeSpec   `json:"spec"`
	Status fakeStatus `json:"status,omitempty"`
}

func TestEligibilityRejectsWriteOnlyField(t *testing.T) {
	ft := reflect.TypeOf(&fakeMR{})
	if err := checkEligibleType(ft, "forProvider.writeOnly"); err == nil {
		t.Error("want rejection of a field with no counterpart in status.atProvider")
	}
	if err := checkEligibleType(ft, "forProvider.mirrored"); err != nil {
		t.Errorf("want a mirrored field accepted, got %v", err)
	}
}

// The mutation this guards against: turning checkEligible's rejection into a
// skip-and-continue. Simulated here directly, since the production code path
// has no such branch to flip via a config flag -- this is the behaviour the
// real function must NOT exhibit.
func TestEligibilityRejectionIsNeverASkip(t *testing.T) {
	paths := []string{"forProvider.k3sVersion", "forProvider.host", "forProvider.extraArgs"}
	sawRejection := false
	for _, path := range paths {
		if err := checkEligible(&v1alpha1.Cluster{}, path); err != nil {
			sawRejection = true
			continue // this is the mutation under test: skip-and-continue
		}
	}
	if !sawRejection {
		t.Fatal("test setup broken: expected at least one path to be rejected")
	}
	// The correct behaviour (ReadConfig) must reject the whole list, not
	// silently accept the eligible remainder the way this loop does.
	cr := mr(config(dd.ModeEnabled, paths...), params("v1.0.0-seed"))
	if _, err := ReadConfig(cr); err == nil {
		t.Error("ReadConfig must reject the whole config; a skip-and-continue loop like the one " +
			"above would have accepted the eligible paths and silently dropped the ineligible one")
	}
}

func TestPathGrammar(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "forProvider.k3sVersion", want: "forProvider.k3sVersion"},
		{in: "/forProvider/k3sVersion", want: "forProvider.k3sVersion"},
		{in: "forProvider.a.b.c", want: "forProvider.a.b.c"},
		{in: "forProvider.rules[0].ttl", wantErr: true},
		{in: "forProvider.rules[*].ttl", wantErr: true},
		{in: "status.atProvider.k3sVersion", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizePath(tc.in)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("normalizePath(%q): want error, got %q", tc.in, got)
		case !tc.wantErr && err != nil:
			t.Errorf("normalizePath(%q): %v", tc.in, err)
		case !tc.wantErr && got != tc.want:
			t.Errorf("normalizePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMalformedPathFailsClosed(t *testing.T) {
	r := &fakeRemote{state: params("upstream")}
	cr := mr(config(dd.ModeEnabled, "forProvider.rules[*].ttl"), params("v1.0.0-seed"))

	e := WrapClient(r.client())
	ctx := context.Background()
	if _, err := e.Observe(ctx, cr); err == nil {
		t.Error("want Observe to fail on an unusable ignore path")
	}
	if _, err := e.Update(ctx, cr); err == nil {
		t.Error("want Update to fail closed on an unusable ignore path")
	}
	if r.updates != 0 {
		t.Errorf("want no write issued, got %d", r.updates)
	}
}

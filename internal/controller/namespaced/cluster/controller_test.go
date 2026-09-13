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

package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/namespaced/v1alpha1"
)

// testIsActiveCmd is the exact systemctl probe Observe sends for the k3s
// server unit, shared across every fixture that keys a canned SSH response
// on it. The reply is two lines -- LoadState then ActiveState -- matching
// what ParseServiceProbe expects from k3s.ServiceProbeCommand.
const testIsActiveCmd = "systemctl show -p LoadState -p ActiveState --value k3s 2>/dev/null"

// newTestKubeClient builds a fake kube client seeded with objs, for
// exercising persistLastAppliedClusterConfig's conflict-safe
// read-modify-write against a real (fake) API server rather than an
// in-memory struct.
func newTestKubeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		panic(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func clusterParams(k3sVersion, extraArgs string) v1alpha1.ClusterParameters {
	return v1alpha1.ClusterParameters{
		Host:              "10.0.0.1",
		Port:              22,
		K3sVersion:        k3sVersion,
		K3sChannel:        "stable",
		ClusterInit:       true,
		TLSSAN:            "cluster.example.com",
		DisableTraefik:    true,
		DisableServiceLB:  false,
		ExtraArgs:         extraArgs,
		DatastoreEndpoint: "mysql://user:pass@tcp(db:3306)/k3s",
	}
}

func newClusterCR(name, k3sVersion, extraArgs string) *v1alpha1.Cluster {
	cr := &v1alpha1.Cluster{}
	cr.SetName(name)
	cr.Spec.ForProvider = clusterParams(k3sVersion, extraArgs)
	return cr
}

// TestIsUpToDateReportsInSync mirrors the required T-series shape: a
// resource whose mutable fields match what this controller last applied is
// up to date.
func TestIsUpToDateReportsInSync(t *testing.T) {
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedClusterConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedClusterConfig: %v", err)
	}

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		t.Fatalf("clusterIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date: mutable fields match the last-applied configuration")
	}
}

// TestIsUpToDateReportsDrift proves the comparison is a real one and not a
// hardcoded true: changing a mutable field after the annotation was written
// must be detected.
func TestIsUpToDateReportsDrift(t *testing.T) {
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedClusterConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedClusterConfig: %v", err)
	}

	// The user changes a mutable field after the last apply.
	cr.Spec.ForProvider.K3sVersion = "v1.29.0+k3s1"

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		t.Fatalf("clusterIsUpToDate: %v", err)
	}
	if upToDate {
		t.Error("want drift reported: k3sVersion changed since the last apply")
	}
}

// TestIsUpToDateWithNoAnnotationIsNotUpToDate covers the adoption path: a
// resource with no recorded last-applied configuration (freshly adopted, or
// created before this annotation existed) must not be reported as up to
// date -- there is nothing to compare against, so the safe answer is to
// converge once and seed the annotation.
func TestIsUpToDateWithNoAnnotationIsNotUpToDate(t *testing.T) {
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		t.Fatalf("clusterIsUpToDate: %v", err)
	}
	if upToDate {
		t.Error("want not up to date when no last-applied configuration is recorded")
	}
}

// TestIsUpToDateIgnoresImmutableField proves host -- immutable and CEL
// enforced -- plays no part in the comparison. If it did, every resource
// would report drift forever, since host is never captured in the
// last-applied annotation at all.
func TestIsUpToDateIgnoresImmutableField(t *testing.T) {
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedClusterConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedClusterConfig: %v", err)
	}

	// CEL would reject this in a real cluster; simulated here to prove the
	// comparison itself does not depend on host.
	cr.Spec.ForProvider.Host = "10.0.0.99"
	cr.Spec.ForProvider.ClusterInit = false
	cr.Spec.ForProvider.DatastoreEndpoint = "mysql://someone-else/k3s"

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		t.Fatalf("clusterIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date: only immutable fields differ, and they are excluded from comparison")
	}
}

// TestPersistLastAppliedClusterConfigIsDurable proves the annotation is
// actually written through to the API server rather than only mutated on
// the in-memory cr. crossplane-runtime persists ONLY the status subresource
// after a successful external.Update() -- an in-memory-only annotation
// write would compare true on cr itself yet vanish on the very next
// reconcile's fresh Get, and isUpToDate would report drift forever.
func TestPersistLastAppliedClusterConfigIsDurable(t *testing.T) {
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	ctx := context.Background()

	if err := persistLastAppliedClusterConfig(ctx, kube, cr); err != nil {
		t.Fatalf("persistLastAppliedClusterConfig: %v", err)
	}

	// Fetch a FRESH copy from the fake API server -- proves the write went
	// through the client, not just the in-memory cr passed in.
	fresh := &v1alpha1.Cluster{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cr), fresh); err != nil {
		t.Fatalf("fetch fresh CR: %v", err)
	}

	upToDate, err := clusterIsUpToDate(fresh)
	if err != nil {
		t.Fatalf("clusterIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date on a freshly-fetched copy: the annotation was not durably persisted")
	}
}

// TestInstallParamsEchoesImmutableFields proves the PUT-upsert exception:
// the install script is a whole-object replace, so clusterInit and
// datastoreEndpoint -- both immutable and CEL-enforced -- are still present
// in the command parameters built for Update, sourced from the observed
// spec. Omitting them would let the install script fall back to its own
// defaults and silently reset a running server's HA configuration.
func TestInstallParamsEchoesImmutableFields(t *testing.T) {
	p := clusterParams("v1.28.2+k3s1", "--node-label foo=bar")

	got := installParamsFor(p)

	if !got.ClusterInit {
		t.Error("want clusterInit echoed in the install params, got false")
	}
	if got.DatastoreEndpoint != p.DatastoreEndpoint {
		t.Errorf("want datastoreEndpoint echoed, got %q want %q", got.DatastoreEndpoint, p.DatastoreEndpoint)
	}
}

// TestInstallParamsCarriesMutableFields proves the same call also carries
// every mutable field, so Update genuinely converges rather than only
// preserving what was already there.
func TestInstallParamsCarriesMutableFields(t *testing.T) {
	p := clusterParams("v1.29.0+k3s1", "--node-label env=prod")

	got := installParamsFor(p)

	if got.K3sVersion != "v1.29.0+k3s1" {
		t.Errorf("want k3sVersion carried through, got %q", got.K3sVersion)
	}
	if got.ExtraArgs != "--node-label env=prod" {
		t.Errorf("want extraArgs carried through, got %q", got.ExtraArgs)
	}
}

// TestNamespacedObserveMinimalResponse calls Observe against a minimal SSH
// response: k3s reports active, but the version, node-token and kubeconfig
// reads all come back empty -- the way they would on a host where sudo
// access is unavailable or those files don't exist yet. Observe must
// complete without panicking on any of the zero-valued reads and must
// return a sane ExternalObservation.
func TestNamespacedObserveMinimalResponse(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "loaded\nactive"},
	}, sshResponse{}) // fallback: empty stdout, exit 0 -- covers version/node-token/kubeconfig reads

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceExists {
		t.Error("want ResourceExists true: k3s reports active")
	}
	if obs.ResourceUpToDate {
		t.Error("want ResourceUpToDate false: no last-applied annotation recorded yet")
	}
	if cr.Status.AtProvider.K3sVersion != "" {
		t.Errorf("want empty K3sVersion from a minimal response, got %q", cr.Status.AtProvider.K3sVersion)
	}
	if !cr.Status.AtProvider.Ready {
		t.Error("want Ready true once k3s reports active")
	}
	if _, ok := obs.ConnectionDetails["kubeconfig"]; ok {
		t.Error("want no kubeconfig connection detail from an empty kubeconfig read")
	}
	if _, ok := obs.ConnectionDetails["node-token"]; ok {
		t.Error("want no node-token connection detail from an empty node-token read")
	}
	if _, ok := obs.ConnectionDetails["endpoint"]; !ok {
		t.Error("want the endpoint connection detail always present")
	}
}

// TestNamespacedObserveConvergingIsNotResourceNotFound proves the fix for
// the same class of near-timeout wedge already fixed for Node: Create/Update
// now return as soon as the install script's restart is queued rather than
// blocking until the server reports ready, so Observe must be able to see
// the service still starting ("activating") without treating that as
// absence -- misreporting it as ResourceExists: false would make
// crossplane-runtime call Create() again on top of an install already in
// flight, restarting a server that was mid-converge.
func TestNamespacedObserveConvergingIsNotResourceNotFound(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "loaded\nactivating"},
	}, sshResponse{})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceExists {
		t.Error("want ResourceExists true: a restart in flight is not absence")
	}
	if cr.Status.AtProvider.Ready {
		t.Error("want Ready false: the service has not reported active yet")
	}
	if cond := cr.GetCondition(xpv2.TypeReady); cond.Reason == xpv2.ReasonAvailable {
		t.Error("want no Available condition set while the service is still converging")
	}
	if cond := cr.GetCondition(xpv2.TypeReady); cond.Reason != xpv2.ReasonCreating {
		t.Errorf("want Creating condition explicitly set while converging, got reason %q", cond.Reason)
	}
}

// TestNamespacedObserveNotReadyIsNotUpToDate is the regression test for a
// cluster that hangs at Ready:False forever while Observe still reports it
// up to date -- the server unit is permanently stuck loaded-but-not-active.
// Readiness is part of desired state, not a side channel clusterIsUpToDate
// alone can certify, so ResourceUpToDate must be false here even though the
// last-applied configuration matches spec exactly: an unqualified
// clusterIsUpToDate() result would report ResourceUpToDate: true, making a
// permanently broken cluster silently indistinguishable from a healthy one
// -- crossplane-runtime would log "external resource is up to date" every
// poll and take no further action.
func TestNamespacedObserveNotReadyIsNotUpToDate(t *testing.T) {
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedClusterConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedClusterConfig: %v", err)
	}

	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "loaded\nactivating"},
	}, sshResponse{})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: kube}

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceExists {
		t.Error("want ResourceExists true: a not-ready unit must not be re-Created")
	}
	if obs.ResourceUpToDate {
		t.Error("want ResourceUpToDate false: the unit is not ready, even though its last-applied configuration matches spec exactly")
	}
}

// TestNamespacedDeleteServerError proves an uninstall failure on the host is
// surfaced as a wrapped error from Delete, not swallowed or panicked.
func TestNamespacedDeleteServerError(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{
		Stderr:   "k3s-uninstall.sh: command not found",
		ExitCode: 1,
	})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	_, err := e.Delete(context.Background(), cr)
	if err == nil {
		t.Fatal("want an error when the uninstall command fails on the host")
	}
	if !strings.Contains(err.Error(), "cannot uninstall k3s") {
		t.Errorf("want the error wrapped with its Delete-path context, got %q", err.Error())
	}
	if errors.Unwrap(err) == nil {
		t.Error("want the error wrapped via errors.Wrap, got an unwrapped error")
	}
}

// TestCreateReturnsPromptlyOnContextDeadline proves the fix for the
// dangling external-create-pending wedge: a Create() whose SSH install
// outlives the caller's context must return promptly with a wrapped
// deadline error, not block until the remote command finishes on its own
// schedule. Blocking past the deadline is what left the caller's context
// already expired by the time Create tried to persist its result,
// permanently wedging the resource.
func TestCreateReturnsPromptlyOnContextDeadline(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{
		Stdout: "installed",
		Delay:  5 * time.Second,
	})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := e.Create(ctx, cr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when the install command outlives the context deadline")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("want Create to return at the context deadline (~200ms), got %s -- Execute blocked for the remote command's full duration instead", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want the error to unwrap to context.DeadlineExceeded, got %q", err.Error())
	}
}

// TestNamespacedObserveNotFound (T3) proves systemd reporting no loaded
// artifact for the unit at all is treated as absence, not an error.
func TestNamespacedObserveNotFound(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "not-found\ninactive"},
	}, sshResponse{})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.ResourceExists {
		t.Error("want ResourceExists false: systemd has no loaded artifact for the unit")
	}
}

// TestNamespacedObserveInstalledButNotRunningIsNotResourceNotFound is the
// direct regression test for the 76-Create-loop bug: a unit that is
// installed (LoadState loaded) but currently "inactive" or "failed" --
// exactly what a real k3s host reported during an auto-restart backoff gap
// -- must be reported as existing and not-ready, never as absent. The old
// probe (`systemctl is-active k3s 2>/dev/null || echo inactive`) corrupted
// its own stdout into a two-line, unclassifiable reply on every one of
// these states, which fell through to "not found" and drove
// crossplane-runtime to re-run Create on top of an install already in
// flight.
func TestNamespacedObserveInstalledButNotRunningIsNotResourceNotFound(t *testing.T) {
	for _, activeState := range []string{"inactive", "failed"} {
		t.Run(activeState, func(t *testing.T) {
			host, port := startFakeSSHServer(t, map[string]sshResponse{
				testIsActiveCmd: {Stdout: "loaded\n" + activeState},
			}, sshResponse{})

			e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
			cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

			obs, err := e.Observe(context.Background(), cr)
			if err != nil {
				t.Fatalf("Observe: %v", err)
			}
			if !obs.ResourceExists {
				t.Errorf("want ResourceExists true: the unit is installed (LoadState loaded), only ActiveState is %q", activeState)
			}
			if cr.Status.AtProvider.Ready {
				t.Errorf("want Ready false: ActiveState is %q, not active", activeState)
			}
			if cond := cr.GetCondition(xpv2.TypeReady); cond.Reason != xpv2.ReasonCreating {
				t.Errorf("want Creating condition set while not ready, got reason %q", cond.Reason)
			}
		})
	}
}

// TestNamespacedObserveServerError (T4) proves an SSH transport failure is
// surfaced as a wrapped error rather than swallowed or panicked.
func TestNamespacedObserveServerError(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{})
	ssh := newTestSSHClient(t, host, port)
	if err := ssh.Close(); err != nil {
		t.Fatalf("close ssh client: %v", err)
	}

	e := &external{ssh: ssh, host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	_, err := e.Observe(context.Background(), cr)
	if err == nil {
		t.Fatal("want an error when the SSH transport is unavailable")
	}
	if !strings.Contains(err.Error(), "cannot check k3s status") {
		t.Errorf("want the error wrapped with its Observe-path context, got %q", err.Error())
	}
	if errors.Unwrap(err) == nil {
		t.Error("want the error wrapped via errors.Wrap, got an unwrapped error")
	}
}

// TestNamespacedObserveSuccessPopulatesFullMirror (T2, T7, T10) is the
// happy path: SSH returns a full response, the last-applied annotation
// already matches spec, and every atProvider field comes out populated,
// asserted field by field rather than by struct equality against a fixture
// the test itself built.
func TestNamespacedObserveSuccessPopulatesFullMirror(t *testing.T) {
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedClusterConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedClusterConfig: %v", err)
	}

	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd:                       {Stdout: "loaded\nactive"},
		"k3s --version 2>/dev/null | head -1": {Stdout: "k3s version v1.28.2+k3s1"},
		"sudo cat /var/lib/rancher/k3s/server/node-token 2>/dev/null": {Stdout: "the-node-token"},
		"sudo cat /etc/rancher/k3s/k3s.yaml 2>/dev/null":              {Stdout: "server: https://127.0.0.1:6443\n"},
	}, sshResponse{})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: kube}

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceExists {
		t.Error("want ResourceExists true")
	}
	if !obs.ResourceUpToDate {
		t.Error("want ResourceUpToDate true: the last-applied annotation matches spec")
	}

	ap := cr.Status.AtProvider
	if ap.ID != cr.Spec.ForProvider.Host {
		t.Errorf("want ID %q (the external-name, deterministic from host), got %q", cr.Spec.ForProvider.Host, ap.ID)
	}
	if !ap.Ready {
		t.Error("want Ready true")
	}
	if ap.Host != cr.Spec.ForProvider.Host {
		t.Errorf("want Host mirrored from spec, got %q want %q", ap.Host, cr.Spec.ForProvider.Host)
	}
	if ap.Port != cr.Spec.ForProvider.Port {
		t.Errorf("want Port mirrored from spec, got %d want %d", ap.Port, cr.Spec.ForProvider.Port)
	}
	if ap.K3sVersion != "k3s version v1.28.2+k3s1" {
		t.Errorf("want K3sVersion from the live probe, got %q", ap.K3sVersion)
	}
	if ap.K3sChannel != cr.Spec.ForProvider.K3sChannel {
		t.Errorf("want K3sChannel mirrored from the last-applied record, got %q want %q", ap.K3sChannel, cr.Spec.ForProvider.K3sChannel)
	}
	if ap.ClusterInit != cr.Spec.ForProvider.ClusterInit {
		t.Errorf("want ClusterInit mirrored from spec, got %v want %v", ap.ClusterInit, cr.Spec.ForProvider.ClusterInit)
	}
	if ap.TLSSAN != cr.Spec.ForProvider.TLSSAN {
		t.Errorf("want TLSSAN mirrored from the last-applied record, got %q want %q", ap.TLSSAN, cr.Spec.ForProvider.TLSSAN)
	}
	if ap.DisableTraefik != cr.Spec.ForProvider.DisableTraefik {
		t.Errorf("want DisableTraefik mirrored from the last-applied record, got %v want %v", ap.DisableTraefik, cr.Spec.ForProvider.DisableTraefik)
	}
	if ap.DisableServiceLB != cr.Spec.ForProvider.DisableServiceLB {
		t.Errorf("want DisableServiceLB mirrored from the last-applied record, got %v want %v", ap.DisableServiceLB, cr.Spec.ForProvider.DisableServiceLB)
	}
	if ap.ExtraArgs != cr.Spec.ForProvider.ExtraArgs {
		t.Errorf("want ExtraArgs mirrored from the last-applied record, got %q want %q", ap.ExtraArgs, cr.Spec.ForProvider.ExtraArgs)
	}
	if ap.DatastoreEndpoint != cr.Spec.ForProvider.DatastoreEndpoint {
		t.Errorf("want DatastoreEndpoint mirrored from spec, got %q want %q", ap.DatastoreEndpoint, cr.Spec.ForProvider.DatastoreEndpoint)
	}
	if obs.ConnectionDetails["kubeconfig"] == nil {
		t.Error("want kubeconfig connection detail present")
	}
	if obs.ConnectionDetails["node-token"] == nil {
		t.Error("want node-token connection detail present")
	}
}

// TestNamespacedObserveSetsExternalName proves identity is established from
// host as soon as Observe runs, even before the resource is confirmed to
// exist.
func TestNamespacedObserveSetsExternalName(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "not-found\ninactive"},
	}, sshResponse{})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	if _, err := e.Observe(context.Background(), cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got := meta.GetExternalName(cr); got != cr.Spec.ForProvider.Host {
		t.Errorf("want external-name set to host %q, got %q", cr.Spec.ForProvider.Host, got)
	}
}

// TestNamespacedCreateSuccess (Create POST success) proves a successful
// install both returns no error and durably persists the last-applied
// configuration.
func TestNamespacedCreateSuccess(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{Stdout: "installed"})

	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: kube}

	if _, err := e.Create(context.Background(), cr); err != nil {
		t.Fatalf("Create: %v", err)
	}

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		t.Fatalf("clusterIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date immediately after Create: the last-applied annotation was just seeded from this same spec")
	}
}

// TestNamespacedUpdateSuccess (Update PUT success) proves a successful
// reconfigure both returns no error and durably persists the new
// last-applied configuration.
func TestNamespacedUpdateSuccess(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{Stdout: "reconfigured"})

	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedClusterConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedClusterConfig: %v", err)
	}
	cr.Spec.ForProvider.ExtraArgs = "--node-label env=prod"

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: kube}
	if _, err := e.Update(context.Background(), cr); err != nil {
		t.Fatalf("Update: %v", err)
	}

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		t.Fatalf("clusterIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date immediately after Update: the last-applied annotation was just re-seeded from this same spec")
	}
}

// TestNamespacedDeleteSuccess (T8) proves a successful uninstall returns no
// error.
func TestNamespacedDeleteSuccess(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{Stdout: "uninstalled"})

	e := &external{ssh: newTestSSHClient(t, host, port), host: host, kube: newTestKubeClient()}
	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")

	if _, err := e.Delete(context.Background(), cr); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

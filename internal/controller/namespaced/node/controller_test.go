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

package node

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

// testNodeRoleAgent is the Role value shared by every test fixture in this
// file. Named as a constant (rather than repeating the literal) to keep the
// duplicate-string linter from flagging the fixtures as needing dedup.
const testNodeRoleAgent = "agent"

// testIsActiveCmd is the exact systemctl probe Observe sends for an agent
// role, shared across every fixture that keys a canned SSH response on it.
// The reply is two lines -- LoadState then ActiveState -- matching what
// ParseServiceProbe expects from k3s.ServiceProbeCommand.
const testIsActiveCmd = "systemctl show -p LoadState -p ActiveState --value k3s-agent 2>/dev/null"

// testServerHost and testNodeToken are the connector-resolved cluster
// identity shared by every fixture in this file that builds an *external
// directly (bypassing Connect) or asserts on Connect's resolved output (the
// values Connect would have produced from resolveClusterInfo).
const (
	testServerHost = "server.example.com"
	testNodeToken  = "the-node-token"
)

// newTestKubeClient builds a fake kube client seeded with objs, for
// exercising persistLastAppliedNodeConfig's conflict-safe read-modify-write
// against a real (fake) API server rather than an in-memory struct. The
// corev1 scheme is required alongside this provider's own types because
// Connect() reads a referenced Cluster's connection Secret directly.
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

func nodeParams(k3sVersion, extraArgs string) v1alpha1.NodeParameters {
	cluster := "my-cluster"
	return v1alpha1.NodeParameters{
		Host:       "10.0.0.2",
		Port:       22,
		Cluster:    &cluster,
		ClusterRef: &xpv2.NamespacedReference{Name: "my-cluster"},
		Role:       testNodeRoleAgent,
		K3sVersion: k3sVersion,
		K3sChannel: "stable",
		ExtraArgs:  extraArgs,
		TLSSAN:     "node.example.com",
	}
}

func newNodeCR(name, k3sVersion, extraArgs string) *v1alpha1.Node {
	cr := &v1alpha1.Node{}
	cr.SetName(name)
	cr.Spec.ForProvider = nodeParams(k3sVersion, extraArgs)
	return cr
}

// TestIsUpToDateReportsInSync mirrors the required T-series shape: a
// resource whose mutable fields match what this controller last applied is
// up to date.
func TestIsUpToDateReportsInSync(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedNodeConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedNodeConfig: %v", err)
	}

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		t.Fatalf("nodeIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date: mutable fields match the last-applied configuration")
	}
}

// TestIsUpToDateReportsDrift proves the comparison is a real one and not a
// hardcoded true: changing a mutable field after the annotation was written
// must be detected.
func TestIsUpToDateReportsDrift(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedNodeConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedNodeConfig: %v", err)
	}

	cr.Spec.ForProvider.ExtraArgs = "--node-label foo=baz"

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		t.Fatalf("nodeIsUpToDate: %v", err)
	}
	if upToDate {
		t.Error("want drift reported: extraArgs changed since the last apply")
	}
}

// TestIsUpToDateWithNoAnnotationIsNotUpToDate covers the adoption path: a
// resource with no recorded last-applied configuration must not be reported
// as up to date.
func TestIsUpToDateWithNoAnnotationIsNotUpToDate(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		t.Fatalf("nodeIsUpToDate: %v", err)
	}
	if upToDate {
		t.Error("want not up to date when no last-applied configuration is recorded")
	}
}

// TestIsUpToDateIgnoresImmutableField proves role -- immutable and CEL
// enforced -- plays no part in the comparison.
func TestIsUpToDateIgnoresImmutableField(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedNodeConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedNodeConfig: %v", err)
	}

	// CEL would reject these in a real cluster; simulated here to prove the
	// comparison itself does not depend on them.
	cr.Spec.ForProvider.Role = "server"
	cr.Spec.ForProvider.Host = "10.0.0.99"
	cr.Spec.ForProvider.ClusterRef = &xpv2.NamespacedReference{Name: "someone-elses-cluster"}

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		t.Fatalf("nodeIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date: only immutable fields differ, and they are excluded from comparison")
	}
}

// TestPersistLastAppliedNodeConfigIsDurable proves the annotation is
// actually written through to the API server rather than only mutated on
// the in-memory cr. crossplane-runtime persists ONLY the status subresource
// after a successful external.Update() -- an in-memory-only annotation
// write would compare true on cr itself yet vanish on the very next
// reconcile's fresh Get, and isUpToDate would report drift forever.
func TestPersistLastAppliedNodeConfigIsDurable(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	ctx := context.Background()

	if err := persistLastAppliedNodeConfig(ctx, kube, cr); err != nil {
		t.Fatalf("persistLastAppliedNodeConfig: %v", err)
	}

	// Fetch a FRESH copy from the fake API server -- proves the write went
	// through the client, not just the in-memory cr passed in.
	fresh := &v1alpha1.Node{}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cr), fresh); err != nil {
		t.Fatalf("fetch fresh CR: %v", err)
	}

	upToDate, err := nodeIsUpToDate(fresh)
	if err != nil {
		t.Fatalf("nodeIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date on a freshly-fetched copy: the annotation was not durably persisted")
	}
}

// TestJoinParamsEchoesImmutableRole proves the PUT-upsert exception: the
// join script is a whole-object replace, so role -- immutable and
// CEL-enforced -- is still present in the command parameters built for
// Update, sourced from the observed spec.
func TestJoinParamsEchoesImmutableRole(t *testing.T) {
	p := nodeParams("v1.28.2+k3s1", "--node-label foo=bar")
	p.Role = "server"

	got := joinParamsFor(p, "10.0.0.1", "token123")

	if got.Role != "server" {
		t.Errorf("want role echoed in the join params, got %q", got.Role)
	}
}

// TestJoinParamsCarriesMutableFieldsAndResolvedIdentity proves the same call
// also carries every mutable field plus the connector-resolved cluster
// identity, so Update genuinely converges.
func TestJoinParamsCarriesMutableFieldsAndResolvedIdentity(t *testing.T) {
	p := nodeParams("v1.29.0+k3s1", "--node-label env=prod")

	got := joinParamsFor(p, testServerHost, testNodeToken)

	if got.K3sVersion != "v1.29.0+k3s1" {
		t.Errorf("want k3sVersion carried through, got %q", got.K3sVersion)
	}
	if got.ExtraArgs != "--node-label env=prod" {
		t.Errorf("want extraArgs carried through, got %q", got.ExtraArgs)
	}
	if got.ServerHost != testServerHost {
		t.Errorf("want the connector-resolved server host, got %q", got.ServerHost)
	}
	if got.NodeToken != testNodeToken {
		t.Errorf("want the connector-resolved node token, got %q", got.NodeToken)
	}
}

// TestObserveMinimalResponse calls Observe against a minimal SSH response:
// the agent service reports active and nothing else is read from the host.
// Observe must complete without panicking and return a sane
// ExternalObservation.
func TestObserveMinimalResponse(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "loaded\nactive"},
	}, sshResponse{})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ResourceExists {
		t.Error("want ResourceExists true: the agent service reports active")
	}
	if obs.ResourceUpToDate {
		t.Error("want ResourceUpToDate false: no last-applied annotation recorded yet")
	}
	if !cr.Status.AtProvider.Ready {
		t.Error("want Ready true once the agent service reports active")
	}
	if cr.Status.AtProvider.Role != testNodeRoleAgent {
		t.Errorf("want Role %q recorded in atProvider, got %q", testNodeRoleAgent, cr.Status.AtProvider.Role)
	}
}

// TestDeleteServerError proves an uninstall failure on the host is surfaced
// as a wrapped error from Delete, not swallowed or panicked.
func TestDeleteServerError(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{
		Stderr:   "k3s-agent-uninstall.sh: command not found",
		ExitCode: 1,
	})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

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
// dangling external-create-pending wedge: a Create() whose SSH join
// outlives the caller's context must return promptly with a wrapped
// deadline error, not block until the remote command finishes on its own
// schedule. Blocking past the deadline is what left the caller's context
// already expired by the time Create tried to persist its result,
// permanently wedging the resource.
func TestCreateReturnsPromptlyOnContextDeadline(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{
		Stdout: "joined",
		Delay:  5 * time.Second,
	})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := e.Create(ctx, cr)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when the join command outlives the context deadline")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("want Create to return at the context deadline (~200ms), got %s -- Execute blocked for the remote command's full duration instead", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want the error to unwrap to context.DeadlineExceeded, got %q", err.Error())
	}
}

// TestObserveNotFound (T3) proves systemd reporting no loaded artifact for
// the unit at all is treated as absence, not an error.
func TestObserveNotFound(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "not-found\ninactive"},
	}, sshResponse{})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

	obs, err := e.Observe(context.Background(), cr)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if obs.ResourceExists {
		t.Error("want ResourceExists false: systemd has no loaded artifact for the agent unit")
	}
}

// TestObserveInstalledButNotRunningIsNotResourceNotFound is the direct
// regression test for the 76-Create-loop bug: a unit that is installed
// (LoadState loaded) but currently "inactive" or "failed" -- exactly what a
// real k3s host reported during an auto-restart backoff gap -- must be
// reported as existing and not-ready, never as absent. The old probe
// (`systemctl is-active k3s-agent 2>/dev/null || echo inactive`) corrupted
// its own stdout into a two-line, unclassifiable reply on every one of
// these states, which fell through to "not found" and drove
// crossplane-runtime to re-run Create on top of a join already in flight.
func TestObserveInstalledButNotRunningIsNotResourceNotFound(t *testing.T) {
	for _, activeState := range []string{"inactive", "failed"} {
		t.Run(activeState, func(t *testing.T) {
			host, port := startFakeSSHServer(t, map[string]sshResponse{
				testIsActiveCmd: {Stdout: "loaded\n" + activeState},
			}, sshResponse{})

			e := &external{
				ssh:        newTestSSHClient(t, host, port),
				serverHost: testServerHost,
				nodeToken:  testNodeToken,
				role:       testNodeRoleAgent,
				kube:       newTestKubeClient(),
			}
			cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

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

// TestObserveConvergingIsNotResourceNotFound proves the fix for the
// near-timeout Create()/Update() wedge: Create/Update now return as soon as
// the join's restart is queued rather than blocking until the agent/server
// reports ready, so Observe must be able to see the service still starting
// ("activating") without treating that as absence -- misreporting it as
// ResourceExists: false would make crossplane-runtime call Create() again
// on top of an install already in flight.
func TestObserveConvergingIsNotResourceNotFound(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "loaded\nactivating"},
	}, sshResponse{})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

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

// TestObserveNotReadyIsNotUpToDate is the regression test for a node that
// hangs at Ready:False forever while Observe still reports it up to date --
// the agent can never reach a server that itself never becomes ready, so the
// unit is permanently stuck loaded-but-not-active. Readiness is part of
// desired state, not a side channel nodeIsUpToDate alone can certify, so
// ResourceUpToDate must be false here even though the last-applied
// configuration matches spec exactly: an unqualified nodeIsUpToDate() result
// would report ResourceUpToDate: true, making a permanently broken node
// silently indistinguishable from a healthy one -- crossplane-runtime would
// log "external resource is up to date" every poll and take no further
// action.
func TestObserveNotReadyIsNotUpToDate(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedNodeConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedNodeConfig: %v", err)
	}

	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "loaded\nactivating"},
	}, sshResponse{})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       kube,
	}

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

// TestObserveServerError (T4) proves an SSH transport failure is surfaced
// as a wrapped error rather than swallowed or panicked.
func TestObserveServerError(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{})
	ssh := newTestSSHClient(t, host, port)
	if err := ssh.Close(); err != nil {
		t.Fatalf("close ssh client: %v", err)
	}

	e := &external{
		ssh:        ssh,
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

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

// TestObserveSuccessPopulatesFullMirror (T2, T7, T10) is the happy path:
// SSH returns a full response, the last-applied annotation already matches
// spec, and every atProvider field comes out populated, asserted field by
// field rather than by struct equality against a fixture the test itself
// built.
func TestObserveSuccessPopulatesFullMirror(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedNodeConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedNodeConfig: %v", err)
	}

	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd:                       {Stdout: "loaded\nactive"},
		"k3s --version 2>/dev/null | head -1": {Stdout: "k3s version v1.28.2+k3s1"},
	}, sshResponse{})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       kube,
	}

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
	if ap.Cluster != *cr.Spec.ForProvider.Cluster {
		t.Errorf("want Cluster mirrored from spec, got %q want %q", ap.Cluster, *cr.Spec.ForProvider.Cluster)
	}
	if ap.Host != cr.Spec.ForProvider.Host {
		t.Errorf("want Host mirrored from spec, got %q want %q", ap.Host, cr.Spec.ForProvider.Host)
	}
	if ap.Port != cr.Spec.ForProvider.Port {
		t.Errorf("want Port mirrored from spec, got %d want %d", ap.Port, cr.Spec.ForProvider.Port)
	}
	if ap.Role != testNodeRoleAgent {
		t.Errorf("want Role %q, got %q", testNodeRoleAgent, ap.Role)
	}
	if ap.K3sVersion != "k3s version v1.28.2+k3s1" {
		t.Errorf("want K3sVersion from the live probe, got %q", ap.K3sVersion)
	}
	if ap.K3sChannel != cr.Spec.ForProvider.K3sChannel {
		t.Errorf("want K3sChannel mirrored from the last-applied record, got %q want %q", ap.K3sChannel, cr.Spec.ForProvider.K3sChannel)
	}
	if ap.ExtraArgs != cr.Spec.ForProvider.ExtraArgs {
		t.Errorf("want ExtraArgs mirrored from the last-applied record, got %q want %q", ap.ExtraArgs, cr.Spec.ForProvider.ExtraArgs)
	}
	if ap.TLSSAN != cr.Spec.ForProvider.TLSSAN {
		t.Errorf("want TLSSAN mirrored from the last-applied record, got %q want %q", ap.TLSSAN, cr.Spec.ForProvider.TLSSAN)
	}
}

// TestObserveSetsExternalName proves identity is established from host as
// soon as Observe runs, even before the resource is confirmed to exist.
func TestObserveSetsExternalName(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		testIsActiveCmd: {Stdout: "not-found\ninactive"},
	}, sshResponse{})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

	if _, err := e.Observe(context.Background(), cr); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if got := meta.GetExternalName(cr); got != cr.Spec.ForProvider.Host {
		t.Errorf("want external-name set to host %q, got %q", cr.Spec.ForProvider.Host, got)
	}
}

// TestCreateSuccess (Create POST success) proves a successful join both
// returns no error and durably persists the last-applied configuration.
func TestCreateSuccess(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{Stdout: "joined"})

	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       kube,
	}

	if _, err := e.Create(context.Background(), cr); err != nil {
		t.Fatalf("Create: %v", err)
	}

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		t.Fatalf("nodeIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date immediately after Create: the last-applied annotation was just seeded from this same spec")
	}
}

// TestUpdateSuccess (Update PUT success) proves a successful reconfigure
// both returns no error and durably persists the new last-applied
// configuration.
func TestUpdateSuccess(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{Stdout: "reconfigured"})

	cr := newNodeCR("test-node", "v1.28.2+k3s1", "--node-label foo=bar")
	kube := newTestKubeClient(cr)
	if err := persistLastAppliedNodeConfig(context.Background(), kube, cr); err != nil {
		t.Fatalf("persistLastAppliedNodeConfig: %v", err)
	}
	cr.Spec.ForProvider.ExtraArgs = "--node-label env=prod"

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       kube,
	}
	if _, err := e.Update(context.Background(), cr); err != nil {
		t.Fatalf("Update: %v", err)
	}

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		t.Fatalf("nodeIsUpToDate: %v", err)
	}
	if !upToDate {
		t.Error("want up to date immediately after Update: the last-applied annotation was just re-seeded from this same spec")
	}
}

// TestCreateFailsWhenClusterRefUnresolved proves the fix's other half: a
// Connect()-time clusterRef resolution failure, deferred onto the external
// client rather than failing Connect() itself, still surfaces loudly from
// Create -- never silently joining a node to nothing with an empty
// nodeToken.
func TestCreateFailsWhenClusterRefUnresolved(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")
	e := &external{
		clusterErr: errors.Wrap(errors.New(`Cluster.k3s.m.crossplane.io "my-k3s-cluster" not found`), errGetCluster),
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}

	if _, err := e.Create(context.Background(), cr); err == nil {
		t.Fatal("want Create to fail when the clusterRef resolution deferred from Connect() failed")
	} else if !strings.Contains(err.Error(), errGetCluster) {
		t.Errorf("want the deferred cluster-resolution error surfaced from Create, got %q", err.Error())
	}
}

// TestUpdateFailsWhenClusterRefUnresolved is TestCreateFailsWhenClusterRefUnresolved's
// Update-path twin: re-running the join script with an empty nodeToken
// would silently reconfigure the node against nothing.
func TestUpdateFailsWhenClusterRefUnresolved(t *testing.T) {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")
	e := &external{
		clusterErr: errors.Wrap(errors.New(`Cluster.k3s.m.crossplane.io "my-k3s-cluster" not found`), errGetCluster),
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}

	if _, err := e.Update(context.Background(), cr); err == nil {
		t.Fatal("want Update to fail when the clusterRef resolution deferred from Connect() failed")
	} else if !strings.Contains(err.Error(), errGetCluster) {
		t.Errorf("want the deferred cluster-resolution error surfaced from Update, got %q", err.Error())
	}
}

// TestDeleteSuccess (T8) proves a successful uninstall returns no error.
func TestDeleteSuccess(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{Stdout: "uninstalled"})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: testServerHost,
		nodeToken:  testNodeToken,
		role:       testNodeRoleAgent,
		kube:       newTestKubeClient(),
	}
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")

	if _, err := e.Delete(context.Background(), cr); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

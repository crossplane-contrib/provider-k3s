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

	xpv1 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/namespaced/v1alpha1"
)

// newTestKubeClient builds a fake kube client seeded with objs, for
// exercising persistLastAppliedNodeConfig's conflict-safe read-modify-write
// against a real (fake) API server rather than an in-memory struct.
func newTestKubeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
		panic(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func nodeParams(k3sVersion, extraArgs string) v1alpha1.NodeParameters {
	return v1alpha1.NodeParameters{
		Host:       "10.0.0.2",
		Port:       22,
		ClusterRef: xpv1.Reference{Name: "my-cluster"},
		Role:       "agent",
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
	cr.Spec.ForProvider.ClusterRef = xpv1.Reference{Name: "someone-elses-cluster"}

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

	got := joinParamsFor(p, "server.example.com", "the-node-token")

	if got.K3sVersion != "v1.29.0+k3s1" {
		t.Errorf("want k3sVersion carried through, got %q", got.K3sVersion)
	}
	if got.ExtraArgs != "--node-label env=prod" {
		t.Errorf("want extraArgs carried through, got %q", got.ExtraArgs)
	}
	if got.ServerHost != "server.example.com" {
		t.Errorf("want the connector-resolved server host, got %q", got.ServerHost)
	}
	if got.NodeToken != "the-node-token" {
		t.Errorf("want the connector-resolved node token, got %q", got.NodeToken)
	}
}

// TestObserveMinimalResponse calls Observe against a minimal SSH response:
// the agent service reports active and nothing else is read from the host.
// Observe must complete without panicking and return a sane
// ExternalObservation.
func TestObserveMinimalResponse(t *testing.T) {
	host, port := startFakeSSHServer(t, map[string]sshResponse{
		"systemctl is-active k3s-agent 2>/dev/null || echo inactive": {Stdout: "active"},
	}, sshResponse{})

	e := &external{
		ssh:        newTestSSHClient(t, host, port),
		serverHost: "server.example.com",
		nodeToken:  "the-node-token",
		role:       "agent",
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
	if cr.Status.AtProvider.Role != "agent" {
		t.Errorf("want Role %q recorded in atProvider, got %q", "agent", cr.Status.AtProvider.Role)
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
		serverHost: "server.example.com",
		nodeToken:  "the-node-token",
		role:       "agent",
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

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

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/namespaced/v1alpha1"
)

// newTestKubeClient builds a fake kube client seeded with objs, for
// exercising persistLastAppliedClusterConfig's conflict-safe
// read-modify-write against a real (fake) API server rather than an
// in-memory struct.
func newTestKubeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	if err := v1alpha1.SchemeBuilder.AddToScheme(s); err != nil {
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
		"systemctl is-active k3s 2>/dev/null || echo inactive": {Stdout: "active"},
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

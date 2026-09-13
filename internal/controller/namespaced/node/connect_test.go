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

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/namespaced/v1alpha1"
)

const testNamespace = "default"

// kindProviderConfig is the namespaced ProviderConfig Kind literal, pulled
// into a constant because enough fixtures reference it that one more
// literal trips goconst.
const kindProviderConfig = "ProviderConfig"

// newTestProviderConfig builds a namespaced ProviderConfig sourcing
// credentials from secretName, and the matching Secret carrying an SSH
// password -- so Connect()'s SSH dial has a non-empty auth method to offer
// the fake server, which accepts any credential.
func newTestProviderConfig(name, secretName string) *v1alpha1.ProviderConfig {
	pc := &v1alpha1.ProviderConfig{}
	pc.SetName(name)
	pc.SetNamespace(testNamespace)
	pc.Spec = v1alpha1.ProviderConfigSpec{
		Username: "test",
		Credentials: v1alpha1.ProviderCredentials{
			Source: xpv2.CredentialsSourceSecret,
			CommonCredentialSelectors: xpv2.CommonCredentialSelectors{
				SecretRef: &xpv2.SecretKeySelector{
					SecretReference: xpv2.SecretReference{Name: secretName, Namespace: testNamespace},
					Key:             "password",
				},
			},
		},
	}
	return pc
}

func newTestClusterProviderConfig(name, secretName string) *v1alpha1.ClusterProviderConfig {
	cpc := &v1alpha1.ClusterProviderConfig{}
	cpc.SetName(name)
	cpc.Spec = v1alpha1.ProviderConfigSpec{
		Username: "test",
		Credentials: v1alpha1.ProviderCredentials{
			Source: xpv2.CredentialsSourceSecret,
			CommonCredentialSelectors: xpv2.CommonCredentialSelectors{
				SecretRef: &xpv2.SecretKeySelector{
					SecretReference: xpv2.SecretReference{Name: secretName, Namespace: testNamespace},
					Key:             "password",
				},
			},
		},
	}
	return cpc
}

// newTestProviderConfigWithForeignSecretNamespace builds a namespaced
// ProviderConfig whose spec names a credential Secret namespace the CR does
// NOT live in -- proving Connect() pins the lookup to the CR's own
// namespace rather than trusting the spec's value.
func newTestProviderConfigWithForeignSecretNamespace(name, secretName, specNamespace string) *v1alpha1.ProviderConfig {
	pc := &v1alpha1.ProviderConfig{}
	pc.SetName(name)
	pc.SetNamespace(testNamespace)
	pc.Spec = v1alpha1.ProviderConfigSpec{
		Username: "test",
		Credentials: v1alpha1.ProviderCredentials{
			Source: xpv2.CredentialsSourceSecret,
			CommonCredentialSelectors: xpv2.CommonCredentialSelectors{
				SecretRef: &xpv2.SecretKeySelector{
					SecretReference: xpv2.SecretReference{Name: secretName, Namespace: specNamespace},
					Key:             "password",
				},
			},
		},
	}
	return pc
}

func newTestCredentialsSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       map[string][]byte{"password": []byte("test-password")},
	}
}

// newTestClusterWithConnSecret builds a namespaced Cluster carrying a
// writeConnectionSecretToRef (local to the Cluster's own namespace), plus
// the connection Secret it points at.
func newTestClusterWithConnSecret(name, host, connSecretName string) (*v1alpha1.Cluster, *corev1.Secret) {
	cluster := &v1alpha1.Cluster{}
	cluster.SetName(name)
	cluster.SetNamespace(testNamespace)
	cluster.Spec.ForProvider.Host = host
	cluster.Spec.ForProvider.Port = 22
	cluster.Spec.WriteConnectionSecretToReference = &xpv2.LocalSecretReference{Name: connSecretName}

	connSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: connSecretName, Namespace: testNamespace},
		Data:       map[string][]byte{"node-token": []byte("the-node-token")},
	}
	return cluster, connSecret
}

func newTestConnectCR(host string, port int, uid string) *v1alpha1.Node {
	cr := newNodeCR("test-node", "v1.28.2+k3s1", "")
	cr.SetNamespace(testNamespace)
	cr.SetUID(types.UID(uid))
	cr.Spec.ForProvider.Host = host
	cr.Spec.ForProvider.Port = port
	return cr
}

// TestConnectSuccess proves the "ProviderConfig" arm of the namespaced
// Kind-routing switch: PC fetch scoped to the CR's own namespace, credential
// extraction, SSH dial, and cluster resolution against the referenced
// Cluster's connection secret -- succeeds end to end. Cluster/ClusterRef
// carry the values the reconciler's reference resolver would have already
// written by the time Connect runs.
func TestConnectSuccess(t *testing.T) {
	sshHost, sshPort := startFakeSSHServer(t, nil, sshResponse{})

	pc := newTestProviderConfig("test-pc", "ssh-creds")
	secret := newTestCredentialsSecret("ssh-creds")
	cluster, connSecret := newTestClusterWithConnSecret("my-cluster", "server.example.com", "cluster-conn")
	kube := newTestKubeClient(pc, secret, cluster, connSecret)

	cr := newTestConnectCR(sshHost, sshPort, "test-uid")
	cr.Spec.ForProvider.Cluster = ptr.To("my-cluster")
	cr.Spec.ForProvider.ClusterRef = &xpv2.NamespacedReference{Name: "my-cluster"}
	cr.SetProviderConfigReference(&xpv2.ProviderConfigReference{Kind: kindProviderConfig, Name: "test-pc"})

	c := &connector{kube: kube, usage: resource.NewProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	client, err := c.Connect(context.Background(), cr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Disconnect(context.Background()); err != nil {
		t.Errorf("Disconnect: %v", err)
	}
}

// TestConnectProviderConfigNotFound proves a missing (namespaced)
// ProviderConfig fails Connect with a wrapped error.
func TestConnectProviderConfigNotFound(t *testing.T) {
	kube := newTestKubeClient()

	cr := newTestConnectCR("10.0.0.1", 22, "test-uid")
	cr.SetProviderConfigReference(&xpv2.ProviderConfigReference{Kind: kindProviderConfig, Name: "missing-pc"})

	c := &connector{kube: kube, usage: resource.NewProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	_, err := c.Connect(context.Background(), cr)
	if err == nil {
		t.Fatal("want an error when the referenced ProviderConfig does not exist")
	}
	if !strings.Contains(err.Error(), "cannot get ProviderConfig") {
		t.Errorf("want the error wrapped with its Connect-path context, got %q", err.Error())
	}
}

// TestConnectClusterProviderConfigKindRouting proves the
// "ClusterProviderConfig" arm of the Kind-routing switch.
func TestConnectClusterProviderConfigKindRouting(t *testing.T) {
	sshHost, sshPort := startFakeSSHServer(t, nil, sshResponse{})

	cpc := newTestClusterProviderConfig("test-cpc", "ssh-creds")
	secret := newTestCredentialsSecret("ssh-creds")
	cluster, connSecret := newTestClusterWithConnSecret("my-cluster", "server.example.com", "cluster-conn")
	kube := newTestKubeClient(cpc, secret, cluster, connSecret)

	cr := newTestConnectCR(sshHost, sshPort, "test-uid")
	cr.Spec.ForProvider.Cluster = ptr.To("my-cluster")
	cr.Spec.ForProvider.ClusterRef = &xpv2.NamespacedReference{Name: "my-cluster"}
	cr.SetProviderConfigReference(&xpv2.ProviderConfigReference{Kind: "ClusterProviderConfig", Name: "test-cpc"})

	c := &connector{kube: kube, usage: resource.NewProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	client, err := c.Connect(context.Background(), cr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Disconnect(context.Background()); err != nil {
		t.Errorf("Disconnect: %v", err)
	}
}

// TestConnectPinsCredentialSecretNamespace proves the namespaced
// ProviderConfig's credential Secret always resolves in the referencing
// CR's own namespace: the PC here names an attacker-controlled namespace
// in its spec, and the only Secret the fake client holds lives in the CR's
// namespace. If Connect() trusted the spec's namespace, credential
// extraction would fail; success proves the lookup was pinned to
// cr.GetNamespace() instead.
func TestConnectPinsCredentialSecretNamespace(t *testing.T) {
	sshHost, sshPort := startFakeSSHServer(t, nil, sshResponse{})

	pc := newTestProviderConfigWithForeignSecretNamespace("test-pc", "ssh-creds", "attacker-namespace")
	secret := newTestCredentialsSecret("ssh-creds") // lives in testNamespace, NOT "attacker-namespace"
	kube := newTestKubeClient(pc, secret)

	cr := newTestConnectCR(sshHost, sshPort, "test-uid")
	cr.Spec.ForProvider.Cluster = nil // credential resolution alone is under test; cluster resolution is optional for Connect
	cr.Spec.ForProvider.ClusterRef = nil
	cr.SetProviderConfigReference(&xpv2.ProviderConfigReference{Kind: kindProviderConfig, Name: "test-pc"})

	c := &connector{kube: kube, usage: resource.NewProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	client, err := c.Connect(context.Background(), cr)
	if err != nil {
		t.Fatalf("Connect: want the credential Secret namespace pinned to the CR's own namespace (%q), got %v", testNamespace, err)
	}
	if err := client.Disconnect(context.Background()); err != nil {
		t.Errorf("Disconnect: %v", err)
	}
}

// TestConnectUnsupportedProviderConfigKind proves the routing switch's
// default arm rejects an unrecognised Kind with a clear error.
func TestConnectUnsupportedProviderConfigKind(t *testing.T) {
	kube := newTestKubeClient()

	cr := newTestConnectCR("10.0.0.1", 22, "test-uid")
	cr.SetProviderConfigReference(&xpv2.ProviderConfigReference{Kind: "SomeOtherKind", Name: "whatever"})

	c := &connector{kube: kube, usage: resource.NewProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	_, err := c.Connect(context.Background(), cr)
	if err == nil {
		t.Fatal("want an error for an unsupported ProviderConfig Kind")
	}
	if !strings.Contains(err.Error(), "unsupported provider config kind") {
		t.Errorf("want the unsupported-Kind error, got %q", err.Error())
	}
}

// TestConnectMissingClusterDoesNotFailConnect proves the delete-wedge fix:
// a Connect() whose referenced Cluster is gone still returns a usable
// client rather than an error. If Connect() itself failed here, the
// managed reconciler would never reach Observe or Delete, and the Node's
// finalizer could never clear once its Cluster is gone -- the only
// recovery being a manual finalizer strip. Create must still fail loudly
// rather than silently proceed with an empty nodeToken, since CEL only
// requires clusterRef when managementPolicies allows Create or Update.
func TestConnectMissingClusterDoesNotFailConnect(t *testing.T) {
	sshHost, sshPort := startFakeSSHServer(t, nil, sshResponse{})

	pc := newTestProviderConfig("test-pc", "ssh-creds")
	secret := newTestCredentialsSecret("ssh-creds")
	kube := newTestKubeClient(pc, secret) // deliberately no Cluster object

	cr := newTestConnectCR(sshHost, sshPort, "test-uid")
	cr.Spec.ForProvider.Cluster = ptr.To("gone-cluster")
	cr.Spec.ForProvider.ClusterRef = &xpv2.NamespacedReference{Name: "gone-cluster"}
	cr.SetProviderConfigReference(&xpv2.ProviderConfigReference{Kind: kindProviderConfig, Name: "test-pc"})

	c := &connector{kube: kube, usage: resource.NewProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	client, err := c.Connect(context.Background(), cr)
	if err != nil {
		t.Fatalf("Connect: want no error when the referenced Cluster is missing (Observe/Delete must still work), got %v", err)
	}
	defer func() {
		if err := client.Disconnect(context.Background()); err != nil {
			t.Errorf("Disconnect: %v", err)
		}
	}()

	if _, err := client.Create(context.Background(), cr); err == nil {
		t.Error("want Create to fail loudly when clusterRef could not be resolved, not silently join with an empty nodeToken")
	} else if !strings.Contains(err.Error(), errGetCluster) {
		t.Errorf("want the deferred cluster-resolution error surfaced from Create, got %q", err.Error())
	}
}

// TestConnectClusterRefOptionalForObserve proves the Cluster value being
// unresolved (a legal Observe-only adoption, per the root-level CEL rule
// that gates clusterRef/clusterSelector on managementPolicies allowing
// Create or Update) does not fail Connect.
func TestConnectClusterRefOptionalForObserve(t *testing.T) {
	sshHost, sshPort := startFakeSSHServer(t, nil, sshResponse{})

	pc := newTestProviderConfig("test-pc", "ssh-creds")
	secret := newTestCredentialsSecret("ssh-creds")
	kube := newTestKubeClient(pc, secret)

	cr := newTestConnectCR(sshHost, sshPort, "test-uid")
	cr.Spec.ForProvider.Cluster = nil // legal: Observe-only adoption, not yet resolved
	cr.Spec.ForProvider.ClusterRef = nil
	cr.SetProviderConfigReference(&xpv2.ProviderConfigReference{Kind: kindProviderConfig, Name: "test-pc"})

	c := &connector{kube: kube, usage: resource.NewProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	client, err := c.Connect(context.Background(), cr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Disconnect(context.Background()); err != nil {
		t.Errorf("Disconnect: %v", err)
	}
}

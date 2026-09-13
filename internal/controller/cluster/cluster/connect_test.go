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

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/cluster/v1alpha1"
)

// newTestProviderConfig builds a ProviderConfig sourcing credentials from
// secretName, and the matching Secret carrying an SSH password -- so
// Connect()'s SSH dial has a non-empty auth method to offer the fake
// server, which accepts any credential.
func newTestProviderConfig(name, secretName string) *v1alpha1.ProviderConfig {
	pc := &v1alpha1.ProviderConfig{}
	pc.SetName(name)
	pc.Spec = v1alpha1.ProviderConfigSpec{
		Username: "test",
		Credentials: v1alpha1.ProviderCredentials{
			Source: xpv2.CredentialsSourceSecret,
			CommonCredentialSelectors: xpv2.CommonCredentialSelectors{
				SecretRef: &xpv2.SecretKeySelector{
					SecretReference: xpv2.SecretReference{Name: secretName, Namespace: "crossplane-system"},
					Key:             "password",
				},
			},
		},
	}
	return pc
}

func newTestCredentialsSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "crossplane-system"},
		Data:       map[string][]byte{"password": []byte("test-password")},
	}
}

// TestConnectSuccess (this is convention's "cluster direct fetch" case:
// cluster-scoped Connect resolves its ProviderConfig with a plain
// types.NamespacedName{Name: ref.Name}, no namespace and no Kind routing)
// proves the full path -- ProviderConfig fetch, credential extraction, SSH
// dial against host:port from spec -- succeeds end to end.
func TestConnectSuccess(t *testing.T) {
	host, port := startFakeSSHServer(t, nil, sshResponse{})

	pc := newTestProviderConfig("test-pc", "ssh-creds")
	secret := newTestCredentialsSecret("ssh-creds")
	kube := newTestKubeClient(pc, secret)

	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")
	cr.SetUID(types.UID("test-uid"))
	cr.Spec.ForProvider.Host = host
	cr.Spec.ForProvider.Port = port
	cr.SetProviderConfigReference(&xpv2.Reference{Name: "test-pc"})

	c := &connector{kube: kube, usage: resource.NewLegacyProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	client, err := c.Connect(context.Background(), cr)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Disconnect(context.Background()); err != nil {
		t.Errorf("Disconnect: %v", err)
	}
}

// TestConnectProviderConfigNotFound proves a missing ProviderConfig fails
// Connect with a wrapped error, before any SSH dial is attempted.
func TestConnectProviderConfigNotFound(t *testing.T) {
	kube := newTestKubeClient() // no ProviderConfig seeded

	cr := newClusterCR("test-cluster", "v1.28.2+k3s1", "")
	cr.SetUID(types.UID("test-uid"))
	cr.SetProviderConfigReference(&xpv2.Reference{Name: "missing-pc"})

	c := &connector{kube: kube, usage: resource.NewLegacyProviderConfigUsageTracker(kube, &v1alpha1.ProviderConfigUsage{})}

	_, err := c.Connect(context.Background(), cr)
	if err == nil {
		t.Fatal("want an error when the referenced ProviderConfig does not exist")
	}
	if !strings.Contains(err.Error(), "cannot get ProviderConfig") {
		t.Errorf("want the error wrapped with its Connect-path context, got %q", err.Error())
	}
}

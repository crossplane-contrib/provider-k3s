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

package config

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/crossplane-contrib/provider-k3s/apis/namespaced/v1alpha1"
)

func TestClusterProviderConfigKinds(t *testing.T) {
	got := clusterProviderConfigKinds()

	if diff := cmp.Diff(v1alpha1.ClusterProviderConfigGroupVersionKind, got.Config); diff != "" {
		t.Errorf("Config: -want, +got:\n%s", diff)
	}
	if diff := cmp.Diff(v1alpha1.ProviderConfigUsageGroupVersionKind, got.Usage); diff != "" {
		t.Errorf("Usage: -want, +got:\n%s", diff)
	}
	if diff := cmp.Diff(v1alpha1.ProviderConfigUsageListGroupVersionKind, got.UsageList); diff != "" {
		t.Errorf("UsageList: -want, +got:\n%s", diff)
	}
}

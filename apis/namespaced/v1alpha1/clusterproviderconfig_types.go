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

package v1alpha1

import (
	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"
	xpv2 "github.com/crossplane/crossplane-runtime/v2/apis/common/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true

// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="SECRET-NAME",type="string",JSONPath=".spec.credentials.secretRef.name",priority=1
// +kubebuilder:resource:scope=Cluster,categories={crossplane,provider,k3s}
// A ClusterProviderConfig configures the K3s provider with SSH credentials at the cluster level for cross-namespace access.
type ClusterProviderConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProviderConfigSpec   `json:"spec"`
	Status ProviderConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterProviderConfigList contains a list of ClusterProviderConfig.
type ClusterProviderConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterProviderConfig `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion

// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="CONFIG-NAME",type="string",JSONPath=".providerConfigRef.name"
// +kubebuilder:printcolumn:name="RESOURCE-KIND",type="string",JSONPath=".resourceRef.kind"
// +kubebuilder:printcolumn:name="RESOURCE-NAME",type="string",JSONPath=".resourceRef.name"
// +kubebuilder:resource:scope=Cluster,categories={crossplane,provider,k3s}
// A ClusterProviderConfigUsage indicates that a resource is using a ClusterProviderConfig.
type ClusterProviderConfigUsage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	xpv2.TypedProviderConfigUsage `json:",inline"`
}

// +kubebuilder:object:root=true

// ClusterProviderConfigUsageList contains a list of ClusterProviderConfigUsage
type ClusterProviderConfigUsageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterProviderConfigUsage `json:"items"`
}

// GetCondition returns the condition for the given ConditionType.
func (pc *ClusterProviderConfig) GetCondition(ct xpv1.ConditionType) xpv1.Condition {
	return pc.Status.GetCondition(ct)
}

// SetConditions sets conditions on the resource status.
func (pc *ClusterProviderConfig) SetConditions(c ...xpv1.Condition) {
	pc.Status.SetConditions(c...)
}

// GetUsers returns the number of users.
func (pc *ClusterProviderConfig) GetUsers() int64 {
	return pc.Status.Users
}

// SetUsers sets the number of users.
func (pc *ClusterProviderConfig) SetUsers(i int64) {
	pc.Status.Users = i
}

func init() {
	SchemeBuilder.Register(&ClusterProviderConfig{}, &ClusterProviderConfigList{})
	SchemeBuilder.Register(&ClusterProviderConfigUsage{}, &ClusterProviderConfigUsageList{})
}

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
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/reference"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"

	"github.com/crossplane-contrib/provider-k3s/apis/common/driftdetection"
)

// ClusterObjectName extracts the referenced Cluster's Kubernetes object
// name. The generator's default reference.ExternalName() extractor would
// instead resolve to the Cluster's external-name -- this provider looks up
// the target Cluster by its object name (to read the Cluster's connection
// secret), an identifier the external-name does not reliably carry.
func ClusterObjectName() reference.ExtractValueFn {
	return func(mg resource.Managed) string {
		return mg.GetName()
	}
}

// NodeParameters are the configurable fields of a Node.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.port) || has(self.port)",message="port cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.clusterRef) || has(self.clusterRef)",message="clusterRef cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cluster) || has(self.cluster)",message="cluster cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.role) || has(self.role)",message="role cannot be removed once set"
type NodeParameters struct {
	// Host is the DNS name or IP address of the target machine. It is the
	// SSH connection target and the sole identity this provider has for the
	// joined node; it cannot be changed after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="host is immutable after creation"
	Host string `json:"host"`

	// Port is the SSH port. Defaults to 22. It addresses the same SSH
	// connection target as host and cannot be changed after creation.
	// +optional
	// +kubebuilder:default=22
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="port is immutable after creation"
	Port int `json:"port,omitempty"`

	// Cluster is the resolved Kubernetes object name of the Cluster this
	// node joins, populated from ClusterRef or ClusterSelector. Changing
	// the resolved cluster after creation would mean leaving one cluster
	// and joining another, which this provider does not attempt as an
	// in-place update.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf || oldSelf == ''",message="cluster is immutable once set"
	// +crossplane:generate:reference:type=github.com/crossplane-contrib/provider-k3s/apis/cluster/v1alpha1.Cluster
	// +crossplane:generate:reference:extractor=github.com/crossplane-contrib/provider-k3s/apis/cluster/v1alpha1.ClusterObjectName()
	Cluster *string `json:"cluster,omitempty"`

	// ClusterRef references the Cluster resource this node joins, by name.
	// Required when managementPolicies allows Create or Update (see the
	// root-level CEL rule on Node) unless clusterSelector is set instead --
	// Observe only needs host to address the object, per convention.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef is immutable after creation"
	ClusterRef *xpv2.Reference `json:"clusterRef,omitempty"`

	// ClusterSelector selects the Cluster resource this node joins, by
	// label match.
	// +optional
	ClusterSelector *xpv2.Selector `json:"clusterSelector,omitempty"`

	// Role is the role of this node: "agent" (worker) or "server" (additional control plane).
	// Required when managementPolicies allows Create or Update (see the
	// root-level CEL rule on Node) -- Observe only needs host to address
	// the object. Switching a joined node's role requires leaving and
	// rejoining, which this provider does not attempt as an in-place
	// update.
	// +optional
	// +kubebuilder:validation:Enum=agent;server
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="role is immutable after creation"
	Role string `json:"role,omitempty"`

	// K3sVersion is the specific k3s version to install.
	// +optional
	K3sVersion string `json:"k3sVersion,omitempty"`

	// K3sChannel is the release channel.
	// +optional
	// +kubebuilder:default="stable"
	K3sChannel string `json:"k3sChannel,omitempty"`

	// ExtraArgs are additional arguments passed to k3s.
	// +optional
	ExtraArgs string `json:"extraArgs,omitempty"`

	// TLSSAN adds an additional TLS SAN (only applicable for server role).
	// +optional
	TLSSAN string `json:"tlsSAN,omitempty"`
}

// NodeObservation are the observable fields of a Node.
type NodeObservation struct {
	// ID is this resource's identity, mirrored from the external-name
	// annotation once Observe or Create has run. Uptest's import recovery
	// test compares this against the external-name recorded before the
	// resource's status was cleared.
	ID string `json:"id,omitempty"`

	// Ready indicates the node has successfully joined the cluster.
	Ready bool `json:"ready,omitempty"`

	// Cluster mirrors spec.forProvider.cluster: the resolved Kubernetes
	// object name of the Cluster this node joined. Immutable once set, so
	// it cannot diverge from spec once the resource exists.
	Cluster string `json:"cluster,omitempty"`

	// Host mirrors spec.forProvider.host: the SSH target this Node was
	// joined on. Immutable, so it cannot diverge from spec once the
	// resource exists.
	Host string `json:"host,omitempty"`

	// Port mirrors spec.forProvider.port. Immutable, so it cannot diverge
	// from spec once the resource exists.
	Port int `json:"port,omitempty"`

	// Role is the observed role of the node. Immutable, so it cannot
	// diverge from spec once the resource exists.
	Role string `json:"role,omitempty"`

	// K3sVersion is the installed version reported by the node.
	K3sVersion string `json:"k3sVersion,omitempty"`

	// K3sChannel mirrors the release channel this controller most
	// recently confirmed it applied. Empty until the first successful
	// Create or Update: the k3s join script reports no channel of its
	// own, so there is nothing to mirror before this controller has
	// recorded one.
	K3sChannel string `json:"k3sChannel,omitempty"`

	// ExtraArgs mirrors the value this controller most recently confirmed
	// it applied. Empty until the first successful Create or Update.
	ExtraArgs string `json:"extraArgs,omitempty"`

	// TLSSAN mirrors the value this controller most recently confirmed it
	// applied. Empty until the first successful Create or Update.
	TLSSAN string `json:"tlsSAN,omitempty"`
}

// A NodeSpec defines the desired state of a Node.
type NodeSpec struct {
	xpv2.ClusterManagedResourceSpec `json:",inline"`

	// DriftDetection configures which forProvider fields are owned outside
	// Crossplane and how drift in those fields is detected and corrected.
	// Absent configuration means drift detection is enabled with no
	// ignored paths -- today's behaviour.
	// +optional
	DriftDetection *driftdetection.DriftDetection `json:"driftDetection,omitempty"`

	ForProvider NodeParameters `json:"forProvider"`
}

// A NodeStatus represents the observed state of a Node.
type NodeStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`
	AtProvider                 NodeObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Node joins a machine to an existing k3s cluster as an agent or server via SSH.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={crossplane,managed,k3s}
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.managementPolicies) || !('*' in self.spec.managementPolicies || 'Create' in self.spec.managementPolicies || 'Update' in self.spec.managementPolicies) || has(self.spec.forProvider.clusterRef) || has(self.spec.forProvider.clusterSelector)",message="clusterRef or clusterSelector is required when managementPolicies allows Create or Update"
// +kubebuilder:validation:XValidation:rule="!has(self.spec) || !has(self.spec.managementPolicies) || !('*' in self.spec.managementPolicies || 'Create' in self.spec.managementPolicies || 'Update' in self.spec.managementPolicies) || has(self.spec.forProvider.role)",message="role is required when managementPolicies allows Create or Update"
type Node struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeSpec   `json:"spec"`
	Status NodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeList contains a list of Node
type NodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Node `json:"items"`
}

// Node type metadata.
var (
	NodeKind             = reflect.TypeOf(Node{}).Name()
	NodeGroupKind        = schema.GroupKind{Group: Group, Kind: NodeKind}.String()
	NodeKindAPIVersion   = NodeKind + "." + SchemeGroupVersion.String()
	NodeGroupVersionKind = SchemeGroupVersion.WithKind(NodeKind)
)

func init() {
	SchemeBuilder.Register(&Node{}, &NodeList{})
}

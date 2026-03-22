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

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"
	xpv2 "github.com/crossplane/crossplane-runtime/v2/apis/common/v2"
)

// NodeParameters are the configurable fields of a Node.
type NodeParameters struct {
	// Host is the DNS name or IP address of the target machine.
	Host string `json:"host"`

	// Port is the SSH port. Defaults to 22.
	// +optional
	// +kubebuilder:default=22
	Port int `json:"port,omitempty"`

	// ClusterRef is a reference to the Cluster resource this node joins.
	ClusterRef xpv1.Reference `json:"clusterRef"`

	// Role is the role of this node: "agent" (worker) or "server" (additional control plane).
	// +kubebuilder:validation:Enum=agent;server
	Role string `json:"role"`

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
	// Ready indicates the node has successfully joined the cluster.
	Ready bool `json:"ready,omitempty"`

	// Role is the observed role of the node.
	Role string `json:"role,omitempty"`
}

// A NodeSpec defines the desired state of a Node.
type NodeSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`
	ForProvider              NodeParameters `json:"forProvider"`
}

// A NodeStatus represents the observed state of a Node.
type NodeStatus struct {
	xpv1.ResourceStatus `json:",inline"`
	AtProvider          NodeObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Node joins a machine to an existing k3s cluster as an agent or server via SSH.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={crossplane,managed,k3s}
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

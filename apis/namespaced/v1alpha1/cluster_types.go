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

// ClusterParameters are the configurable fields of a Cluster.
type ClusterParameters struct {
	// Host is the DNS name or IP address of the target machine.
	Host string `json:"host"`

	// Port is the SSH port. Defaults to 22.
	// +optional
	// +kubebuilder:default=22
	Port int `json:"port,omitempty"`

	// K3sVersion is the specific k3s version to install (e.g., "v1.28.2+k3s1").
	// +optional
	K3sVersion string `json:"k3sVersion,omitempty"`

	// K3sChannel is the release channel (stable, latest, v1.28, etc.).
	// +optional
	// +kubebuilder:default="stable"
	K3sChannel string `json:"k3sChannel,omitempty"`

	// ClusterInit enables embedded etcd for HA multi-server setup.
	// +optional
	ClusterInit bool `json:"clusterInit,omitempty"`

	// TLSSAN adds an additional hostname or IP as a TLS Subject Alternative Name.
	// +optional
	TLSSAN string `json:"tlsSAN,omitempty"`

	// DisableTraefik disables the default Traefik ingress controller.
	// +optional
	DisableTraefik bool `json:"disableTraefik,omitempty"`

	// DisableServiceLB disables the default ServiceLB load balancer.
	// +optional
	DisableServiceLB bool `json:"disableServiceLB,omitempty"`

	// ExtraArgs are additional arguments passed to k3s server.
	// +optional
	ExtraArgs string `json:"extraArgs,omitempty"`

	// DatastoreEndpoint is an external datastore URL for HA (MySQL/PostgreSQL).
	// +optional
	DatastoreEndpoint string `json:"datastoreEndpoint,omitempty"`
}

// ClusterObservation are the observable fields of a Cluster.
type ClusterObservation struct {
	// Ready indicates the k3s server is running.
	Ready bool `json:"ready,omitempty"`

	// K3sVersion is the installed version reported by the server.
	K3sVersion string `json:"k3sVersion,omitempty"`
}

// A ClusterSpec defines the desired state of a Cluster.
type ClusterSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`
	ForProvider              ClusterParameters `json:"forProvider"`
}

// A ClusterStatus represents the observed state of a Cluster.
type ClusterStatus struct {
	xpv1.ResourceStatus `json:",inline"`
	AtProvider          ClusterObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A Cluster installs and manages a k3s server on a remote host via SSH.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,categories={crossplane,managed,k3s}
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec"`
	Status ClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterList contains a list of Cluster
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

// Cluster type metadata.
var (
	ClusterKind             = reflect.TypeOf(Cluster{}).Name()
	ClusterGroupKind        = schema.GroupKind{Group: Group, Kind: ClusterKind}.String()
	ClusterKindAPIVersion   = ClusterKind + "." + SchemeGroupVersion.String()
	ClusterGroupVersionKind = SchemeGroupVersion.WithKind(ClusterKind)
)

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{})
}

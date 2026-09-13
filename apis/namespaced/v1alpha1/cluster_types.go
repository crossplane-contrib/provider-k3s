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

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"

	"github.com/crossplane-contrib/provider-k3s/apis/common/driftdetection"
)

// ClusterParameters are the configurable fields of a Cluster.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.port) || has(self.port)",message="port cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.clusterInit) || has(self.clusterInit)",message="clusterInit cannot be removed once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.datastoreEndpoint) || has(self.datastoreEndpoint)",message="datastoreEndpoint cannot be removed once set"
type ClusterParameters struct {
	// Host is the DNS name or IP address of the target machine. It is the
	// SSH connection target and the sole identity this provider has for the
	// installed server; it cannot be changed after creation.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="host is immutable after creation"
	Host string `json:"host"`

	// Port is the SSH port. Defaults to 22. It addresses the same SSH
	// connection target as host and cannot be changed after creation.
	// +optional
	// +kubebuilder:default=22
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="port is immutable after creation"
	Port int `json:"port,omitempty"`

	// K3sVersion is the specific k3s version to install (e.g., "v1.28.2+k3s1").
	// +optional
	K3sVersion string `json:"k3sVersion,omitempty"`

	// K3sChannel is the release channel (stable, latest, v1.28, etc.).
	// +optional
	// +kubebuilder:default="stable"
	K3sChannel string `json:"k3sChannel,omitempty"`

	// ClusterInit enables embedded etcd for HA multi-server setup. A running
	// server cannot be converted between embedded-etcd and non-HA mode in
	// place, so this is immutable after creation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterInit is immutable after creation"
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
	// A running server cannot be re-pointed at a new datastore in place, so
	// this is immutable after creation.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="datastoreEndpoint is immutable after creation"
	DatastoreEndpoint string `json:"datastoreEndpoint,omitempty"`
}

// ClusterObservation are the observable fields of a Cluster.
type ClusterObservation struct {
	// ID is this resource's identity, mirrored from the external-name
	// annotation once Observe or Create has run. Uptest's import recovery
	// test compares this against the external-name recorded before the
	// resource's status was cleared.
	ID string `json:"id,omitempty"`

	// Ready indicates the k3s server is running.
	Ready bool `json:"ready,omitempty"`

	// Host mirrors spec.forProvider.host: the SSH target this Cluster was
	// installed on. Immutable, so it cannot diverge from spec once the
	// resource exists.
	Host string `json:"host,omitempty"`

	// Port mirrors spec.forProvider.port. Immutable, so it cannot diverge
	// from spec once the resource exists.
	Port int `json:"port,omitempty"`

	// K3sVersion is the installed version reported by the server.
	K3sVersion string `json:"k3sVersion,omitempty"`

	// K3sChannel mirrors the release channel this controller most
	// recently confirmed it applied. Empty until the first successful
	// Create or Update: the k3s install script reports no channel of its
	// own, so there is nothing to mirror before this controller has
	// recorded one.
	K3sChannel string `json:"k3sChannel,omitempty"`

	// ClusterInit mirrors spec.forProvider.clusterInit. Immutable, so it
	// cannot diverge from spec once the resource exists.
	ClusterInit bool `json:"clusterInit,omitempty"`

	// TLSSAN mirrors the TLS SAN this controller most recently confirmed
	// it applied. Empty until the first successful Create or Update, for
	// the same reason as k3sChannel.
	TLSSAN string `json:"tlsSAN,omitempty"`

	// DisableTraefik mirrors the value this controller most recently
	// confirmed it applied. Empty (false) until the first successful
	// Create or Update.
	DisableTraefik bool `json:"disableTraefik,omitempty"`

	// DisableServiceLB mirrors the value this controller most recently
	// confirmed it applied. Empty (false) until the first successful
	// Create or Update.
	DisableServiceLB bool `json:"disableServiceLB,omitempty"`

	// ExtraArgs mirrors the value this controller most recently confirmed
	// it applied. Empty until the first successful Create or Update.
	ExtraArgs string `json:"extraArgs,omitempty"`

	// DatastoreEndpoint mirrors spec.forProvider.datastoreEndpoint.
	// Immutable, so it cannot diverge from spec once the resource exists.
	DatastoreEndpoint string `json:"datastoreEndpoint,omitempty"`
}

// A ClusterSpec defines the desired state of a Cluster.
type ClusterSpec struct {
	xpv2.ManagedResourceSpec `json:",inline"`

	// DriftDetection configures which forProvider fields are owned outside
	// Crossplane and how drift in those fields is detected and corrected.
	// Absent configuration means drift detection is enabled with no
	// ignored paths -- today's behaviour.
	// +optional
	DriftDetection *driftdetection.DriftDetection `json:"driftDetection,omitempty"`

	ForProvider ClusterParameters `json:"forProvider"`
}

// A ClusterStatus represents the observed state of a Cluster.
type ClusterStatus struct {
	xpv2.ManagedResourceStatus `json:",inline"`
	AtProvider                 ClusterObservation `json:"atProvider,omitempty"`
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

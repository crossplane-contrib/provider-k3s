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

// Package driftdetection contains the shared drift detection configuration
// type embedded in every managed resource spec (spec.driftDetection). It
// declares which forProvider fields are owned outside Crossplane and how
// drift in those fields is detected and corrected.
// +kubebuilder:object:generate=true
package driftdetection

// Mode selects the drift detection behaviour for a managed resource.
// +kubebuilder:validation:Enum=enabled;warn;disabled
type Mode string

const (
	// ModeEnabled detects and corrects drift. This is the default.
	ModeEnabled Mode = "enabled"
	// ModeWarn detects drift and reports it on the DriftDetected condition,
	// but does not correct it.
	ModeWarn Mode = "warn"
	// ModeDisabled neither detects nor corrects drift.
	ModeDisabled Mode = "disabled"
)

// IgnoreRule declares fields whose value is owned by something other than
// Crossplane. Listed fields are seeded from spec on create, then neither
// compared nor written from spec: the value observed on the external
// resource is carried forward instead.
type IgnoreRule struct {
	// Paths to ignore, in Crossplane field path notation rooted at the
	// managed resource spec, for example forProvider.k3sVersion. This is
	// the grammar Composition patches use; JSON Pointer form
	// (/forProvider/k3sVersion) is also accepted. List indices and
	// wildcards are not supported -- ignore the whole list instead.
	// +optional
	Paths []string `json:"paths"`
}

// DriftDetection configures how drift between spec and the external
// resource is detected and corrected.
type DriftDetection struct {
	// Mode selects the drift detection behaviour.
	// +optional
	// +kubebuilder:default=enabled
	Mode Mode `json:"mode,omitempty"`

	// Ignore declares fields owned outside Crossplane.
	// +optional
	Ignore []IgnoreRule `json:"ignore"`
}

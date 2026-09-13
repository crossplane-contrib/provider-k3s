//go:build ignore

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

// generate-registration scans the provider's directory structure and emits two
// generated files:
//
//   - apis/zz_generated_register.go  — scheme registration
//   - internal/controller/zz_generated_register.go — controller registration
//
// Run from the module root:
//
//	go run hack/generate-registration.go <module-path>
//
// The output files carry a "DO NOT EDIT" header and are committed to the
// repository so that make check-diff validates them.
//
// Directory-scanning convention:
//   - apis/cluster/v1alpha1/register.go     → legacy-group ProviderConfig + cluster-scoped MRs
//   - apis/namespaced/v1alpha1/register.go  → namespaced-group ProviderConfig + namespaced MRs
//   - apis/cluster/<resource>/<ver>/register.go  → cluster-scoped MRs (per-resource layout)
//   - apis/namespaced/<resource>/<ver>/register.go → namespaced MRs (per-resource layout)
//   - internal/controller/<pkg>/     → controllers, one level under internal/controller
//   - internal/controller/config/    → ProviderConfig controller, when present (always
//     included, never gated)
//
// This provider's dual-scope split places controller packages one level
// higher than the fleet's typical per-resource layout: internal/controller/
// holds only "cluster" and "namespaced" (each fanning out to its own
// per-resource and per-scope-config controllers internally, including its
// own ProviderConfig controller). There is no top-level internal/controller/config
// package here, so the "config" special case below is conditional on that
// package actually existing — it does on providers using the flatter layout,
// and does not here.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run hack/generate-registration.go <module-path>")
	}
	modulePath := os.Args[1]

	schemeEntries, err := scanSchemeEntries(modulePath)
	if err != nil {
		log.Fatalf("scanning scheme entries: %v", err)
	}

	controllerEntries, hasConfig, err := scanControllerEntries(modulePath, schemeEntries)
	if err != nil {
		log.Fatalf("scanning controller entries: %v", err)
	}

	if err := generateSchemeFile(schemeEntries); err != nil {
		log.Fatalf("generating apis/zz_generated_register.go: %v", err)
	}
	fmt.Println("generate-registration: wrote apis/zz_generated_register.go")

	if err := generateControllerFile(modulePath, controllerEntries, hasConfig); err != nil {
		log.Fatalf("generating internal/controller/zz_generated_register.go: %v", err)
	}
	fmt.Println("generate-registration: wrote internal/controller/zz_generated_register.go")
}

// schemeEntry represents one importable API package.
type schemeEntry struct {
	// Alias is the Go import alias, e.g. "clusterv1alpha1".
	Alias string
	// ImportPath is the full Go import path.
	ImportPath string
}

// controllerEntry represents one importable controller package.
type controllerEntry struct {
	// ImportPath is the full Go import path.
	ImportPath string
	// PkgName is the last path segment, e.g. "cluster" or "namespaced".
	PkgName string
}

// scanSchemeEntries discovers all API packages that require scheme registration.
// The scan order produces a deterministic result:
//  1. apis/cluster/<resource>/<version> — sorted alphabetically (includes the
//     legacy-group ProviderConfig at apis/cluster/v1alpha1)
//  2. apis/namespaced/<resource>/<version> — sorted alphabetically (includes
//     the namespaced-group ProviderConfig at apis/namespaced/v1alpha1)
func scanSchemeEntries(modulePath string) ([]schemeEntry, error) {
	var entries []schemeEntry

	for _, scope := range []string{"cluster", "namespaced"} {
		scopeDir := filepath.Join("apis", scope)
		if _, err := os.Stat(scopeDir); os.IsNotExist(err) {
			continue
		}
		scoped, err := walkScopeDir(modulePath, scope, scopeDir)
		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", scopeDir, err)
		}
		entries = append(entries, scoped...)
	}

	return entries, nil
}

// walkScopeDir discovers all scheme-registrable packages under scopeDir and
// returns the corresponding scheme entries sorted alphabetically.
//
// Two package layout patterns are supported:
//
//  1. Shared-version layout:
//     apis/cluster/v1alpha1/register.go
//     Detected when register.go exists directly in the first-level subdirectory.
//     Alias: <scope><version> (e.g. "clusterv1alpha1")
//
//  2. Per-resource layout:
//     apis/cluster/<resource>/<version>/register.go
//     Detected when register.go is absent at the first level but present at
//     the second level.
//     Alias: <scope><resource><version> (e.g. "clustertailnetkeyv1alpha1")
func walkScopeDir(modulePath, scope, scopeDir string) ([]schemeEntry, error) {
	var entries []schemeEntry

	topNames, err := readDirNames(scopeDir)
	if err != nil {
		return nil, err
	}
	for _, topName := range topNames {
		topDir := filepath.Join(scopeDir, topName)
		info, err := os.Stat(topDir)
		if err != nil || !info.IsDir() {
			continue
		}

		// Pattern 1: register.go directly in the first-level subdirectory
		// (shared-version layout, e.g. apis/cluster/v1alpha1/register.go).
		directRegister := filepath.Join(topDir, "register.go")
		if _, err := os.Stat(directRegister); err == nil {
			alias := scope + sanitizeIdent(topName)
			importPath := modulePath + "/apis/" + scope + "/" + topName
			entries = append(entries, schemeEntry{
				Alias:      alias,
				ImportPath: importPath,
			})
			continue
		}

		// Pattern 2: register.go at the second level
		// (per-resource layout, e.g. apis/cluster/<resource>/<version>/register.go).
		versions, err := readDirNames(topDir)
		if err != nil {
			return nil, err
		}
		for _, ver := range versions {
			registerFile := filepath.Join(topDir, ver, "register.go")
			if _, err := os.Stat(registerFile); os.IsNotExist(err) {
				continue
			}
			// Alias: <scope><resource><version> — no hyphens or underscores.
			alias := scope + sanitizeIdent(topName) + ver
			importPath := modulePath + "/apis/" + scope + "/" + topName + "/" + ver
			entries = append(entries, schemeEntry{
				Alias:      alias,
				ImportPath: importPath,
			})
		}
	}
	return entries, nil
}

// scanControllerEntries discovers controller packages one level under
// internal/controller/.
//
// A top-level internal/controller/config package — the flatter, single-scope
// fleet layout — is special-cased when present: it is reported separately
// (hasConfig=true) so the caller can emit it as the never-gated ProviderConfig
// entry, and it is excluded from the returned entries.
//
// This provider has no such package: each of its two scope packages
// (cluster, namespaced) owns its own ProviderConfig controller(s) internally
// and exposes them only through its own Setup/SetupGated. Both "cluster" and
// "namespaced" are therefore ordinary entries here, on equal footing with any
// per-resource controller package a flatter layout would have at this level.
func scanControllerEntries(modulePath string, schemeEntries []schemeEntry) (entries []controllerEntry, hasConfig bool, err error) {
	// Build a set of known API resources from the scheme entries, used only
	// to filter a per-resource layout — see sharedVersionLayout below.
	apiResources := make(map[string]bool)
	sharedVersionLayout := true // assume shared-version until proven otherwise
	for _, e := range schemeEntries {
		rel := strings.TrimPrefix(e.ImportPath, modulePath+"/apis/")
		parts := strings.SplitN(rel, "/", 3)
		if len(parts) >= 2 && (parts[0] == "cluster" || parts[0] == "namespaced") {
			name := parts[1]
			apiResources[name] = true
			// If we see a name that doesn't look like a version ("v" + digits),
			// it's a per-resource layout.
			if len(name) < 2 || name[0] != 'v' {
				sharedVersionLayout = false
			}
		}
	}

	controllerDir := filepath.Join("internal", "controller")
	if _, statErr := os.Stat(controllerDir); os.IsNotExist(statErr) {
		return nil, false, nil
	}

	pkgNames, err := readDirNames(controllerDir)
	if err != nil {
		return nil, false, err
	}

	for _, pkg := range pkgNames {
		dir := filepath.Join(controllerDir, pkg)
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			continue
		}
		if pkg == "config" {
			hasConfig = true
			continue
		}
		// For per-resource layout, only include packages that correspond to a
		// known API resource. For shared-version layout (including this
		// provider's scope-fanout packages), include all packages since
		// controller names don't match the version directory name.
		if !sharedVersionLayout && !apiResources[pkg] {
			continue
		}
		entries = append(entries, controllerEntry{
			ImportPath: modulePath + "/internal/controller/" + pkg,
			PkgName:    pkg,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].PkgName < entries[j].PkgName
	})
	return entries, hasConfig, nil
}

// --------------------------------------------------------------------------
// Template data types
// --------------------------------------------------------------------------

type schemeFileData struct {
	ScopeEntries []schemeEntry
}

// --------------------------------------------------------------------------
// File generators
// --------------------------------------------------------------------------

const schemeFileTmpl = `// Code generated by generate-registration. DO NOT EDIT.
//
// SetupGated defers MR controller startup until CRDs are installed (SafeStart).
// Setup starts all controllers immediately (RBAC fallback path).
// ProviderConfig (config.Setup) is NEVER gated — it must always be available.

package apis

import (
	"k8s.io/apimachinery/pkg/runtime"

{{- range .ScopeEntries}}
	{{.Alias}} "{{.ImportPath}}"
{{- end}}
)

// AddToSchemes may be used to add all resources defined in the project to a Scheme.
var AddToSchemes runtime.SchemeBuilder

func init() {
	AddToSchemes = append(AddToSchemes,
{{- range .ScopeEntries}}
		{{.Alias}}.SchemeBuilder.AddToScheme,
{{- end}}
	)
}

// AddToScheme adds all Resources to the Scheme.
func AddToScheme(s *runtime.Scheme) error {
	return AddToSchemes.AddToScheme(s)
}
`

const controllerFileTmplWithConfig = `// Code generated by generate-registration. DO NOT EDIT.
//
// SetupGated defers MR controller startup until CRDs are installed (SafeStart).
// Setup starts all controllers immediately (RBAC fallback path).
// ProviderConfig (config.Setup) is NEVER gated — it must always be available.

package controller

import (
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	ctrl "sigs.k8s.io/controller-runtime"

	"{{.ConfigPkg}}"
{{- range .Controllers}}
	"{{.ImportPath}}"
{{- end}}
)

// SetupGated creates all controllers with safe-start (gate) support.
// MR controllers defer startup until CRDs are installed.
// ProviderConfig (config.Setup) is never gated — it must always be available.
func SetupGated(mgr ctrl.Manager, o controller.Options) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
		// ProviderConfig — never gated, must always be available.
		config.Setup,
{{- range .Controllers}}
		{{.PkgName}}.SetupGated,
{{- end}}
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}

// Setup creates all controllers without gate support (RBAC fallback path).
// MR controllers start immediately without waiting for CRD installation.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
		// ProviderConfig — never gated, must always be available.
		config.Setup,
{{- range .Controllers}}
		{{.PkgName}}.Setup,
{{- end}}
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}
`

// controllerFileTmplNoConfig is used when there is no top-level
// internal/controller/config package — each scanned entry owns and gates its
// own ProviderConfig controller(s) internally.
const controllerFileTmplNoConfig = `// Code generated by generate-registration. DO NOT EDIT.
//
// SetupGated defers controller startup until CRDs are installed (SafeStart).
// Setup starts all controllers immediately (RBAC fallback path).
// Each entry below owns and gates its own ProviderConfig controller(s)
// internally — there is no top-level, ungated ProviderConfig entry here.

package controller

import (
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	ctrl "sigs.k8s.io/controller-runtime"
{{range .Controllers}}
	"{{.ImportPath}}"
{{- end}}
)

// SetupGated creates all controllers with safe-start (gate) support.
func SetupGated(mgr ctrl.Manager, o controller.Options) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
{{- range .Controllers}}
		{{.PkgName}}.SetupGated,
{{- end}}
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}

// Setup creates all controllers without gate support (RBAC fallback path).
func Setup(mgr ctrl.Manager, o controller.Options) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
{{- range .Controllers}}
		{{.PkgName}}.Setup,
{{- end}}
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}
`

type controllerFileData struct {
	ConfigPkg   string
	Controllers []controllerEntry
}

func generateSchemeFile(entries []schemeEntry) error {
	data := schemeFileData{
		ScopeEntries: entries,
	}

	return renderGoFile("apis/zz_generated_register.go", schemeFileTmpl, data)
}

func generateControllerFile(modulePath string, controllers []controllerEntry, hasConfig bool) error {
	data := controllerFileData{
		Controllers: controllers,
	}
	tmpl := controllerFileTmplNoConfig
	if hasConfig {
		data.ConfigPkg = modulePath + "/internal/controller/config"
		tmpl = controllerFileTmplWithConfig
	}
	return renderGoFile("internal/controller/zz_generated_register.go", tmpl, data)
}

// renderGoFile executes tmplStr with data, formats the result with gofmt, and
// writes it to outPath.
func renderGoFile(outPath, tmplStr string, data interface{}) error {
	t, err := template.New("").Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return fmt.Errorf("executing template: %w", err)
	}
	src, err := format.Source(buf.Bytes())
	if err != nil {
		// Emit the unformatted source so the caller can debug.
		_ = os.WriteFile(outPath, buf.Bytes(), 0o644)
		return fmt.Errorf("formatting %s (raw output written): %w", outPath, err)
	}
	if err := os.WriteFile(outPath, src, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", outPath, err)
	}
	return nil
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// readDirNames returns the sorted names of entries in dir (subdirs and files).
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// sanitizeIdent strips non-alphanumeric characters (e.g. hyphens) from s so
// it is safe to use in a Go identifier.
func sanitizeIdent(s string) string {
	return strings.NewReplacer("-", "", "_", "").Replace(s)
}

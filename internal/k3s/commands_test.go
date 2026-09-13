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

package k3s

import (
	"strings"
	"testing"
)

// TestInstallCommandSkipsBlockingStart mirrors
// TestJoinCommandSkipsBlockingStart for the Cluster server install: the
// piped install script must be told to skip its own blocking `systemctl
// restart` (Type=notify waits for the server to actually finish starting)
// so the SSH call returns promptly instead of consuming the whole
// external-call budget on a resource-constrained host.
func TestInstallCommandSkipsBlockingStart(t *testing.T) {
	got := InstallCommand(InstallParams{})

	if !strings.Contains(got, "INSTALL_K3S_SKIP_START='true'") {
		t.Errorf("want INSTALL_K3S_SKIP_START='true' in the piped install command, got %q", got)
	}
}

// TestInstallCommandQueuesNonBlockingRestart mirrors
// TestJoinCommandQueuesNonBlockingRestart: the install command chains its
// own non-blocking restart of the "k3s" server unit after the (now
// non-blocking) install script, using --no-block so the SSH call does not
// wait for the unit to report ready.
func TestInstallCommandQueuesNonBlockingRestart(t *testing.T) {
	got := InstallCommand(InstallParams{})

	wantRestart := "systemctl restart k3s --no-block"
	if !strings.Contains(got, wantRestart) {
		t.Errorf("want %q chained onto the install command, got %q", wantRestart, got)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "'") {
		t.Errorf("want the restart wrapped in its own sh -c so sudo fallback is scoped to it, got %q", got)
	}
}

// TestInstallCommandRestartFallsBackToSudo mirrors
// TestJoinCommandRestartFallsBackToSudo: the appended restart command
// mirrors get.k3s.io's own root-detection idiom rather than assuming either
// passwordless root or a sudo-capable user unconditionally.
func TestInstallCommandRestartFallsBackToSudo(t *testing.T) {
	got := InstallCommand(InstallParams{})

	if !strings.Contains(got, `[ "$(id -u)" = 0 ]`) {
		t.Errorf("want a root-uid check guarding the sudo fallback, got %q", got)
	}
	if !strings.Contains(got, "SUDO=sudo") {
		t.Errorf("want a sudo fallback for a non-root SSH user, got %q", got)
	}
}

// TestInstallCommandStillCarriesExecFlags proves the non-blocking-start
// change did not disturb the existing INSTALL_K3S_EXEC construction: every
// server flag is still present in the piped install command.
func TestInstallCommandStillCarriesExecFlags(t *testing.T) {
	got := InstallCommand(InstallParams{
		TLSSAN:            "cluster.example.com",
		ClusterInit:       true,
		DatastoreEndpoint: "etcd://localhost:2379",
		DisableTraefik:    true,
		DisableServiceLB:  true,
		ExtraArgs:         "--node-label foo=bar",
		K3sVersion:        "v1.28.2+k3s1",
	})

	for _, want := range []string{
		"--tls-san cluster.example.com",
		"--cluster-init",
		"--datastore-endpoint etcd://localhost:2379",
		"--disable traefik",
		"--disable servicelb",
		"--node-label foo=bar",
		"INSTALL_K3S_VERSION='v1.28.2+k3s1'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q present in the install command, got %q", want, got)
		}
	}
}

// TestJoinCommandSkipsBlockingStart proves the fix for the near-timeout
// Create()/Update() wedge: the piped install script must be told to skip
// its own blocking `systemctl restart` (which under the unit's default
// Type=notify waits for the agent/server to actually finish joining) so
// the SSH call returns promptly.
func TestJoinCommandSkipsBlockingStart(t *testing.T) {
	got := JoinCommand(JoinParams{ServerHost: "server.example.com", NodeToken: "tok", Role: "agent"})

	if !strings.Contains(got, "INSTALL_K3S_SKIP_START='true'") {
		t.Errorf("want INSTALL_K3S_SKIP_START='true' in the piped install command, got %q", got)
	}
}

// TestJoinCommandQueuesNonBlockingRestart proves the command chains its own
// non-blocking restart after the (now non-blocking) install, using
// --no-block so the SSH call does not wait for the unit to report ready.
func TestJoinCommandQueuesNonBlockingRestart(t *testing.T) {
	cases := []struct {
		name        string
		role        string
		wantService string
	}{
		{name: "agent", role: "agent", wantService: "k3s-agent"},
		{name: "additional server", role: "server", wantService: "k3s"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JoinCommand(JoinParams{ServerHost: "server.example.com", NodeToken: "tok", Role: tc.role})

			wantRestart := "systemctl restart " + tc.wantService + " --no-block"
			if !strings.Contains(got, wantRestart) {
				t.Errorf("want %q chained onto the join command, got %q", wantRestart, got)
			}
			if !strings.HasSuffix(strings.TrimSpace(got), "'") {
				t.Errorf("want the restart wrapped in its own sh -c so sudo fallback is scoped to it, got %q", got)
			}
		})
	}
}

// TestJoinCommandRestartFallsBackToSudo proves the appended restart command
// mirrors get.k3s.io's own root-detection idiom rather than assuming
// either passwordless root or a sudo-capable user unconditionally.
func TestJoinCommandRestartFallsBackToSudo(t *testing.T) {
	got := JoinCommand(JoinParams{ServerHost: "server.example.com", NodeToken: "tok", Role: "agent"})

	if !strings.Contains(got, `[ "$(id -u)" = 0 ]`) {
		t.Errorf("want a root-uid check guarding the sudo fallback, got %q", got)
	}
	if !strings.Contains(got, "SUDO=sudo") {
		t.Errorf("want a sudo fallback for a non-root SSH user, got %q", got)
	}
}

// TestServiceNameForRole proves the exported helper mirrors get.k3s.io's
// own SYSTEM_NAME derivation, the same mapping Observe uses to build its
// systemctl probe.
func TestServiceNameForRole(t *testing.T) {
	if got := ServiceNameForRole("agent"); got != "k3s-agent" {
		t.Errorf("want k3s-agent for role agent, got %q", got)
	}
	if got := ServiceNameForRole("server"); got != "k3s" {
		t.Errorf("want k3s for role server, got %q", got)
	}
	if got := ServiceNameForRole(""); got != "k3s-agent" {
		t.Errorf("want the agent default for an empty role, got %q", got)
	}
}

// LoadState and ActiveState values shared across every ServiceProbe fixture
// below, named as constants rather than repeated literals to keep the
// duplicate-string linter from flagging the tables as needing dedup.
const (
	testLoadStateLoaded   = "loaded"
	testLoadStateNotFound = "not-found"

	testActiveStateActive       = "active"
	testActiveStateActivating   = "activating"
	testActiveStateReloading    = "reloading"
	testActiveStateDeactivating = "deactivating"
	testActiveStateFailed       = "failed"
	testActiveStateInactive     = "inactive"
)

// TestServiceProbeCommand proves the probe command names the exact unit
// requested and asks systemctl for both properties this provider needs, in
// the order ParseServiceProbe expects to find them.
func TestServiceProbeCommand(t *testing.T) {
	got := ServiceProbeCommand("k3s-agent")
	want := "systemctl show -p LoadState -p ActiveState --value k3s-agent 2>/dev/null"
	if got != want {
		t.Errorf("ServiceProbeCommand(%q) = %q, want %q", "k3s-agent", got, want)
	}
}

// TestParseServiceProbe proves the two-line reply is split into LoadState
// (first) and ActiveState (second), matching the -p flag order
// ServiceProbeCommand builds the command with.
func TestParseServiceProbe(t *testing.T) {
	cases := []struct {
		name       string
		stdout     string
		wantLoad   string
		wantActive string
	}{
		{name: "loaded and active", stdout: testLoadStateLoaded + "\n" + testActiveStateActive, wantLoad: testLoadStateLoaded, wantActive: testActiveStateActive},
		{name: "loaded and activating", stdout: testLoadStateLoaded + "\n" + testActiveStateActivating, wantLoad: testLoadStateLoaded, wantActive: testActiveStateActivating},
		{name: "not-found and inactive", stdout: testLoadStateNotFound + "\n" + testActiveStateInactive, wantLoad: testLoadStateNotFound, wantActive: testActiveStateInactive},
		{name: "trailing whitespace trimmed", stdout: testLoadStateLoaded + "\n" + testActiveStateFailed + "\n", wantLoad: testLoadStateLoaded, wantActive: testActiveStateFailed},
		{name: "empty reply", stdout: "", wantLoad: "", wantActive: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseServiceProbe(tc.stdout)
			if got.LoadState != tc.wantLoad {
				t.Errorf("LoadState = %q, want %q", got.LoadState, tc.wantLoad)
			}
			if got.ActiveState != tc.wantActive {
				t.Errorf("ActiveState = %q, want %q", got.ActiveState, tc.wantActive)
			}
		})
	}
}

// TestServiceProbeExists proves existence is decided from LoadState alone,
// and in particular that every ActiveState a non-blocking restart's
// crash-loop or queued-but-not-yet-running window can produce --
// activating, failed, inactive -- is still existence as long as systemd has
// a loaded artifact for the unit. This is the direct fix for the regression
// that misclassified 71 of 92 installed-but-not-active Observes as absence:
// the old probe (`systemctl is-active <unit> 2>/dev/null || echo inactive`)
// corrupted its own stdout into a two-line reply ("activating\ninactive") on
// every one of these states, which matched no known case and fell through
// to "not found".
func TestServiceProbeExists(t *testing.T) {
	cases := []struct {
		name  string
		probe ServiceProbe
		want  bool
	}{
		{name: "loaded and active", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateActive}, want: true},
		{name: "loaded and activating (queued restart in flight)", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateActivating}, want: true},
		{name: "loaded and reloading", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateReloading}, want: true},
		{name: "loaded and deactivating", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateDeactivating}, want: true},
		{name: "loaded and failed (crash before ready)", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateFailed}, want: true},
		{name: "loaded and inactive (auto-restart backoff gap)", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateInactive}, want: true},
		{name: "not-found", probe: ServiceProbe{LoadState: testLoadStateNotFound, ActiveState: testActiveStateInactive}, want: false},
		{name: "empty probe (SSH read failed)", probe: ServiceProbe{}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.probe.Exists(); got != tc.want {
				t.Errorf("Exists() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestServiceProbeReady proves readiness is decided from ActiveState alone,
// and only a clean active state counts -- every transitional or failed
// state reports not-ready without being treated as absence (see
// TestServiceProbeExists).
func TestServiceProbeReady(t *testing.T) {
	cases := []struct {
		name  string
		probe ServiceProbe
		want  bool
	}{
		{name: "active", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateActive}, want: true},
		{name: "activating", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateActivating}, want: false},
		{name: "failed", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateFailed}, want: false},
		{name: "inactive", probe: ServiceProbe{LoadState: testLoadStateLoaded, ActiveState: testActiveStateInactive}, want: false},
		{name: "not-found", probe: ServiceProbe{LoadState: testLoadStateNotFound, ActiveState: testActiveStateInactive}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.probe.Ready(); got != tc.want {
				t.Errorf("Ready() = %v, want %v", got, tc.want)
			}
		})
	}
}

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
	"fmt"
	"strings"
)

// InstallParams holds parameters for building a k3s server install command.
type InstallParams struct {
	K3sVersion        string
	K3sChannel        string
	ClusterInit       bool
	TLSSAN            string
	DisableTraefik    bool
	DisableServiceLB  bool
	ExtraArgs         string
	DatastoreEndpoint string
}

// JoinParams holds parameters for building a k3s join command.
type JoinParams struct {
	ServerHost string
	NodeToken  string
	Role       string // "agent" or "server"
	K3sVersion string
	K3sChannel string
	ExtraArgs  string
	TLSSAN     string
}

// InstallCommand builds the k3s server install command string.
//
// Like JoinCommand (see its doc comment for the full mechanism), this skips
// the install script's own blocking `systemctl restart` -- which, under the
// unit's Type=notify, waits for the server to actually finish starting --
// and issues a non-blocking restart itself instead. Without this, a single
// blocked SSH call on a resource-constrained host can consume nearly the
// entire external-call budget on its own, leaving Observe's poll loop no
// chance to see incremental progress and Create/Update no budget left to
// persist their result.
func InstallCommand(params InstallParams) string {
	envParts := make([]string, 0, 2)

	// Build INSTALL_K3S_EXEC
	execArgs := []string{"server"}

	if params.TLSSAN != "" {
		execArgs = append(execArgs, fmt.Sprintf("--tls-san %s", params.TLSSAN))
	}

	if params.ClusterInit {
		execArgs = append(execArgs, "--cluster-init")
	}

	if params.DatastoreEndpoint != "" {
		execArgs = append(execArgs, fmt.Sprintf("--datastore-endpoint %s", params.DatastoreEndpoint))
	}

	if params.DisableTraefik {
		execArgs = append(execArgs, "--disable traefik")
	}

	if params.DisableServiceLB {
		execArgs = append(execArgs, "--disable servicelb")
	}

	if params.ExtraArgs != "" {
		execArgs = append(execArgs, params.ExtraArgs)
	}

	envParts = append(envParts, fmt.Sprintf("INSTALL_K3S_EXEC='%s'", strings.Join(execArgs, " ")))

	// Version specification
	envParts = append(envParts, versionEnv(params.K3sVersion, params.K3sChannel)...)

	// See the doc comment above: skip the install script's own blocking
	// restart and issue a non-blocking one ourselves.
	envParts = append(envParts, "INSTALL_K3S_SKIP_START='true'")

	install := fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -", strings.Join(envParts, " "))

	return install + " && " + nonBlockingRestartCommand(ServiceNameForRole("server"))
}

// JoinCommand builds the k3s agent/server join command string.
//
// The install script this pipes into (get.k3s.io) creates a systemd unit
// with Type=notify and TimeoutStartSec=0, and -- unless told to skip it --
// starts that unit itself with a plain `systemctl restart`, which under
// Type=notify BLOCKS until the k3s process itself calls sd_notify(READY=1):
// that is, until the agent/server has actually finished joining and is
// fully serving, not merely until the process has been launched. On a
// resource-constrained host that wait measured close to the entire
// reconcile external-call budget, so a single blocked SSH session consumed
// nearly the whole timeout on its own, leaving Observe's own poll loop no
// chance to see incremental progress.
//
// INSTALL_K3S_SKIP_START tells the install script to stop short of that
// blocking restart -- the binary, systemd unit and environment file are
// still written -- and the command chains its own non-blocking restart
// (systemctl ... --no-block) afterward, which returns as soon as the
// restart job is queued rather than waiting for it to finish. The unit's
// Type=notify is untouched, so `systemctl is-active` still only reports
// "active" once the agent/server is genuinely ready -- Observe's poll loop
// sees "activating" in the meantime instead of a blocked SSH call.
func JoinCommand(params JoinParams) string {
	var envParts []string

	// K3S_URL and K3S_TOKEN are required for joining
	envParts = append(envParts,
		fmt.Sprintf("K3S_URL='https://%s:6443'", params.ServerHost),
		fmt.Sprintf("K3S_TOKEN='%s'", params.NodeToken),
	)

	// If joining as an additional server, set INSTALL_K3S_EXEC
	if params.Role == "server" {
		execArgs := []string{"server"}

		if params.TLSSAN != "" {
			execArgs = append(execArgs, fmt.Sprintf("--tls-san %s", params.TLSSAN))
		}

		if params.ExtraArgs != "" {
			execArgs = append(execArgs, params.ExtraArgs)
		}

		envParts = append(envParts, fmt.Sprintf("INSTALL_K3S_EXEC='%s'", strings.Join(execArgs, " ")))
	} else if params.ExtraArgs != "" {
		// Agent with extra args
		envParts = append(envParts, fmt.Sprintf("INSTALL_K3S_EXEC='%s'", params.ExtraArgs))
	}

	// Version specification
	envParts = append(envParts, versionEnv(params.K3sVersion, params.K3sChannel)...)

	// See the doc comment above: skip the install script's own blocking
	// restart and issue a non-blocking one ourselves.
	envParts = append(envParts, "INSTALL_K3S_SKIP_START='true'")

	install := fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -", strings.Join(envParts, " "))

	return install + " && " + nonBlockingRestartCommand(ServiceNameForRole(params.Role))
}

// ServiceNameForRole returns the systemd unit name get.k3s.io derives for a
// join of the given role, mirroring its own setup_env logic: joining as an
// additional "server" resolves CMD_K3S to "server", giving SYSTEM_NAME
// "k3s" (the same unit name as the original cluster server); anything else
// resolves to the "agent" default, giving SYSTEM_NAME "k3s-agent". Exported
// so Observe's own systemctl probe uses the exact same mapping Create and
// Update build their restart command from.
func ServiceNameForRole(role string) string {
	if role == "server" {
		return "k3s"
	}
	return "k3s-agent"
}

// nonBlockingRestartCommand builds a `systemctl restart --no-block` for the
// given service, falling back to sudo exactly like get.k3s.io's own $SUDO
// detection (unless already running as root) since the install script has
// already left the unit enabled and its environment file written -- this
// only ever needs to trigger the start, never wait for it.
func nonBlockingRestartCommand(service string) string {
	return fmt.Sprintf(
		`sh -c 'SUDO=; [ "$(id -u)" = 0 ] || SUDO=sudo; $SUDO systemctl restart %s --no-block'`,
		service,
	)
}

// ServiceProbeCommand builds the systemctl query Observe uses to decide
// both whether a unit's install artifact exists at all, and separately
// whether it is currently serving.
//
// This replaces an earlier probe of the shape `systemctl is-active <unit>
// 2>/dev/null || echo inactive`. That command is broken for every reply
// except a clean "active": `is-active` exits non-zero for "activating",
// "reloading", "deactivating", "failed" and "inactive" alike, and on a bare
// shell `||` chain the exit code is all that is tested -- so the `echo
// inactive` fallback ALSO fires on top of whatever state is-active already
// printed, turning a single-token reply into two lines ("activating\ninactive",
// "failed\ninactive", ...). That two-line string matches none of the exact
// single-token cases a classifier can switch on, so every non-"active" reply
// -- including the transitional states a non-blocking restart is specifically
// meant to surface -- was silently misclassified as absence, and
// crossplane-runtime re-ran Create on top of an install already in flight.
// Measured directly: a local systemd unit built to reproduce the same
// Type=notify crash-loop a failing join produces (process exits before
// calling sd_notify, Restart=on-failure) showed is-active's raw reply stuck
// at "activating\ninactive" for the unit's entire auto-restart cycle.
//
// `systemctl show` has neither failure mode: it always exits 0 regardless of
// whether the unit exists, so it needs no error-swallowing shell fallback,
// and its `--value` output is one clean line per requested property with no
// exit-code-driven interference. LoadState answers whether systemd has an
// artifact for the unit at all -- "not-found" only when it has never heard
// of it, and "loaded" (or any other value) from the moment get.k3s.io's
// install script writes the unit file, independent of whether the process
// behind it has ever successfully started. ActiveState answers whether that
// process has actually signalled ready, separately.
func ServiceProbeCommand(service string) string {
	return fmt.Sprintf("systemctl show -p LoadState -p ActiveState --value %s 2>/dev/null", service)
}

// ServiceProbe is the parsed reply from a command ServiceProbeCommand built.
type ServiceProbe struct {
	LoadState   string
	ActiveState string
}

// ParseServiceProbe splits the two-line reply ServiceProbeCommand's output
// carries -- LoadState first, ActiveState second -- into a ServiceProbe. That
// order is systemd's own internal property order, NOT the order the -p flags
// were given in: `systemctl show -p ActiveState -p LoadState --value` prints
// exactly the same two lines in the same sequence, so reordering the flags in
// ServiceProbeCommand would not reorder this reply. A short or empty reply (a
// probe that failed to run at all) leaves the corresponding field empty, which
// Exists and Ready both already treat as "no".
func ParseServiceProbe(stdout string) ServiceProbe {
	lines := strings.SplitN(strings.TrimSpace(stdout), "\n", 2)
	probe := ServiceProbe{LoadState: strings.TrimSpace(lines[0])}
	if len(lines) > 1 {
		probe.ActiveState = strings.TrimSpace(lines[1])
	}
	return probe
}

// Exists reports whether systemd has a loaded artifact for the unit --
// true from the moment get.k3s.io's install script writes the unit file,
// independent of whether the process behind it has ever started or is
// mid-crash-loop. This is the existence signal Observe must use for a unit
// started by the non-blocking restart JoinCommand and InstallCommand build:
// the queued restart, an auto-restart backoff gap, or a transient "failed"
// all still report a loaded artifact, and treating any of them as absence
// re-triggers Create on the very restart this provider just queued.
func (p ServiceProbe) Exists() bool {
	return p.LoadState != "" && p.LoadState != "not-found"
}

// Ready reports whether the unit's own Type=notify process has signalled
// ready.
func (p ServiceProbe) Ready() bool {
	return p.ActiveState == "active"
}

// UninstallServerCommand returns the k3s server uninstall command.
func UninstallServerCommand() string {
	return "/usr/local/bin/k3s-uninstall.sh"
}

// UninstallAgentCommand returns the k3s agent uninstall command.
func UninstallAgentCommand() string {
	return "/usr/local/bin/k3s-agent-uninstall.sh"
}

// RewriteKubeconfig replaces 127.0.0.1 and localhost with the actual host address in a kubeconfig.
func RewriteKubeconfig(kubeconfig, host string) string {
	result := strings.ReplaceAll(kubeconfig, "127.0.0.1", host)
	result = strings.ReplaceAll(result, "localhost", host)
	return result
}

func versionEnv(version, channel string) []string {
	var parts []string
	if version != "" {
		parts = append(parts, fmt.Sprintf("INSTALL_K3S_VERSION='%s'", version))
	} else if channel != "" {
		parts = append(parts, fmt.Sprintf("INSTALL_K3S_CHANNEL='%s'", channel))
	}
	return parts
}

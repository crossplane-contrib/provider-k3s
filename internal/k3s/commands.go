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
func InstallCommand(params InstallParams) string {
	var envParts []string

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

	return fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -", strings.Join(envParts, " "))
}

// JoinCommand builds the k3s agent/server join command string.
func JoinCommand(params JoinParams) string {
	var envParts []string

	// K3S_URL and K3S_TOKEN are required for joining
	envParts = append(envParts, fmt.Sprintf("K3S_URL='https://%s:6443'", params.ServerHost))
	envParts = append(envParts, fmt.Sprintf("K3S_TOKEN='%s'", params.NodeToken))

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

	return fmt.Sprintf("curl -sfL https://get.k3s.io | %s sh -", strings.Join(envParts, " "))
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

# provider-k3s

`provider-k3s` is a [Crossplane](https://crossplane.io/) Provider that manages
[k3s](https://k3s.io/) clusters on remote hosts via SSH. It allows you to
declaratively install k3s servers, join worker or additional server nodes, and
retrieve kubeconfigs — all through Kubernetes custom resources.

This provider is inspired by [k3sup](https://github.com/alexellis/k3sup) and
uses the same approach of SSH-based k3s installation and cluster joining. While
k3sup is a CLI tool for one-shot operations, provider-k3s brings the same
workflow into the Crossplane reconciliation loop for continuous desired-state
management.

## Features

- Install k3s server on a remote host via SSH
- Join worker (agent) or additional server nodes to an existing cluster
- Retrieve kubeconfig and node-token via Crossplane connection secrets
- Support for TLS SANs, HA with embedded etcd, custom k3s versions/channels
- ProviderConfig with SSH key or password authentication

## Resources

### Cluster-Scoped (`k3s.crossplane.io`)

| Kind | API Group | Description |
|------|-----------|-------------|
| `ProviderConfig` | `k3s.crossplane.io/v1alpha1` | SSH credentials (cluster-scoped) |
| `Cluster` | `k3s.crossplane.io/v1alpha1` | k3s server installation |
| `Node` | `k3s.crossplane.io/v1alpha1` | Join agent/server to cluster |

### Namespaced (`k3s.m.crossplane.io`)

| Kind | API Group | Description |
|------|-----------|-------------|
| `ProviderConfig` | `k3s.m.crossplane.io/v1alpha1` | SSH credentials (namespaced) |
| `ClusterProviderConfig` | `k3s.m.crossplane.io/v1alpha1` | SSH credentials (cluster-wide, for cross-namespace access) |
| `Cluster` | `k3s.m.crossplane.io/v1alpha1` | k3s server installation (namespaced) |
| `Node` | `k3s.m.crossplane.io/v1alpha1` | Join agent/server to cluster (namespaced) |

## Quick Start

### 1. Create SSH credentials

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: ssh-credentials
  namespace: crossplane-system
type: Opaque
stringData:
  ssh-privatekey: |
    -----BEGIN OPENSSH PRIVATE KEY-----
    ...
    -----END OPENSSH PRIVATE KEY-----
```

### 2. Create a ProviderConfig

```yaml
apiVersion: k3s.crossplane.io/v1alpha1
kind: ProviderConfig
metadata:
  name: ssh-ubuntu
spec:
  username: ubuntu
  credentials:
    source: Secret
    secretRef:
      namespace: crossplane-system
      name: ssh-credentials
      key: ssh-privatekey
```

### 3. Install a k3s Cluster

```yaml
apiVersion: k3s.crossplane.io/v1alpha1
kind: Cluster
metadata:
  name: my-cluster
spec:
  forProvider:
    host: 192.168.1.100
    port: 22
    k3sChannel: stable
    tlsSAN: k3s.example.com
    disableTraefik: true
  providerConfigRef:
    name: ssh-ubuntu
  writeConnectionSecretToRef:
    name: my-cluster-kubeconfig
    namespace: crossplane-system
```

The controller will SSH into the target host, install k3s, and publish the
kubeconfig, endpoint, and node-token to the connection secret.

### 4. Join a Worker Node

```yaml
apiVersion: k3s.crossplane.io/v1alpha1
kind: Node
metadata:
  name: worker-1
spec:
  forProvider:
    host: 192.168.1.101
    port: 22
    clusterRef:
      name: my-cluster
    role: agent
    k3sChannel: stable
  providerConfigRef:
    name: ssh-ubuntu
```

The Node controller resolves the server host and node-token from the referenced
Cluster resource and its connection secret, then SSHs into the worker to run the
k3s join command.

## How It Works

```
ProviderConfig (SSH user + key)
       |
       v
   Cluster CR ──────────────> SSH into server host
   (host, k3s params)         curl -sfL https://get.k3s.io | sh -
       |                      ↓
       |                 connection secret:
       |                   - kubeconfig
       |                   - endpoint
       |                   - node-token
       v
    Node CR ──────────────> SSH into worker host
    (host, clusterRef)      K3S_URL=... K3S_TOKEN=... curl ... | sh -
```

Each machine's SSH target (host + port) is specified on the Cluster or Node
resource itself. The ProviderConfig only holds the SSH identity (username +
credentials), so a single ProviderConfig can be reused across multiple machines
that share the same SSH user and key.

## Acknowledgements

This provider is built on top of the patterns established by
[k3sup](https://github.com/alexellis/k3sup) by Alex Ellis. k3sup provides the
foundational approach of installing and joining k3s clusters over SSH, which this
provider adapts into the Crossplane reconciliation model.

## Developing

1. Run `make submodules` to initialize the "build" Make submodule.
2. Run `make generate` to run code generation (deepcopy, CRDs, methodsets).
3. Run `make build` to build the provider binary.
4. Run `make reviewable` to run linters and tests.

Refer to Crossplane's [CONTRIBUTING.md] file for more information on how the
Crossplane community prefers to work. The [Provider Development][provider-dev]
guide may also be of use.

[CONTRIBUTING.md]: https://github.com/crossplane/crossplane/blob/master/CONTRIBUTING.md
[provider-dev]: https://github.com/crossplane/crossplane/blob/master/contributing/guide-provider-development.md

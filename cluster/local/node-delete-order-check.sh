#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 The Crossplane Authors <https://crossplane.io>
#
# SPDX-License-Identifier: Apache-2.0
#
# Regression guard for the Node delete-wedge.
#
# `make e2e`'s own delete stage always removes every Node before its Cluster
# (examples/node/testhooks/delete-node-namespaced.sh is the Cluster's own
# pre-delete-hook), so that path structurally can never exercise the reverse
# order. This script does, directly against the kind cluster and
# k3s-nodepool fixture already left running by the `e2e` run that is this
# target's own prerequisite, so re-joining the already-provisioned host is
# fast rather than a full cold install.
#
# Scenario: apply the namespaced Node+Cluster pair (one manifest bundles
# both), wait for both Ready, delete the Cluster FIRST, then the Node, and
# confirm the Node clears its finalizer instead of hanging on a stale
# "cannot get referenced Cluster: ... not found" reconcile error.
set -euo pipefail

: "${KUBECTL:?KUBECTL must be set to the kubectl binary path}"

NS="crossplane-system"
# node-namespaced.yaml bundles its own Cluster prerequisite in the same file
# (two YAML documents) -- applying it alone creates both objects and cannot
# double-apply the Cluster the way a separate cluster-namespaced.yaml +
# node-namespaced.yaml pair would.
MANIFEST="examples/node/node-namespaced.yaml"
CLUSTER_RES="cluster.k3s.m.crossplane.io/my-k3s-cluster"
NODE_RES="node.k3s.m.crossplane.io/worker-1"

echo "--- node-delete-order-check: applying ${MANIFEST} ---"
${KUBECTL} apply -f "${MANIFEST}"

echo "--- node-delete-order-check: waiting for ${CLUSTER_RES} to become Ready ---"
${KUBECTL} -n "${NS}" wait --for=condition=Ready "${CLUSTER_RES}" --timeout=1800s

echo "--- node-delete-order-check: waiting for ${NODE_RES} to become Ready ---"
${KUBECTL} -n "${NS}" wait --for=condition=Ready "${NODE_RES}" --timeout=1800s

echo "--- node-delete-order-check: deleting ${CLUSTER_RES} and waiting for it to be FULLY gone ---"
# No --wait=false: a non-blocking delete only sets deletionTimestamp, and the
# Cluster object keeps satisfying a GET (with a deletionTimestamp, but still
# present) until its own finalizer clears. Node's Connect() would then still
# resolve it successfully -- a race that could pass this check on the exact
# defect it exists to catch. Blocking here is what guarantees Node's own
# delete, issued next, genuinely hits a Cluster that is gone from etcd.
if ! ${KUBECTL} -n "${NS}" delete "${CLUSTER_RES}" --timeout=300s; then
  echo "FAIL: ${CLUSTER_RES} did not itself clear within 300s -- not the scenario under test," >&2
  echo "but the Node delete below cannot be trusted without it. Dumping the stuck object:" >&2
  ${KUBECTL} -n "${NS}" get "${CLUSTER_RES}" -o yaml >&2 || true
  exit 1
fi

echo "--- node-delete-order-check: deleting ${NODE_RES} now that its Cluster is confirmed gone ---"
if ! ${KUBECTL} -n "${NS}" delete "${NODE_RES}" --timeout=300s; then
  echo "FAIL: ${NODE_RES} did not clear within 300s of its Cluster being deleted first." >&2
  echo "This is the Node delete-wedge regressing -- dumping the stuck object:" >&2
  ${KUBECTL} -n "${NS}" get "${NODE_RES}" -o yaml >&2 || true
  exit 1
fi

echo "--- node-delete-order-check: confirming no lingering finalizer on ${NODE_RES} ---"
if ${KUBECTL} -n "${NS}" get "${NODE_RES}" >/dev/null 2>&1; then
  echo "FAIL: ${NODE_RES} still exists after its own delete returned." >&2
  ${KUBECTL} -n "${NS}" get "${NODE_RES}" -o yaml >&2 || true
  exit 1
fi

echo "PASS: node-delete-order-check -- Node cleared its finalizer with its Cluster fully deleted first."

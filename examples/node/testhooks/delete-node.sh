#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 The Crossplane Authors <https://crossplane.io>
#
# SPDX-License-Identifier: Apache-2.0
set -aeuo pipefail

# Delete the Node resource before deleting the Cluster itself
${KUBECTL} delete nodes.k3s.crossplane.io --all

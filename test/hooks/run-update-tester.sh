#!/usr/bin/env bash
# Every test/hooks/post-assert-<resource>.sh is a symlink to this script; the
# invocation name ($0) selects the manifest. ROOT is absolute because `go -C`
# runs the tool with tools/update-tester as its working directory.
#
# --skip-converge drops both `converge` steps from this hook's own sequence.
# This provider observes convergence once, for the whole run, via a single
# shared barrier (test/hooks/converge-barrier.sh, wired in as uptest's
# --post-assert-script) instead of one window per resource here. The `run`
# (per-field update test) step still runs per-resource — it mutates state
# and cannot safely interleave across resources.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
exec go -C "$ROOT/tools/update-tester" tool crossplane-update-tester \
  hook "$(basename "$0")" --root "$ROOT" --skip-converge

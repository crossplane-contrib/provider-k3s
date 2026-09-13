#!/usr/bin/env bash
# converge-barrier.sh — one shared convergence window for the whole E2E run.
#
# Passed to uptest via --post-assert-script, so it runs ONCE, after every
# resource in the case has already passed its own per-resource assertions —
# not once per resource the way the individual post-assert-<resource>.sh
# hooks used to run their two `converge` steps. Convergence is a read-only
# check (kubectl reads only, no patches, no controller restarts), so nothing
# stops every resource in the case from being observed over the same stretch
# of wall-clock time instead of N sequential windows that each see a
# different stretch.
#
# The per-resource hooks (test/hooks/run-update-tester.sh) still run their
# own `run` (per-field update test) and, where applicable,
# `check-external-name-prefix` / `resolve-recover` steps — those mutate
# state and cannot safely interleave across resources. Only the two
# `converge` steps move here, via --skip-converge on the hook invocation.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# CASE_DIR must be worktree-unique (the Makefile sets it from
# KIND_CLUSTER_NAME, which is itself derived per worktree). There is no safe
# shared fallback: a fixed path would read another concurrent run's rendered
# manifests and report THEM stable — green, but for the wrong cluster. Fail
# closed instead.
: "${CASE_DIR:?converge-barrier: CASE_DIR is unset — refusing to fall back to a shared path}"

INPUT="$CASE_DIR/test-input.yaml"
if [ ! -f "$INPUT" ]; then
  echo "converge-barrier: FATAL no rendered manifests at $INPUT" >&2
  exit 1
fi

# UPDATE_TESTER_POLL_INTERVAL is exported unconditionally by the Makefile
# (derived from E2E_POLL_INTERVAL, default 10s) for every e2e invocation, so
# this fallback only matters for a direct manual invocation outside `make`.
POLL="${UPDATE_TESTER_POLL_INTERVAL:-10s}"

# No re-default here: the Makefile is the one place a fleet-wide exclusion
# default would be spelled, and this provider never sets
# UPDATE_TESTER_IGNORE_FIELDS at all — most e2e.<resource> targets need no
# exclusion. Read whatever the Makefile set, empty or not — converge-all's
# own --ignore-fields default is the empty string, so an unset variable here
# means exactly what it means to the per-resource converge path this
# replaces: no exclusions.
IGNORE="${UPDATE_TESTER_IGNORE_FIELDS:-}"

# --timeout / --readiness-timeout are derived from UPDATE_TESTER_TIMEOUT —
# the same variable the per-resource hook path reads (main.go's envTimeout)
# — rather than a literal, so the poll-interval/timeout pairing a raised
# E2E_POLL_INTERVAL relies on is not silently defeated by a second,
# unrelated hardcoded value here. Unset falls back to 120, matching
# converge-all's own documented default for both flags. This provider's
# slowest single object (a from-scratch k3s server install over SSH) already
# gets its own budget from UPDATE_TESTER_TIMEOUT on the per-resource hook
# path; convergence itself is a read-only steady-state check and needs no
# larger a window than that.
TIMEOUT_SECONDS="${UPDATE_TESTER_TIMEOUT:-120}"

# Split CASE_DIR's rendered test-input.yaml into one file per managed
# resource. Written OUTSIDE the repo tree via mktemp, and cleaned up on exit
# — the repo checkout is a shared worktree and must not be dirtied mid-run.
SPLIT="$(mktemp -d "${TMPDIR:-/tmp}/converge-barrier.XXXXXX")" || {
  echo "converge-barrier: FATAL could not create a scratch directory" >&2
  exit 1
}
trap 'rm -rf "$SPLIT"' EXIT

python3 - "$INPUT" "$SPLIT" <<'PY'
import sys, yaml, pathlib
src, out = sys.argv[1], pathlib.Path(sys.argv[2])
skip = {"Secret", "ProviderConfig", "ClusterProviderConfig"}
n = 0
for doc in yaml.safe_load_all(open(src)):
    if not doc or doc.get("kind") in skip:
        continue
    n += 1
    (out / f"{n:02d}-{doc['kind']}-{doc['metadata']['name']}.yaml").write_text(yaml.safe_dump(doc))
print(f"converge-barrier: {n} managed resource(s) from test-input.yaml", file=sys.stderr)
PY
split_status=$?
if [ "$split_status" -ne 0 ]; then
  echo "converge-barrier: FATAL failed to split $INPUT into per-resource manifests" >&2
  exit 1
fi

shopt -s nullglob
manifests=("$SPLIT"/*.yaml)
shopt -u nullglob
if [ "${#manifests[@]}" -eq 0 ]; then
  echo "converge-barrier: FATAL zero managed resources parsed from $INPUT" >&2
  exit 1
fi

MANIFESTS=$(IFS=,; echo "${manifests[*]}")

echo "converge-barrier: observing ${#manifests[@]} manifest(s) over one shared window (poll-interval=$POLL, timeout=${TIMEOUT_SECONDS}s, ignore-fields=${IGNORE:-<none>})"

# Not `exec`: exec replaces this process image without running the EXIT
# trap above, which would leave $SPLIT behind on every invocation. Run
# normally and propagate the tool's own exit code instead.
#
# UPDATE_TESTER_ROOT points converge-all at this provider repository root
# (the directory holding package/crds/) so that, for any target whose
# verdict comes back FAILING, the advisory spec.forProvider <->
# status.atProvider round-trip report is inlined immediately under that
# target's own verdict line instead of requiring a second, separate
# invocation.
converge_status=0
UPDATE_TESTER_ROOT="$ROOT" \
  go -C "$ROOT/tools/update-tester" tool crossplane-update-tester converge-all "$MANIFESTS" \
    --poll-interval "$POLL" \
    --timeout "${TIMEOUT_SECONDS}s" \
    --readiness-timeout "${TIMEOUT_SECONDS}s" \
    --concurrency 8 \
    --ignore-fields "$IGNORE" || converge_status=$?

exit "$converge_status"

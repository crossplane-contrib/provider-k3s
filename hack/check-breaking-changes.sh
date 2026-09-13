#!/usr/bin/env bash
# hack/check-breaking-changes.sh — fail a pull request that reshapes a served
# CRD schema under its existing consumers.
#
# `make check-diff` already proves the committed CRDs match what the generator
# produces. That is consistency, not compatibility: a regeneration can be
# perfectly faithful and still delete a field somebody has applied. This script
# is the other half — it compares each changed CRD against the same file on the
# base branch and reports what a consumer would lose.
#
# Before the first release there is nothing to protect: a repository that has
# published no release has made no promise about any schema, so reshaping one
# breaks nobody. That state is declared in RELEASES.md, where consumers read it,
# and it ends the moment the first release ships. See "the prerelease
# declaration" below.
#
# Environment:
#   BASE_REF          branch to compare against (default: origin/main)
#   CRDDIFF_VERSION   crddiff module version (default: the pin below)
#   CRD_DIR           directory of generated CRDs (default: package/crds)
#   RELEASES_FILE     published release policy (default: RELEASES.md)
#   GITHUB_STEP_SUMMARY  optional; findings are appended when set
#
# No environment variable turns the gate off. The prerelease exemption is read
# from the published policy and from nowhere else, because an exemption a caller
# can pass on the command line is one an automated caller can pass by accident.
#
# Exit 0  no breaking change (or none of the changed CRDs was comparable for a
#         reason this script names explicitly, or every finding is exempt under
#         the prerelease declaration)
# Exit 1  at least one breaking change
# Exit 2  the check could not run — a missing base ref, an unresolvable CRD, a
#         crddiff failure that is not a schema finding. Never a silent pass:
#         a gate that cannot see is not a gate that approves.

set -euo pipefail

# The crddiff pin lives here and nowhere else. This script is fleet-copied
# verbatim, so a single definition cannot drift per provider and needs no
# parity check to police it — and `make`, which is where the gate actually
# runs, reads no workflow environment.
#
# Pinned to a commit rather than a tag: the newest tag of the upstream module
# is several years behind its main branch. The module path is
# github.com/upbound/uptest — the repository was renamed, the module path was
# not.
CRDDIFF_DEFAULT=v0.12.1-0.20260728095952-c230a8044006

BASE_REF="${BASE_REF:-origin/main}"
CRDDIFF_VERSION="${CRDDIFF_VERSION-$CRDDIFF_DEFAULT}"
CRD_DIR="${CRD_DIR:-package/crds}"
RELEASES_FILE="${RELEASES_FILE:-RELEASES.md}"

# ── The prerelease declaration ──────────────────────────────────────────────
#
# The rule this gate enforces protects consumers of a shipped schema. Until a
# repository publishes its first release it has no such consumers, and the
# guarantee it would be enforcing has not begun. A repository in that state says
# so in RELEASES.md, between these two markers, so that anyone who finds it
# before the first release reads the same fact the gate reads.
#
# Three properties keep this from becoming a way to switch the gate off:
#
#   - It is not silent. Every finding is still compared, still printed in full,
#     and still counted into `breaking=`. Only the exit status differs, and the
#     line that changes it says so.
#   - It self-revokes. The declaration is removed at the first release, and from
#     then on the gate is absolute with no further decision to make.
#   - It fails closed. A repository with no RELEASES.md, or with only one of the
#     two markers, is treated as published and gated strictly. Absence never
#     grants the exemption.
PRERELEASE=0
if [ -f "$RELEASES_FILE" ] &&
   grep -qx '<!-- prerelease -->' "$RELEASES_FILE" &&
   grep -qx '<!-- end-prerelease -->' "$RELEASES_FILE"; then
  PRERELEASE=1
fi

[ -n "$BASE_REF" ] || { echo "ERROR: BASE_REF is empty — cannot determine what to compare against"; exit 2; }
[ -n "$CRDDIFF_VERSION" ] || { echo "ERROR: CRDDIFF_VERSION is empty — refusing to run an unpinned tool"; exit 2; }

# Resolve the base to something local. A shallow checkout has no branch ref for
# the base, and the reference implementation this replaces treated that absence
# as "nothing to compare" — printing success having compared nothing. Resolve it
# or fail; those are the only two outcomes.
BASE=""
for cand in "refs/remotes/origin/${BASE_REF}" "origin/${BASE_REF}" "${BASE_REF}"; do
  if git rev-parse --verify --quiet "${cand}^{commit}" >/dev/null; then BASE="$cand"; break; fi
done
if [ -z "$BASE" ]; then
  echo "ERROR: no local ref resolves '${BASE_REF}' — fetch the base branch before running this"
  exit 2
fi

mapfile -t CHANGED < <(git diff --name-only "$BASE" -- "$CRD_DIR" | grep -E '\.ya?ml$' || true)

if [ "${#CHANGED[@]}" -eq 0 ]; then
  echo "no CRD changes against ${BASE_REF} — nothing to compare"
  echo "crds_changed=0 crds_compared=0 breaking=0"
  exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# served <file> — the API versions a CRD serves.
#
# controller-gen emits spec.versions as a list whose item keys sit at four
# spaces, so a four-space `name:` inside that block is a version and the
# `name:` values nested under additionalPrinterColumns (six spaces) are not.
# Matching `name:` at any depth would read printer-column names as API
# versions.
served() {
  python3 -c '
import re, sys
inv = False
out = []
for line in open(sys.argv[1], errors="ignore"):
    line = line.rstrip("\n")
    if re.match(r"^  versions:\s*$", line):
        inv = True
        continue
    if inv:
        if line.strip() and not re.match(r"^ {2}- |^ {4}", line):
            inv = False
            continue
        m = re.match(r"^ {4}name: (\S+)$", line)
        if m:
            out.append(m.group(1))
print("\n".join(out))
' "$1"
}

changed=0 compared=0 skipped_new=0 removed=0 breaking=0
FINDINGS="$WORK/findings.md"
: > "$FINDINGS"

# resolve_crddiff — installs the pinned crddiff binary the first time a
# comparison actually needs it, and caches the path for the rest of the run.
#
# `go run pkg@version` conflates two failure modes under one exit code: the
# module could not be resolved/built, or the tool ran and found a breaking
# change. Both surface as exit 1, so a proxy hiccup was reported as a
# fabricated BREAKING finding against a named CRD. `go install pkg@version`
# performs the same module-aware, no-go.mod-required resolution but never
# executes the tool, so a resolution failure can only reach this function's
# own exit-2 path and a later exit 1 from the installed binary can only mean
# a schema finding.
CRDDIFF_BIN=""
resolve_crddiff() {
  [ -n "$CRDDIFF_BIN" ] && return 0
  local bindir="$WORK/bin"
  mkdir -p "$bindir"
  if ! GOBIN="$bindir" go install "github.com/upbound/uptest/cmd/crddiff@${CRDDIFF_VERSION}" >"$WORK/crddiff-install.log" 2>&1; then
    echo "ERROR: could not resolve crddiff ${CRDDIFF_VERSION} — this is a tool failure, not a schema verdict"
    cat "$WORK/crddiff-install.log"
    exit 2
  fi
  CRDDIFF_BIN="$bindir/crddiff"
}

for crd in "${CHANGED[@]}"; do
  changed=$((changed + 1))

  if ! git cat-file -e "${BASE}:${crd}" 2>/dev/null; then
    echo "NEW: ${crd} does not exist on ${BASE_REF} — adding a CRD breaks no consumer"
    skipped_new=$((skipped_new + 1))
    continue
  fi

  if [ ! -f "$crd" ]; then
    # Deleting a whole CRD is a breaking change, and crddiff has nothing to
    # compare. Report it here rather than letting the file vanish from the
    # count.
    echo "BREAKING: ${crd} is served on ${BASE_REF} and deleted here — the kind disappears from the API"
    printf -- '- **%s** — CRD deleted; the kind disappears from the API\n' "$crd" >> "$FINDINGS"
    breaking=$((breaking + 1))
    continue
  fi

  base_file="$WORK/base.yaml"
  git cat-file -p "${BASE}:${crd}" > "$base_file"

  base_versions="$(served "$base_file")"
  head_versions="$(served "$crd")"
  dropped="$(comm -23 <(echo "$base_versions" | sort -u) <(echo "$head_versions" | sort -u) | tr -d ' ')"

  if [ -n "$dropped" ]; then
    # The sanctioned move for an incompatible change is to
    # drop the version and serve its successor, not to reshape the version in
    # place. crddiff cannot compare a version that is gone, and it should not
    # have to — removal is the remedy this gate exists to steer people toward.
    echo "VERSION-REMOVED: ${crd} no longer serves $(echo "$dropped" | tr '\n' ' ')— replacement, not mutation; permitted"
    removed=$((removed + 1))
    continue
  fi

  resolve_crddiff

  set +e
  out="$("$CRDDIFF_BIN" revision "$base_file" "$crd" 2>&1)"
  rc=$?
  set -e
  compared=$((compared + 1))

  case "$rc" in
    0)
      echo "OK: ${crd}"
      ;;
    1)
      echo "BREAKING: ${crd}"
      echo "$out"
      breaking=$((breaking + 1))
      {
        printf -- '- **%s**\n\n  ```\n%s\n  ```\n\n' "$crd" "$(echo "$out" | sed 's/^/  /')"
      } >> "$FINDINGS"
      ;;
    *)
      echo "ERROR: crddiff exited ${rc} on ${crd} — this is a tool failure, not a verdict"
      echo "$out"
      exit 2
      ;;
  esac
done

# A job that compared nothing while CRDs changed is indistinguishable from a
# passing one in the CI summary, which is how a dead gate survives for months.
if [ "$compared" -eq 0 ] && [ "$breaking" -eq 0 ] && [ "$removed" -eq 0 ] && [ "$skipped_new" -ne "$changed" ]; then
  echo "ERROR: ${changed} CRD(s) changed and none could be compared — the check did not run"
  exit 2
fi

echo "crds_changed=${changed} crds_compared=${compared} new=${skipped_new} version_removed=${removed} breaking=${breaking} prerelease=${PRERELEASE}"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### Breaking CRD schema changes"
    echo
    echo "Compared ${compared} of ${changed} changed CRD(s) against \`${BASE_REF}\`."
    echo
    if [ "$breaking" -gt 0 ]; then
      echo "**${breaking} breaking change(s):**"
      echo
      cat "$FINDINGS"
      echo
      if [ "$PRERELEASE" -eq 1 ]; then
        echo "Permitted: ${RELEASES_FILE} declares that this repository has published"
        echo "no release, so no consumer relies on the schema being reshaped. Every"
        echo "finding above becomes a blocking failure once that declaration is"
        echo "removed at the first release."
      else
        echo "An API element cannot be removed from a version, or have its behaviour"
        echo "significantly changed, once shipped — on any track, alpha included."
        echo "Serve the change under a new version instead of reshaping this one."
      fi
    else
      echo "No breaking changes."
    fi
  } >> "$GITHUB_STEP_SUMMARY"
fi

if [ "$breaking" -gt 0 ] && [ "$PRERELEASE" -eq 1 ]; then
  echo "PRERELEASE-EXEMPT: ${breaking} breaking change(s) permitted — ${RELEASES_FILE} declares this repository has published no release"
  echo "  Each finding above becomes a blocking failure when that declaration is removed at the first release."
  exit 0
fi

[ "$breaking" -eq 0 ]

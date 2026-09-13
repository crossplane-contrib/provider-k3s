#!/usr/bin/env bash
# hack/check-breaking-changes-test.sh — offline regression harness for
# hack/check-breaking-changes.sh.
#
# Builds a throwaway git repository per case, so the script under test sees a
# real base ref and real blobs rather than a mocked git. crddiff itself is NOT
# stubbed: the cases that matter are the ones where its verdict and this
# script's interpretation of that verdict have to agree, and a stub would
# assert only that the stub was called.
#
#   hack/check-breaking-changes-test.sh        # exit 0 = all pass

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNDER_TEST="${SCRIPT_DIR}/check-breaking-changes.sh"
CRDDIFF_VERSION="${CRDDIFF_VERSION:-v0.12.1-0.20260728095952-c230a8044006}"

[ -x "$UNDER_TEST" ] || { echo "FATAL: $UNDER_TEST not executable"; exit 1; }

PASS=0; FAIL=0
SANDBOX="$(mktemp -d)"
trap 'rm -rf "$SANDBOX"' EXIT

# A minimal but structurally real CRD: spec.versions is a list, the version
# name sits at four spaces, and additionalPrinterColumns carries a `name:` at
# six spaces that must NOT be read as an API version.
base_crd() {
  cat <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.crossplane.io
spec:
  group: example.crossplane.io
  names:
    kind: Widget
    listKind: WidgetList
    plural: widgets
    singular: widget
  scope: Cluster
  versions:
  - additionalPrinterColumns:
    - jsonPath: .status.conditions[?(@.type=='Ready')].status
      name: READY
      type: string
    name: v1alpha1
    schema:
      openAPIV3Schema:
        properties:
          spec:
            properties:
              forProvider:
                properties:
                  region:
                    type: string
                  size:
                    type: string
                required:
                - region
                type: object
            type: object
        type: object
    served: true
    storage: true
YAML
}

# new_repo <dir> [base-mutator] — a repo whose 'main' holds the base CRD,
# checked out on a working branch ready to receive the revision. The optional
# base mutator runs before the base commit, so a case can set up a base the
# revision then diverges from.
new_repo() {
  local d="$1" base_mutate="${2:-}"
  mkdir -p "$d/package/crds"
  git -C "$d" init -q -b main
  git -C "$d" config user.email t@example.com
  git -C "$d" config user.name test
  base_crd > "$d/package/crds/example.crossplane.io_widgets.yaml"
  [ -z "$base_mutate" ] || ( cd "$d" && "$base_mutate" )
  git -C "$d" add -A
  git -C "$d" commit -qm base
  git -C "$d" checkout -q -b pr
}

# run_case <name> <expected-exit> <expect-substring> <mutator-fn> [base-mutator]
run_case() {
  local name="$1" want_rc="$2" want_txt="$3" mutate="$4" base_mutate="${5:-}"
  local d="$SANDBOX/$name"
  new_repo "$d" "$base_mutate"
  ( cd "$d" && "$mutate" )
  git -C "$d" add -A >/dev/null 2>&1
  git -C "$d" -c user.email=t@example.com -c user.name=test commit -qm revision >/dev/null 2>&1 || true

  local out rc
  out="$(cd "$d" && BASE_REF=main CRDDIFF_VERSION="$CRDDIFF_VERSION" \
          bash "$UNDER_TEST" 2>&1)"
  rc=$?

  local ok=1
  [ "$rc" = "$want_rc" ] || ok=0
  [ -z "$want_txt" ] || grep -q -- "$want_txt" <<<"$out" || ok=0

  if [ "$ok" = 1 ]; then
    echo "PASS: $name (exit $rc)"
    PASS=$((PASS+1))
  else
    echo "FAIL: $name — wanted exit $want_rc containing '$want_txt', got exit $rc:"
    sed 's/^/      /' <<<"$out"
    FAIL=$((FAIL+1))
  fi
}

CRD=package/crds/example.crossplane.io_widgets.yaml

m_noop()      { :; }
m_comment()   { printf '  # trailing comment\n' >> "$CRD"; }
m_add_opt()   { perl -0pi -e 's/                required:\n/                  color:\n                    type: string\n                required:\n/' "$CRD"; }
m_del_prop()  { perl -0pi -e 's/                  size:\n                    type: string\n//' "$CRD"; }
m_retype()    { perl -0pi -e 's/(                  size:\n                    type: )string/${1}integer/' "$CRD"; }
m_newreq()    { perl -0pi -e 's/                - region\n/                - region\n                - size\n/' "$CRD"; }
m_rotate()    { perl -0pi -e 's/    name: v1alpha1\n/    name: v1alpha2\n/' "$CRD"; }
m_delete()    { rm -f "$CRD"; }
m_newcrd()    { base_crd | sed 's/widgets/gadgets/g; s/Widget/Gadget/g; s/widget/gadget/g' \
                  > package/crds/example.crossplane.io_gadgets.yaml; }
# The printer-column trap: the BASE carries a printer column whose name is
# version-shaped, and the revision renames it while also deleting a property.
# A parser that matched `name:` at any depth would read v1beta9 as an API
# version, see it disappear, take the version-removal exemption and pass a
# real breaking change. The correct reading is four-space `name:` only.
m_colname()   { perl -0pi -e "s/      name: READY/      name: v1beta9/" "$CRD"; }
m_coltrap()   { perl -0pi -e "s/      name: v1beta9/      name: READY/" "$CRD"; m_del_prop; }

echo "crddiff pin under test: $CRDDIFF_VERSION"
echo

run_case no-crd-change        0 "crds_changed=0"      m_noop
run_case comment-only         0 "OK:"                 m_comment
run_case optional-field-added 0 "OK:"                 m_add_opt
run_case property-deleted     1 "BREAKING:"           m_del_prop
run_case type-changed         1 "BREAKING:"           m_retype
run_case optional-now-required 1 "BREAKING:"          m_newreq
run_case version-rotated      0 "VERSION-REMOVED:"    m_rotate
run_case crd-deleted          1 "kind disappears"     m_delete
run_case crd-added            0 "NEW:"                m_newcrd
run_case printer-column-trap  1 "BREAKING:"           m_coltrap            m_colname

# The default pin: with CRDDIFF_VERSION unset the script must still run, using
# CRDDIFF_DEFAULT. This is the path `make check-breaking-changes` takes, since
# make reads no workflow environment.
d="$SANDBOX/default-pin"; new_repo "$d"
( cd "$d" && m_del_prop ) 2>/dev/null || perl -0pi -e 's/                  size:\n                    type: string\n//' "$d/$CRD"
out="$(cd "$d" && BASE_REF=main bash "$UNDER_TEST" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -q "BREAKING:" <<<"$out"; then
  echo "PASS: default-pin-used (exit 1)"; PASS=$((PASS+1))
else
  echo "FAIL: default-pin-used — wanted exit 1 with BREAKING, got $rc:"; sed 's/^/      /' <<<"$out"; FAIL=$((FAIL+1))
fi

# The make entry point, which is what `reviewable` reaches. Proves the target
# forwards BASE_REF and propagates the exit status.
d="$SANDBOX/via-make"; new_repo "$d"
perl -0pi -e 's/                  size:\n                    type: string\n//' "$d/$CRD"
cat >> "$d/Makefile" <<'MK'
BASE_REF ?= origin/main

check-breaking-changes:
	@BASE_REF=$(BASE_REF) ./hack/check-breaking-changes.sh

.PHONY: check-breaking-changes
MK
mkdir -p "$d/hack" && cp "$UNDER_TEST" "$d/hack/check-breaking-changes.sh"
chmod +x "$d/hack/check-breaking-changes.sh"
out="$(cd "$d" && make check-breaking-changes BASE_REF=main 2>&1)"; rc=$?
if [ "$rc" = 2 ] && grep -q "BREAKING:" <<<"$out"; then
  echo "PASS: via-make (exit 2 — make wraps the script's 1)"; PASS=$((PASS+1))
elif [ "$rc" != 0 ] && grep -q "BREAKING:" <<<"$out"; then
  echo "PASS: via-make (exit $rc, non-zero, BREAKING reported)"; PASS=$((PASS+1))
else
  echo "FAIL: via-make — wanted non-zero with BREAKING, got $rc:"; sed 's/^/      /' <<<"$out"; FAIL=$((FAIL+1))
fi

# Preconditions are errors, never quiet passes.
d="$SANDBOX/no-base-ref"; new_repo "$d"
out="$(cd "$d" && BASE_REF=nonexistent CRDDIFF_VERSION="$CRDDIFF_VERSION" bash "$UNDER_TEST" 2>&1)"; rc=$?
if [ "$rc" = 2 ] && grep -q "no local ref resolves" <<<"$out"; then
  echo "PASS: unresolvable-base-ref (exit 2)"; PASS=$((PASS+1))
else
  echo "FAIL: unresolvable-base-ref — wanted exit 2, got $rc: $out"; FAIL=$((FAIL+1))
fi

d="$SANDBOX/no-pin"; new_repo "$d"
out="$(cd "$d" && BASE_REF=main CRDDIFF_VERSION='' bash "$UNDER_TEST" 2>&1)"; rc=$?
if [ "$rc" = 2 ] && grep -q "refusing to run an unpinned tool" <<<"$out"; then
  echo "PASS: unpinned-tool-refused (exit 2)"; PASS=$((PASS+1))
else
  echo "FAIL: unpinned-tool-refused — wanted exit 2, got $rc: $out"; FAIL=$((FAIL+1))
fi

# A pin that resolves to nothing (garbage revision) fails at `go install`
# time, after the script has already invoked the tool-resolution path — not
# a schema comparison. Before the fix this was indistinguishable from
# crddiff's own exit 1 for "breaking change found"; it must exit 2, name no
# CRD as BREAKING, and never appear in a breaking= count.
d="$SANDBOX/crddiff-unresolvable"; new_repo "$d"
( cd "$d" && m_del_prop )
git -C "$d" add -A >/dev/null 2>&1
git -C "$d" -c user.email=t@example.com -c user.name=test commit -qm revision >/dev/null 2>&1 || true
out="$(cd "$d" && BASE_REF=main CRDDIFF_VERSION='v0.0.0-garbage-nonexistent-000000000000' bash "$UNDER_TEST" 2>&1)"; rc=$?
if [ "$rc" = 2 ] && grep -q "could not resolve crddiff" <<<"$out" && ! grep -q "^BREAKING:" <<<"$out" && ! grep -q "breaking=1" <<<"$out"; then
  echo "PASS: crddiff-unresolvable (exit 2)"; PASS=$((PASS+1))
else
  echo "FAIL: crddiff-unresolvable — wanted exit 2 with no BREAKING verdict, got $rc:"
  sed 's/^/      /' <<<"$out"
  FAIL=$((FAIL+1))
fi

# ── The prerelease declaration ──────────────────────────────────────────────
#
# A repository that has published no release says so in RELEASES.md, and a
# breaking finding there is reported in full but does not block. The cases
# below pin the three properties that keep this from being a way to switch the
# gate off: the finding is still printed and still counted; a partial or absent
# declaration fails closed; and no environment variable can forge it.

# prerelease_releases_md <dir> <open-marker?> <close-marker?>
write_releases() {
  local d="$1" open="$2" close="$3"
  {
    echo "# Releases and Support"
    echo
    [ "$open" = yes ] && echo '<!-- prerelease -->'
    [ "$open" = yes ] && echo '**This repository has published no release.**'
    [ "$close" = yes ] && echo '<!-- end-prerelease -->'
    echo
    echo "Body."
  } > "$d/RELEASES.md"
}

# prerelease_case <name> <want-rc> <open> <close> <want-txt> [reject-txt]
prerelease_case() {
  local name="$1" want_rc="$2" open="$3" close="$4" want_txt="$5" reject_txt="${6:-}"
  local d="$SANDBOX/$name"
  new_repo "$d"
  write_releases "$d" "$open" "$close"
  ( cd "$d" && m_del_prop )
  git -C "$d" add -A >/dev/null 2>&1
  git -C "$d" -c user.email=t@example.com -c user.name=test commit -qm revision >/dev/null 2>&1 || true

  local out rc ok=1
  out="$(cd "$d" && BASE_REF=main CRDDIFF_VERSION="$CRDDIFF_VERSION" bash "$UNDER_TEST" 2>&1)"; rc=$?
  [ "$rc" = "$want_rc" ] || ok=0
  [ -z "$want_txt" ] || grep -q -- "$want_txt" <<<"$out" || ok=0
  # The finding itself is never suppressed, whatever the verdict.
  grep -q "^BREAKING:" <<<"$out" || ok=0
  grep -q "breaking=1" <<<"$out" || ok=0
  [ -z "$reject_txt" ] || ! grep -q -- "$reject_txt" <<<"$out" || ok=0

  if [ "$ok" = 1 ]; then
    echo "PASS: $name (exit $rc)"; PASS=$((PASS+1))
  else
    echo "FAIL: $name — wanted exit $want_rc containing '$want_txt', got exit $rc:"
    sed 's/^/      /' <<<"$out"; FAIL=$((FAIL+1))
  fi
}

prerelease_case prerelease-exempt        0 yes yes "PRERELEASE-EXEMPT:"
prerelease_case prerelease-declared-absent 1 no  no  "breaking=1" "PRERELEASE-EXEMPT:"
prerelease_case prerelease-open-marker-only 1 yes no "breaking=1" "PRERELEASE-EXEMPT:"
prerelease_case prerelease-close-marker-only 1 no yes "breaking=1" "PRERELEASE-EXEMPT:"

# No RELEASES.md at all is the same fail-closed answer as a file without the
# declaration — absence never grants the exemption.
d="$SANDBOX/prerelease-no-releases-file"; new_repo "$d"
( cd "$d" && m_del_prop )
git -C "$d" add -A >/dev/null 2>&1
git -C "$d" -c user.email=t@example.com -c user.name=test commit -qm revision >/dev/null 2>&1 || true
out="$(cd "$d" && BASE_REF=main CRDDIFF_VERSION="$CRDDIFF_VERSION" bash "$UNDER_TEST" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && grep -q "prerelease=0" <<<"$out" && ! grep -q "PRERELEASE-EXEMPT:" <<<"$out"; then
  echo "PASS: prerelease-no-releases-file (exit 1)"; PASS=$((PASS+1))
else
  echo "FAIL: prerelease-no-releases-file — wanted exit 1, no exemption, got $rc:"
  sed 's/^/      /' <<<"$out"; FAIL=$((FAIL+1))
fi

# The exemption is readable from the published policy and nowhere else. An
# exported PRERELEASE must not forge it — an exemption a caller can pass on the
# command line is one an automated caller can pass by accident.
d="$SANDBOX/prerelease-env-cannot-forge"; new_repo "$d"
( cd "$d" && m_del_prop )
git -C "$d" add -A >/dev/null 2>&1
git -C "$d" -c user.email=t@example.com -c user.name=test commit -qm revision >/dev/null 2>&1 || true
out="$(cd "$d" && BASE_REF=main CRDDIFF_VERSION="$CRDDIFF_VERSION" PRERELEASE=1 bash "$UNDER_TEST" 2>&1)"; rc=$?
if [ "$rc" = 1 ] && ! grep -q "PRERELEASE-EXEMPT:" <<<"$out"; then
  echo "PASS: prerelease-env-cannot-forge (exit 1)"; PASS=$((PASS+1))
else
  echo "FAIL: prerelease-env-cannot-forge — wanted exit 1 with no exemption, got $rc:"
  sed 's/^/      /' <<<"$out"; FAIL=$((FAIL+1))
fi

echo
echo "check-breaking-changes-test: ${PASS} passed, ${FAIL} failed"
[ "$FAIL" -eq 0 ]

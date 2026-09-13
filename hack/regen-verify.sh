#!/usr/bin/env bash
# hack/regen-verify.sh — verify that every committed generated file is still
# exactly what the generator pipeline (controller-gen + angryjet, invoked
# from apis/generate.go) would emit today.
#
# This provider has no separate catalog/type generator: every *_types.go
# under apis/ is hand-written. `apis/generate.go` runs three commands —
# remove package/crds, controller-gen's object+crd generators, and
# angryjet's generate-methodsets — over those hand-written types to produce
# the derived artifacts: zz_generated.deepcopy.go,
# zz_generated.{managed,managedlist,pc,pcu,pculist,resolvers}.go, and the
# CRD YAML under package/crds/. `make generate` runs this same pipeline
# in-place, so it DOES detect drift when run directly — but only when
# someone remembers to run it and check `git diff`. A hand-normalised
# generated file (goimports stripping an empty `import ()` block, an
# angryjet output hand-edited after the fact) leaves the working tree
# looking clean without this adapter to catch it in CI/review.
#
# Method: because neither controller-gen's object/crd generators nor
# angryjet's generate-methodsets deduplicate against pre-existing output
# (unlike the openapi2crd catalog generator other providers use, which
# scans its own output directory for declarations already emitted by
# sibling resources), there is no batch-removal hazard here. The whole
# pipeline is regenerated in one pass, in a scratch copy, and every
# generated file is diffed against the committed one. Never mutates the
# real working tree.
#
# Usage: hack/regen-verify.sh
# Run from anywhere; the script locates the provider root itself.
# Exit 0: every committed generated file reproduces byte-identically.
# Exit non-zero: prints one "DIVERGENT: <path>" line per file whose
# regeneration differs from what is committed (or exit 3 for a setup
# problem: missing apis/, missing go.mod, or the generator pipeline itself
# failing to run).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

[ -d apis ] || {
  echo "regen-verify: no apis/ under $ROOT — this script must live at hack/ under the provider root" >&2
  exit 3
}
[ -f go.mod ] || {
  echo "regen-verify: no go.mod under $ROOT — this script must live at hack/ under the provider root" >&2
  exit 3
}
[ -f hack/boilerplate.go.txt ] || {
  echo "regen-verify: no hack/boilerplate.go.txt under $ROOT — apis/generate.go's directives need it" >&2
  exit 3
}

# Discover every committed generated file up front, from the tree under
# test — never hardcode the list, so a resolver file that appears later
# (angryjet only emits zz_generated.resolvers.go for a package that
# actually has reference fields) is picked up automatically.
mapfile -t GO_TARGETS < <(find apis -name 'zz_generated.*.go' | sort)
mapfile -t CRD_TARGETS < <(find package/crds -name '*.yaml' | sort)

if [ "${#GO_TARGETS[@]}" -eq 0 ]; then
  echo "regen-verify: found no apis/**/zz_generated.*.go — generated-file discovery is broken" >&2
  exit 3
fi
if [ "${#CRD_TARGETS[@]}" -eq 0 ]; then
  echo "regen-verify: found no package/crds/*.yaml — CRD discovery is broken" >&2
  exit 3
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cp -a apis "$WORK/apis"
cp -a hack "$WORK/hack"
cp -a package "$WORK/package"
cp go.mod go.sum "$WORK/"

# Run the exact pipeline apis/generate.go declares (rm package/crds,
# controller-gen object+crd, angryjet generate-methodsets), from the
# scratch copy's apis/ directory so its relative "../package/crds" and
# "../hack/boilerplate.go.txt" paths resolve inside $WORK, never inside
# the real tree.
if ! ( cd "$WORK/apis" && go generate ./... ) >"$WORK/gen.log" 2>&1; then
  echo "regen-verify: generator pipeline failed on a fresh scratch copy" >&2
  cat "$WORK/gen.log" >&2
  exit 3
fi

FAILED=()

for f in "${GO_TARGETS[@]}"; do
  if ! diff -q "$f" "$WORK/$f" >/dev/null 2>&1; then
    FAILED+=("$f")
  fi
done

for f in "${CRD_TARGETS[@]}"; do
  if ! diff -q "$f" "$WORK/$f" >/dev/null 2>&1; then
    FAILED+=("$f")
  fi
done

# Catch files the regeneration added or removed outright (a resolver file
# that should now exist, or shouldn't anymore), not only content drift on
# files present on both sides.
mapfile -t WORK_GO_TARGETS < <(cd "$WORK" && find apis -name 'zz_generated.*.go' | sort)
if [ "${#WORK_GO_TARGETS[@]}" -ne "${#GO_TARGETS[@]}" ]; then
  comm -3 <(printf '%s\n' "${GO_TARGETS[@]}") <(printf '%s\n' "${WORK_GO_TARGETS[@]}") | while read -r extra; do
    echo "DIVERGENT (set mismatch): $extra" >&2
  done
  FAILED+=("(generated-file set changed — see above)")
fi

CHECKED=$(( ${#GO_TARGETS[@]} + ${#CRD_TARGETS[@]} ))

if [ "${#FAILED[@]}" -gt 0 ]; then
  echo "regen-verify: ${#FAILED[@]}/${CHECKED} generated file(s) diverged from a fresh isolated regeneration:"
  for r in "${FAILED[@]}"; do
    echo "DIVERGENT: $r"
  done
  exit 1
fi

echo "regen-verify: ${CHECKED}/${CHECKED} generated files regenerate byte-identically (deepcopy, methodsets, resolvers, CRDs)"

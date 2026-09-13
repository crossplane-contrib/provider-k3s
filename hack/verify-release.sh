#!/usr/bin/env bash
# hack/verify-release.sh — verify the full supply chain security ladder for a
# published release: image signature, both SBOM attestations, both VEX
# attestations, SLSA provenance, and the vens risk-scoring context
# attestation. Exits non-zero on any
# failure, including a cross-check that every attestation's subject digest
# equals the digest the release tag currently resolves to.
#
# Usage: hack/verify-release.sh <version>     (e.g. v0.1.0-rc2)
#
# Requires: cosign >= the version pinned by this repo's own
# .github/workflows/release.yaml (sigstore/cosign-installer step), crane
# (go-containerregistry), jq. Run from the repository root. Deliberately
# avoids GNU-only flags (grep -P, sort -V) and a standalone base64 binary
# (jq's @base64d builtin decodes instead) so this script also runs on
# BSD/macOS userland and under busybox, not just glibc/coreutils Linux --
# this is a customer-facing script, run on whatever the customer has.
#
# UP_ORG and PROJECT_NAME are read from THIS repo's own Makefile, never
# hardcoded — go.mod's module path is derived from these same values rather
# than a literal, so the two cannot drift apart. That is what lets this one
# script run unmodified in every provider: nothing here needs to know which
# fork (the build fork or the release fork) it happens to be checked out
# from.
set -euo pipefail

usage() { echo "usage: $0 <version>  (e.g. v0.1.0-rc2)" >&2; exit 2; }
[ $# -eq 1 ] || usage
VERSION="$1"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

for bin in cosign crane jq; do
  command -v "$bin" >/dev/null 2>&1 || {
    echo "FATAL: required tool '$bin' not found on PATH" >&2
    exit 2
  }
done

[ -f Makefile ] || { echo "FATAL: no Makefile in $ROOT -- run this from the repo root" >&2; exit 2; }

# sed -E (not grep -P, which BSD/busybox grep does not support) extracts the
# value after the Makefile's assignment operator. Both patterns anchor on
# start-of-line and stop at the first run of whitespace, same as the -P \K
# forms this replaces.
UP_ORG=$(sed -n -E 's/^UP_ORG[[:space:]]*\?=[[:space:]]*([^[:space:]]+).*/\1/p' Makefile 2>/dev/null | head -1)
PROJECT_NAME=$(sed -n -E 's/^PROJECT_NAME[[:space:]]*:=[[:space:]]*([^[:space:]]+).*/\1/p' Makefile 2>/dev/null | head -1)
if [ -z "$UP_ORG" ] || [ -z "$PROJECT_NAME" ]; then
  echo "FATAL: cannot derive UP_ORG or PROJECT_NAME from Makefile" >&2
  exit 2
fi

IMAGE="xpkg.upbound.io/${UP_ORG}/${PROJECT_NAME}"
ISSUER="https://token.actions.githubusercontent.com"

# GITHUB_ORG is the org hosting the upstream repository that cuts releases.
# It is a fact distinct from UP_ORG, the xpkg registry org: a provider may
# publish its package to one org while its upstream repository lives in
# another. The value is a recorded fact for this provider, defaulting to
# "upbound" when none is recorded.
#
# The repository segment, unlike the org segment, is a naming rule that
# binds every provider: releases are only ever cut upstream, never from a
# fork, so the repository is always named ${PROJECT_NAME} -- read from a
# recorded rule, not guessed.
#
# VERSION is regexp-escaped before interpolation -- version strings always
# contain literal dots, and an unescaped dot is a wildcard matching any
# character, which would let a decoy tag differing from VERSION only at a
# '.' position (e.g. v0X1X0 for v0.1.0) verify here too.
GITHUB_ORG="crossplane-contrib"
VERSION_RE="${VERSION//./\\.}"
IDENTITY_REGEXP="^https://github\\.com/${GITHUB_ORG}/${PROJECT_NAME}/\\.github/workflows/release\\.yaml@refs/tags/${VERSION_RE}\$"

# Minimum cosign version is read directly from this repo's own release
# workflow file rather than hardcoded here, so the two values cannot drift
# apart.
WORKFLOW=".github/workflows/release.yaml"
MIN_COSIGN_VERSION=""
if [ -f "$WORKFLOW" ]; then
  MIN_COSIGN_VERSION=$(grep -A2 'sigstore/cosign-installer@' "$WORKFLOW" 2>/dev/null \
    | grep -oE "cosign-release:[[:space:]]*'?\"?v?[0-9]+\.[0-9]+\.[0-9]+" \
    | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)
fi

echo "==> installed cosign version"
cosign version
if [ -n "$MIN_COSIGN_VERSION" ]; then
  INSTALLED=$(cosign version 2>/dev/null | awk -F': ' '/GitVersion/{print $2}' | tr -d '[:space:]' | sed 's/^v//')
  if [ -n "$INSTALLED" ]; then
    # awk numeric-triplet comparison replaces `sort -V`, which is a GNU
    # coreutils extension absent from BSD/busybox sort.
    OLDER=$(awk -v want="$MIN_COSIGN_VERSION" -v have="$INSTALLED" 'BEGIN {
      split(want, w, "."); split(have, h, ".");
      for (i = 1; i <= 3; i++) {
        wi = w[i] + 0; hi = h[i] + 0;
        if (hi < wi) { print "yes"; exit }
        if (hi > wi) { print "no"; exit }
      }
      print "no"
    }')
    if [ "$OLDER" = "yes" ]; then
      echo "FATAL: installed cosign $INSTALLED is older than the minimum $MIN_COSIGN_VERSION this release was signed with -- attestations are OCI 1.1 referrers, and an older cosign (or a policy engine expecting the legacy .sig/.att tags) reports \"no signatures found\" on a correctly signed image" >&2
      exit 1
    fi
  fi
fi

echo "==> resolving ${IMAGE}:${VERSION}"
DIGEST=$(crane digest "${IMAGE}:${VERSION}") || {
  echo "FATAL: could not resolve ${IMAGE}:${VERSION}" >&2
  exit 1
}
SUBJECT="${IMAGE}@${DIGEST}"
echo "    ${VERSION} currently resolves to ${SUBJECT}"

FAIL=0

echo "==> verifying image signature"
if ! cosign verify \
    --certificate-identity-regexp "$IDENTITY_REGEXP" \
    --certificate-oidc-issuer "$ISSUER" \
    "$SUBJECT" >/dev/null; then
  echo "FAIL: signature did not verify" >&2
  FAIL=1
fi

# check_attestation <label> <cosign --type value>
#
# Verifies the attestation, then cross-checks that its DSSE payload names the
# digest ${VERSION} currently resolves to -- an attestation can verify
# cryptographically while describing a stale digest if the tag moved (or was
# force-pushed) after the attestation was made, and that gap is exactly what
# a signature check alone will not catch.
check_attestation() {
  local label="$1" type="$2" out envelope payload subj
  echo "==> verifying $label attestation ($type)"
  if ! out=$(cosign verify-attestation \
        --certificate-identity-regexp "$IDENTITY_REGEXP" \
        --certificate-oidc-issuer "$ISSUER" \
        --type "$type" \
        "$SUBJECT" 2>&1); then
    echo "FAIL: $label attestation did not verify" >&2
    echo "$out" >&2
    FAIL=1
    return
  fi
  envelope=$(printf '%s\n' "$out" | grep -E '^\{' | tail -1)
  # jq's @base64d builtin decodes the DSSE payload -- no standalone base64
  # binary needed, and no GNU-vs-BSD `-d`/`-D` flag mismatch to paper over.
  payload=$(printf '%s' "$envelope" | jq -r '.payload | @base64d' 2>/dev/null || true)
  subj=$(printf '%s' "$payload" | jq -r '.subject[0].digest.sha256 // empty' 2>/dev/null || true)
  if [ -z "$subj" ]; then
    echo "FAIL: $label attestation verified but its subject digest could not be extracted" >&2
    FAIL=1
  elif [ "sha256:$subj" != "$DIGEST" ]; then
    echo "FAIL: $label attestation subject sha256:$subj does not match ${VERSION}'s current digest $DIGEST" >&2
    FAIL=1
  fi
}

check_attestation "SBOM (SPDX, container image)"   "spdxjson"
check_attestation "SBOM (CycloneDX, Go modules)"   "cyclonedx"
check_attestation "VEX (OpenVEX, reachability)"    "openvex"
check_attestation "VEX (CycloneDX, risk-scored)"   "https://cyclonedx.org/vex"
check_attestation "SLSA provenance"                "slsaprovenance1"
check_attestation "vens risk-scoring context"      "https://vens.dev/risk-context/v1"

if [ "$FAIL" -ne 0 ]; then
  echo "FAILED: one or more checks above did not pass" >&2
  exit 1
fi

echo "PASSED: signature and every attestation verify against ${SUBJECT}; every attestation subject digest matches what ${VERSION} currently resolves to"

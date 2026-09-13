# Releases and Support

<!-- prerelease -->
**This provider has published no release.** Until its first release, the
guarantees below are not yet in force: `v1alpha1` can still be reshaped in
place, without a new API version. Everything below takes effect at the first
release, and this notice is removed then.
<!-- end-prerelease -->

This document states what you can rely on when you depend on this provider:
which releases receive fixes, what each version number means, and what is
guaranteed when you upgrade or roll back.

## Versioning

Releases follow [semantic versioning](https://semver.org):
`<major>.<minor>.<patch>`.

- **Major** — breaking schema or behavioural changes. There is no
  backward-compatibility guarantee across a major version. Release notes list
  the breaking changes and how to adapt to them.
- **Minor** — new resources, fields or capabilities. Backward compatible with
  earlier minors in the same major.
- **Patch** — bug fixes only. No new features and no change to the API surface.

Upgrading to a higher minor or patch within the same major is backward
compatible, and you can skip intermediate releases — `v1.2.3` to `v1.5.0` is
supported. Simulate any upgrade outside production first.

## Supported releases

The **current and previous minor** release lines are supported, for **6 months**
from the date each minor was first released.

Support means security fixes are backported to those lines and a patch release
is cut. It does not carry a response-time commitment: this provider is
maintained on a best-effort basis by the people named in [OWNERS.md](OWNERS.md).

Anything outside that window ships in the next minor release instead.

## Backports

A fix is backported to a maintenance branch only when all of the following
hold:

- it addresses a security issue, a critical regression, or a risk of data loss;
- it is low-risk and can be cherry-picked as a self-contained change;
- it introduces no new features, no new APIs, and no breaking schema or
  behavioural change.

Backports are deliberately cautious and patch-only. Where a safe, targeted fix
is not possible, the change ships in the next minor release instead. New
features, refactors and non-critical fixes are never backported.

## Maintenance branches

Each supported minor has a maintenance branch named `release-X.Y`. These
branches accept cherry-picked fixes only — never new features or new API
surface — and a backport release increments the patch component within that
line (`X.Y.Z` becomes `X.Y.(Z+1)`).

## API versions

The custom resource definitions this provider installs carry their own API
version, independent of the provider's release version. The two move
separately: a provider release can change without any CRD API version changing.

**No API version changes incompatibly in place.** A field is not removed from a
version it has already shipped in, and its meaning does not change there. This
holds for every version, `v1alpha1` included. What differs between versions is
how long the version itself is kept:

- **`v1alpha1`** — has not been through a full qualification process. Intended
  for evaluation and testing rather than production. The version may be
  **removed entirely in any release, with no prior deprecation notice**, and
  replaced by `v1alpha2`. Resources stored under a removed version have to be
  recreated under its replacement; the release notes say so when that happens.
- **`v1beta1`** — qualified and tested for common cases. An incompatible change
  arrives as a new API version rather than altering this one, and the version is
  announced as deprecated before it stops being served.
- **`v1`** — fully defined. Not removed within the same major version of the
  provider.

Some incompatible changes cannot be hidden behind a new API version — a field
that becomes genuinely required is the common example, and it affects every
served version. Changes of that kind are called out in the release notes.

**API versions served by this release:** `v1alpha1`

## Downgrading

**Backward compatibility is not guaranteed when downgrading.**

Moving to an earlier release may require manual intervention to bring the
provider and its resources back to a healthy, synced state, and in some cases
uninstalling the package and reinstalling the earlier version. Resources
written by a newer schema are not guaranteed to be readable by an older one.

Always rehearse a downgrade outside production before performing one against
resources you care about.

## Reporting a problem

Open an issue on this repository. For anything security-sensitive, contact the
maintainers listed in [OWNERS.md](OWNERS.md) directly rather than opening a
public issue.

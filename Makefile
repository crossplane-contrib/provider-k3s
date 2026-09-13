# ====================================================================================
# Setup Project

# Force -mod=readonly regardless of what an ambient GOFLAGS carries. Without
# this, an inherited GOFLAGS=-mod=mod causes `go tool` invocations later in the
# generate pipeline (goimports resolution walks the full module graph) to
# rewrite go.sum with hashes the pruned file legitimately omits, so
# `make check-diff` fails on a tree nobody edited. -mod=readonly is already
# Go's default; forcing it changes nothing in a correct environment and denies
# a hostile one the ability to rewrite a committed artifact. Any unrelated
# ambient GOFLAGS value is preserved via the filter-out.
export GOFLAGS := -mod=readonly $(filter-out -mod=%,$(GOFLAGS))

PROJECT_NAME := provider-k3s
# UP_ORG is the GitHub org this project is published under -- the same org the
# module path, PROJECT_REPO and the xpkg registry org (below) all name. It is
# a recorded fact, not derived from PROJECT_NAME.
UP_ORG ?= crossplane-contrib
ifeq ($(strip $(UP_ORG)),)
UP_ORG := crossplane-contrib
endif
PROJECT_REPO := github.com/crossplane-contrib/$(PROJECT_NAME)

PLATFORMS ?= linux_amd64 linux_arm64

# build/makelib/common.mk derives VERSION by testing whether `git tag` returns
# anything at all -- an existence check, not a reachability check. A tag that
# is not an ancestor of HEAD (the normal state on a release-X.Y maintenance
# branch once a patch tag has landed there instead of on main) takes the
# `git describe --tags` branch, which cannot describe HEAD, falls back to
# `--always`, and yields a bare SHA instead of a semver string -- which then
# breaks xpkg packaging downstream. This guard reproduces common.mk's own
# tagless derivation whenever no *reachable* tag exists, so VERSION never
# silently becomes a bare SHA. It must stay immediately before the include:
# common.mk only computes VERSION when $(origin VERSION) is still "undefined".
ifeq ($(origin VERSION), undefined)
ifeq ($(shell git describe --tags --abbrev=0 2>/dev/null),)
VERSION := $(shell echo "v0.0.0-$$(git rev-list HEAD --count)-g$$(git describe --dirty --always)" | sed 's/-/./2' | sed 's/-/./2' | sed 's/-/./2')
endif
endif

-include build/makelib/common.mk

# ====================================================================================
# Setup Output

-include build/makelib/output.mk

# ====================================================================================
# Setup Go

NPROCS ?= 1
GO_TEST_PARALLEL := $(shell echo $$(( $(NPROCS) / 2 )))
GO_STATIC_PACKAGES = $(GO_PROJECT)/cmd/provider
GO_REQUIRED_VERSION ?= 1.26
GO_LDFLAGS += -X $(GO_PROJECT)/internal/version.Version=$(VERSION)
GO_SUBDIRS += cmd internal apis
GO111MODULE = on
GOLANGCILINT_VERSION = 2.13.2
-include build/makelib/golang.mk

# ====================================================================================
# Setup Kubernetes tools

-include build/makelib/k8s_tools.mk

# ====================================================================================
# Setup Images

IMAGES = provider-k3s
-include build/makelib/imagelight.mk

# ====================================================================================
# Setup XPKG

XPKG_REG_ORGS ?= xpkg.upbound.io/$(UP_ORG)
# NOTE(hasheddan): skip promoting on xpkg.upbound.io as channel tags are
# inferred.
XPKG_REG_ORGS_NO_PROMOTE ?= xpkg.upbound.io/$(UP_ORG)
XPKGS = provider-k3s
-include build/makelib/xpkg.mk

# ====================================================================================
# Publishing
#
# The crossplane/build xpkg machinery publishes via `crossplane xpkg push`,
# which authenticates through the Docker keychain (~/.docker/config.json). We
# publish with the `up` CLI instead so CI can authenticate with an Upbound API
# token -- the same UP_API_TOKEN / UP_ORG wiring the Upbound configuration
# packages use. Do NOT pass `--create`: it hits the api.upbound.io
# repositories endpoint, and robot tokens 401 there. Registry repositories
# are created out-of-band.
#
# NOTE: the `up login` performed by upbound/action-up is NOT what authenticates
# the push. UP_API_TOKEN is a robot token, and up's RegistryKeychain falls back
# to the Docker keychain for robot tokens ("robot tokens cannot be used for
# registry login 401 Unauthorized"), so CI must additionally `docker login`
# to xpkg.upbound.io with UP_ROBOT_ID as the username. See the "Login to xpkg
# with robot" step in .github/workflows/release.yaml.
UP ?= up

xpkg.push.up: ## Push the built xpkg to xpkg.upbound.io using the up CLI.
	@$(INFO) Pushing package $(XPKG_REG_ORGS)/$(PROJECT_NAME):$(VERSION)
	@$(UP) xpkg push \
		$(foreach p,$(XPKG_LINUX_PLATFORMS),-f $(XPKG_OUTPUT_DIR)/$(p)/$(PROJECT_NAME)-$(VERSION).xpkg) \
		$(XPKG_REG_ORGS)/$(PROJECT_NAME):$(VERSION) || $(FAIL)
	@$(OK) Pushed package $(XPKG_REG_ORGS)/$(PROJECT_NAME):$(VERSION)

.PHONY: xpkg.push.up

BASE_REF ?= origin/main

check-breaking-changes: ## Fail if a changed CRD reshapes a served schema incompatibly
	@BASE_REF=$(BASE_REF) ./hack/check-breaking-changes.sh

# Standalone tool modules (their own go.mod, kept out of GO_SUBDIRS so `make
# test`/`make reviewable` never pull them into the provider's module graph)
# and shell guardrail tests under test/hooks/ have no runner otherwise.
# Discovers modules rather than naming them, and skips package-less ones —
# a stub module holding only go.mod/go.sum makes `go test ./...` exit 1
# ("no packages to test") even though there is nothing to test.
test.tools:
	@rc=0; for d in $$(find tools -name go.mod -not -path '*/vendor/*' -exec dirname {} \; 2>/dev/null); do \
		[ -n "$$(cd $$d && go list ./... 2>/dev/null)" ] || continue; \
		$(INFO) go test $$d; \
		(cd $$d && go test ./... -count=1) || rc=1; \
	done; [ $$rc -eq 0 ] || $(FAIL)
	@for s in test/hooks/*_test.sh; do \
		[ -e "$$s" ] || continue; \
		$(INFO) running $$s; \
		bash "$$s" || exit 1; \
	done
	@$(OK) tool tests passed

reviewable: check-breaking-changes check-conventions test.tools

.PHONY: check-breaking-changes test.tools

# NOTE(hasheddan): we force image building to happen prior to xpkg build so that
# we ensure image is present in daemon.
xpkg.build.provider-k3s: do.build.images

fallthrough: submodules
	@echo Initial setup complete. Running make again . . .
	@make

# Run integration tests.
test-integration: $(KIND) $(KUBECTL) $(CROSSPLANE_CLI) $(HELM3)
	@$(INFO) running integration tests using kind $(KIND_VERSION)
	@KIND_NODE_IMAGE_TAG=${KIND_NODE_IMAGE_TAG} $(ROOT_DIR)/cluster/local/integration_tests.sh || $(FAIL)
	@$(OK) integration tests passed

# End to End testing
CROSSPLANE_VERSION = 2.3.4
CROSSPLANE_CLI_VERSION = v2.3.4
CROSSPLANE_NAMESPACE = crossplane-system
-include build/makelib/local.xpkg.mk
-include build/makelib/controlplane.mk

# uptest fork — carries sidecar-annotation support (harness annotations read
# from a `<manifest>.yaml.uptest` file beside each example instead of inline),
# required the moment any example manifest ships a sidecar: the upstream
# binary k8s_tools.mk pins renders a migrated tree with zero hooks and zero
# annotations at exit 0, so E2E would assert nothing while reporting green.
#
# k8s_tools.mk's own $(TOOLS_HOST_DIR)/uptest-$(UPTEST_VERSION) recipe stays
# untouched — overriding UPTEST_VERSION alone 404s, because its download rule
# hardcodes the upstream org. This block instead points UPTEST at a NEW
# filename under the same $(TOOLS_HOST_DIR) and supplies its own download
# recipe for that filename, so the two never collide on one make target.
# $(TOOLS_HOST_DIR) is hardlinked from a shared cross-worktree tool cache —
# reusing the stock uptest-$(UPTEST_VERSION) path here would swap the binary
# under every other worktree's concurrently-running E2E.
UPTEST_FORK_REPO := kaessert/uptest
UPTEST_FORK_REF  := v2.3.0-fork.e21896e
UPTEST           := $(TOOLS_HOST_DIR)/uptest-fork-$(UPTEST_FORK_REF)

$(UPTEST):
	@$(INFO) installing uptest fork $(UPTEST_FORK_REF)
	@mkdir -p $(TOOLS_HOST_DIR)
	@curl -fsSLo $(UPTEST) https://github.com/$(UPTEST_FORK_REPO)/releases/download/$(UPTEST_FORK_REF)/uptest_$(SAFEHOSTPLATFORM) || $(FAIL)
	@chmod +x $(UPTEST)
	@$(OK) installing uptest fork $(UPTEST_FORK_REF)

# uptest's built-in default process budget is 1200s, shorter than this
# fixture's own slowest single object: a from-scratch k3s server install over
# SSH runs long enough on a nested/systemd-in-container host to exceed it.
# Capped at the e2e-semaphore slot TTL (5400s) -- a longer budget does not buy
# a longer run, it buys an unprotected one.
UPTEST_DEFAULT_TIMEOUT ?= 5400s

# Minimum free space (GB) required on / before an E2E run starts. A full disk
# fails E2E as a bogus PROVIDER error 30+ minutes into the run instead of
# failing fast up front. Overridable, e.g. `make e2e-preflight E2E_MIN_FREE_GB=5`
# for a quick local smoke check.
E2E_MIN_FREE_GB ?= 15

# E2E preflight: this provider needs no external credentials -- the SSH
# nodepool fixture (cluster/local/setup.sh) generates and supplies its own
# keypair. Validate the local toolchain and disk space instead of any
# credential env var.
e2e-preflight: ## Validate E2E prerequisites
	@FREE=$$(df -BG --output=avail / | tail -1 | tr -dc '0-9'); \
	if [ "$$FREE" -lt "$(E2E_MIN_FREE_GB)" ]; then \
	  echo "ERROR: only $${FREE}GB free on / — E2E needs >= $(E2E_MIN_FREE_GB)GB." >&2; \
	  echo "  A full disk fails E2E as a bogus PROVIDER error 30+ minutes from now." >&2; \
	  echo "  Reclaim: docker image prune -f; rm -rf /tmp/tmp.* /tmp/go-build*;" >&2; \
	  echo "           trim \$$(go env GOCACHE) (oldest-first, keep it under ~30GB)." >&2; \
	  exit 1; \
	fi; \
	echo "e2e-preflight: $${FREE}GB free on / (minimum $(E2E_MIN_FREE_GB)GB)"
	@command -v docker >/dev/null 2>&1 || { echo "ERROR: docker is required to run the kind E2E cluster" >&2; exit 1; }
	@docker info >/dev/null 2>&1 || { echo "ERROR: docker daemon is not reachable" >&2; exit 1; }
	@echo "e2e-preflight: no external credentials required (the SSH nodepool fixture supplies its own keys)"

# e2e-preflight must gate the chain, not trail it: `e2e: e2e-preflight` would
# only append to the recipe-less `e2e` target's prerequisite list and run
# LAST, after the very run it exists to prevent. `build` has its own recipe
# and is the first prerequisite reached by every E2E entry point below, so
# attaching the guard here lets it abort before any image build or kind
# cluster is created. Filtered on the top-level goal so a plain `make build`
# never gains a disk/toolchain preflight: `e2e.%` covers every per-resource
# target (e2e.cluster, e2e.node, e2e.node-delete-order) alongside the bare
# `e2e` aggregate.
build: $(if $(filter e2e e2e.%,$(MAKECMDGOALS)),e2e-preflight,)

.PHONY: e2e-preflight

# Cluster is a SINGLETON at the external-infrastructure layer: its
# cluster-scoped and namespaced example both install a k3s server on the
# SAME host (k3s-nodepool-0), so they cannot run in one uptest pass — they
# would race installing the same server. Node's own example bundles that
# same Cluster as a prerequisite (so `make e2e.node` can stand alone
# without depending on the Cluster target), and each Node scope variant
# embeds a differently-scoped copy of it, so for the identical reason the
# Node scope pair also cannot share one pass. Both resources therefore run
# as two SEQUENTIAL uptest passes below rather than one comma-joined list.
# GATED: the E2E fixture provisions only two nodepool hosts, and CORE's one
# manifest (node-namespaced.yaml, below) already claims host k3s-nodepool-0
# for its own bundled Cluster prerequisite — no spare host exists to run
# either Cluster example in the same default pass. Use `make e2e.cluster`.
UPTEST_MANIFESTS_CLUSTER_CLUSTER := examples/cluster/cluster.yaml
UPTEST_MANIFESTS_CLUSTER_NS := examples/cluster/cluster-namespaced.yaml
UPTEST_MANIFESTS_NODE_CLUSTER := examples/node/node.yaml
UPTEST_MANIFESTS_NODE_NS := examples/node/node-namespaced.yaml

# CORE: the default `make e2e` run. Only the namespaced Node example is
# included — it already bundles its own Cluster prerequisite, so this one
# pass exercises both resources' basic lifecycle. Folding in the
# cluster-scoped Node variant or either standalone Cluster example would
# either apply the same Cluster object twice in one pass or install two k3s
# servers on the same host concurrently — both avoided by keeping CORE to
# this one manifest and reaching the other scope/resource combinations only
# through the dedicated sequential-pass targets below.
UPTEST_MANIFESTS_CORE := $(UPTEST_MANIFESTS_NODE_NS)

UPTEST_EXAMPLE_LIST ?= $(UPTEST_MANIFESTS_CORE)

# UPTEST_TEST_DIRECTORY is KIND_CLUSTER_NAME-derived and therefore
# worktree-unique, so two concurrent E2E runs never stage into (or read
# back) one another's rendered case files. CASE_DIR points the shared
# convergence barrier (test/hooks/converge-barrier.sh) at uptest's own
# rendered manifests for this run — never examples/, whose sources still
# carry unsubstituted placeholders. Exported so the barrier, invoked by
# uptest itself as a subprocess, inherits it.
#
# --post-assert-script replaces the per-resource converge windows the
# individual post-assert-<resource>.sh hooks used to run with ONE shared
# barrier invoked once per uptest pass, after every resource's own
# assertions pass.
UPTEST_TEST_DIRECTORY := /tmp/uptest-e2e-$(KIND_CLUSTER_NAME)
export CASE_DIR = $(UPTEST_TEST_DIRECTORY)/case

uptest: $(UPTEST) $(KUBECTL) $(KIND) $(CHAINSAW) $(CROSSPLANE_CLI)
	@$(INFO) running automated tests
	@KUBECTL=$(KUBECTL) KIND=$(KIND) CHAINSAW=$(CHAINSAW) CROSSPLANE_CLI=$(CROSSPLANE_CLI) CROSSPLANE_NAMESPACE=$(CROSSPLANE_NAMESPACE) KIND_CLUSTER_NAME=$(KIND_CLUSTER_NAME) $(UPTEST) e2e "$(UPTEST_EXAMPLE_LIST)" --setup-script=cluster/local/setup.sh --default-timeout=$(UPTEST_DEFAULT_TIMEOUT) --test-directory=$(UPTEST_TEST_DIRECTORY) --post-assert-script=$(abspath test/hooks/converge-barrier.sh) || $(FAIL)
	@$(OK) running automated tests

# DRC_FILE is read directly by local.xpkg.deploy.provider.% (build/makelib/
# local.xpkg.mk) — set as a target-specific variable here (GNU Make
# propagates it to this target's prerequisites) rather than passed via a
# recursive `$(MAKE) local.xpkg.deploy.provider... DRC_FILE=...` recipe
# call. A recipe line that contains the literal text $(MAKE) is executed by
# GNU Make for real even under `make -n`, precisely so a recursive submake
# can inherit the -n flag and keep dry-running; but any OTHER shell command
# chained onto that same recipe line with `&&` is NOT covered by that
# propagation and runs for real regardless. This target used to chain two
# `kubectl wait` calls onto exactly such a line, so `make -n e2e.<slug>`
# ran them against a cluster that does not exist and aborted the whole
# dry-run before ever reaching the `uptest` recipe that names the manifest
# set. Listing the deploy target as an ordinary prerequisite instead keeps
# every recipe line here free of $(MAKE), so `make -n` only ever prints.
local-deploy: DRC_FILE := ./examples/deploymentruntimeconfig.yaml
local-deploy: build controlplane.up $(YQ) local.xpkg.deploy.provider.$(PROJECT_NAME)
	@$(INFO) running locally built provider
	@$(KUBECTL) wait provider.pkg $(PROJECT_NAME) --for condition=Healthy --timeout 5m
	@$(KUBECTL) -n $(CROSSPLANE_NAMESPACE) wait --for=condition=Available deployment --all --timeout=5m
	@$(OK) running locally built provider

e2e: local-deploy uptest

# UPDATE_TESTER_TIMEOUT overrides the update-tester's own per-field
# `run --timeout` (tool default 120s) -- the Synced-wait window each of the
# corpus's 20 field-scope entries (6 Cluster + 6 Cluster-namespaced + 4 Node
# + 4 Node-namespaced) gets from test/hooks/run-update-tester.sh's `hook`
# step. Set at FILE scope with `?=` and exported below, so every `make e2e*`
# goal sees it -- including the bare default goal `make e2e`, not only the
# three named per-resource targets -- while a caller-supplied
# `UPDATE_TESTER_TIMEOUT=<n> make e2e` still wins over this default. Every
# entry re-runs the full k3s installer
# (`curl -sfL https://get.k3s.io | sh`) over SSH -- there is no field that
# only patches config without also going through that reinstall -- so a
# transient download retry anywhere in the corpus can consume the window on
# its own. A live run (2026-09-11) measured this happening on Node's
# k3sChannel: pod log timestamps show the installer's first
# "[ERROR] Download failed: cannot execute SSH command: Process exited with
# status 1" at 20:56:14Z, seven further retries, and eventual success
# (`Successfully requested update of external resource`) at 20:58:18Z -- a
# 124s stall against a no-retry happy path measured at 65s (tlsSAN) to 68s
# (k3sVersion, the corpus's slowest field). The 120s default left ~55s of
# headroom over that happy path, and this one retry episode used more than
# that. 300s clears the 124s worst case measured so far by 176s (142%
# margin) while staying far inside each manifest's own
# uptest.upbound.io/timeout (1800 Cluster / 3000 Node) and the 5400s
# admission-slot TTL: a field only ever waits the full window when it is
# actually retrying, so the extra budget is spent only by whichever field
# hits a transient failure, not by the other ~19.
#
# A field that still exceeds this window has its late-landing change picked
# up and reported as a "side effect" on the NEXT field tested (observed on
# extraArgs in the same run, immediately after k3sChannel's timeout) -- that
# is inherent to testing fields sequentially against one external host, not
# a separate defect: raising the window to clear the measured worst case is
# what closes it in practice, and the side-effect line itself already names
# which field the change actually belongs to.
UPDATE_TESTER_TIMEOUT ?= 300
export UPDATE_TESTER_TIMEOUT

# Per-resource targets — each singleton Cluster/Node scope pair runs as two
# SEQUENTIAL uptest passes (never comma-joined into one UPTEST_EXAMPLE_LIST)
# because both variants install a k3s server on the same external host and
# would otherwise race. Each recipe line here is bare `$(MAKE) e2e ...` with
# nothing else chained onto it, so — like local-deploy above — `make -n`
# stays safe: the recursive submake inherits -n and only prints its own plan.
# UPDATE_TESTER_TIMEOUT is already exported at file scope above and needs no
# per-target override here.
e2e.cluster:
	$(MAKE) e2e UPTEST_EXAMPLE_LIST=$(UPTEST_MANIFESTS_CLUSTER_CLUSTER)
	$(MAKE) e2e UPTEST_EXAMPLE_LIST=$(UPTEST_MANIFESTS_CLUSTER_NS)

# e2e.node-delete-order: proves the Node delete-wedge fix survives, by
# exercising the ONE delete order `make e2e` cannot reach on its own --
# examples/node/testhooks/delete-node-namespaced.sh is the Cluster's own
# pre-delete-hook, so a standard uptest run always deletes every Node before
# its Cluster. This target runs a normal namespaced Node pass first (Node's
# own bundled Cluster prerequisite, Node-first delete ordering unchanged),
# then reuses the still-live kind cluster and k3s-nodepool fixture to
# re-apply the same namespaced pair, delete the Cluster FIRST, delete the
# Node second, and assert the Node clears its finalizer. Reuses
# UPTEST_MANIFESTS_NODE_NS rather than composing a fresh comma-pair: that
# variable already names the one file bundling both objects, so reusing it
# cannot double-apply the Cluster the way a
# cluster-namespaced.yaml,node-namespaced.yaml concatenation would. Not part
# of the default `make e2e`/`make e2e.node` runs -- a deliberately separate,
# explicitly-invoked target, run whenever the Node controller's Connect()
# changes. Runs the identical UPTEST_MANIFESTS_NODE_NS corpus as e2e.node and
# is exposed to the same installer-retry risk; UPDATE_TESTER_TIMEOUT is
# already exported at file scope above and needs no per-target override here.
e2e.node-delete-order:
	$(MAKE) e2e UPTEST_EXAMPLE_LIST=$(UPTEST_MANIFESTS_NODE_NS)
	@$(INFO) node-delete-order-check: exercising Cluster-deleted-BEFORE-Node
	@KUBECTL=$(KUBECTL) ./cluster/local/node-delete-order-check.sh
	@$(OK) node-delete-order-check passed

# UPDATE_TESTER_TIMEOUT is already exported at file scope above and needs no
# per-target override here.
e2e.node:
	$(MAKE) e2e UPTEST_EXAMPLE_LIST=$(UPTEST_MANIFESTS_NODE_CLUSTER)
	$(MAKE) e2e UPTEST_EXAMPLE_LIST=$(UPTEST_MANIFESTS_NODE_NS)

.PHONY: e2e.cluster
.PHONY: e2e.node
.PHONY: e2e.node-delete-order

# The update-tester validates that an annotated example manifest documents
# every mutable field its type actually has, so a manifest can't silently
# drift out of sync with the resource it targets. It is consumed as a
# pinned module from tools/update-tester (a stub module holding only
# go.mod/go.sum -- no vendored source), so there is no build step: `go -C`
# runs it directly from the module cache.
UPDATE_TESTER := go -C tools/update-tester tool crossplane-update-tester
#
# apis/ on this provider is flat per scope: apis/cluster/v1alpha1/ and
# apis/namespaced/v1alpha1/ hold every resource's own *_types.go directly,
# with no per-resource subdirectory and no zz_ prefix (this provider is
# hand-written, not code-generated). The types file for a manifest is
# therefore named after its own examples/ directory: examples/cluster/*.yaml
# resolves to apis/$scope/v1alpha1/cluster_types.go, examples/node/*.yaml to
# the equivalent node_types.go.
#
# The scope is namespaced when the API group contains a ".m." marker (e.g.
# k3s.m.crossplane.io) and cluster otherwise (e.g. k3s.crossplane.io). A
# self-check cross-references the manifest filename convention
# (*-namespaced.yaml) against the resolved scope so a future naming drift
# fails loudly instead of silently validating against the wrong types file.
#
# Discovery matches a manifest carrying the annotation either inline or in
# its sidecar (<manifest>.yaml.uptest): the sidecar carries the annotation's
# text unchanged, just outside the manifest's own *.yaml extension, so a
# glob scoped to *.yaml alone would silently validate zero fixtures once a
# manifest migrates -- the false-green this union avoids.
update-test.validate:
	@fail=0; \
	for f in $$( { grep -rl 'crossplane.io/update-test:' examples --include='*.yaml'; \
	  find examples -name '*.yaml.uptest' -exec grep -l 'crossplane.io/update-test:' {} \; \
	    | sed 's/\.uptest$$//'; } | sort -u); do \
	  resource=$$(basename $$(dirname "$$f")); \
	  av=$$(grep '^apiVersion:' "$$f" | grep 'crossplane.io' | head -1 | awk '{print $$2}'); \
	  grp=$$(echo "$$av" | cut -d/ -f1); \
	  case "$$grp" in \
	    *.m.crossplane.io) scope=namespaced ;; \
	    *) scope=cluster ;; \
	  esac; \
	  case "$$f" in \
	    *-namespaced.yaml) \
	      if [ "$$scope" != "namespaced" ]; then \
	        echo "FAIL: $$f looks namespaced by filename but resolved scope=$$scope from apiVersion=$$av"; \
	        fail=1; \
	        continue; \
	      fi ;; \
	    *) \
	      if [ "$$scope" != "cluster" ]; then \
	        echo "FAIL: $$f looks cluster-scoped by filename but resolved scope=$$scope from apiVersion=$$av"; \
	        fail=1; \
	        continue; \
	      fi ;; \
	  esac; \
	  types="apis/$$scope/v1alpha1/$${resource}_types.go"; \
	  if [ ! -f "$$types" ]; then \
	    echo "SKIP: $$f — no types file at $$types (apiVersion=$$av)"; \
	    fail=1; \
	    continue; \
	  fi; \
	  echo "=== $$f ($$types) ==="; \
	  $(UPDATE_TESTER) validate --types-file "$$PWD/$$types" "$$PWD/$$f" || fail=1; \
	done; \
	exit $$fail

.PHONY: update-test.validate

# Generate registration files from directory structure.
# Produces apis/zz_generated_register.go and
# internal/controller/zz_generated_register.go.
generate-registration:
	@$(INFO) generating registration files
	@go run hack/generate-registration.go $(PROJECT_REPO) || $(FAIL)
	@$(OK) generating registration files

# Wire generate-registration into the generate chain so it runs alongside
# go.generate (controller-gen deepcopy + CRD generation).
generate.run: generate-registration

.PHONY: generate-registration

# Update the submodules, such as the common build scripts.
submodules:
	@git submodule sync
	@git submodule update --init --recursive

# NOTE(hasheddan): the build submodule currently overrides XDG_CACHE_HOME in
# order to force the Helm 3 to use the .work/helm directory. This causes Go on
# Linux machines to use that directory as the build cache as well. We should
# adjust this behavior in the build submodule because it is also causing Linux
# users to duplicate their build cache, but for now we just make it easier to
# identify its location in CI so that we cache between builds.
go.cachedir:
	@go env GOCACHE

go.mod.cachedir:
	@go env GOMODCACHE

# NOTE(hasheddan): we must ensure up is installed in tool cache prior to build
# as including the k8s_tools machinery prior to the xpkg machinery sets UP to
# point to tool cache.
build.init: $(CROSSPLANE_CLI)

# This is for running out-of-cluster locally, and is for convenience. Running
# this make target will print out the command which was used. For more control,
# try running the binary directly with different arguments.
run: go.build
	@$(INFO) Running Crossplane locally out-of-cluster . . .
	@# To see other arguments that can be provided, run the command with --help instead
	$(GO_OUT_DIR)/provider --debug

dev: $(KIND) $(KUBECTL)
	@$(INFO) Creating kind cluster
	@$(KIND) create cluster --name=$(PROJECT_NAME)-dev
	@$(KUBECTL) cluster-info --context kind-$(PROJECT_NAME)-dev
	@$(INFO) Installing Provider K3s CRDs
	@$(KUBECTL) apply -R -f package/crds
	@$(INFO) Starting Provider K3s controllers
	@$(GO) run cmd/provider/main.go --debug

dev-clean: $(KIND) $(KUBECTL)
	@$(INFO) Deleting kind cluster
	@$(KIND) delete cluster --name=$(PROJECT_NAME)-dev

.PHONY: submodules fallthrough test-integration run dev dev-clean

# ====================================================================================
# Special Targets

# Install gomplate
GOMPLATE_VERSION := 3.10.0
GOMPLATE := $(TOOLS_HOST_DIR)/gomplate-$(GOMPLATE_VERSION)

$(GOMPLATE):
	@$(INFO) installing gomplate $(SAFEHOSTPLATFORM)
	@mkdir -p $(TOOLS_HOST_DIR)
	@curl -fsSLo $(GOMPLATE) https://github.com/hairyhenderson/gomplate/releases/download/v$(GOMPLATE_VERSION)/gomplate_$(SAFEHOSTPLATFORM) || $(FAIL)
	@chmod +x $(GOMPLATE)
	@$(OK) installing gomplate $(SAFEHOSTPLATFORM)

export GOMPLATE

# This target prepares repo for your provider by replacing all "k3s"
# occurrences with your provider name.
# This target can only be run once, if you want to rerun for some reason,
# consider stashing/resetting your git state.
# Arguments:
#   provider: Camel case name of your provider, e.g. GitHub, PlanetScale
provider.prepare:
	@[ "${provider}" ] || ( echo "argument \"provider\" is not set"; exit 1 )
	@PROVIDER=$(provider) ./hack/helpers/prepare.sh

# This target adds a new api type and its controller.
# You would still need to register new api in "apis/<provider>.go" and
# controller in "internal/controller/<provider>.go".
# Arguments:
#   provider: Camel case name of your provider, e.g. GitHub, PlanetScale
#   group: API group for the type you want to add.
#   kind: Kind of the type you want to add
#	apiversion: API version of the type you want to add. Optional and defaults to "v1alpha1"
provider.addtype: $(GOMPLATE)
	@[ "${provider}" ] || ( echo "argument \"provider\" is not set"; exit 1 )
	@[ "${group}" ] || ( echo "argument \"group\" is not set"; exit 1 )
	@[ "${kind}" ] || ( echo "argument \"kind\" is not set"; exit 1 )
	@PROVIDER=$(provider) GROUP=$(group) KIND=$(kind) APIVERSION=$(apiversion) PROJECT_REPO=$(PROJECT_REPO) ./hack/helpers/addtype.sh

define CROSSPLANE_MAKE_HELP
Crossplane Targets:
    submodules            Update the submodules, such as the common build scripts.
    run                   Run crossplane locally, out-of-cluster. Useful for development.

endef
# The reason CROSSPLANE_MAKE_HELP is used instead of CROSSPLANE_HELP is because the crossplane
# binary will try to use CROSSPLANE_HELP if it is set, and this is for something different.
export CROSSPLANE_MAKE_HELP

crossplane.help:
	@echo "$$CROSSPLANE_MAKE_HELP"

help-special: crossplane.help

vendor: modules.download
vendor.check: modules.check

.PHONY: crossplane.help help-special

check-conventions: ## Detect convention violations (test names, error wrapping, kubectl usage)
	@# PascalCase test names: no underscores after "Test"
	@! grep -rn 'func Test.*_' --include='*_test.go' . 2>/dev/null || \
	  (echo "FAIL: test names must use PascalCase (no underscores)" && exit 1)
	@# KUBECTL env var: test scripts must use $${KUBECTL:-kubectl}, never a bare
	@# `kubectl` invocation. Three passes over each test script keep this to
	@# genuine invocations: comment lines are blanked (not deleted, so line
	@# numbers stay accurate), double-quoted string literals are stripped (so
	@# prose inside a quoted message like `assert_true "... kubectl" "$$rc"`
	@# can't match), and a word boundary is required before `kubectl` (so
	@# `_kubectl` inside a longer identifier, e.g. `__stub_kubectl_empty`,
	@# doesn't match). A `KUBECTL:-kubectl` backstop catches the one shape
	@# quote-stripping and the word boundary both miss: an unquoted
	@# `KUBECTL=$${KUBECTL:-kubectl}` assignment.
	@bad=0; \
	for f in $$(find test/ -name '*.sh' 2>/dev/null); do \
	  m=$$(sed -E 's/^[[:space:]]*#.*$$//' "$$f" | sed -E 's/"[^"]*"//g' \
	    | grep -nE '(^|[^[:alnum:]_$$])kubectl' | grep -v 'KUBECTL:-kubectl'); \
	  [ -z "$$m" ] && continue; \
	  for ln in $$(echo "$$m" | cut -d: -f1); do \
	    echo "$$f:$$ln:$$(sed -n "$${ln}p" "$$f")"; \
	    bad=1; \
	  done; \
	done; \
	if [ "$$bad" -eq 1 ]; then echo "FAIL: use \$${KUBECTL:-kubectl} instead of bare kubectl"; exit 1; fi
	@# Error wrapping: no fmt.Errorf in production code. Comment lines are
	@# filtered out so the controllers' own "never fmt.Errorf" doc comments
	@# do not trip the check that documents them.
	@! grep -rn 'fmt\.Errorf' internal/ --include='*.go' 2>/dev/null \
	    | grep -v '_test.go' \
	    | grep -vE '^[^:]+:[0-9]+:[[:space:]]*//' || \
	  (echo "FAIL: use errors.Wrap/Errorf from crossplane-runtime, not fmt.Errorf" && exit 1)
	@# No bare stdlib "errors" import (match import lines only, not JSON struct tags).
	@! grep -rn '^\s*"errors"\s*$$' internal/ --include='*.go' 2>/dev/null | grep -v '_test.go' || \
	  (echo "FAIL: use crossplane-runtime/v2/pkg/errors, not stdlib \"errors\"" && exit 1)
	@echo "check-conventions: all checks passed"

.PHONY: check-conventions

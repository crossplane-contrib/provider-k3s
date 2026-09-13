/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/cluster/v1alpha1"
	sshclient "github.com/crossplane-contrib/provider-k3s/internal/clients/ssh"
	"github.com/crossplane-contrib/provider-k3s/internal/criticalannotation"
	"github.com/crossplane-contrib/provider-k3s/internal/driftdetection"
	"github.com/crossplane-contrib/provider-k3s/internal/k3s"
)

const (
	errTrackPCUsage         = "cannot track ProviderConfig usage"
	errGetPC                = "cannot get ProviderConfig"
	errGetCreds             = "cannot get credentials"
	errNewClient            = "cannot create SSH client"
	errMarshalLastApplied   = "cannot marshal last-applied configuration"
	errUnmarshalLastApplied = "cannot unmarshal last-applied configuration"
)

// annotationLastAppliedConfig records the mutable ClusterParameters this
// controller last sent to the host, so Observe can detect drift even though
// the k3s install script does not return the server's own configuration.
const annotationLastAppliedConfig = "k3s.crossplane.io/last-applied-cluster-config"

// externalTimeout bounds the cumulative duration of one reconcile's calls to
// the external API (Connect/Observe/Create/Update/Delete). Create and Update
// dispatch the k3s server install over SSH and return as soon as the
// install script has skipped its own blocking restart and queued a
// non-blocking one instead (see k3s.InstallCommand's doc comment) rather
// than blocking until the server is actually ready, so this budget no
// longer has to cover the full install duration -- only the SSH round trip
// needed to install the binary, write the systemd unit and queue the
// restart. The margin here is deliberately generous rather than trimmed
// tight: crossplane-runtime persists the create-result annotation with the
// SAME reconcile context Create ran under, so a Create that merely times
// out promptly still needs enough of that budget left afterward for the
// write to land, or the resource wedges on a dangling
// external-create-pending annotation with no retry.
const externalTimeout = 10 * time.Minute

// No WithCreationGracePeriod override: Create no longer blocks until the
// server is ready (see k3s.InstallCommand's doc comment) -- it returns once
// the restart is queued on the host, so the resource genuinely may not
// exist yet, from the external API's point of view, when the immediate
// post-create Observe runs. That is not a problem the grace period exists
// to paper over: Observe's systemctl probe reports the unit's LoadState as
// loaded (exists, not yet ready) rather than absent, so no spurious
// re-Create follows regardless of how soon that first Observe lands.

// External-Name Strategy
//
// Identity is spec.forProvider.host: the SSH connection target, and the
// only field either Cluster or Node carries that addresses the remote
// resource. Classified on the two identity axes:
//
//   - Assignment: deterministic. The caller supplies host before create;
//     nothing is minted by the remote side.
//   - Stability: stable. host is immutable (self == oldSelf, enforced by
//     CEL) -- there is no update path that could rotate it, so it never
//     behaves like a derived handle.
//
// Re-imaging the same address out-of-band (the box is wiped and
// reinstalled while spec.forProvider.host stays the same) is NOT a new
// resource under this model: identity here is address-based, not
// content-derived, and the controller has no signal to distinguish a
// re-image from the original install continuing to run. Observe's
// existence probe (systemd LoadState over SSH) simply reports no loaded
// artifact until k3s is reinstalled, and the same external-name continues
// to address it -- that is the correct behaviour for a stable identifier,
// not a limitation of it.
//
// managed.WithDeterministicExternalName(true) is set below, and Observe
// keeps the external-name annotation in sync with host on every pass
// (idempotent) rather than only at Create: the value is always knowable
// from spec, and setting it before existence is even checked matches the
// deterministic classification -- there is no pre-create guard, because
// absence of the resource is exactly what the systemctl probe already
// reports.

// Setup adds a controller that reconciles cluster-scoped Cluster managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.ClusterGroupKind)

	opts := []managed.ReconcilerOption{
		managed.WithTypedExternalConnector[*v1alpha1.Cluster](driftdetection.WrapConnector[*v1alpha1.Cluster](&connector{
			kube:  mgr.GetClient(),
			usage: resource.NewLegacyProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
		})),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithTimeout(externalTimeout),
		// host is deterministic and stable (see "External-Name Strategy"
		// above), so it is always safe to re-queue and retry a Create that
		// left the outcome ambiguous -- there is no risk of leaking a
		// second, differently-named resource the way there would be for a
		// server-assigned name.
		managed.WithDeterministicExternalName(true),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))), //nolint:staticcheck // event.NewAPIRecorder still requires the legacy record.EventRecorder.
		// The critical-annotation write that clears the pending-create marker
		// after Create() must not inherit the context Create's own blocking
		// SSH call just spent -- see the criticalannotation package doc for
		// why a fresh, detached budget is required here.
		managed.WithCriticalAnnotationUpdater(criticalannotation.WrapUpdater(managed.NewRetryingCriticalAnnotationUpdater(mgr.GetClient()))),
	}

	if o.Features.Enabled(feature.EnableBetaManagementPolicies) {
		opts = append(opts, managed.WithManagementPolicies())
	}

	if o.Features.Enabled(feature.EnableAlphaChangeLogs) {
		opts = append(opts, managed.WithChangeLogger(o.ChangeLogOptions.ChangeLogger))
	}

	if o.MetricOptions != nil {
		opts = append(opts, managed.WithMetricRecorder(o.MetricOptions.MRMetrics))
	}

	if o.MetricOptions != nil && o.MetricOptions.MRStateMetrics != nil {
		stateMetricsRecorder := statemetrics.NewMRStateRecorder(
			mgr.GetClient(), o.Logger, o.MetricOptions.MRStateMetrics, &v1alpha1.ClusterList{}, o.MetricOptions.PollStateMetricInterval,
		)
		if err := mgr.Add(stateMetricsRecorder); err != nil {
			return errors.Wrap(err, "cannot register MR state metrics recorder for kind v1alpha1.ClusterList")
		}
	}

	r := managed.NewReconciler(mgr, resource.ManagedKind(v1alpha1.ClusterGroupVersionKind), opts...)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Cluster{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

type connector struct {
	kube  client.Client
	usage *resource.LegacyProviderConfigUsageTracker
}

func (c *connector) Connect(ctx context.Context, cr *v1alpha1.Cluster) (managed.TypedExternalClient[*v1alpha1.Cluster], error) {
	if err := c.usage.Track(ctx, cr); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	// Get cluster-scoped ProviderConfig
	ref := cr.GetProviderConfigReference()

	pc := &v1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: ref.Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	// Extract credentials
	data, err := resource.CommonCredentialExtractor(ctx, pc.Spec.Credentials.Source, c.kube, pc.Spec.Credentials.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	// Build SSH config using host/port from the Cluster CR, credentials from ProviderConfig
	sshCfg := sshclient.Config{
		Host:     cr.Spec.ForProvider.Host,
		Port:     cr.Spec.ForProvider.Port,
		Username: pc.Spec.Username,
	}
	sshCfg.ConfigureAuth(data)

	sshClient, err := sshclient.NewClient(sshCfg)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	return &external{
		ssh:  sshClient,
		host: cr.Spec.ForProvider.Host,
		kube: c.kube,
	}, nil
}

type external struct {
	ssh  *sshclient.Client
	host string
	// kube writes the last-applied-config annotation directly to the API
	// server from Update(). crossplane-runtime does not persist an in-memory
	// annotation mutation made inside external.Update() -- only the status
	// subresource is written back after a successful Update -- so relying on
	// the reconciler here would silently discard the annotation forever.
	kube client.Client
}

func (e *external) Observe(ctx context.Context, cr *v1alpha1.Cluster) (managed.ExternalObservation, error) {
	// Identity is deterministic and known from spec before existence is
	// even checked (see "External-Name Strategy" above) -- keep the
	// annotation in sync on every pass rather than only setting it once.
	if meta.GetExternalName(cr) != cr.Spec.ForProvider.Host {
		meta.SetExternalName(cr, cr.Spec.ForProvider.Host)
	}

	stdout, _, err := e.ssh.Execute(ctx, k3s.ServiceProbeCommand("k3s"))
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, "cannot check k3s status")
	}
	probe := k3s.ParseServiceProbe(stdout)
	if !probe.Exists() {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	ready := probe.Ready()

	versionOut, _, _ := e.ssh.Execute(ctx, "k3s --version 2>/dev/null | head -1")

	nodeToken, _, _ := e.ssh.Execute(ctx, "sudo cat /var/lib/rancher/k3s/server/node-token 2>/dev/null")

	kubeconfig, _, _ := e.ssh.Execute(ctx, "sudo cat /etc/rancher/k3s/k3s.yaml 2>/dev/null")
	if kubeconfig != "" {
		kubeconfig = k3s.RewriteKubeconfig(kubeconfig, e.host)
	}

	// Create and Update now return as soon as the install script's restart
	// is queued (see k3s.InstallCommand's doc comment), so a loaded-but-not-
	// active read here is a restart genuinely still in flight -- report
	// Creating() explicitly rather than leaving a stale Available condition
	// in place from a prior pass (an Update-driven restart, or a flapping
	// unit, can revisit this path after Available was already set once).
	if ready {
		cr.SetConditions(xpv2.Available())
	} else {
		cr.SetConditions(xpv2.Creating())
	}

	// The k3s install script reports no live configuration of its own, so
	// the last-applied-config annotation is this provider's only source
	// for the mutable fields' observed state (see clusterIsUpToDate and
	// lastAppliedClusterConfig below). Immutable fields can never diverge
	// from spec once the resource exists (CEL enforces it), so they mirror
	// straight from spec.
	last, hasLast, err := lastAppliedClusterConfig(cr)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	cr.Status.AtProvider = v1alpha1.ClusterObservation{
		Ready:             ready,
		Host:              cr.Spec.ForProvider.Host,
		Port:              cr.Spec.ForProvider.Port,
		K3sVersion:        versionOut,
		ClusterInit:       cr.Spec.ForProvider.ClusterInit,
		DatastoreEndpoint: cr.Spec.ForProvider.DatastoreEndpoint,
	}
	// Uptest's import-recovery test compares this against the external-name
	// recorded before status was cleared -- set as its own statement (not
	// folded into the composite literal above) so the assignment stays
	// mechanically greppable as the identity write it is.
	cr.Status.AtProvider.ID = meta.GetExternalName(cr)
	if hasLast {
		cr.Status.AtProvider.K3sChannel = last.K3sChannel
		cr.Status.AtProvider.TLSSAN = last.TLSSAN
		cr.Status.AtProvider.DisableTraefik = last.DisableTraefik
		cr.Status.AtProvider.DisableServiceLB = last.DisableServiceLB
		cr.Status.AtProvider.ExtraArgs = last.ExtraArgs
	}

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	return managed.ExternalObservation{
		ResourceExists: true,
		// Readiness is part of desired state, not a side channel: a unit
		// stuck loaded-but-never-active must never be reported up to
		// date, or it is silently indistinguishable from a healthy,
		// converged cluster -- crossplane-runtime logs "external
		// resource is up to date" every poll and takes no further
		// action, forever. Gating on ready here means Update dispatches
		// the install script again on every not-ready poll -- safe,
		// since the script is idempotent and safe to re-run (see
		// clusterIsUpToDate) -- so a stuck server keeps producing
		// visible reconcile activity instead of silence. ResourceExists
		// staying true (unaffected by ready) is what keeps this from
		// reopening the create loop.
		ResourceUpToDate: ready && upToDate,
		// No server-defaulted spec fields to backfill: port and k3sChannel
		// already carry kubebuilder defaults the API server fills in before
		// this controller ever observes the resource, and the k3s install
		// script returns no other configuration this provider could adopt
		// into spec.
		ResourceLateInitialized: false,
		ConnectionDetails:       clusterConnectionDetails(e.host, kubeconfig, nodeToken),
	}, nil
}

// clusterConnectionDetails builds the connection secret payload from the
// endpoint (always known) plus whichever of kubeconfig and node-token were
// actually readable this pass -- both come back empty on a host where sudo
// access is unavailable, or while the server is still converging and has
// not written those files yet.
func clusterConnectionDetails(host, kubeconfig, nodeToken string) managed.ConnectionDetails {
	connDetails := managed.ConnectionDetails{
		"endpoint": []byte(fmt.Sprintf("https://%s:6443", host)),
	}
	if kubeconfig != "" {
		connDetails["kubeconfig"] = []byte(kubeconfig)
	}
	if nodeToken != "" {
		connDetails["node-token"] = []byte(nodeToken)
	}
	return connDetails
}

func (e *external) Create(ctx context.Context, cr *v1alpha1.Cluster) (managed.ExternalCreation, error) {
	cr.SetConditions(xpv2.Creating())

	cmd := k3s.InstallCommand(installParamsFor(cr.Spec.ForProvider))

	_, redactedStderr, err := e.ssh.Execute(ctx, cmd)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrapf(err, "cannot install k3s: %s", redactedStderr)
	}

	if err := persistLastAppliedClusterConfig(ctx, e.kube, cr); err != nil {
		return managed.ExternalCreation{}, err
	}

	return managed.ExternalCreation{}, nil
}

// Update re-runs the k3s install script with the resource's current
// configuration. The install script always rewrites the full systemd unit
// from the flags it is given and restarts the service, so — like a
// whole-object PUT — every field is echoed on every call, including the
// immutable ones (host, port, clusterInit, datastoreEndpoint): CEL rejects
// any change to them, so their value here always matches what is already
// running.
func (e *external) Update(ctx context.Context, cr *v1alpha1.Cluster) (managed.ExternalUpdate, error) {
	cmd := k3s.InstallCommand(installParamsFor(cr.Spec.ForProvider))

	_, redactedStderr, err := e.ssh.Execute(ctx, cmd)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrapf(err, "cannot reconfigure k3s: %s", redactedStderr)
	}

	if err := persistLastAppliedClusterConfig(ctx, e.kube, cr); err != nil {
		return managed.ExternalUpdate{}, err
	}

	return managed.ExternalUpdate{}, nil
}

// installParamsFor builds the FULL k3s install command parameters from the
// resource's current spec. The install script is a whole-object replace, so
// every field is echoed here, including the immutable ones (clusterInit,
// datastoreEndpoint): CEL rejects any change to them on this resource, so
// their value here always matches what is already running. host and port
// are not install-script flags at all — they address the SSH connection
// itself, not the k3s server configuration.
func installParamsFor(p v1alpha1.ClusterParameters) k3s.InstallParams {
	return k3s.InstallParams{
		K3sVersion:        p.K3sVersion,
		K3sChannel:        p.K3sChannel,
		ClusterInit:       p.ClusterInit,
		TLSSAN:            p.TLSSAN,
		DisableTraefik:    p.DisableTraefik,
		DisableServiceLB:  p.DisableServiceLB,
		ExtraArgs:         p.ExtraArgs,
		DatastoreEndpoint: p.DatastoreEndpoint,
	}
}

func (e *external) Delete(ctx context.Context, cr *v1alpha1.Cluster) (managed.ExternalDelete, error) {
	cr.SetConditions(xpv2.Deleting())

	_, redactedStderr, err := e.ssh.Execute(ctx, k3s.UninstallServerCommand())
	if err != nil {
		return managed.ExternalDelete{}, errors.Wrapf(err, "cannot uninstall k3s: %s", redactedStderr)
	}

	return managed.ExternalDelete{}, nil
}

func (e *external) Disconnect(ctx context.Context) error {
	return e.ssh.Close()
}

// mutableClusterFields is the subset of ClusterParameters this controller
// owns convergence for. host, port, clusterInit and datastoreEndpoint are
// immutable (enforced by CEL on the CRD) and excluded here: comparing them
// would report drift the API can never resolve, and Update would loop
// forever trying to correct a field it can't change.
type mutableClusterFields struct {
	K3sVersion       string `json:"k3sVersion"`
	K3sChannel       string `json:"k3sChannel"`
	TLSSAN           string `json:"tlsSAN"`
	DisableTraefik   bool   `json:"disableTraefik"`
	DisableServiceLB bool   `json:"disableServiceLB"`
	ExtraArgs        string `json:"extraArgs"`
}

func mutableClusterFieldsOf(p v1alpha1.ClusterParameters) mutableClusterFields {
	return mutableClusterFields{
		K3sVersion:       p.K3sVersion,
		K3sChannel:       p.K3sChannel,
		TLSSAN:           p.TLSSAN,
		DisableTraefik:   p.DisableTraefik,
		DisableServiceLB: p.DisableServiceLB,
		ExtraArgs:        p.ExtraArgs,
	}
}

// lastAppliedClusterConfig reads the mutable configuration this controller
// most recently confirmed it applied. It reports (zero value, false, nil)
// when nothing has been recorded yet -- freshly adopted, or created before
// this annotation existed -- which is the honest answer for Observe's
// atProvider mirror: there is nothing to report for those fields until this
// controller has actually confirmed a value against the host.
func lastAppliedClusterConfig(cr *v1alpha1.Cluster) (mutableClusterFields, bool, error) {
	raw, ok := cr.GetAnnotations()[annotationLastAppliedConfig]
	if !ok || raw == "" {
		return mutableClusterFields{}, false, nil
	}
	var last mutableClusterFields
	if err := json.Unmarshal([]byte(raw), &last); err != nil {
		return mutableClusterFields{}, false, errors.Wrap(err, errUnmarshalLastApplied)
	}
	return last, true, nil
}

// clusterIsUpToDate compares the resource's mutable fields against the
// configuration this controller last applied. The k3s install script never
// returns the server's own configuration, so there is nothing to compare
// spec against directly (convention: last-applied-config annotation
// pattern). A resource with no recorded annotation — freshly adopted, or
// created before this annotation existed — is reported as NOT up to date:
// the install script is safe to re-run with the resource's own declared
// configuration, and doing so once seeds the annotation.
func clusterIsUpToDate(cr *v1alpha1.Cluster) (bool, error) {
	last, ok, err := lastAppliedClusterConfig(cr)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return reflect.DeepEqual(last, mutableClusterFieldsOf(cr.Spec.ForProvider)), nil
}

// persistLastAppliedClusterConfig durably records the mutable configuration
// this controller just sent to the host, so the next Observe can detect
// drift. It writes directly to the API server -- retried on conflict --
// rather than mutating cr in memory and trusting the reconciler to persist
// it: crossplane-runtime writes back the full object after Create (and
// after an Observe that reports ResourceLateInitialized), but after a
// successful Update() it persists ONLY the status subresource. An
// annotation set inside Update() and left for the reconciler to carry
// forward is silently discarded, and the comparison it feeds never
// converges.
func persistLastAppliedClusterConfig(ctx context.Context, kube client.Client, cr *v1alpha1.Cluster) error {
	b, err := json.Marshal(mutableClusterFieldsOf(cr.Spec.ForProvider))
	if err != nil {
		return errors.Wrap(err, errMarshalLastApplied)
	}
	value := string(b)
	key := client.ObjectKeyFromObject(cr)

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.Cluster{}
		if err := kube.Get(ctx, key, latest); err != nil {
			return err
		}
		if latest.GetAnnotations()[annotationLastAppliedConfig] != value {
			meta.AddAnnotations(latest, map[string]string{annotationLastAppliedConfig: value})
			if err := kube.Update(ctx, latest); err != nil {
				return err
			}
		}
		// Mirror onto the in-memory object so this reconcile's own view is
		// current, even though the persisted write came from a fresh copy.
		meta.AddAnnotations(cr, map[string]string{annotationLastAppliedConfig: value})
		return nil
	})
}

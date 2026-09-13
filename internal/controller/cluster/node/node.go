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

package node

import (
	"context"
	"encoding/json"
	"reflect"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
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
	errGetCluster           = "cannot get referenced Cluster"
	errGetConnSecret        = "cannot get Cluster's connection secret"
	errNoNodeToken          = "Cluster connection secret has no node-token key"
	errNoConnSecretRef      = "referenced Cluster has no writeConnectionSecretToRef"
	errMarshalLastApplied   = "cannot marshal last-applied configuration"
	errUnmarshalLastApplied = "cannot unmarshal last-applied configuration"
)

// annotationLastAppliedConfig records the mutable NodeParameters this
// controller last sent to the host, so Observe can detect drift even though
// the k3s join script does not return the server's own configuration.
const annotationLastAppliedConfig = "k3s.crossplane.io/last-applied-node-config"

// externalTimeout bounds the cumulative duration of one reconcile's calls to
// the external API (Connect/Observe/Create/Update/Delete). Create and Update
// each dispatch the k3s agent/server join over SSH and return as soon as
// the remote restart is queued (see k3s.JoinCommand's doc comment) rather
// than blocking until the agent/server is actually ready, so this budget no
// longer has to cover the full join duration -- only the SSH round trip
// needed to install the binary, write the systemd unit and queue the
// restart. The margin here is deliberately generous rather than trimmed
// tight: crossplane-runtime persists the create-result annotation with the
// SAME reconcile context Create ran under, so a Create that merely times
// out promptly still needs enough of that budget left afterward for the
// write to land, or the resource wedges on a dangling
// external-create-pending annotation with no retry.
const externalTimeout = 10 * time.Minute

// No WithCreationGracePeriod override: Create no longer blocks until the
// agent/server is ready (see k3s.JoinCommand's doc comment) -- it returns
// once the join is queued on the host, so the resource genuinely may not
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

// Setup adds a controller that reconciles cluster-scoped Node managed resources.
//
// +kubebuilder:rbac:groups=k3s.crossplane.io,resources=nodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k3s.crossplane.io,resources=nodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=k3s.crossplane.io,resources=clusters,verbs=get;list;watch
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.NodeGroupKind)

	opts := []managed.ReconcilerOption{
		managed.WithTypedExternalConnector[*v1alpha1.Node](driftdetection.WrapConnector[*v1alpha1.Node](&connector{
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
			mgr.GetClient(), o.Logger, o.MetricOptions.MRStateMetrics, &v1alpha1.NodeList{}, o.MetricOptions.PollStateMetricInterval,
		)
		if err := mgr.Add(stateMetricsRecorder); err != nil {
			return errors.Wrap(err, "cannot register MR state metrics recorder for kind v1alpha1.NodeList")
		}
	}

	r := managed.NewReconciler(mgr, resource.ManagedKind(v1alpha1.NodeGroupVersionKind), opts...)

	return ctrl.NewControllerManagedBy(mgr).
		Named(name).
		WithOptions(o.ForControllerRuntime()).
		WithEventFilter(resource.DesiredStateChanged()).
		For(&v1alpha1.Node{}).
		Complete(ratelimiter.NewReconciler(name, r, o.GlobalRateLimiter))
}

type connector struct {
	kube  client.Client
	usage *resource.LegacyProviderConfigUsageTracker
}

func (c *connector) Connect(ctx context.Context, cr *v1alpha1.Node) (managed.TypedExternalClient[*v1alpha1.Node], error) {
	if err := c.usage.Track(ctx, cr); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	// Get cluster-scoped ProviderConfig
	ref := cr.GetProviderConfigReference()

	pc := &v1alpha1.ProviderConfig{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: ref.Name}, pc); err != nil {
		return nil, errors.Wrap(err, errGetPC)
	}

	data, err := resource.CommonCredentialExtractor(ctx, pc.Spec.Credentials.Source, c.kube, pc.Spec.Credentials.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

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

	// clusterRef/clusterSelector is optional in the schema (required instead
	// by a CEL rule gated on managementPolicies allowing Create or Update,
	// per convention) so an Observe-only adoption carrying just host can
	// pass admission. The reconciler resolves ClusterRef/ClusterSelector
	// into the Cluster value before Connect runs (Node implements
	// ResolveReferences), so by the time Create/Update needs it resolution
	// has already happened -- CEL already guarantees one of clusterRef or
	// clusterSelector is present whenever either write path can run. Skip
	// resolution when Cluster is still unset: Observe never needs
	// serverHost or nodeToken, only Create/Update do.
	//
	// A resolution failure (most commonly: the referenced Cluster was
	// deleted) must NOT fail Connect(): Observe and Delete never need
	// serverHost/nodeToken, and if Connect() itself errors, the managed
	// reconciler never reaches Delete at all -- the Node's finalizer can
	// then never clear once its Cluster is gone, and the only recovery is
	// a manual finalizer strip. Instead, remember the error and let
	// Create/Update -- the only two operations that actually need
	// serverHost/nodeToken -- fail loudly on it themselves.
	var serverHost, nodeToken string
	var clusterErr error
	if cr.Spec.ForProvider.Cluster != nil && *cr.Spec.ForProvider.Cluster != "" {
		serverHost, nodeToken, clusterErr = c.resolveClusterInfo(ctx, *cr.Spec.ForProvider.Cluster)
	}

	return &external{
		ssh:        sshClient,
		serverHost: serverHost,
		nodeToken:  nodeToken,
		clusterErr: clusterErr,
		role:       cr.Spec.ForProvider.Role,
		kube:       c.kube,
	}, nil
}

func (c *connector) resolveClusterInfo(ctx context.Context, clusterName string) (serverHost, nodeToken string, err error) {
	cluster := &v1alpha1.Cluster{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: clusterName}, cluster); err != nil {
		return "", "", errors.Wrap(err, errGetCluster)
	}

	connSecretRef := cluster.Spec.WriteConnectionSecretToReference
	if connSecretRef == nil {
		return "", "", errors.New(errNoConnSecretRef)
	}

	connSecret := &corev1.Secret{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: connSecretRef.Name, Namespace: connSecretRef.Namespace}, connSecret); err != nil {
		return "", "", errors.Wrap(err, errGetConnSecret)
	}

	token := string(connSecret.Data["node-token"])
	if token == "" {
		return "", "", errors.New(errNoNodeToken)
	}

	return cluster.Spec.ForProvider.Host, token, nil
}

type external struct {
	ssh        *sshclient.Client
	serverHost string
	nodeToken  string
	// clusterErr carries a Connect()-time clusterRef resolution failure
	// (see the comment in Connect above). Observe and Delete never
	// consult it -- only Create and Update do, since they are the only
	// operations that need serverHost/nodeToken to be genuinely resolved
	// rather than silently empty.
	clusterErr error
	role       string
	// kube writes the last-applied-config annotation directly to the API
	// server from Update(). crossplane-runtime does not persist an in-memory
	// annotation mutation made inside external.Update() -- only the status
	// subresource is written back after a successful Update -- so relying on
	// the reconciler here would silently discard the annotation forever.
	kube client.Client
}

func (e *external) Observe(ctx context.Context, cr *v1alpha1.Node) (managed.ExternalObservation, error) {
	// Identity is deterministic and known from spec before existence is
	// even checked (see "External-Name Strategy" above) -- keep the
	// annotation in sync on every pass rather than only setting it once.
	if meta.GetExternalName(cr) != cr.Spec.ForProvider.Host {
		meta.SetExternalName(cr, cr.Spec.ForProvider.Host)
	}

	service := k3s.ServiceNameForRole(e.role)

	stdout, _, err := e.ssh.Execute(ctx, k3s.ServiceProbeCommand(service))
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, "cannot check k3s status")
	}
	probe := k3s.ParseServiceProbe(stdout)
	if !probe.Exists() {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}
	ready := probe.Ready()

	versionOut, _, _ := e.ssh.Execute(ctx, "k3s --version 2>/dev/null | head -1")

	// Create and Update return as soon as the restart is queued (see
	// k3s.JoinCommand's doc comment), so a loaded-but-not-active read here is
	// a restart genuinely still in flight -- report Creating() explicitly
	// rather than leaving a stale Available condition in place from a
	// prior pass (an Update-driven restart, or a flapping unit, can revisit
	// this path after Available was already set once).
	if ready {
		cr.SetConditions(xpv2.Available())
	} else {
		cr.SetConditions(xpv2.Creating())
	}

	// The k3s join script reports no live configuration of its own, so the
	// last-applied-config annotation is this provider's only source for
	// the mutable fields' observed state (see nodeIsUpToDate and
	// lastAppliedNodeConfig below). Immutable fields can never diverge
	// from spec once the resource exists (CEL enforces it), so they mirror
	// straight from spec.
	last, hasLast, err := lastAppliedNodeConfig(cr)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	cr.Status.AtProvider = v1alpha1.NodeObservation{
		Ready:      ready,
		Host:       cr.Spec.ForProvider.Host,
		Port:       cr.Spec.ForProvider.Port,
		Role:       e.role,
		K3sVersion: versionOut,
	}
	// Uptest's import-recovery test compares this against the external-name
	// recorded before status was cleared -- set as its own statement (not
	// folded into the composite literal above) so the assignment stays
	// mechanically greppable as the identity write it is.
	cr.Status.AtProvider.ID = meta.GetExternalName(cr)
	// Cluster is *string in spec (unset on an Observe-only adoption that
	// has not resolved a reference yet) -- mirror it only once resolved,
	// rather than reporting an empty string as if it had been confirmed.
	if cr.Spec.ForProvider.Cluster != nil {
		cr.Status.AtProvider.Cluster = *cr.Spec.ForProvider.Cluster
	}
	if hasLast {
		cr.Status.AtProvider.K3sChannel = last.K3sChannel
		cr.Status.AtProvider.ExtraArgs = last.ExtraArgs
		cr.Status.AtProvider.TLSSAN = last.TLSSAN
	}

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	return managed.ExternalObservation{
		ResourceExists: true,
		// Readiness is part of desired state, not a side channel: a unit
		// stuck loaded-but-never-active (the agent can never reach a
		// server that itself never becomes ready) must never be reported
		// up to date, or it is silently indistinguishable from a healthy,
		// converged node -- crossplane-runtime logs "external resource is
		// up to date" every poll and takes no further action, forever.
		// Gating on ready here means Update dispatches the join script
		// again on every not-ready poll -- safe, since the script is
		// idempotent and safe to re-run (see nodeIsUpToDate) -- so a
		// stuck node keeps producing visible reconcile activity instead
		// of silence. ResourceExists staying true (unaffected by ready)
		// is what keeps this from reopening the create loop.
		ResourceUpToDate: ready && upToDate,
		// No server-defaulted spec fields to backfill: port and k3sChannel
		// already carry kubebuilder defaults the API server fills in before
		// this controller ever observes the resource, and the k3s join
		// script returns no other configuration this provider could adopt
		// into spec.
		ResourceLateInitialized: false,
	}, nil
}

// Create dispatches the k3s join over SSH and returns as soon as the
// install completes and the restart is queued (see k3s.JoinCommand's doc
// comment) -- it does not wait for the agent/server to actually finish
// joining and report ready. Observe's own poll loop picks up convergence
// from there.
func (e *external) Create(ctx context.Context, cr *v1alpha1.Node) (managed.ExternalCreation, error) {
	// A Create that silently proceeds with an empty nodeToken would join a
	// node to nothing, so a deferred clusterRef resolution failure from
	// Connect() must fail here rather than be swallowed.
	if e.clusterErr != nil {
		return managed.ExternalCreation{}, e.clusterErr
	}

	cr.SetConditions(xpv2.Creating())

	cmd := k3s.JoinCommand(joinParamsFor(cr.Spec.ForProvider, e.serverHost, e.nodeToken))

	_, redactedStderr, err := e.ssh.Execute(ctx, cmd)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrapf(err, "cannot join k3s cluster: %s", redactedStderr)
	}

	if err := persistLastAppliedNodeConfig(ctx, e.kube, cr); err != nil {
		return managed.ExternalCreation{}, err
	}

	return managed.ExternalCreation{}, nil
}

// Update re-runs the k3s join script with the resource's current
// configuration. The join script always rewrites the full systemd unit from
// the flags it is given and restarts the service, so — like a whole-object
// PUT — every field is echoed on every call, including the immutable ones
// (host, port, clusterRef, role): CEL rejects any change to them, so their
// value here always matches what is already running. Like Create, this
// returns as soon as the restart is queued rather than waiting for the
// reconfigured agent/server to report ready again (see k3s.JoinCommand's
// doc comment).
func (e *external) Update(ctx context.Context, cr *v1alpha1.Node) (managed.ExternalUpdate, error) {
	// Same reasoning as Create: a deferred clusterRef resolution failure
	// must not be swallowed by re-running the join script with an empty
	// nodeToken.
	if e.clusterErr != nil {
		return managed.ExternalUpdate{}, e.clusterErr
	}

	cmd := k3s.JoinCommand(joinParamsFor(cr.Spec.ForProvider, e.serverHost, e.nodeToken))

	_, redactedStderr, err := e.ssh.Execute(ctx, cmd)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrapf(err, "cannot reconfigure k3s node: %s", redactedStderr)
	}

	if err := persistLastAppliedNodeConfig(ctx, e.kube, cr); err != nil {
		return managed.ExternalUpdate{}, err
	}

	return managed.ExternalUpdate{}, nil
}

// joinParamsFor builds the FULL k3s join command parameters from the
// resource's current spec plus the connector-resolved cluster identity. The
// join script is a whole-object replace, so every field is echoed here,
// including the immutable ones (role): CEL rejects any change to it on this
// resource, so its value here always matches what is already running. host,
// port and clusterRef are not join-script flags at all — host/port address
// the SSH connection itself, and clusterRef has already been resolved into
// serverHost/nodeToken by Connect.
func joinParamsFor(p v1alpha1.NodeParameters, serverHost, nodeToken string) k3s.JoinParams {
	return k3s.JoinParams{
		ServerHost: serverHost,
		NodeToken:  nodeToken,
		Role:       p.Role,
		K3sVersion: p.K3sVersion,
		K3sChannel: p.K3sChannel,
		ExtraArgs:  p.ExtraArgs,
		TLSSAN:     p.TLSSAN,
	}
}

func (e *external) Delete(ctx context.Context, cr *v1alpha1.Node) (managed.ExternalDelete, error) {
	cr.SetConditions(xpv2.Deleting())

	var cmd string
	if cr.Spec.ForProvider.Role == "server" {
		cmd = k3s.UninstallServerCommand()
	} else {
		cmd = k3s.UninstallAgentCommand()
	}

	_, redactedStderr, err := e.ssh.Execute(ctx, cmd)
	if err != nil {
		return managed.ExternalDelete{}, errors.Wrapf(err, "cannot uninstall k3s: %s", redactedStderr)
	}

	return managed.ExternalDelete{}, nil
}

func (e *external) Disconnect(ctx context.Context) error {
	return e.ssh.Close()
}

// mutableNodeFields is the subset of NodeParameters this controller owns
// convergence for. host, port, clusterRef and role are immutable (enforced
// by CEL on the CRD) and excluded here: comparing them would report drift
// the API can never resolve, and Update would loop forever trying to
// correct a field it can't change.
type mutableNodeFields struct {
	K3sVersion string `json:"k3sVersion"`
	K3sChannel string `json:"k3sChannel"`
	ExtraArgs  string `json:"extraArgs"`
	TLSSAN     string `json:"tlsSAN"`
}

func mutableNodeFieldsOf(p v1alpha1.NodeParameters) mutableNodeFields {
	return mutableNodeFields{
		K3sVersion: p.K3sVersion,
		K3sChannel: p.K3sChannel,
		ExtraArgs:  p.ExtraArgs,
		TLSSAN:     p.TLSSAN,
	}
}

// lastAppliedNodeConfig reads the mutable configuration this controller
// most recently confirmed it applied. It reports (zero value, false, nil)
// when nothing has been recorded yet -- freshly adopted, or created before
// this annotation existed -- which is the honest answer for Observe's
// atProvider mirror: there is nothing to report for those fields until this
// controller has actually confirmed a value against the host.
func lastAppliedNodeConfig(cr *v1alpha1.Node) (mutableNodeFields, bool, error) {
	raw, ok := cr.GetAnnotations()[annotationLastAppliedConfig]
	if !ok || raw == "" {
		return mutableNodeFields{}, false, nil
	}
	var last mutableNodeFields
	if err := json.Unmarshal([]byte(raw), &last); err != nil {
		return mutableNodeFields{}, false, errors.Wrap(err, errUnmarshalLastApplied)
	}
	return last, true, nil
}

// nodeIsUpToDate compares the resource's mutable fields against the
// configuration this controller last applied. The k3s join script never
// returns the server's own configuration, so there is nothing to compare
// spec against directly (convention: last-applied-config annotation
// pattern). A resource with no recorded annotation — freshly adopted, or
// created before this annotation existed — is reported as NOT up to date:
// the join script is safe to re-run with the resource's own declared
// configuration, and doing so once seeds the annotation.
func nodeIsUpToDate(cr *v1alpha1.Node) (bool, error) {
	last, ok, err := lastAppliedNodeConfig(cr)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return reflect.DeepEqual(last, mutableNodeFieldsOf(cr.Spec.ForProvider)), nil
}

// persistLastAppliedNodeConfig durably records the mutable configuration
// this controller just sent to the host, so the next Observe can detect
// drift. It writes directly to the API server -- retried on conflict --
// rather than mutating cr in memory and trusting the reconciler to persist
// it: crossplane-runtime writes back the full object after Create (and
// after an Observe that reports ResourceLateInitialized), but after a
// successful Update() it persists ONLY the status subresource. An
// annotation set inside Update() and left for the reconciler to carry
// forward is silently discarded, and the comparison it feeds never
// converges.
func persistLastAppliedNodeConfig(ctx context.Context, kube client.Client, cr *v1alpha1.Node) error {
	b, err := json.Marshal(mutableNodeFieldsOf(cr.Spec.ForProvider))
	if err != nil {
		return errors.Wrap(err, errMarshalLastApplied)
	}
	value := string(b)
	key := client.ObjectKeyFromObject(cr)

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &v1alpha1.Node{}
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

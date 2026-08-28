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
	xpv1 "github.com/crossplane/crossplane/apis/v2/core/v2"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/namespaced/v1alpha1"
	sshclient "github.com/crossplane-contrib/provider-k3s/internal/clients/ssh"
	"github.com/crossplane-contrib/provider-k3s/internal/driftdetection"
	"github.com/crossplane-contrib/provider-k3s/internal/k3s"
)

const (
	errTrackPCUsage         = "cannot track ProviderConfig usage"
	errGetPC                = "cannot get ProviderConfig"
	errGetCPC               = "cannot get ClusterProviderConfig"
	errGetCreds             = "cannot get credentials"
	errNewClient            = "cannot create SSH client"
	errMarshalLastApplied   = "cannot marshal last-applied configuration"
	errUnmarshalLastApplied = "cannot unmarshal last-applied configuration"
)

// annotationLastAppliedConfig records the mutable ClusterParameters this
// controller last sent to the host, so Observe can detect drift even though
// the k3s install script does not return the server's own configuration.
const annotationLastAppliedConfig = "k3s.crossplane.io/last-applied-cluster-config"

// Setup adds a controller that reconciles namespaced Cluster managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.ClusterGroupKind)

	opts := []managed.ReconcilerOption{
		managed.WithTypedExternalConnector[*v1alpha1.Cluster](driftdetection.WrapConnector[*v1alpha1.Cluster](&connector{
			kube:  mgr.GetClient(),
			usage: resource.NewProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
		})),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))), //nolint:staticcheck // event.NewAPIRecorder still requires the legacy record.EventRecorder.
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
	usage *resource.ProviderConfigUsageTracker
}

func (c *connector) Connect(ctx context.Context, cr *v1alpha1.Cluster) (managed.TypedExternalClient[*v1alpha1.Cluster], error) {
	if err := c.usage.Track(ctx, cr); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	// Get ProviderConfig (namespaced or ClusterProviderConfig)
	ref := cr.GetProviderConfigReference()

	var pcSpec v1alpha1.ProviderConfigSpec
	var cd v1alpha1.ProviderCredentials

	switch ref.Kind {
	case "ProviderConfig":
		pc := &v1alpha1.ProviderConfig{}
		if err := c.kube.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: cr.GetNamespace()}, pc); err != nil {
			return nil, errors.Wrap(err, errGetPC)
		}
		pcSpec = pc.Spec
		cd = pc.Spec.Credentials
	case "ClusterProviderConfig":
		cpc := &v1alpha1.ClusterProviderConfig{}
		if err := c.kube.Get(ctx, types.NamespacedName{Name: ref.Name}, cpc); err != nil {
			return nil, errors.Wrap(err, errGetCPC)
		}
		pcSpec = cpc.Spec
		cd = cpc.Spec.Credentials
	default:
		return nil, errors.Errorf("unsupported provider config kind: %s", ref.Kind)
	}

	data, err := resource.CommonCredentialExtractor(ctx, cd.Source, c.kube, cd.CommonCredentialSelectors)
	if err != nil {
		return nil, errors.Wrap(err, errGetCreds)
	}

	sshCfg := sshclient.Config{
		Host:     cr.Spec.ForProvider.Host,
		Port:     cr.Spec.ForProvider.Port,
		Username: pcSpec.Username,
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
	stdout, _, err := e.ssh.Execute("systemctl is-active k3s 2>/dev/null || echo inactive")
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, "cannot check k3s status")
	}
	if stdout != "active" {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	versionOut, _, _ := e.ssh.Execute("k3s --version 2>/dev/null | head -1")
	cr.Status.AtProvider.K3sVersion = versionOut
	cr.Status.AtProvider.Ready = true

	nodeToken, _, _ := e.ssh.Execute("sudo cat /var/lib/rancher/k3s/server/node-token 2>/dev/null")

	kubeconfig, _, _ := e.ssh.Execute("sudo cat /etc/rancher/k3s/k3s.yaml 2>/dev/null")
	if kubeconfig != "" {
		kubeconfig = k3s.RewriteKubeconfig(kubeconfig, e.host)
	}

	cr.SetConditions(xpv1.Available())

	upToDate, err := clusterIsUpToDate(cr)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	connDetails := managed.ConnectionDetails{
		"endpoint": []byte(fmt.Sprintf("https://%s:6443", e.host)),
	}
	if kubeconfig != "" {
		connDetails["kubeconfig"] = []byte(kubeconfig)
	}
	if nodeToken != "" {
		connDetails["node-token"] = []byte(nodeToken)
	}

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: upToDate,
		// No server-defaulted spec fields to backfill: port and k3sChannel
		// already carry kubebuilder defaults the API server fills in before
		// this controller ever observes the resource, and the k3s install
		// script returns no other configuration this provider could adopt
		// into spec.
		ResourceLateInitialized: false,
		ConnectionDetails:       connDetails,
	}, nil
}

func (e *external) Create(ctx context.Context, cr *v1alpha1.Cluster) (managed.ExternalCreation, error) {
	cr.SetConditions(xpv1.Creating())

	cmd := k3s.InstallCommand(installParamsFor(cr.Spec.ForProvider))

	_, stderr, err := e.ssh.Execute(cmd)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrapf(err, "cannot install k3s: %s", stderr)
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

	_, stderr, err := e.ssh.Execute(cmd)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrapf(err, "cannot reconfigure k3s: %s", stderr)
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
	cr.SetConditions(xpv1.Deleting())

	_, stderr, err := e.ssh.Execute(k3s.UninstallServerCommand())
	if err != nil {
		return managed.ExternalDelete{}, errors.Wrapf(err, "cannot uninstall k3s: %s", stderr)
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

// clusterIsUpToDate compares the resource's mutable fields against the
// configuration this controller last applied. The k3s install script never
// returns the server's own configuration, so there is nothing to compare
// spec against directly (convention: last-applied-config annotation
// pattern). A resource with no recorded annotation — freshly adopted, or
// created before this annotation existed — is reported as NOT up to date:
// the install script is safe to re-run with the resource's own declared
// configuration, and doing so once seeds the annotation.
func clusterIsUpToDate(cr *v1alpha1.Cluster) (bool, error) {
	raw, ok := cr.GetAnnotations()[annotationLastAppliedConfig]
	if !ok || raw == "" {
		return false, nil
	}
	var last mutableClusterFields
	if err := json.Unmarshal([]byte(raw), &last); err != nil {
		return false, errors.Wrap(err, errUnmarshalLastApplied)
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

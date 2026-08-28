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
	xpv1 "github.com/crossplane/crossplane/apis/v2/core/v2"

	v1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/cluster/v1alpha1"
	sshclient "github.com/crossplane-contrib/provider-k3s/internal/clients/ssh"
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

// Setup adds a controller that reconciles cluster-scoped Node managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.NodeGroupKind)

	opts := []managed.ReconcilerOption{
		managed.WithTypedExternalConnector[*v1alpha1.Node](driftdetection.WrapConnector[*v1alpha1.Node](&connector{
			kube:  mgr.GetClient(),
			usage: resource.NewLegacyProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
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

	serverHost, nodeToken, err := c.resolveClusterInfo(ctx, cr.Spec.ForProvider.ClusterRef.Name)
	if err != nil {
		sshClient.Close() //nolint:errcheck
		return nil, err
	}

	return &external{
		ssh:        sshClient,
		serverHost: serverHost,
		nodeToken:  nodeToken,
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
	role       string
	// kube writes the last-applied-config annotation directly to the API
	// server from Update(). crossplane-runtime does not persist an in-memory
	// annotation mutation made inside external.Update() -- only the status
	// subresource is written back after a successful Update -- so relying on
	// the reconciler here would silently discard the annotation forever.
	kube client.Client
}

func (e *external) Observe(ctx context.Context, cr *v1alpha1.Node) (managed.ExternalObservation, error) {
	service := "k3s-agent"
	if e.role == "server" {
		service = "k3s"
	}

	stdout, _, err := e.ssh.Execute("systemctl is-active " + service + " 2>/dev/null || echo inactive")
	if err != nil {
		return managed.ExternalObservation{}, errors.Wrap(err, "cannot check k3s status")
	}
	if stdout != "active" {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	cr.Status.AtProvider.Ready = true
	cr.Status.AtProvider.Role = e.role
	cr.SetConditions(xpv1.Available())

	upToDate, err := nodeIsUpToDate(cr)
	if err != nil {
		return managed.ExternalObservation{}, err
	}

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: upToDate,
		// No server-defaulted spec fields to backfill: port and k3sChannel
		// already carry kubebuilder defaults the API server fills in before
		// this controller ever observes the resource, and the k3s join
		// script returns no other configuration this provider could adopt
		// into spec.
		ResourceLateInitialized: false,
	}, nil
}

func (e *external) Create(ctx context.Context, cr *v1alpha1.Node) (managed.ExternalCreation, error) {
	cr.SetConditions(xpv1.Creating())

	cmd := k3s.JoinCommand(joinParamsFor(cr.Spec.ForProvider, e.serverHost, e.nodeToken))

	_, stderr, err := e.ssh.Execute(cmd)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrapf(err, "cannot join k3s cluster: %s", stderr)
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
// value here always matches what is already running.
func (e *external) Update(ctx context.Context, cr *v1alpha1.Node) (managed.ExternalUpdate, error) {
	cmd := k3s.JoinCommand(joinParamsFor(cr.Spec.ForProvider, e.serverHost, e.nodeToken))

	_, stderr, err := e.ssh.Execute(cmd)
	if err != nil {
		return managed.ExternalUpdate{}, errors.Wrapf(err, "cannot reconfigure k3s node: %s", stderr)
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
	cr.SetConditions(xpv1.Deleting())

	var cmd string
	if cr.Spec.ForProvider.Role == "server" {
		cmd = k3s.UninstallServerCommand()
	} else {
		cmd = k3s.UninstallAgentCommand()
	}

	_, stderr, err := e.ssh.Execute(cmd)
	if err != nil {
		return managed.ExternalDelete{}, errors.Wrapf(err, "cannot uninstall k3s: %s", stderr)
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

// nodeIsUpToDate compares the resource's mutable fields against the
// configuration this controller last applied. The k3s join script never
// returns the server's own configuration, so there is nothing to compare
// spec against directly (convention: last-applied-config annotation
// pattern). A resource with no recorded annotation — freshly adopted, or
// created before this annotation existed — is reported as NOT up to date:
// the join script is safe to re-run with the resource's own declared
// configuration, and doing so once seeds the annotation.
func nodeIsUpToDate(cr *v1alpha1.Node) (bool, error) {
	raw, ok := cr.GetAnnotations()[annotationLastAppliedConfig]
	if !ok || raw == "" {
		return false, nil
	}
	var last mutableNodeFields
	if err := json.Unmarshal([]byte(raw), &last); err != nil {
		return false, errors.Wrap(err, errUnmarshalLastApplied)
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

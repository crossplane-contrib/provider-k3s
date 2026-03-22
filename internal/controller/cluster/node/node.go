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

	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	xpv1 "github.com/crossplane/crossplane-runtime/v2/apis/common/v1"
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	"github.com/crossplane/crossplane-runtime/v2/pkg/event"
	"github.com/crossplane/crossplane-runtime/v2/pkg/ratelimiter"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/statemetrics"

	v1alpha1 "github.com/crossplane/provider-k3s/apis/cluster/v1alpha1"
	sshclient "github.com/crossplane/provider-k3s/internal/clients/ssh"
	"github.com/crossplane/provider-k3s/internal/k3s"
)

const (
	errNotNode         = "managed resource is not a Node custom resource"
	errTrackPCUsage    = "cannot track ProviderConfig usage"
	errGetPC           = "cannot get ProviderConfig"
	errGetCreds        = "cannot get credentials"
	errNewClient       = "cannot create SSH client"
	errGetCluster      = "cannot get referenced Cluster"
	errGetConnSecret   = "cannot get Cluster's connection secret"
	errNoNodeToken     = "Cluster connection secret has no node-token key"
	errNoConnSecretRef = "referenced Cluster has no writeConnectionSecretToRef"
)

// Setup adds a controller that reconciles cluster-scoped Node managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.NodeGroupKind)

	opts := []managed.ReconcilerOption{
		managed.WithExternalConnector(&connector{
			kube:  mgr.GetClient(),
			usage: resource.NewProviderConfigUsageTracker(mgr.GetClient(), &v1alpha1.ProviderConfigUsage{}),
		}),
		managed.WithLogger(o.Logger.WithValues("controller", name)),
		managed.WithPollInterval(o.PollInterval),
		managed.WithRecorder(event.NewAPIRecorder(mgr.GetEventRecorderFor(name))),
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
	usage *resource.ProviderConfigUsageTracker
}

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Node)
	if !ok {
		return nil, errors.New(errNotNode)
	}

	if err := c.usage.Track(ctx, cr); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	// Get cluster-scoped ProviderConfig
	m := mg.(resource.ModernManaged)
	ref := m.GetProviderConfigReference()

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

	credStr := string(data)
	if len(data) > 0 && len(credStr) >= 10 && credStr[:10] == "-----BEGIN" {
		sshCfg.PrivateKey = data
	} else {
		sshCfg.Password = credStr
	}

	sshClient, err := sshclient.NewClient(sshCfg)
	if err != nil {
		return nil, errors.Wrap(err, errNewClient)
	}

	// Resolve Cluster reference to get server host and node token
	clusterRef := cr.Spec.ForProvider.ClusterRef
	cluster := &v1alpha1.Cluster{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: clusterRef.Name}, cluster); err != nil {
		sshClient.Close() //nolint:errcheck
		return nil, errors.Wrap(err, errGetCluster)
	}

	serverHost := cluster.Spec.ForProvider.Host

	connSecretRef := cluster.Spec.WriteConnectionSecretToReference
	if connSecretRef == nil {
		sshClient.Close() //nolint:errcheck
		return nil, errors.New(errNoConnSecretRef)
	}

	connSecret := &corev1.Secret{}
	if err := c.kube.Get(ctx, types.NamespacedName{Name: connSecretRef.Name, Namespace: cluster.GetNamespace()}, connSecret); err != nil {
		sshClient.Close() //nolint:errcheck
		return nil, errors.Wrap(err, errGetConnSecret)
	}

	nodeToken := string(connSecret.Data["node-token"])
	if nodeToken == "" {
		sshClient.Close() //nolint:errcheck
		return nil, errors.New(errNoNodeToken)
	}

	return &external{
		ssh:        sshClient,
		serverHost: serverHost,
		nodeToken:  nodeToken,
		role:       cr.Spec.ForProvider.Role,
	}, nil
}

type external struct {
	ssh        *sshclient.Client
	serverHost string
	nodeToken  string
	role       string
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Node)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotNode)
	}

	service := "k3s-agent"
	if e.role == "server" {
		service = "k3s"
	}

	stdout, _, err := e.ssh.Execute("systemctl is-active " + service + " 2>/dev/null || echo inactive")
	if err != nil || stdout != "active" {
		return managed.ExternalObservation{ResourceExists: false}, nil
	}

	cr.Status.AtProvider.Ready = true
	cr.Status.AtProvider.Role = e.role
	cr.SetConditions(xpv1.Available())

	return managed.ExternalObservation{
		ResourceExists:   true,
		ResourceUpToDate: true,
	}, nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Node)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotNode)
	}

	cr.SetConditions(xpv1.Creating())

	cmd := k3s.JoinCommand(k3s.JoinParams{
		ServerHost: e.serverHost,
		NodeToken:  e.nodeToken,
		Role:       cr.Spec.ForProvider.Role,
		K3sVersion: cr.Spec.ForProvider.K3sVersion,
		K3sChannel: cr.Spec.ForProvider.K3sChannel,
		ExtraArgs:  cr.Spec.ForProvider.ExtraArgs,
		TLSSAN:     cr.Spec.ForProvider.TLSSAN,
	})

	_, stderr, err := e.ssh.Execute(cmd)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrapf(err, "cannot join k3s cluster: %s", stderr)
	}

	return managed.ExternalCreation{}, nil
}

func (e *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	return managed.ExternalUpdate{}, nil
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) (managed.ExternalDelete, error) {
	cr, ok := mg.(*v1alpha1.Node)
	if !ok {
		return managed.ExternalDelete{}, errors.New(errNotNode)
	}

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

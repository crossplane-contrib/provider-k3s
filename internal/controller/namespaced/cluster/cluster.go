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
	"fmt"

	"github.com/crossplane/crossplane-runtime/v2/pkg/feature"
	"github.com/pkg/errors"
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

	v1alpha1 "github.com/crossplane/provider-k3s/apis/namespaced/v1alpha1"
	sshclient "github.com/crossplane/provider-k3s/internal/clients/ssh"
	"github.com/crossplane/provider-k3s/internal/k3s"
)

const (
	errNotCluster   = "managed resource is not a Cluster custom resource"
	errTrackPCUsage = "cannot track ProviderConfig usage"
	errGetPC        = "cannot get ProviderConfig"
	errGetCPC       = "cannot get ClusterProviderConfig"
	errGetCreds     = "cannot get credentials"
	errNewClient    = "cannot create SSH client"
)

// Setup adds a controller that reconciles namespaced Cluster managed resources.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	name := managed.ControllerName(v1alpha1.ClusterGroupKind)

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

func (c *connector) Connect(ctx context.Context, mg resource.Managed) (managed.ExternalClient, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return nil, errors.New(errNotCluster)
	}

	if err := c.usage.Track(ctx, cr); err != nil {
		return nil, errors.Wrap(err, errTrackPCUsage)
	}

	// Get ProviderConfig (namespaced or ClusterProviderConfig)
	m := mg.(resource.ModernManaged)
	ref := m.GetProviderConfigReference()

	var pcSpec v1alpha1.ProviderConfigSpec
	var cd v1alpha1.ProviderCredentials

	switch ref.Kind {
	case "ProviderConfig":
		pc := &v1alpha1.ProviderConfig{}
		if err := c.kube.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: m.GetNamespace()}, pc); err != nil {
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

	return &external{
		ssh:  sshClient,
		host: cr.Spec.ForProvider.Host,
	}, nil
}

type external struct {
	ssh  *sshclient.Client
	host string
}

func (e *external) Observe(ctx context.Context, mg resource.Managed) (managed.ExternalObservation, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalObservation{}, errors.New(errNotCluster)
	}

	stdout, _, err := e.ssh.Execute("systemctl is-active k3s 2>/dev/null || echo inactive")
	if err != nil || stdout != "active" {
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
		ResourceExists:    true,
		ResourceUpToDate:  true,
		ConnectionDetails: connDetails,
	}, nil
}

func (e *external) Create(ctx context.Context, mg resource.Managed) (managed.ExternalCreation, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalCreation{}, errors.New(errNotCluster)
	}

	cr.SetConditions(xpv1.Creating())

	cmd := k3s.InstallCommand(k3s.InstallParams{
		K3sVersion:        cr.Spec.ForProvider.K3sVersion,
		K3sChannel:        cr.Spec.ForProvider.K3sChannel,
		ClusterInit:       cr.Spec.ForProvider.ClusterInit,
		TLSSAN:            cr.Spec.ForProvider.TLSSAN,
		DisableTraefik:    cr.Spec.ForProvider.DisableTraefik,
		DisableServiceLB:  cr.Spec.ForProvider.DisableServiceLB,
		ExtraArgs:         cr.Spec.ForProvider.ExtraArgs,
		DatastoreEndpoint: cr.Spec.ForProvider.DatastoreEndpoint,
	})

	_, stderr, err := e.ssh.Execute(cmd)
	if err != nil {
		return managed.ExternalCreation{}, errors.Wrapf(err, "cannot install k3s: %s", stderr)
	}

	return managed.ExternalCreation{}, nil
}

func (e *external) Update(ctx context.Context, mg resource.Managed) (managed.ExternalUpdate, error) {
	return managed.ExternalUpdate{}, nil
}

func (e *external) Delete(ctx context.Context, mg resource.Managed) (managed.ExternalDelete, error) {
	cr, ok := mg.(*v1alpha1.Cluster)
	if !ok {
		return managed.ExternalDelete{}, errors.New(errNotCluster)
	}

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

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
	"github.com/crossplane/crossplane-runtime/v2/pkg/controller"
	ctrl "sigs.k8s.io/controller-runtime"

	clusterv1alpha1 "github.com/crossplane-contrib/provider-k3s/apis/cluster/v1alpha1"
	"github.com/crossplane-contrib/provider-k3s/internal/controller/cluster/cluster"
	"github.com/crossplane-contrib/provider-k3s/internal/controller/cluster/config"
	"github.com/crossplane-contrib/provider-k3s/internal/controller/cluster/node"
)

// Setup creates all cluster-scoped k3s controllers with the supplied logger and adds them to
// the supplied manager.
func Setup(mgr ctrl.Manager, o controller.Options) error {
	for _, setup := range []func(ctrl.Manager, controller.Options) error{
		config.Setup,
		cluster.Setup,
		node.Setup,
	} {
		if err := setup(mgr, o); err != nil {
			return err
		}
	}
	return nil
}

// SetupGated creates all cluster-scoped k3s controllers with SafeStart capability.
func SetupGated(mgr ctrl.Manager, o controller.Options) error {
	o.Gate.Register(func() {
		if err := config.Setup(mgr, o); err != nil {
			panic(err)
		}
	}, clusterv1alpha1.ProviderConfigGroupVersionKind)

	o.Gate.Register(func() {
		if err := cluster.Setup(mgr, o); err != nil {
			panic(err)
		}
	}, clusterv1alpha1.ClusterGroupVersionKind)

	o.Gate.Register(func() {
		if err := node.Setup(mgr, o); err != nil {
			panic(err)
		}
	}, clusterv1alpha1.NodeGroupVersionKind)

	return nil
}

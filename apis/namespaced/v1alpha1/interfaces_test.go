package v1alpha1

import (
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
)

// Compile-time assertions that Cluster and Node satisfy the
// crossplane-runtime interfaces the managed reconciler requires. Both are
// namespaced-scoped MRs: they embed xpv2.ManagedResourceSpec and must
// satisfy ModernManaged, not LegacyManaged. A silent regression here (a
// dropped angryjet marker, an accidental swap to ClusterManagedResourceSpec,
// or a missing object-root marker) still compiles cleanly — the break only
// surfaces later when the reconciler is wired up, far from its cause.
var (
	_ resource.Managed       = &Cluster{}
	_ resource.ModernManaged = &Cluster{}

	_ resource.Managed       = &Node{}
	_ resource.ModernManaged = &Node{}
)

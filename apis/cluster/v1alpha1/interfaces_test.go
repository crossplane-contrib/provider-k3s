package v1alpha1

import (
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
)

// Compile-time assertions that Cluster and Node satisfy the
// crossplane-runtime interfaces the managed reconciler requires. Both are
// cluster-scoped MRs: they embed xpv2.ClusterManagedResourceSpec and must
// satisfy LegacyManaged, not ModernManaged. A silent regression here (a
// dropped angryjet marker, an accidental swap to ManagedResourceSpec, or a
// missing object-root marker) still compiles cleanly — the break only
// surfaces later when the reconciler is wired up, far from its cause.
var (
	_ resource.Managed = &Cluster{}
	//nolint:staticcheck // LegacyManaged is deprecated in favor of ModernManaged, but cluster-scoped MRs still must satisfy it; this assertion intentionally pins that requirement.
	_ resource.LegacyManaged = &Cluster{}

	_ resource.Managed = &Node{}
	//nolint:staticcheck // LegacyManaged is deprecated in favor of ModernManaged, but cluster-scoped MRs still must satisfy it; this assertion intentionally pins that requirement.
	_ resource.LegacyManaged = &Node{}
)

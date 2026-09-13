package resources

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestDiffersFromDetectsLegacyAggregateQuotaKeys(t *testing.T) {
	quota := &v1.ResourceQuota{Spec: v1.ResourceQuotaSpec{Hard: v1.ResourceList{
		v1.ResourceCPU:            resource.MustParse("1"),
		v1.ResourceMemory:         resource.MustParse("1Gi"),
		v1.ResourceRequestsCPU:    resource.MustParse("1"),
		v1.ResourceLimitsCPU:      resource.MustParse("1"),
		v1.ResourceRequestsMemory: resource.MustParse("1Gi"),
		v1.ResourceLimitsMemory:   resource.MustParse("1Gi"),
	}}}
	desired := &Resources{Cpu: 1000, Memory: 1074, RequestsCpu: 1000, RequestsMemory: 1074}

	if !desired.DiffersFrom(quota) {
		t.Fatal("expected legacy cpu and memory keys to require a cleanup patch")
	}
}

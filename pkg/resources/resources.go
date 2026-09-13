package resources

import (
	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// This package combines Cpu and Memory resources into a single struct, combined with some actions upon them.
// For ease of use each function updates a struct pointer, as well as returns a reference to that struct. This
// allows chaining of function calls. For example:
//
//  example := &Resources{Cpu: 1}
//  example.Add(&Resources{Cpu: 1}).Add(&Resources{Cpu: 1}) // example.Cpu = 3
//  example.Limit(&Resources{Cpu: 2})                       // example.Cpu = 2

// Resources combines Cpu and Memory into a single container
type Resources struct {
	Cpu            int64 // CPU limits in millicores (or plain CPU for non-quota capacity calculations)
	Memory         int64 // Memory limits in megabytes (or plain memory for non-quota capacity calculations)
	RequestsCpu    int64 // CPU requests in millicores for ResourceQuota calculations
	RequestsMemory int64 // Memory requests in megabytes for ResourceQuota calculations
	Storage        int64 // Storage is included here for logging purposes, but not intended to be scaled (yet)
}

// Add adds the new resources to the existing resources. Result is updated and also returned.
func (res *Resources) Add(new *Resources) *Resources {
	res.Cpu += new.Cpu
	res.Memory += new.Memory
	res.RequestsCpu += new.RequestsCpu
	res.RequestsMemory += new.RequestsMemory
	res.Storage += new.Storage
	return res
}

// Replace replaces fields in `res` if they are non-default in `new`. Result is updated and also returned.
func (res *Resources) Replace(new *Resources) *Resources {
	if new == nil {
		return res
	}

	if new.Cpu != 0 {
		res.Cpu = new.Cpu
	}
	if new.Memory != 0 {
		res.Memory = new.Memory
	}
	if new.RequestsCpu != 0 {
		res.RequestsCpu = new.RequestsCpu
	}
	if new.RequestsMemory != 0 {
		res.RequestsMemory = new.RequestsMemory
	}

	if new.Storage != 0 {
		res.Storage = new.Storage
	}

	return res
}

// Limit limits the resources with a maximum of the specified limit. Result is updated and also returned.
func (res *Resources) Limit(limit *Resources) *Resources {
	if res.Cpu > limit.Cpu {
		res.Cpu = limit.Cpu
	}
	if res.Memory > limit.Memory {
		res.Memory = limit.Memory
	}
	if res.RequestsCpu > limit.RequestsCpu {
		res.RequestsCpu = limit.RequestsCpu
	}
	if res.RequestsMemory > limit.RequestsMemory {
		res.RequestsMemory = limit.RequestsMemory
	}
	return res
}

// Max updates res with the maximum values of res and new. Result is updated and also returned.
func (res *Resources) Max(new *Resources) *Resources {
	if new.Cpu > res.Cpu {
		res.Cpu = new.Cpu
	}
	if new.Memory > res.Memory {
		res.Memory = new.Memory
	}
	if new.RequestsCpu > res.RequestsCpu {
		res.RequestsCpu = new.RequestsCpu
	}
	if new.RequestsMemory > res.RequestsMemory {
		res.RequestsMemory = new.RequestsMemory
	}
	return res
}

// IsEmpty returns true when Cpu and Memory are both zero.
func (res *Resources) IsEmpty() bool {
	return res.Cpu == 0 && res.Memory == 0 && res.RequestsCpu == 0 && res.RequestsMemory == 0
}

func (res *Resources) DiffersFrom(quota *v1.ResourceQuota) bool {
	// Explicit request/limit quotas replaced the legacy aggregate aliases. A
	// merge patch must still run once to remove aliases left by older versions.
	if _, legacy := quota.Spec.Hard[v1.ResourceCPU]; legacy {
		_, requests := quota.Spec.Hard[v1.ResourceRequestsCPU]
		_, limits := quota.Spec.Hard[v1.ResourceLimitsCPU]
		if requests || limits {
			return true
		}
	}
	if _, legacy := quota.Spec.Hard[v1.ResourceMemory]; legacy {
		_, requests := quota.Spec.Hard[v1.ResourceRequestsMemory]
		_, limits := quota.Spec.Hard[v1.ResourceLimitsMemory]
		if requests || limits {
			return true
		}
	}
	if res.Cpu != resourceValue(quota.Spec.Hard, v1.ResourceLimitsCPU, v1.ResourceCPU, resource.Milli) {
		return true
	}
	if res.Memory != resourceValue(quota.Spec.Hard, v1.ResourceLimitsMemory, v1.ResourceMemory, resource.Mega) {
		return true
	}
	if res.RequestsCpu != resourceValue(quota.Spec.Hard, v1.ResourceRequestsCPU, v1.ResourceCPU, resource.Milli) {
		return true
	}
	if res.RequestsMemory != resourceValue(quota.Spec.Hard, v1.ResourceRequestsMemory, v1.ResourceMemory, resource.Mega) {
		return true
	}
	return false
}

func (res *Resources) IsScaleDown(quota *v1.ResourceQuota) bool {
	return res.Cpu < resourceValue(quota.Spec.Hard, v1.ResourceLimitsCPU, v1.ResourceCPU, resource.Milli) ||
		res.Memory < resourceValue(quota.Spec.Hard, v1.ResourceLimitsMemory, v1.ResourceMemory, resource.Mega) ||
		res.RequestsCpu < resourceValue(quota.Spec.Hard, v1.ResourceRequestsCPU, v1.ResourceCPU, resource.Milli) ||
		res.RequestsMemory < resourceValue(quota.Spec.Hard, v1.ResourceRequestsMemory, v1.ResourceMemory, resource.Mega)
}

func (res *Resources) ForceNoScaleDownWhenScaleUp(quota *v1.ResourceQuota) {
	current := Resources{
		Cpu:            resourceValue(quota.Spec.Hard, v1.ResourceLimitsCPU, v1.ResourceCPU, resource.Milli),
		Memory:         resourceValue(quota.Spec.Hard, v1.ResourceLimitsMemory, v1.ResourceMemory, resource.Mega),
		RequestsCpu:    resourceValue(quota.Spec.Hard, v1.ResourceRequestsCPU, v1.ResourceCPU, resource.Milli),
		RequestsMemory: resourceValue(quota.Spec.Hard, v1.ResourceRequestsMemory, v1.ResourceMemory, resource.Mega),
	}
	if res.Cpu > current.Cpu || res.Memory > current.Memory || res.RequestsCpu > current.RequestsCpu || res.RequestsMemory > current.RequestsMemory {
		if res.Cpu < current.Cpu {
			res.Cpu = current.Cpu
		}
		if res.Memory < current.Memory {
			res.Memory = current.Memory
		}
		if res.RequestsCpu < current.RequestsCpu {
			res.RequestsCpu = current.RequestsCpu
		}
		if res.RequestsMemory < current.RequestsMemory {
			res.RequestsMemory = current.RequestsMemory
		}
		logging.LogInfo("[%s] Prevented mixed quota scale-up and scale-down", quota.Namespace)
	}
}

func resourceValue(list v1.ResourceList, name, legacyName v1.ResourceName, scale resource.Scale) int64 {
	if value, ok := list[name]; ok {
		return value.ScaledValue(scale)
	}
	value := list[legacyName]
	return value.ScaledValue(scale)
}

// EnsureRequestsFitLimits keeps the aggregate request quotas from exceeding the
// corresponding limit quotas after the four dimensions have scaled independently.
func (res *Resources) EnsureRequestsFitLimits() {
	if res.RequestsCpu > res.Cpu {
		res.Cpu = res.RequestsCpu
	}
	if res.RequestsMemory > res.Memory {
		res.Memory = res.RequestsMemory
	}
}

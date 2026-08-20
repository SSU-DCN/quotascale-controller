package quota

// This file validates and calculates QuotaAutoscaler behavior. After validating a QuotaAutoscaler CRD
// instance using `ValidateQuotaScaler` the behavior policies can be effectuated. This causes the desired
// scale to be calculated.
//
// Usage:
//  scaler := &QuotaAutoscaler{ ... }
//  quota := &ResourceQuota{ ... }
//
//  validatedScaler, err := ValidateQuotaScaler(scaler)
//  if err != nil { panic(err) }
//
//  for _, policy := range scaler.Spec.Behavior.ScaleDown.Policies {
//    isScalingUp := false
//    active, desired := validatedScaler.ActivatePolicy(isScalingUp, policy, quota)
//    if desired != -1 { writeYourScaleDownFunction(active) }
//  }

import (
	"strings"

	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	v1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	"github.com/SSU-DCN/quotascale-controller/pkg/utils"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

type ValidatedQuotaScaler struct {
	MinCpu int64 `json:"min.cpu,omitempty"`
	MaxCpu int64 `json:"max.cpu,omitempty"`

	MinMemory int64 `json:"min.memory,omitempty"`
	MaxMemory int64 `json:"max.memory,omitempty"`
}

type ActivePolicy struct {
	IsCpu                  bool
	CurrentMaximum         int64
	CurrentUsagePercentage int64
	PolicyThreshold        int64
	TargetUtilization      int64
	QuotaLimit             int64
	Used                   int64
}

// ValidateQuotaScaler validates all fields of the given QuotaAutoscaler and converts them to Milli Cores for
// CPU and Mega Bytes for Memory. When no values are provided defaults are filled in.
func ValidateQuotaScaler(scaler *v1.QuotaAutoscaler) *ValidatedQuotaScaler {
	spec := scaler.Spec
	return &ValidatedQuotaScaler{
		MinCpu:    ParseQuantityWithDefault(spec.MinCpu, resource.Milli, 400),
		MaxCpu:    ParseQuantityWithDefault(spec.MaxCpu, resource.Milli, 35000),
		MinMemory: ParseQuantityWithDefault(spec.MinMemory, resource.Mega, 1000),
		MaxMemory: ParseQuantityWithDefault(spec.MaxMemory, resource.Mega, 150000),
	}
}

// ParseQuantityWithDefault attempts to parse the given value as the provided scale. Default is used when
// parsing fails.
func ParseQuantityWithDefault(value string, scale resource.Scale, def int64) int64 {
	if value == "" {
		return def
	}

	parsedValue, err := resource.ParseQuantity(value)
	if err != nil {
		logging.LogError("Unable to parse quantity %s: %s", value, err.Error())
		return def
	}

	return parsedValue.ScaledValue(scale)
}

func (scaler *ValidatedQuotaScaler) ForceLimitToDefaultMax() {
	// Get the default maximum values, these are the max allowed
	defaultScaler := ValidateQuotaScaler(&v1.QuotaAutoscaler{})

	if scaler.MaxMemory > defaultScaler.MaxMemory {
		scaler.MaxMemory = defaultScaler.MaxMemory
	}
	if scaler.MaxCpu > defaultScaler.MaxCpu {
		scaler.MaxCpu = defaultScaler.MaxCpu
	}
}

// ToActivePolicy converts a QuotaScalePolicy to an ActivePolicy given the scaleUp type and ResourceQuota values.
func (scaler *ValidatedQuotaScaler) ToActivePolicy(scaleUp bool, policy v1.QuotaScalePolicy, quota *v12.ResourceQuota) *ActivePolicy {
	active := &ActivePolicy{
		PolicyThreshold:   int64(policy.Value),
		TargetUtilization: int64(policy.TargetUtilization),
	}
	if active.TargetUtilization == 0 {
		active.TargetUtilization = active.PolicyThreshold
	}

	if strings.ToLower(policy.Method) == "memory" {
		active.IsCpu = false
		active.CurrentMaximum = quota.Spec.Hard.Memory().ScaledValue(resource.Mega)
		active.Used = ResourceQuotaUsedMemoryLimit(quota).ScaledValue(resource.Mega)

		if scaleUp {
			active.QuotaLimit = scaler.MaxMemory
		} else {
			active.QuotaLimit = scaler.MinMemory
		}
	} else if strings.ToLower(policy.Method) == "cpu" {
		active.IsCpu = true
		active.CurrentMaximum = quota.Spec.Hard.Cpu().ScaledValue(resource.Milli)
		active.Used = ResourceQuotaUsedCpuLimit(quota).ScaledValue(resource.Milli)

		if scaleUp {
			active.QuotaLimit = scaler.MaxCpu
		} else {
			active.QuotaLimit = scaler.MinCpu
		}
	}

	if active.CurrentMaximum != 0 {
		active.CurrentUsagePercentage = int64(float64(active.Used) / float64(active.CurrentMaximum) * 100)
	}
	return active
}

// ActivatePolicy first converts a QuotaScalePolicy to an ActivePolicy given the scaleUp type and ResourceQuota values.
// It then uses the active policy to calculate the desired scaling value (Milli Cores for CPU and Mega Bytes for
// Memory). When a policy is not in effect (no scaling should occur) it returns the converted policy and 0.
func (scaler *ValidatedQuotaScaler) ActivatePolicy(scaleUp bool, policy v1.QuotaScalePolicy, quota *v12.ResourceQuota) (*ActivePolicy, int64) {
	active := scaler.ToActivePolicy(scaleUp, policy, quota)
	if active.TargetUtilization <= 0 {
		logging.LogError("Invalid target utilization %d for %s policy", active.TargetUtilization, policy.Method)
		return active, 0
	}
	if active.PolicyThreshold == 100 {
		// When PolicyThreshold is 100 we will never scale up based on used ResourceQuota, because Used quota will always
		// be lower or equal to 100 percent. Instead, events need to bump up the quota.
		if !scaleUp && active.Used != active.CurrentMaximum {
			return active, utils.Max(active.Used, active.QuotaLimit)
		}
	} else {
		if scaleUp && active.CurrentUsagePercentage > active.PolicyThreshold {
			return active, CalculateScaleUp(active)
		} else if !scaleUp && active.CurrentUsagePercentage < active.PolicyThreshold {
			return active, CalculateScaleDown(active)
		}
	}

	return active, 0 // Nothing to do
}

func (scaler *ValidatedQuotaScaler) ActivateScalerPolicy(policy v1.QuotaScalePolicy, quota *v12.ResourceQuota, scaleUp bool) *resources.Resources {
	desired := &resources.Resources{}

	active, target := scaler.ActivatePolicy(scaleUp, policy, quota)
	if target != 0 {
		if active.IsCpu {
			desired.Cpu = target
		} else {
			desired.Memory = target
		}
	}

	return desired
}

// CalculateScaleUp calculates the desired value a quota should have given the scaleUp policy.
func CalculateScaleUp(policy *ActivePolicy) int64 {
	desired := calculateDesiredForTargetUtilization(policy)
	return utils.Min(desired, policy.QuotaLimit)
}

// CalculateScaleDown calculates the desired value a quota should have given the scaleDown policy.
func CalculateScaleDown(policy *ActivePolicy) int64 {
	desired := calculateDesiredForTargetUtilization(policy)
	return utils.Max(desired, policy.QuotaLimit)
}

func calculateDesiredForTargetUtilization(policy *ActivePolicy) int64 {
	return int64(float64(policy.Used) * 100 / float64(policy.TargetUtilization))
}

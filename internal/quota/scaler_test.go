package quota

import (
	"testing"

	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestActivatePolicyUsesTargetUtilizationForScaleUp(t *testing.T) {
	scaler := ValidateQuotaScaler(&scalerv1.QuotaAutoscaler{
		Spec: scalerv1.QuotaAutoscalerSpec{
			MaxCpu: "10",
		},
	})
	quota := resourceQuotaWithCPU("1", "900m")
	policy := scalerv1.QuotaScalePolicy{
		Method:            "cpu",
		Value:             80,
		TargetUtilization: 50,
	}

	active, desired := scaler.ActivatePolicy(true, policy, quota)

	if active.CurrentUsagePercentage != 90 {
		t.Fatalf("expected current usage to be 90%%, got %d%%", active.CurrentUsagePercentage)
	}
	if desired != 1800 {
		t.Fatalf("expected desired CPU to target 50%% utilization at 1800m, got %dm", desired)
	}
}

func TestActivatePolicyUsesTargetUtilizationForScaleDown(t *testing.T) {
	scaler := ValidateQuotaScaler(&scalerv1.QuotaAutoscaler{
		Spec: scalerv1.QuotaAutoscalerSpec{
			MinCpu: "100m",
		},
	})
	quota := resourceQuotaWithCPU("1", "300m")
	policy := scalerv1.QuotaScalePolicy{
		Method:            "cpu",
		Value:             40,
		TargetUtilization: 50,
	}

	active, desired := scaler.ActivatePolicy(false, policy, quota)

	if active.CurrentUsagePercentage != 30 {
		t.Fatalf("expected current usage to be 30%%, got %d%%", active.CurrentUsagePercentage)
	}
	if desired != 600 {
		t.Fatalf("expected desired CPU to target 50%% utilization at 600m, got %dm", desired)
	}
}

func TestActivatePolicyDefaultsTargetUtilizationToValue(t *testing.T) {
	scaler := ValidateQuotaScaler(&scalerv1.QuotaAutoscaler{
		Spec: scalerv1.QuotaAutoscalerSpec{
			MaxCpu: "10",
		},
	})
	quota := resourceQuotaWithCPU("1", "900m")
	policy := scalerv1.QuotaScalePolicy{
		Method: "cpu",
		Value:  80,
	}

	active, desired := scaler.ActivatePolicy(true, policy, quota)

	if active.TargetUtilization != 80 {
		t.Fatalf("expected target utilization to default to value 80, got %d", active.TargetUtilization)
	}
	if desired != 1125 {
		t.Fatalf("expected desired CPU to preserve old value-based target at 1125m, got %dm", desired)
	}
}

func TestActivatePolicyUsesCPULimitsWhenHigherThanRequests(t *testing.T) {
	scaler := ValidateQuotaScaler(&scalerv1.QuotaAutoscaler{
		Spec: scalerv1.QuotaAutoscalerSpec{MaxCpu: "10"},
	})
	quota := resourceQuotaWithCPU("4", "1")
	quota.Status.Used[corev1.ResourceLimitsCPU] = resource.MustParse("3")
	policy := scalerv1.QuotaScalePolicy{
		Method:            "cpu",
		Value:             70,
		TargetUtilization: 50,
	}

	active, desired := scaler.ActivatePolicy(true, policy, quota)

	if active.Used != 3000 {
		t.Fatalf("expected CPU limit usage 3000m, got %dm", active.Used)
	}
	if active.CurrentUsagePercentage != 75 {
		t.Fatalf("expected limit-backed usage to be 75%%, got %d%%", active.CurrentUsagePercentage)
	}
	if desired != 6000 {
		t.Fatalf("expected desired CPU quota 6000m, got %dm", desired)
	}
}

func resourceQuotaWithCPU(hard, used string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(hard),
			},
		},
		Status: corev1.ResourceQuotaStatus{
			Used: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse(used),
			},
		},
	}
}

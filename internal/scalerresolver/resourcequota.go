package scalerresolver

import (
	"context"
	"fmt"

	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	scalerclient "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const DefaultResourceQuotaName = "resource-quota"

func ResolveResourceQuota(ctx context.Context, kubeClient kubernetes.Interface, quotaAutoscalerClient scalerclient.Interface, scaler *scalerv1.QuotaAutoscaler) (*corev1.ResourceQuota, error) {
	if kubeClient == nil {
		return nil, fmt.Errorf("kubernetes client is not configured")
	}
	if scaler == nil {
		return nil, fmt.Errorf("quota autoscaler is nil")
	}

	if scaler.Spec.ResourceQuota != "" {
		return kubeClient.CoreV1().ResourceQuotas(scaler.Namespace).Get(ctx, scaler.Spec.ResourceQuota, metav1.GetOptions{})
	}

	cpu, err := resource.ParseQuantity(scaler.Spec.MinCpu)
	if err != nil {
		return nil, fmt.Errorf("invalid spec.min.cpu %q: %w", scaler.Spec.MinCpu, err)
	}
	memory, err := resource.ParseQuantity(scaler.Spec.MinMemory)
	if err != nil {
		return nil, fmt.Errorf("invalid spec.min.memory %q: %w", scaler.Spec.MinMemory, err)
	}

	controller := true
	blockOwnerDeletion := true
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultResourceQuotaName,
			Namespace: scaler.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         scalerv1.SchemeGroupVersion.String(),
				Kind:               "QuotaAutoscaler",
				Name:               scaler.Name,
				UID:                scaler.UID,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}},
		},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    cpu,
			corev1.ResourceLimitsCPU:      cpu,
			corev1.ResourceRequestsMemory: memory,
			corev1.ResourceLimitsMemory:   memory,
		}},
	}

	createdQuota, err := kubeClient.CoreV1().ResourceQuotas(scaler.Namespace).Create(ctx, quota, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		createdQuota, err = kubeClient.CoreV1().ResourceQuotas(scaler.Namespace).Get(ctx, DefaultResourceQuotaName, metav1.GetOptions{})
		if err == nil && !isControlledByScaler(createdQuota, scaler) {
			return nil, fmt.Errorf("ResourceQuota %s/%s already exists and is not controlled by QuotaAutoscaler %s; set spec.resourceQuota explicitly to adopt it", scaler.Namespace, DefaultResourceQuotaName, scaler.Name)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("create managed ResourceQuota %s/%s: %w", scaler.Namespace, DefaultResourceQuotaName, err)
	}

	scaler.Spec.ResourceQuota = createdQuota.Name
	if quotaAutoscalerClient == nil {
		logging.LogWarning("[%s/%s] Created or resolved managed ResourceQuota %s, but QuotaAutoscaler client is unavailable so spec.resourceQuota was not persisted", scaler.Namespace, scaler.Name, createdQuota.Name)
		return createdQuota, nil
	}

	updatedScaler, updateErr := quotaAutoscalerClient.IchpV1().QuotaAutoscalers(scaler.Namespace).Update(ctx, scaler, metav1.UpdateOptions{})
	if updateErr != nil {
		logging.LogWarning("[%s/%s] Created or resolved managed ResourceQuota %s, but failed to persist spec.resourceQuota: %v", scaler.Namespace, scaler.Name, createdQuota.Name, updateErr)
		return createdQuota, nil
	}

	*scaler = *updatedScaler
	logging.LogInfo("[%s/%s] Automatically set spec.resourceQuota to managed ResourceQuota %s", scaler.Namespace, scaler.Name, createdQuota.Name)
	return createdQuota, nil
}

func isControlledByScaler(quota *corev1.ResourceQuota, scaler *scalerv1.QuotaAutoscaler) bool {
	for _, owner := range quota.OwnerReferences {
		if owner.Controller != nil && *owner.Controller &&
			owner.APIVersion == scalerv1.SchemeGroupVersion.String() &&
			owner.Kind == "QuotaAutoscaler" && owner.Name == scaler.Name {
			return true
		}
	}
	return false
}

package scalerresolver

import (
	"context"
	"fmt"

	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	scalerclient "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

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

	quotas, err := kubeClient.CoreV1().ResourceQuotas(scaler.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	if len(quotas.Items) == 0 {
		return nil, fmt.Errorf("no ResourceQuota found in namespace %s", scaler.Namespace)
	}
	if len(quotas.Items) > 1 {
		return nil, fmt.Errorf("multiple ResourceQuota objects found in namespace %s; set spec.resourceQuota explicitly", scaler.Namespace)
	}

	resolvedQuota := quotas.Items[0]
	scaler.Spec.ResourceQuota = resolvedQuota.Name
	if quotaAutoscalerClient == nil {
		logging.LogWarning("[%s/%s] Resolved ResourceQuota %s automatically, but QuotaAutoscaler client is unavailable so spec.resourceQuota was not persisted", scaler.Namespace, scaler.Name, resolvedQuota.Name)
		return &resolvedQuota, nil
	}

	updatedScaler, updateErr := quotaAutoscalerClient.IchpV1().QuotaAutoscalers(scaler.Namespace).Update(ctx, scaler, metav1.UpdateOptions{})
	if updateErr != nil {
		logging.LogWarning("[%s/%s] Resolved ResourceQuota %s automatically, but failed to persist spec.resourceQuota: %v", scaler.Namespace, scaler.Name, resolvedQuota.Name, updateErr)
		return &resolvedQuota, nil
	}

	*scaler = *updatedScaler
	logging.LogInfo("[%s/%s] Automatically set spec.resourceQuota to %s", scaler.Namespace, scaler.Name, resolvedQuota.Name)
	return &resolvedQuota, nil
}

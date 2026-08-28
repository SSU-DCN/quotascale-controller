package scalerresolver

import (
	"context"
	"testing"

	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	scalerfake "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestResolveResourceQuotaUsesExplicitName(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "compute-resources", Namespace: "default"},
		},
	)
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		Spec:       scalerv1.QuotaAutoscalerSpec{ResourceQuota: "compute-resources"},
	}

	quota, err := ResolveResourceQuota(context.TODO(), kubeClient, nil, scaler)
	if err != nil {
		t.Fatalf("expected explicit ResourceQuota resolution to succeed, got error: %v", err)
	}
	if quota.Name != "compute-resources" {
		t.Fatalf("expected compute-resources, got %s", quota.Name)
	}
}

func TestResolveResourceQuotaCreatesManagedQuotaFromMinimums(t *testing.T) {
	kubeClient := fake.NewSimpleClientset()
	scalerClient := scalerfake.NewSimpleClientset(
		&scalerv1.QuotaAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		},
	)
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default", UID: "qa-uid"},
		Spec: scalerv1.QuotaAutoscalerSpec{
			MinCpu:    "6",
			MinMemory: "6Gi",
		},
	}

	quota, err := ResolveResourceQuota(context.TODO(), kubeClient, scalerClient, scaler)
	if err != nil {
		t.Fatalf("expected automatic ResourceQuota resolution to succeed, got error: %v", err)
	}
	if quota.Name != DefaultResourceQuotaName {
		t.Fatalf("expected %s, got %s", DefaultResourceQuotaName, quota.Name)
	}
	if scaler.Spec.ResourceQuota != DefaultResourceQuotaName {
		t.Fatalf("expected scaler spec.resourceQuota to be updated in-memory, got %q", scaler.Spec.ResourceQuota)
	}
	for _, name := range []corev1.ResourceName{
		corev1.ResourceRequestsCPU,
		corev1.ResourceLimitsCPU,
	} {
		if got := quota.Spec.Hard[name]; got.String() != "6" {
			t.Fatalf("expected %s to be 6, got %s", name, got.String())
		}
	}
	for _, name := range []corev1.ResourceName{
		corev1.ResourceRequestsMemory,
		corev1.ResourceLimitsMemory,
	} {
		if got := quota.Spec.Hard[name]; got.String() != "6Gi" {
			t.Fatalf("expected %s to be 6Gi, got %s", name, got.String())
		}
	}
	if len(quota.OwnerReferences) != 1 || quota.OwnerReferences[0].Name != "qa" {
		t.Fatalf("expected managed quota to be owned by qa, got %#v", quota.OwnerReferences)
	}

	persisted, err := scalerClient.IchpV1().QuotaAutoscalers("default").Get(context.TODO(), "qa", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected persisted QuotaAutoscaler to be readable, got error: %v", err)
	}
	if persisted.Spec.ResourceQuota != DefaultResourceQuotaName {
		t.Fatalf("expected persisted spec.resourceQuota to be %s, got %q", DefaultResourceQuotaName, persisted.Spec.ResourceQuota)
	}
}

func TestResolveResourceQuotaDoesNotAdoptUnrelatedQuota(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default"},
		},
	)
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		Spec:       scalerv1.QuotaAutoscalerSpec{MinCpu: "1", MinMemory: "1Gi"},
	}

	quota, err := ResolveResourceQuota(context.TODO(), kubeClient, nil, scaler)
	if err != nil {
		t.Fatalf("expected managed ResourceQuota creation to succeed: %v", err)
	}
	if quota.Name != DefaultResourceQuotaName {
		t.Fatalf("expected %s, got %s", DefaultResourceQuotaName, quota.Name)
	}
	quotas, err := kubeClient.CoreV1().ResourceQuotas("default").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(quotas.Items) != 2 {
		t.Fatalf("expected unrelated and managed quotas to coexist, got %d", len(quotas.Items))
	}
}

func TestResolveResourceQuotaRejectsInvalidMinimum(t *testing.T) {
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		Spec:       scalerv1.QuotaAutoscalerSpec{MinCpu: "invalid", MinMemory: "1Gi"},
	}

	if _, err := ResolveResourceQuota(context.TODO(), fake.NewSimpleClientset(), nil, scaler); err == nil {
		t.Fatal("expected invalid spec.min.cpu to be rejected")
	}
}

func TestResolveResourceQuotaRefusesDefaultNameCollision(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(&corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultResourceQuotaName, Namespace: "default"},
	})
	scaler := &scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "qa", Namespace: "default"},
		Spec:       scalerv1.QuotaAutoscalerSpec{MinCpu: "1", MinMemory: "1Gi"},
	}

	if _, err := ResolveResourceQuota(context.TODO(), kubeClient, nil, scaler); err == nil {
		t.Fatal("expected an unrelated resource-quota name collision to be rejected")
	}
}

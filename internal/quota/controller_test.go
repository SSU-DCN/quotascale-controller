package quota

import (
	"strings"
	"testing"

	"github.com/SSU-DCN/quotascale-controller/internal/nodescaling"
	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
)

type captureScaleOutRequestHandler struct {
	called  bool
	request nodescaling.ScaleOutRequest
}

func (handler *captureScaleOutRequestHandler) HandleScaleOutRequest(request nodescaling.ScaleOutRequest) error {
	handler.called = true
	handler.request = request
	return nil
}

func TestRegisterNamespacedEventMarksQuotaDeniedEventsAsImmediate(t *testing.T) {
	watcher := &QuotaWatcher{
		Events: map[string][]corev1.Event{},
	}

	namespace, immediate := watcher.RegisterNamespacedEvent(watch.Event{
		Type: watch.Added,
		Object: &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "failed-create",
				Namespace: "default",
			},
			Reason:  "FailedCreate",
			Message: "Error creating: pods \"demo\" is forbidden: exceeded quota: compute-resources, requested: limits.cpu=1, used: limits.cpu=1, limited: limits.cpu=1",
			InvolvedObject: corev1.ObjectReference{
				Kind:      "ReplicaSet",
				Namespace: "default",
				Name:      "demo-rs",
			},
		},
	})
	if namespace != "default" {
		t.Fatalf("expected namespace default, got %q", namespace)
	}
	if !immediate {
		t.Fatalf("expected quota denied event to trigger immediate reconciliation")
	}
	if len(watcher.Events["default"]) != 1 {
		t.Fatalf("expected event to be stored for namespace default")
	}
}

func TestRegisterNamespacedEventKeepsNonQuotaFailuresAggregated(t *testing.T) {
	watcher := &QuotaWatcher{
		Events: map[string][]corev1.Event{},
	}

	_, immediate := watcher.RegisterNamespacedEvent(watch.Event{
		Type: watch.Added,
		Object: &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "failed-create",
				Namespace: "default",
			},
			Reason:  "FailedCreate",
			Message: "Error creating: pods \"demo\" is forbidden: error looking up service account default/default: serviceaccount \"default\" not found",
			InvolvedObject: corev1.ObjectReference{
				Kind:      "ReplicaSet",
				Namespace: "default",
				Name:      "demo-rs",
			},
		},
	})
	if immediate {
		t.Fatalf("expected non-quota failure to stay on aggregated path")
	}
}

func TestUpdateQuotaIfRequiredRechecksCapacityAfterScaleOut(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1500m"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		},
	)

	handler := &captureScaleOutRequestHandler{}
	watcher := &QuotaWatcher{
		Client:                 client,
		ScaleOutRequestHandler: handler,
	}

	quota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quota",
			Namespace: "default",
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU:             resource.MustParse("1"),
				corev1.ResourceMemory:          resource.MustParse("1Gi"),
				corev1.ResourceLimitsCPU:       resource.MustParse("1"),
				corev1.ResourceLimitsMemory:    resource.MustParse("1Gi"),
				corev1.ResourceRequestsCPU:     resource.MustParse("1"),
				corev1.ResourceRequestsMemory:  resource.MustParse("1Gi"),
				corev1.ResourceRequestsStorage: resource.MustParse("0"),
			},
		},
		Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("1Gi"),
			},
			Used: corev1.ResourceList{
				corev1.ResourceCPU:          resource.MustParse("900m"),
				corev1.ResourceMemory:       resource.MustParse("512Mi"),
				corev1.ResourceLimitsCPU:    resource.MustParse("900m"),
				corev1.ResourceLimitsMemory: resource.MustParse("512Mi"),
			},
		},
	}

	scaler := scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quotascale-controller",
			Namespace: "default",
		},
		Spec: scalerv1.QuotaAutoscalerSpec{
			ResourceQuota: "quota",
			MaxCpu:        "4",
			MaxMemory:     "8Gi",
			Behavior: scalerv1.QuotaAutoscalerSpecBehavior{
				ScaleUp: scalerv1.QuotaScaleBehavior{
					Policies: []scalerv1.QuotaScalePolicy{
						{
							Method:            "cpu",
							Value:             80,
							TargetUtilization: 50,
						},
					},
				},
			},
		},
	}

	err := watcher.UpdateQuotaIfRequired(quota, scaler, nil)
	if err == nil {
		t.Fatalf("expected capacity recheck error after scale-out request")
	}
	if !handler.called {
		t.Fatalf("expected scale-out handler to be called")
	}
	if handler.request.Namespace != "default" {
		t.Fatalf("unexpected handler namespace: %s", handler.request.Namespace)
	}
	if handler.request.Current.Cpu != 1000 {
		t.Fatalf("unexpected current cpu resources: %+v", handler.request.Current)
	}
	if handler.request.Desired.Cpu <= handler.request.Current.Cpu {
		t.Fatalf("expected desired cpu to exceed current cpu, got current=%+v desired=%+v", handler.request.Current, handler.request.Desired)
	}
	if !strings.Contains(err.Error(), "node scale-out completed but desired quota is still unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

package quota

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/SSU-DCN/quotascale-controller/internal/nodescaling"
	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	scalerfake "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned/fake"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
)

type captureScaleOutRequestHandler struct {
	called  bool
	count   int
	request nodescaling.ScaleOutRequest
}

func TestNamespaceNeedsScaleOutUsesCPULimitUsageForFailedCreates(t *testing.T) {
	replicas := int32(11)
	client := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			}},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "classifier-rs", Namespace: "default"},
			Spec: appsv1.ReplicaSetSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")},
					},
				}}}},
			},
			Status: appsv1.ReplicaSetStatus{Replicas: 9},
		},
	)
	watcher := &QuotaWatcher{Client: client}
	quota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "resource-quota", Namespace: "default"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceCPU:             resource.MustParse("35"),
			corev1.ResourceMemory:          resource.MustParse("50Gi"),
			corev1.ResourceRequestsStorage: resource.MustParse("0"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceCPU:       resource.MustParse("11275m"),
			corev1.ResourceLimitsCPU: resource.MustParse("33"),
		}},
	}
	scaler := scalerv1.QuotaAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		Spec:       scalerv1.QuotaAutoscalerSpec{MaxCpu: "50", MaxMemory: "50Gi"},
	}
	events := []corev1.Event{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{
			Kind: "ReplicaSet", Namespace: "default", Name: "classifier-rs",
		},
	}}

	if !watcher.namespaceNeedsScaleOut(quota, scaler, events) {
		t.Fatal("expected missing pods to require scale-out when CPU limit usage plus missing limits exceeds current quota")
	}
}

func (handler *captureScaleOutRequestHandler) HandleScaleOutRequest(request nodescaling.ScaleOutRequest) error {
	handler.called = true
	handler.count++
	handler.request = request
	return nil
}

func TestRegisterNamespacedEventMarksQuotaDeniedEventsAsImmediate(t *testing.T) {
	watcher := &QuotaWatcher{
		Events:  map[string][]corev1.Event{},
		Started: time.Now().UTC().Add(-time.Minute),
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
		Events:  map[string][]corev1.Event{},
		Started: time.Now().UTC().Add(-time.Minute),
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

func TestRegisterNamespacedEventIgnoresHistoricalEventsBeforeWatcherStart(t *testing.T) {
	watcher := &QuotaWatcher{
		Events:  map[string][]corev1.Event{},
		Started: time.Now().UTC(),
	}

	namespace, immediate := watcher.RegisterNamespacedEvent(watch.Event{
		Type: watch.Added,
		Object: &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "old-failed-create",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(time.Now().UTC().Add(-10 * time.Minute)),
			},
			LastTimestamp: metav1.NewTime(time.Now().UTC().Add(-9 * time.Minute)),
			Reason:        "FailedCreate",
			Message:       "Error creating: pods \"demo\" is forbidden: exceeded quota: compute-resources",
			InvolvedObject: corev1.ObjectReference{
				Kind:      "ReplicaSet",
				Namespace: "default",
				Name:      "demo-rs",
			},
		},
	})
	if namespace != "" {
		t.Fatalf("expected historical event to be ignored, got namespace %q", namespace)
	}
	if immediate {
		t.Fatalf("expected historical event not to trigger immediate reconciliation")
	}
	if len(watcher.Events) != 0 {
		t.Fatalf("expected ignored historical event not to be stored")
	}
}

func TestRegisterNamespacedEventKeepsNewEventsAfterWatcherStart(t *testing.T) {
	watcher := &QuotaWatcher{
		Events:  map[string][]corev1.Event{},
		Started: time.Now().UTC().Add(-time.Minute),
	}

	namespace, immediate := watcher.RegisterNamespacedEvent(watch.Event{
		Type: watch.Added,
		Object: &corev1.Event{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "new-failed-create",
				Namespace:         "default",
				CreationTimestamp: metav1.NewTime(time.Now().UTC()),
			},
			EventTime: metav1.MicroTime{Time: time.Now().UTC()},
			Reason:    "FailedCreate",
			Message:   "Error creating: pods \"demo\" is forbidden: exceeded quota: compute-resources",
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
		t.Fatalf("expected new quota denied event to trigger immediate reconciliation")
	}
	if len(watcher.Events["default"]) != 1 {
		t.Fatalf("expected new event to be stored for namespace default")
	}
}

func TestUpdateQuotaIfRequiredDefersQuotaResizeAfterScaleOutRequest(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
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
		t.Fatalf("expected deferred result after scale-out request")
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

	var deferredErr *QuotaDeferredError
	if !errors.As(err, &deferredErr) {
		t.Fatalf("expected deferred error type, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "node scale-out requested; waiting for worker capacity before resizing quota") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateQuotaIfRequiredDeduplicatesPendingScaleOutRequests(t *testing.T) {
	client := fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		},
	)

	handler := &captureScaleOutRequestHandler{}
	watcher := &QuotaWatcher{
		Client:                 client,
		ScaleOutRequestHandler: handler,
		pendingScaleOut:        map[string]struct{}{},
	}

	quota := corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quota",
			Namespace: "default",
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceCPU:          resource.MustParse("1"),
				corev1.ResourceMemory:       resource.MustParse("1Gi"),
				corev1.ResourceLimitsCPU:    resource.MustParse("1"),
				corev1.ResourceLimitsMemory: resource.MustParse("1Gi"),
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
		t.Fatalf("expected deferred result after first scale-out request")
	}
	err = watcher.UpdateQuotaIfRequired(quota, scaler, nil)
	if err == nil {
		t.Fatalf("expected deferred result while scale-out is still pending")
	}
	if handler.count != 1 {
		t.Fatalf("expected only one scale-out request to be sent while pending, got %d", handler.count)
	}
	if !strings.Contains(err.Error(), "node scale-out already pending") {
		t.Fatalf("unexpected second deferred error: %v", err)
	}
}

func TestRegisterScalerEventAutoResolvesSingleResourceQuota(t *testing.T) {
	kubeClient := fake.NewSimpleClientset(
		&corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "compute-resources",
				Namespace: "default",
			},
		},
	)
	scalerClient := scalerfake.NewSimpleClientset(
		&scalerv1.QuotaAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "qa",
				Namespace: "default",
			},
		},
	)
	watcher := &QuotaWatcher{
		Scalers:               map[string]scalerv1.QuotaAutoscaler{},
		Quotas:                map[string]corev1.ResourceQuota{},
		Events:                map[string][]corev1.Event{},
		Client:                kubeClient,
		QuotaAutoscalerClient: scalerClient,
	}

	namespace := watcher.RegisterScalerEvent(watch.Event{
		Type: watch.Added,
		Object: &scalerv1.QuotaAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "qa",
				Namespace: "default",
			},
		},
	})
	if namespace != "default" {
		t.Fatalf("expected namespace default, got %q", namespace)
	}

	storedScaler := watcher.Scalers["default"]
	if storedScaler.Spec.ResourceQuota != "compute-resources" {
		t.Fatalf("expected watcher scaler spec.resourceQuota to be auto-populated, got %q", storedScaler.Spec.ResourceQuota)
	}
	if watcher.Quotas["default"].Name != "compute-resources" {
		t.Fatalf("expected watcher quota cache to be populated, got %#v", watcher.Quotas["default"])
	}

	persistedScaler, err := scalerClient.IchpV1().QuotaAutoscalers("default").Get(context.TODO(), "qa", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected persisted QuotaAutoscaler to be readable, got error: %v", err)
	}
	if persistedScaler.Spec.ResourceQuota != "compute-resources" {
		t.Fatalf("expected persisted spec.resourceQuota to be auto-populated, got %q", persistedScaler.Spec.ResourceQuota)
	}
}

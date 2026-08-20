package quota

// This file contains functions that take namespaced ResourceQuota and QuotaAutoscaler events and turn
// them into behaviour which can invoke the resize API. Event watching is the responsibility of the
// calling function, this also includes stream resets.
//
// Example usage:
//  quotas, _ := core.ResourceQuotas("").Watch(context.TODO(), v1.ListOptions{})
//  scalers, _ := ichp.QuotaAutoscalers("").Watch(context.TODO(), v1.ListOptions{})
//  events, _ := client.CoreV1().Events("").Watch(context.TODO(), v1.ListOptions{})
//
//  // This is a blocking call, until either watcher channel terminates
//  internal.WatchQuotas(client, startScalers, quotas.ResultChan(), scalers.ResultChan(), events.ResultChan(), time.Minute)
//
//	quotas.Stop()
//  scalers.Stop()
//  events.Stop()

import (
	"context"
	"errors"
	"fmt"
	_ "net/http/pprof"
	"strings"
	"sync"
	"time"

	"github.com/SSU-DCN/quotascale-controller/internal/nodescaling"
	"github.com/SSU-DCN/quotascale-controller/internal/resize"
	"github.com/SSU-DCN/quotascale-controller/internal/scalerresolver"
	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	v14 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	scalerclient "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

type QuotaController struct {
	client                 kubernetes.Interface
	quotaAutoscalerClient  scalerclient.Interface
	quotaCheckInterval     time.Duration
	scaleOutRequestHandler nodescaling.ScaleOutRequestHandler
}

type QuotaDeferredError struct {
	Reason string
}

func (err *QuotaDeferredError) Error() string {
	return err.Reason
}

// QuotaWatcher internally manages a list of QuotaAutoscalers and ResourceQuotas.
type QuotaWatcher struct {
	Scalers map[string]v14.QuotaAutoscaler
	Quotas  map[string]v12.ResourceQuota
	Events  map[string][]v12.Event
	Started time.Time

	Client                 kubernetes.Interface
	QuotaAutoscalerClient  scalerclient.Interface
	ScaleOutRequestHandler nodescaling.ScaleOutRequestHandler
	pendingScaleOut        map[string]struct{}
	pendingScaleOutMu      sync.Mutex
}

func NewQuotaController(client kubernetes.Interface, quotaAutoscalerClient scalerclient.Interface, quotaCheckInterval time.Duration, scaleOutRequestHandler nodescaling.ScaleOutRequestHandler) *QuotaController {
	if quotaCheckInterval <= 0 {
		quotaCheckInterval = time.Minute
	}
	return &QuotaController{
		client:                 client,
		quotaAutoscalerClient:  quotaAutoscalerClient,
		quotaCheckInterval:     quotaCheckInterval,
		scaleOutRequestHandler: scaleOutRequestHandler,
	}
}

// Run listens to namespaced ResourceQuotas and QuotaAutoscalers. When both are known for a namespace
// the required behaviour is calculated. If scaling is required, following the behavior, the resize API is
// invoked. This is a blocking call until either channel terminates.
func (controller *QuotaController) Run(startScalers []v14.QuotaAutoscaler, quotas, scalers, events <-chan watch.Event) {
	watcher := &QuotaWatcher{
		Scalers:                map[string]v14.QuotaAutoscaler{},
		Quotas:                 map[string]v12.ResourceQuota{},
		Events:                 map[string][]v12.Event{},
		Started:                time.Now().UTC(),
		Client:                 controller.client,
		QuotaAutoscalerClient:  controller.quotaAutoscalerClient,
		ScaleOutRequestHandler: controller.scaleOutRequestHandler,
		pendingScaleOut:        map[string]struct{}{},
	}

	for _, nsScaler := range startScalers {
		if err := watcher.ResolveScalerResourceQuota(&nsScaler); err != nil {
			logging.LogWarning("[%s/%s] Failed to resolve ResourceQuota during startup: %v", nsScaler.Namespace, nsScaler.Name, err)
		}
		watcher.Scalers[nsScaler.Namespace] = nsScaler
	}

	// Ticker aggregates Namespace and ResourceQuota events
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	quotaCheckTicker := time.NewTicker(controller.quotaCheckInterval)
	defer quotaCheckTicker.Stop()

	for {
		select {
		case event, ok := <-quotas:
			// Quota Changes are very frequent, every Pod "modifies" the Quota status twice.
			// To be a bit more friendly during a scale down we let ticker aggregate Quota
			// events. For most cases this will suffice. The ideal implementation would be to
			// aggregate a few seconds after the first ResourceQuota event per unique namespace.
			if !ok {
				return
			}
			ns := watcher.RegisterQuotaEvent(event)
			if ns != "" {
				logging.LogDebug("[%s] QuotaEvent", ns)
				if _, ok := watcher.Events[ns]; !ok {
					watcher.Events[ns] = []v12.Event{}
					// We let the ticker aggregate events
				}
			}
		case event, ok := <-scalers:
			if !ok {
				return
			}
			ns := watcher.RegisterScalerEvent(event)
			if ns != "" {
				logging.LogDebug("[%s] ScalerEvent", ns)
				watcher.UpdateNs(ns, false)
			}

		case event, ok := <-events:
			if !ok {
				return
			}
			ns, immediate := watcher.RegisterNamespacedEvent(event)
			if ns != "" {
				logging.LogDebug("[%s] PodEvent", ns)
				if immediate {
					logging.LogInfo("[%s] Quota denied event detected, triggering immediate quota evaluation", ns)
					watcher.UpdateNs(ns, true)
					delete(watcher.Events, ns)
				}
			}
			// Non-quota-denied events are still aggregated by the ticker.

		case <-ticker.C:
			for ns := range watcher.Events {
				logging.LogDebug("[%s] Ticker", ns)
				watcher.UpdateNs(ns, true) // New expensive, as reading events will read ReplicaSets, etc
				delete(watcher.Events, ns)
			}
		case <-quotaCheckTicker.C:
			watcher.CheckQuotasPeriodically()
		}
	}
}

// WatchQuotas is kept as a compatibility wrapper around QuotaController.
func WatchQuotas(client kubernetes.Interface, startScalers []v14.QuotaAutoscaler, quotas, scalers, events <-chan watch.Event, quotaCheckInterval time.Duration) {
	NewQuotaController(client, nil, quotaCheckInterval, nil).Run(startScalers, quotas, scalers, events)
}

func (watcher *QuotaWatcher) CheckQuotasPeriodically() {
	for namespace, scaler := range watcher.Scalers {
		scalerCopy := scaler
		if err := watcher.ResolveScalerResourceQuota(&scalerCopy); err != nil {
			continue
		}
		watcher.Scalers[namespace] = scalerCopy

		logging.LogDebug("[%s] Periodic quota utilization check", namespace)
		watcher.UpdateNs(namespace, false)
	}
}

func (watcher *QuotaWatcher) UpdateNs(namespace string, readEvents bool) {
	scaler, scalerOk := watcher.Scalers[namespace]
	quota, quotaOk := watcher.Quotas[namespace]
	var events []v12.Event
	if readEvents {
		events = watcher.Events[namespace]
	}
	logging.LogDebug("[%s] Checking Quota Updates (Found Scaler: %t Quota: %t Events %d (%t) ", namespace, scalerOk, quotaOk, len(events), readEvents)

	if scalerOk && quotaOk {
		go func() {
			err := watcher.UpdateQuotaIfRequired(quota, scaler, events)
			if err != nil {
				var deferredErr *QuotaDeferredError
				if errors.As(err, &deferredErr) {
					logging.LogInfo("[%s] Quota update deferred: %s", namespace, deferredErr.Error())
					return
				}
				logging.LogError("[%s] Failed to update quota for: %s", namespace, err.Error())
			}
		}()
	}
}

// RegisterScalerEvent stores a QuotaAutoscaler in watcher, or deletes it.
func (watcher *QuotaWatcher) RegisterScalerEvent(event watch.Event) string {
	scaler := event.Object.(*v14.QuotaAutoscaler)

	if event.Type == watch.Deleted {
		delete(watcher.Scalers, scaler.Namespace)
		return ""
	}

	watcher.Scalers[scaler.Namespace] = *scaler
	if err := watcher.ResolveScalerResourceQuota(scaler); err != nil {
		logging.LogWarning("[%s/%s] Failed to resolve ResourceQuota: %v", scaler.Namespace, scaler.Name, err)
	}
	watcher.Scalers[scaler.Namespace] = *scaler
	return scaler.Namespace
}

// RegisterQuotaEvent stores a ResourceQuota in watcher, or deletes it.
func (watcher *QuotaWatcher) RegisterQuotaEvent(event watch.Event) string {
	quota := event.Object.(*v12.ResourceQuota)

	// Get QuotaAutoscaler to see event Quota is a target
	scaler, ok := watcher.Scalers[quota.Namespace]
	if ok && scaler.Spec.ResourceQuota == "" {
		scalerCopy := scaler
		if err := watcher.ResolveScalerResourceQuota(&scalerCopy); err == nil {
			watcher.Scalers[quota.Namespace] = scalerCopy
			scaler = scalerCopy
		}
	}
	if ok && scaler.Spec.ResourceQuota == quota.Name {

		if event.Type == watch.Deleted {
			delete(watcher.Quotas, quota.Namespace)
			return ""
		}

		watcher.Quotas[quota.Namespace] = *quota
		return quota.Namespace
	}

	return ""
}

func (watcher *QuotaWatcher) ResolveScalerResourceQuota(scaler *v14.QuotaAutoscaler) error {
	quota, err := scalerresolver.ResolveResourceQuota(context.TODO(), watcher.Client, watcher.QuotaAutoscalerClient, scaler)
	if err != nil {
		return err
	}
	watcher.Quotas[scaler.Namespace] = *quota
	return nil
}

// RegisterNamespacedEvent stores namespaced Events in watcher. Should be cleaned up by aggregate loop
// every iteration (events should be consumed once). This function does not delete any stored events.
// The returned boolean indicates whether quota scaling should run immediately.
func (watcher *QuotaWatcher) RegisterNamespacedEvent(event watch.Event) (string, bool) {
	ev := event.Object.(*v12.Event)
	if watcher.shouldIgnoreHistoricalEvent(ev) {
		return "", false
	}
	target := ev.InvolvedObject

	if _, ok := watcher.Events[target.Namespace]; !ok {
		watcher.Events[target.Namespace] = []v12.Event{*ev}
	} else {
		watcher.Events[target.Namespace] = append(watcher.Events[target.Namespace], *ev)
	}

	return ev.Namespace, IsImmediateQuotaDeniedEvent(ev)
}

func (watcher *QuotaWatcher) shouldIgnoreHistoricalEvent(ev *v12.Event) bool {
	if ev == nil || watcher.Started.IsZero() {
		return false
	}

	occurredAt := eventOccurredAt(ev)
	if occurredAt.IsZero() {
		return false
	}

	return occurredAt.Before(watcher.Started)
}

func eventOccurredAt(ev *v12.Event) time.Time {
	if ev == nil {
		return time.Time{}
	}
	if !ev.EventTime.IsZero() {
		return ev.EventTime.Time.UTC()
	}
	if !ev.LastTimestamp.IsZero() {
		return ev.LastTimestamp.Time.UTC()
	}
	if !ev.FirstTimestamp.IsZero() {
		return ev.FirstTimestamp.Time.UTC()
	}
	if !ev.CreationTimestamp.IsZero() {
		return ev.CreationTimestamp.Time.UTC()
	}
	return time.Time{}
}

func IsImmediateQuotaDeniedEvent(ev *v12.Event) bool {
	if ev == nil {
		return false
	}

	switch ev.Reason {
	case "FailedCreate":
	default:
		return false
	}

	message := strings.ToLower(ev.Message)
	return strings.Contains(message, "exceeded quota") ||
		strings.Contains(message, "is forbidden") && strings.Contains(message, "quota") ||
		strings.Contains(message, "quota") && strings.Contains(message, "denied")
}

func ResourceQuotaUsedMemoryLimit(quota *v12.ResourceQuota) *resource.Quantity {
	memLimit, ok := quota.Status.Used["limits.memory"]
	if !ok {
		return quota.Status.Used.Memory()
	}
	return &memLimit
}

func ResourceQuotaUsedCpuLimit(quota *v12.ResourceQuota) *resource.Quantity {
	cpuLimit, ok := quota.Status.Used["limits.cpu"]
	if !ok {
		return quota.Status.Used.Cpu()
	}
	return &cpuLimit
}

func (watcher *QuotaWatcher) UpdateQuotaIfRequired(quota v12.ResourceQuota, scaler v14.QuotaAutoscaler, events []v12.Event) error {
	defer func() {
		// When this reconciliation no longer needs an external scale-out request,
		// future quota pressure can enqueue a fresh one.
		if !watcher.namespaceNeedsScaleOut(quota, scaler, events) {
			watcher.clearPendingScaleOut(quota.Namespace)
		}
	}()

	validatedScaler := ValidateQuotaScaler(&scaler)
	pendingPodRequests := resources.Resources{}
	desired := &resources.Resources{
		Cpu:    quota.Spec.Hard.Cpu().ScaledValue(resource.Milli),
		Memory: quota.Spec.Hard.Memory().ScaledValue(resource.Mega),
	}

	if quota.Status.Used == nil || quota.Status.Hard == nil {
		return errors.New("quota status is nil")
	}

	for _, policy := range scaler.Spec.Behavior.ScaleDown.Policies {
		desired.Replace(validatedScaler.ActivateScalerPolicy(policy, &quota, false))
	}
	logging.LogDebug("[%s] Desired resources after ScaleDown: %+v\n", scaler.Namespace, desired)
	for _, policy := range scaler.Spec.Behavior.ScaleUp.Policies {
		desired.Replace(validatedScaler.ActivateScalerPolicy(policy, &quota, true))
	}
	logging.LogDebug("[%s] Desired resources after ScaleUp: %+v\n", scaler.Namespace, desired)

	if events != nil {
		if sum, requests, _ := GetResourceDemandsFromPodEvents(watcher.Client, events); sum != nil && !sum.IsEmpty() { // This is a slow call!
			if requests != nil {
				pendingPodRequests = *requests
			}
			logging.LogInfo("[%s] Namespace events require an extra %+v resources\n", scaler.Namespace, sum)
			desired = (&resources.Resources{
				Cpu:    ResourceQuotaUsedCpuLimit(&quota).ScaledValue(resource.Milli),
				Memory: ResourceQuotaUsedMemoryLimit(&quota).ScaledValue(resource.Mega),
			}).Add(sum).Max(desired)
		}
	}

	// We don't do anything with Storage
	storage := quota.Spec.Hard["requests.storage"]

	// Make sure desired quota is within bounds
	desired.Max(&resources.Resources{Cpu: validatedScaler.MinCpu, Memory: validatedScaler.MinMemory})
	desired.Limit(&resources.Resources{Cpu: validatedScaler.MaxCpu, Memory: validatedScaler.MaxMemory})
	desired.Storage = storage.ScaledValue(resource.Giga)

	current := resources.Resources{
		Cpu:     quota.Spec.Hard.Cpu().ScaledValue(resource.Milli),
		Memory:  quota.Spec.Hard.Memory().ScaledValue(resource.Mega),
		Storage: storage.ScaledValue(resource.Giga),
	}
	logging.LogInfo("[%s] Calculated desired resources (%+v -> %+v) for namespace %s\n", quota.Namespace, current, desired, scaler.Namespace)
	desired.ForceNoScaleDownWhenScaleUp(&quota)
	if desired.DiffersFrom(&quota) {
		if err := EnsurePodDemandFitsCluster(watcher.Client, pendingPodRequests); err != nil {
			if watcher.ScaleOutRequestHandler != nil && !desired.IsScaleDown(&quota) {
				if !watcher.beginPendingScaleOut(quota.Namespace) {
					return &QuotaDeferredError{
						Reason: fmt.Sprintf("node scale-out already pending; waiting for worker capacity before resizing quota: %s", err.Error()),
					}
				}

				handleErr := watcher.ScaleOutRequestHandler.HandleScaleOutRequest(nodescaling.ScaleOutRequest{
					Namespace: quota.Namespace,
					Current:   current,
					Desired:   *desired,
					Reason:    err.Error(),
				})
				if handleErr != nil {
					watcher.clearPendingScaleOut(quota.Namespace)
					return handleErr
				}

				if capacityErr := EnsurePodDemandFitsCluster(watcher.Client, pendingPodRequests); capacityErr != nil {
					return &QuotaDeferredError{
						Reason: fmt.Sprintf("node scale-out requested; waiting for worker capacity before resizing quota: %s", capacityErr.Error()),
					}
				}

				watcher.clearPendingScaleOut(quota.Namespace)
				logging.LogInfo("[%s] Worker capacity became available after node scale-out; applying desired quota immediately", quota.Namespace)
			}
			if watcher.ScaleOutRequestHandler == nil || desired.IsScaleDown(&quota) {
				return err
			}
		}

		logging.LogDebug("[%s] InvokeResizeApiAsync", quota.Namespace)
		resize.InvokeResizeApiAsync(quota.Namespace, scaler.Spec.ResourceQuota, current, *desired)
	}

	return nil
}

func (watcher *QuotaWatcher) beginPendingScaleOut(namespace string) bool {
	if namespace == "" {
		return false
	}

	watcher.pendingScaleOutMu.Lock()
	defer watcher.pendingScaleOutMu.Unlock()
	if watcher.pendingScaleOut == nil {
		watcher.pendingScaleOut = map[string]struct{}{}
	}
	if _, exists := watcher.pendingScaleOut[namespace]; exists {
		return false
	}
	watcher.pendingScaleOut[namespace] = struct{}{}
	return true
}

func (watcher *QuotaWatcher) clearPendingScaleOut(namespace string) {
	if namespace == "" {
		return
	}

	watcher.pendingScaleOutMu.Lock()
	delete(watcher.pendingScaleOut, namespace)
	watcher.pendingScaleOutMu.Unlock()
}

func (watcher *QuotaWatcher) namespaceNeedsScaleOut(quota v12.ResourceQuota, scaler v14.QuotaAutoscaler, events []v12.Event) bool {
	validatedScaler := ValidateQuotaScaler(&scaler)
	pendingPodRequests := resources.Resources{}
	desired := &resources.Resources{
		Cpu:    quota.Spec.Hard.Cpu().ScaledValue(resource.Milli),
		Memory: quota.Spec.Hard.Memory().ScaledValue(resource.Mega),
	}

	for _, policy := range scaler.Spec.Behavior.ScaleDown.Policies {
		desired.Replace(validatedScaler.ActivateScalerPolicy(policy, &quota, false))
	}
	for _, policy := range scaler.Spec.Behavior.ScaleUp.Policies {
		desired.Replace(validatedScaler.ActivateScalerPolicy(policy, &quota, true))
	}

	if events != nil {
		if sum, requests, _ := GetResourceDemandsFromPodEvents(watcher.Client, events); sum != nil && !sum.IsEmpty() {
			if requests != nil {
				pendingPodRequests = *requests
			}
			desired = (&resources.Resources{
				Cpu:    ResourceQuotaUsedCpuLimit(&quota).ScaledValue(resource.Milli),
				Memory: ResourceQuotaUsedMemoryLimit(&quota).ScaledValue(resource.Mega),
			}).Add(sum).Max(desired)
		}
	}

	storage := quota.Spec.Hard["requests.storage"]
	desired.Max(&resources.Resources{Cpu: validatedScaler.MinCpu, Memory: validatedScaler.MinMemory})
	desired.Limit(&resources.Resources{Cpu: validatedScaler.MaxCpu, Memory: validatedScaler.MaxMemory})
	desired.Storage = storage.ScaledValue(resource.Giga)
	desired.ForceNoScaleDownWhenScaleUp(&quota)
	if !desired.DiffersFrom(&quota) || desired.IsScaleDown(&quota) {
		return false
	}

	return EnsurePodDemandFitsCluster(watcher.Client, pendingPodRequests) != nil
}

package nodescaling

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SSU-DCN/quotascale-controller/pkg/logging"
	"github.com/SSU-DCN/quotascale-controller/pkg/resources"
	inventoryv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/nodescalinginventory/v1"
	scalerclient "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned"
	v12 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultScaleInTriggerDelay = 5 * time.Minute
	defaultScaleInForceDelay   = 5 * time.Minute
	defaultMaxNodeCount        = int32(3)
	minNodeCount               = int32(1)
	defaultScaleInExemptPodKey = "quotascale.dcn.ssu.ac.kr/scale-in-exempt"
)

type ScaleOutRequest struct {
	Namespace string
	Current   resources.Resources
	Desired   resources.Resources
	Reason    string
}

type ScaleInRequest struct {
	Namespace string
	Reason    string
}

type ScaleOutRequestHandler interface {
	HandleScaleOutRequest(request ScaleOutRequest) error
}

type ScaleInRequestHandler interface {
	HandleScaleInRequest(request ScaleInRequest) error
}

type NodeScalingDeferredError struct {
	Reason string
}

func (err *NodeScalingDeferredError) Error() string {
	return err.Reason
}

type NodeScalingController struct {
	runtime               *NodeScalingRuntime
	client                kubernetes.Interface
	quotaAutoscalerClient scalerclient.Interface
	inventoryStore        NodeScalingInventoryStore
	inventorySyncInterval time.Duration
	scaleInTriggerDelay   time.Duration
	scaleInForceEnabled   bool
	scaleInForceDelay     time.Duration
	maxNodeCount          int32
	scaleInExemptKey      string
	scaleInExemptValue    string
	scaleInExemptNS       map[string]struct{}
	scaleInEligibleSince  time.Time
	scaleOutRequests      chan ScaleOutRequest
	scaleInRequests       chan ScaleInRequest
	scaleInWaiters        map[string]scaleInWaiter
	scaleInWaitersMu      sync.Mutex
}

type scaleInWaiter struct {
	cancel    context.CancelFunc
	startedAt time.Time
}

func NewNodeScalingController(runtime *NodeScalingRuntime, client kubernetes.Interface, inventoryStore NodeScalingInventoryStore, inventorySyncInterval time.Duration) *NodeScalingController {
	if inventorySyncInterval <= 0 {
		inventorySyncInterval = time.Minute
	}
	return &NodeScalingController{
		runtime:               runtime,
		client:                client,
		inventoryStore:        inventoryStore,
		inventorySyncInterval: inventorySyncInterval,
		scaleInTriggerDelay:   defaultScaleInTriggerDelay,
		scaleInForceEnabled:   true,
		scaleInForceDelay:     defaultScaleInForceDelay,
		maxNodeCount:          defaultMaxNodeCount,
		scaleInExemptKey:      defaultScaleInExemptPodKey,
		scaleInExemptValue:    "true",
		scaleInExemptNS: map[string]struct{}{
			"kube-system": {},
		},
		scaleOutRequests:      make(chan ScaleOutRequest, 128),
		scaleInRequests:       make(chan ScaleInRequest, 128),
		scaleInWaiters:        map[string]scaleInWaiter{},
	}
}

func (controller *NodeScalingController) SetQuotaAutoscalerClient(client scalerclient.Interface) {
	controller.quotaAutoscalerClient = client
}

func (controller *NodeScalingController) SetScaleInTriggerDelay(delay time.Duration) {
	if delay <= 0 {
		delay = defaultScaleInTriggerDelay
	}
	controller.scaleInTriggerDelay = delay
}

func (controller *NodeScalingController) SetMaxNodeCount(count int32) {
	if count <= 0 {
		count = defaultMaxNodeCount
	}
	controller.maxNodeCount = count
}

func (controller *NodeScalingController) SetScaleInForceDelay(delay time.Duration) {
	if delay <= 0 {
		delay = defaultScaleInForceDelay
	}
	controller.scaleInForceDelay = delay
}

func (controller *NodeScalingController) SetScaleInForceEnabled(enabled bool) {
	controller.scaleInForceEnabled = enabled
}

func (controller *NodeScalingController) SetScaleInExemptNamespaces(namespaces []string) {
	exempt := map[string]struct{}{}
	for _, namespace := range namespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			continue
		}
		exempt[namespace] = struct{}{}
	}
	controller.scaleInExemptNS = exempt
}

func (controller *NodeScalingController) SetScaleInExemptPodMarker(key, value string) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" {
		key = defaultScaleInExemptPodKey
	}
	if value == "" {
		value = "true"
	}
	controller.scaleInExemptKey = key
	controller.scaleInExemptValue = value
}

func (controller *NodeScalingController) HandleScaleOutRequest(request ScaleOutRequest) error {
	select {
	case controller.scaleOutRequests <- request:
		return nil
	default:
		return fmt.Errorf("node scale-out request queue is full")
	}
}

func (controller *NodeScalingController) HandleScaleInRequest(request ScaleInRequest) error {
	select {
	case controller.scaleInRequests <- request:
		return nil
	default:
		return fmt.Errorf("node scale-in request queue is full")
	}
}

func (controller *NodeScalingController) Run() {
	if err := controller.EnsureMachineDeploymentReplicaBaseline(); err != nil {
		logging.LogError("Node scaling baseline reconcile failed: %s", err.Error())
	}
	if err := controller.SyncScalingNodeInventory(); err != nil {
		logging.LogError("Node scaling inventory sync failed: %s", err.Error())
	}

	ticker := time.NewTicker(controller.inventorySyncInterval)
	defer ticker.Stop()

	for {
		select {
		case request := <-controller.scaleOutRequests:
			if err := controller.ReconcileScaleOut(request); err != nil {
				logging.LogError("[%s] Node scale-out reconcile failed: %s", request.Namespace, err.Error())
			}
		case request := <-controller.scaleInRequests:
			if err := controller.ReconcileScaleIn(request); err != nil {
				var deferredErr *NodeScalingDeferredError
				if errors.As(err, &deferredErr) {
					logging.LogInfo("[%s] Node scale-in deferred: %s", request.Namespace, deferredErr.Error())
					continue
				}
				logging.LogError("[%s] Node scale-in reconcile failed: %s", request.Namespace, err.Error())
			}
		case <-ticker.C:
			if err := controller.SyncScalingNodeInventory(); err != nil {
				logging.LogError("Node scaling inventory sync failed: %s", err.Error())
			}
			if err := controller.EvaluateAutomaticScaleIn(); err != nil {
				logging.LogError("Automatic node scale-in evaluation failed: %s", err.Error())
			}
		}
	}
}

func (controller *NodeScalingController) ReconcileScaleOut(request ScaleOutRequest) error {
	if controller.runtime == nil {
		return nil
	}
	if controller.inventoryStore == nil {
		return fmt.Errorf("node scaling inventory store is not configured")
	}
	if controller.client == nil {
		return fmt.Errorf("kubernetes client is not configured for node scaling")
	}

	if err := controller.syncRepoIfConfigured(); err != nil {
		return err
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		return err
	}

	inventory, err := controller.inventoryStore.Get()
	if err != nil {
		return err
	}

	if nodeName, ok := controller.FindReusableScaleInWaiterNode(inventory); ok {
		controller.resetScaleInTrigger()
		controller.StopScaleInWaiter(nodeName)
		if err := controller.ActivateScalingNode(nodeName); err != nil {
			return err
		}
		if err := controller.inventoryStore.MarkNodeUsed(nodeName); err != nil {
			return err
		}

		logging.LogInfo(
			"[%s] Node scale-out requested (reason: %s). Reused node %s from active scale-in waiter; MachineDeployment replicas stay at %d; quota current=%+v desired=%+v",
			request.Namespace,
			request.Reason,
			nodeName,
			replicas,
			request.Current,
			request.Desired,
		)
		return nil
	}

	node, err := FindFirstUnusedInventoryNode(inventory)
	if err != nil {
		return err
	}

	if controller.maxNodeCount > 0 && replicas >= controller.maxNodeCount {
		return fmt.Errorf(
			"node scale-out requested for namespace %s, but MachineDeployment replicas %d already reached configured max node count %d",
			request.Namespace,
			replicas,
			controller.maxNodeCount,
		)
	}
	if err := controller.ActivateScalingNode(node.Name); err != nil {
		return err
	}
	controller.resetScaleInTrigger()
	if err := controller.inventoryStore.MarkNodeUsed(node.Name); err != nil {
		return err
	}
	if err := controller.runtime.WriteMachineDeploymentReplicas(replicas + 1); err != nil {
		return err
	}
	if err := controller.inventoryStore.UpdateMachineDeploymentReplicas(replicas + 1); err != nil {
		return err
	}
	if controller.runtime.Config.RepoURL != "" {
		if err := controller.runtime.CommitAndPush(fmt.Sprintf("Scale up node MachineDeployment to %d", replicas+1)); err != nil {
			return err
		}
	}

	logging.LogInfo(
		"[%s] Node scale-out requested (reason: %s). Activated node %s and increased MachineDeployment replicas from %d to %d; quota current=%+v desired=%+v",
		request.Namespace,
		request.Reason,
		node.Name,
		replicas,
		replicas+1,
		request.Current,
		request.Desired,
	)
	return nil
}

func (controller *NodeScalingController) ReconcileScaleIn(request ScaleInRequest) error {
	if controller.runtime == nil {
		return nil
	}
	if controller.inventoryStore == nil {
		return fmt.Errorf("node scaling inventory store is not configured")
	}
	if controller.client == nil {
		return fmt.Errorf("kubernetes client is not configured for node scaling")
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		return err
	}
	if replicas <= minNodeCount {
		logging.LogInfo(
			"[%s] Node scale-in skipped (reason: %s). MachineDeployment replicas already at minimum baseline %d",
			request.Namespace,
			request.Reason,
			minNodeCount,
		)
		return nil
	}

	if err := controller.syncRepoIfConfigured(); err != nil {
		return err
	}

	inventory, err := controller.inventoryStore.Get()
	if err != nil {
		return err
	}

	unusedCount := CountUnusedInventoryNodes(inventory)
	if unusedCount == 0 {
		reservedNodes, err := controller.reserveScaleInCandidates(inventory, int(replicas-minNodeCount), request)
		if err != nil {
			return err
		}
		if len(reservedNodes) == 0 {
			return &NodeScalingDeferredError{
				Reason: "waiting for reserved scaling nodes to finish draining",
			}
		}
		return &NodeScalingDeferredError{
			Reason: fmt.Sprintf("waiting for regular pods to drain from reserved scaling nodes %s", strings.Join(reservedNodes, ", ")),
		}
	}

	replicas, err = controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		return err
	}

	targetReplicas := replicas - int32(unusedCount)
	if targetReplicas < minNodeCount {
		targetReplicas = minNodeCount
	}
	if targetReplicas != replicas {
		if err := controller.runtime.WriteMachineDeploymentReplicas(targetReplicas); err != nil {
			return err
		}
		if err := controller.inventoryStore.UpdateMachineDeploymentReplicas(targetReplicas); err != nil {
			return err
		}
		if controller.runtime.Config.RepoURL != "" {
			if err := controller.runtime.CommitAndPush(fmt.Sprintf("Scale in node MachineDeployment to %d", targetReplicas)); err != nil {
				return err
			}
		}
	}

	reservationBudget := int(targetReplicas - minNodeCount)
	if reservationBudget > 0 {
		if _, err := controller.reserveScaleInCandidates(inventory, reservationBudget, request); err != nil {
			return err
		}
	}

	logging.LogInfo("[%s] Node scale-in requested (reason: %s). Reduced MachineDeployment replicas from %d to %d using %d unused scaling nodes", request.Namespace, request.Reason, replicas, targetReplicas, unusedCount)
	return nil
}

func (controller *NodeScalingController) reserveScaleInCandidates(inventory *inventoryv1.NodeScalingInventory, limit int, request ScaleInRequest) ([]string, error) {
	if limit <= 0 {
		return []string{}, nil
	}

	nodes, err := FindHighestOrderUsedInventoryNodes(inventory, limit)
	if err != nil {
		return nil, err
	}

	reservedNodes := make([]string, 0, len(nodes))
	for _, node := range nodes {
		alreadyReserved, err := controller.NodeHasScalingReservation(node.Name)
		if err != nil {
			return nil, err
		}
		if !alreadyReserved {
			if err := controller.ReserveScalingNode(node.Name); err != nil {
				return nil, err
			}
		}
		controller.StartScaleInWaiter(node.Name, request)
		reservedNodes = append(reservedNodes, node.Name)
	}
	return reservedNodes, nil
}

func (controller *NodeScalingController) EnsureMachineDeploymentReplicaBaseline() error {
	if controller.runtime == nil {
		return nil
	}

	if err := controller.syncRepoIfConfigured(); err != nil {
		return err
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		return err
	}
	if replicas != 0 {
		return nil
	}

	if err := controller.runtime.WriteMachineDeploymentReplicas(1); err != nil {
		return err
	}
	if controller.runtime.Config.RepoURL != "" {
		if err := controller.runtime.CommitAndPush("Initialize node scaling MachineDeployment replicas to 1"); err != nil {
			return err
		}
	}

	logging.LogInfo("Node scaling MachineDeployment replicas initialized from 0 to 1")
	return nil
}

func (controller *NodeScalingController) SyncScalingNodeInventory() error {
	if controller.client == nil || controller.inventoryStore == nil {
		return nil
	}

	replicas, err := controller.readMachineDeploymentReplicas()
	if err != nil {
		return err
	}

	nodes, err := controller.client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	nodeNames := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if !nodeHasScalingRole(node) {
			continue
		}
		nodeNames = append(nodeNames, node.Name)
	}
	sort.Strings(nodeNames)

	return controller.inventoryStore.Sync(replicas, nodeNames)
}

func (controller *NodeScalingController) syncRepoIfConfigured() error {
	if controller.runtime == nil {
		return nil
	}
	if controller.runtime.Config.RepoURL == "" {
		return nil
	}
	return controller.runtime.SyncRepo()
}

func (controller *NodeScalingController) readMachineDeploymentReplicas() (int32, error) {
	if controller.runtime == nil {
		return 0, nil
	}
	return controller.runtime.ReadMachineDeploymentReplicas()
}

func (controller *NodeScalingController) ActivateScalingNode(nodeName string) error {
	node, err := controller.client.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if node.Labels != nil {
		if role, ok := node.Labels["role"]; ok && role == "scaling" {
			delete(node.Labels, "role")
		}
		delete(node.Labels, "node-role.kubernetes.io/scaling")
	}

	filteredTaints := make([]v12.Taint, 0, len(node.Spec.Taints))
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/scaling" && taint.Effect == v12.TaintEffectNoSchedule {
			continue
		}
		filteredTaints = append(filteredTaints, taint)
	}
	node.Spec.Taints = filteredTaints

	_, err = controller.client.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	return err
}

func (controller *NodeScalingController) ReserveScalingNode(nodeName string) error {
	node, err := controller.client.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels["role"] = "scaling"
	node.Labels["node-role.kubernetes.io/scaling"] = ""

	hasScalingTaint := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/scaling" && taint.Effect == v12.TaintEffectNoSchedule {
			hasScalingTaint = true
			break
		}
	}
	if !hasScalingTaint {
		node.Spec.Taints = append(node.Spec.Taints, v12.Taint{
			Key:    "node-role.kubernetes.io/scaling",
			Effect: v12.TaintEffectNoSchedule,
		})
	}

	_, err = controller.client.CoreV1().Nodes().Update(context.TODO(), node, metav1.UpdateOptions{})
	return err
}

func (controller *NodeScalingController) NodeHasScalingReservation(nodeName string) (bool, error) {
	node, err := controller.client.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	hasRole := node.Labels["role"] == "scaling"
	_, hasNodeRoleLabel := node.Labels["node-role.kubernetes.io/scaling"]
	hasTaint := false
	for _, taint := range node.Spec.Taints {
		if taint.Key == "node-role.kubernetes.io/scaling" && taint.Effect == v12.TaintEffectNoSchedule {
			hasTaint = true
			break
		}
	}

	return hasRole && hasNodeRoleLabel && hasTaint, nil
}

func (controller *NodeScalingController) StartScaleInWaiter(nodeName string, request ScaleInRequest) {
	controller.scaleInWaitersMu.Lock()
	if _, exists := controller.scaleInWaiters[nodeName]; exists {
		controller.scaleInWaitersMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	controller.scaleInWaiters[nodeName] = scaleInWaiter{
		cancel:    cancel,
		startedAt: time.Now().UTC(),
	}
	controller.scaleInWaitersMu.Unlock()

	go func() {
		ticker := time.NewTicker(controller.inventorySyncInterval)
		defer ticker.Stop()
		defer controller.clearScaleInWaiter(nodeName)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				done, err := controller.ProcessScaleInWaiter(nodeName, request)
				if err != nil {
					logging.LogError("[%s] Node scale-in waiter failed for node %s: %s", request.Namespace, nodeName, err.Error())
					return
				}
				if done {
					return
				}
			}
		}
	}()
}

func (controller *NodeScalingController) ProcessScaleInWaiter(nodeName string, request ScaleInRequest) (bool, error) {
	hasScalingReservation, err := controller.NodeHasScalingReservation(nodeName)
	if err != nil {
		return false, err
	}
	if !hasScalingReservation {
		return true, nil
	}

	hasBlockingPods, err := controller.NodeHasBlockingPods(nodeName)
	if err != nil {
		return false, err
	}
	if hasBlockingPods {
		waitDuration, forceScaleIn := controller.ShouldForceScaleIn(nodeName)
		if !forceScaleIn {
			return false, nil
		}
		logging.LogWarning(
			"[%s] Forcing node scale-in for %s after %s with blocking pods still present",
			request.Namespace,
			nodeName,
			waitDuration.Round(time.Second),
		)
	}

	if err := controller.inventoryStore.MarkNodeUnused(nodeName); err != nil {
		return false, err
	}
	if err := controller.HandleScaleInRequest(request); err != nil {
		return false, err
	}
	return true, nil
}

func (controller *NodeScalingController) NodeHasBlockingPods(nodeName string) (bool, error) {
	pods, err := controller.client.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return false, err
	}

	for _, pod := range pods.Items {
		if pod.Spec.NodeName != nodeName {
			continue
		}
		if pod.Status.Phase == v12.PodSucceeded || pod.Status.Phase == v12.PodFailed {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if isMirrorPod(&pod) {
			continue
		}
		if controller.isScaleInExemptNamespace(pod.Namespace) {
			continue
		}
		if controller.isScaleInExemptPod(&pod) {
			continue
		}
		if isDaemonSetPod(&pod) {
			continue
		}
		return true, nil
	}

	return false, nil
}

func (controller *NodeScalingController) isScaleInExemptNamespace(namespace string) bool {
	_, exempt := controller.scaleInExemptNS[namespace]
	return exempt
}

func (controller *NodeScalingController) isScaleInExemptPod(pod *v12.Pod) bool {
	if pod == nil {
		return false
	}
	if controller.scaleInExemptKey == "" {
		return false
	}
	if value, ok := pod.Labels[controller.scaleInExemptKey]; ok && value == controller.scaleInExemptValue {
		return true
	}
	if value, ok := pod.Annotations[controller.scaleInExemptKey]; ok && value == controller.scaleInExemptValue {
		return true
	}
	return false
}

func (controller *NodeScalingController) clearScaleInWaiter(nodeName string) {
	controller.scaleInWaitersMu.Lock()
	delete(controller.scaleInWaiters, nodeName)
	controller.scaleInWaitersMu.Unlock()
}

func (controller *NodeScalingController) hasActiveScaleInWaiters() bool {
	controller.scaleInWaitersMu.Lock()
	defer controller.scaleInWaitersMu.Unlock()
	return len(controller.scaleInWaiters) > 0
}

func (controller *NodeScalingController) resetScaleInTrigger() {
	controller.scaleInEligibleSince = time.Time{}
}

func (controller *NodeScalingController) StopScaleInWaiter(nodeName string) bool {
	controller.scaleInWaitersMu.Lock()
	waiter, exists := controller.scaleInWaiters[nodeName]
	if exists {
		delete(controller.scaleInWaiters, nodeName)
	}
	controller.scaleInWaitersMu.Unlock()
	if !exists {
		return false
	}

	waiter.cancel()
	return true
}

func (controller *NodeScalingController) ShouldForceScaleIn(nodeName string) (time.Duration, bool) {
	controller.scaleInWaitersMu.Lock()
	waiter, exists := controller.scaleInWaiters[nodeName]
	controller.scaleInWaitersMu.Unlock()
	if !exists {
		return 0, false
	}

	waitDuration := time.Since(waiter.startedAt)
	if !controller.scaleInForceEnabled {
		return waitDuration, false
	}
	return waitDuration, waitDuration >= controller.scaleInForceDelay
}

func (controller *NodeScalingController) FindReusableScaleInWaiterNode(inventory *inventoryv1.NodeScalingInventory) (string, bool) {
	controller.scaleInWaitersMu.Lock()
	activeWaiters := make(map[string]struct{}, len(controller.scaleInWaiters))
	for nodeName := range controller.scaleInWaiters {
		activeWaiters[nodeName] = struct{}{}
	}
	controller.scaleInWaitersMu.Unlock()

	if len(activeWaiters) == 0 {
		return "", false
	}

	nodes := append([]inventoryv1.NodeScalingInventoryNode(nil), inventory.Spec.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Order == nodes[j].Order {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].Order < nodes[j].Order
	})

	for _, node := range nodes {
		if !node.Used {
			continue
		}
		if _, exists := activeWaiters[node.Name]; !exists {
			continue
		}
		return node.Name, true
	}

	return "", false
}

func isDaemonSetPod(pod *v12.Pod) bool {
	if pod == nil {
		return false
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

func isMirrorPod(pod *v12.Pod) bool {
	if pod == nil {
		return false
	}
	_, exists := pod.Annotations[v12.MirrorPodAnnotationKey]
	return exists
}

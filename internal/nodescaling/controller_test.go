package nodescaling

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	inventoryv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/nodescalinginventory/v1"
	scalerv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/quotaautoscaler/v1"
	scalerfake "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/client/clientset/versioned/fake"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type captureNodeScalingInventoryStore struct {
	replicas  int32
	nodes     []string
	calls     int
	inventory *inventoryv1.NodeScalingInventory
}

func (store *captureNodeScalingInventoryStore) Sync(replicas int32, nodes []string) error {
	store.replicas = replicas
	store.nodes = append([]string(nil), nodes...)
	store.calls++
	return nil
}

func (store *captureNodeScalingInventoryStore) Get() (*inventoryv1.NodeScalingInventory, error) {
	if store.inventory == nil {
		return nil, fmt.Errorf("inventory not found")
	}

	cloned := *store.inventory
	cloned.Spec.Nodes = append([]inventoryv1.NodeScalingInventoryNode(nil), store.inventory.Spec.Nodes...)
	return &cloned, nil
}

func (store *captureNodeScalingInventoryStore) MarkNodeUsed(nodeName string) error {
	return store.updateNodeUsage(nodeName, true)
}

func (store *captureNodeScalingInventoryStore) MarkNodeUnused(nodeName string) error {
	return store.updateNodeUsage(nodeName, false)
}

func (store *captureNodeScalingInventoryStore) updateNodeUsage(nodeName string, used bool) error {
	if store.inventory == nil {
		return fmt.Errorf("inventory not found")
	}

	for i := range store.inventory.Spec.Nodes {
		if store.inventory.Spec.Nodes[i].Name != nodeName {
			continue
		}
		store.inventory.Spec.Nodes[i].Used = used
		return nil
	}
	return fmt.Errorf("node %s not found", nodeName)
}

func (store *captureNodeScalingInventoryStore) UpdateMachineDeploymentReplicas(replicas int32) error {
	if store.inventory == nil {
		return fmt.Errorf("inventory not found")
	}
	store.inventory.Spec.MachineDeploymentReplicas = replicas
	return nil
}

func TestNodeScalingControllerEnsureMachineDeploymentReplicaBaselineSetsZeroToOne(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(0))

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, nil, nil, time.Minute)

	if err := controller.EnsureMachineDeploymentReplicaBaseline(); err != nil {
		t.Fatalf("expected baseline reconcile to succeed, got error: %v", err)
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		t.Fatalf("expected replica read to succeed, got error: %v", err)
	}
	if replicas != 1 {
		t.Fatalf("expected replicas to be initialized to 1, got %d", replicas)
	}
}

func TestNodeScalingControllerSyncScalingNodeInventoryUsesScalingNodes(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	store := &captureNodeScalingInventoryStore{}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-b",
				Labels: map[string]string{"node-role.kubernetes.io/scaling": ""},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{"role": "scaling"},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-other",
				Labels: map[string]string{"role": "general"},
			},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Minute)

	if err := controller.SyncScalingNodeInventory(); err != nil {
		t.Fatalf("expected inventory sync to succeed, got error: %v", err)
	}

	if store.calls != 1 {
		t.Fatalf("expected inventory store to be called once, got %d", store.calls)
	}
	if store.replicas != 3 {
		t.Fatalf("expected replicas to be 3, got %d", store.replicas)
	}
	if len(store.nodes) != 2 || store.nodes[0] != "node-a" || store.nodes[1] != "node-b" {
		t.Fatalf("unexpected nodes recorded: %#v", store.nodes)
	}
}

func TestNodeScalingControllerSyncScalingNodeInventoryUsesScalingTaint(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(2))

	store := &captureNodeScalingInventoryStore{}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-tainted",
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{
					{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule},
				},
			},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Minute)

	if err := controller.SyncScalingNodeInventory(); err != nil {
		t.Fatalf("expected inventory sync to succeed, got error: %v", err)
	}
	if len(store.nodes) != 1 || store.nodes[0] != "node-tainted" {
		t.Fatalf("expected tainted scaling node to be recorded, got %#v", store.nodes)
	}
}

func TestNodeScalingControllerSyncScalingNodeInventoryDoesNotRequireRepoPull(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	store := &captureNodeScalingInventoryStore{}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "node-a",
				Labels: map[string]string{"role": "scaling"},
			},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoURL:      "http://invalid.example.local/repo.git",
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Minute)

	if err := controller.SyncScalingNodeInventory(); err != nil {
		t.Fatalf("expected inventory sync to succeed without pulling the repo, got error: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("expected inventory store to be called once, got %d", store.calls)
	}
	if store.replicas != 3 {
		t.Fatalf("expected replicas to be read from the local manifest, got %d", store.replicas)
	}
}

func TestNodeScalingControllerReconcileScaleOutActivatesInventoryNodeAndIncrementsReplicas(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 3,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: false},
					{Name: "node-b", Order: 2, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{
					{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule},
					{Key: "other", Effect: v12.TaintEffectNoSchedule},
				},
			},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Minute)
	controller.SetMaxNodeCount(10)

	err := controller.ReconcileScaleOut(ScaleOutRequest{
		Namespace: "default",
		Reason:    "capacity exceeded",
	})
	if err != nil {
		t.Fatalf("expected reconcile to succeed, got error: %v", err)
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		t.Fatalf("expected replica read to succeed, got error: %v", err)
	}
	if replicas != 4 {
		t.Fatalf("expected replicas to increase to 4, got %d", replicas)
	}

	node, err := client.CoreV1().Nodes().Get(context.TODO(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected node read to succeed, got error: %v", err)
	}
	if _, exists := node.Labels["role"]; exists {
		t.Fatalf("expected role label to be removed, got labels %#v", node.Labels)
	}
	if _, exists := node.Labels["node-role.kubernetes.io/scaling"]; exists {
		t.Fatalf("expected node-role scaling label to be removed, got labels %#v", node.Labels)
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "other" {
		t.Fatalf("expected scaling taint to be removed, got taints %#v", node.Spec.Taints)
	}
	if !store.inventory.Spec.Nodes[0].Used {
		t.Fatalf("expected selected inventory node to be marked used")
	}
	if store.inventory.Spec.MachineDeploymentReplicas != 4 {
		t.Fatalf("expected inventory replicas to be updated to 4, got %d", store.inventory.Spec.MachineDeploymentReplicas)
	}
}

func TestNodeScalingControllerReconcileScaleOutHonorsMaxNodeCount(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 3,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: false},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{
					{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule},
				},
			},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Minute)
	controller.SetMaxNodeCount(3)

	err := controller.ReconcileScaleOut(ScaleOutRequest{
		Namespace: "default",
		Reason:    "capacity exceeded",
	})
	if err == nil {
		t.Fatalf("expected reconcile to fail at max node count")
	}

	replicas, readErr := controller.runtime.ReadMachineDeploymentReplicas()
	if readErr != nil {
		t.Fatalf("expected replica read to succeed, got error: %v", readErr)
	}
	if replicas != 3 {
		t.Fatalf("expected replicas to stay at 3, got %d", replicas)
	}
	if store.inventory.Spec.Nodes[0].Used {
		t.Fatalf("expected inventory node to remain unused when max node count is reached")
	}

	node, getErr := client.CoreV1().Nodes().Get(context.TODO(), "node-a", metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("expected node read to succeed, got error: %v", getErr)
	}
	if _, exists := node.Labels["role"]; !exists {
		t.Fatalf("expected scaling reservation labels to remain untouched when max node count is reached")
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "node-role.kubernetes.io/scaling" {
		t.Fatalf("expected scaling taint to remain untouched, got taints %#v", node.Spec.Taints)
	}
}

func TestNodeScalingControllerReconcileScaleOutReusesActiveScaleInWaiterNode(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 3,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: false},
					{Name: "node-b", Order: 2, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-b",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{
					{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule},
					{Key: "other", Effect: v12.TaintEffectNoSchedule},
				},
			},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Hour)
	controller.StartScaleInWaiter("node-b", ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})

	err := controller.ReconcileScaleOut(ScaleOutRequest{
		Namespace: "default",
		Reason:    "capacity exceeded",
	})
	if err != nil {
		t.Fatalf("expected reconcile to succeed, got error: %v", err)
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		t.Fatalf("expected replica read to succeed, got error: %v", err)
	}
	if replicas != 3 {
		t.Fatalf("expected replicas to stay at 3 when reusing waiter node, got %d", replicas)
	}
	if store.inventory.Spec.MachineDeploymentReplicas != 3 {
		t.Fatalf("expected inventory replicas to stay at 3, got %d", store.inventory.Spec.MachineDeploymentReplicas)
	}

	node, err := client.CoreV1().Nodes().Get(context.TODO(), "node-b", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected node read to succeed, got error: %v", err)
	}
	if _, exists := node.Labels["role"]; exists {
		t.Fatalf("expected role label to be removed, got labels %#v", node.Labels)
	}
	if _, exists := node.Labels["node-role.kubernetes.io/scaling"]; exists {
		t.Fatalf("expected node-role scaling label to be removed, got labels %#v", node.Labels)
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "other" {
		t.Fatalf("expected scaling taint to be removed, got taints %#v", node.Spec.Taints)
	}

	controller.scaleInWaitersMu.Lock()
	_, waiterActive := controller.scaleInWaiters["node-b"]
	controller.scaleInWaitersMu.Unlock()
	if waiterActive {
		t.Fatalf("expected scale-in waiter to be stopped for node-b")
	}
}

func TestNodeScalingControllerReconcileScaleOutReusesLowestOrderActiveScaleInWaiterNode(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 3,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: true},
					{Name: "node-b", Order: 2, Used: true},
					{Name: "node-c", Order: 3, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule}},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-b",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule}},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-c",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule}},
			},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Hour)
	for _, nodeName := range []string{"node-a", "node-b", "node-c"} {
		controller.StartScaleInWaiter(nodeName, ScaleInRequest{
			Namespace: "default",
			Reason:    "quota scaled down",
		})
	}

	err := controller.ReconcileScaleOut(ScaleOutRequest{
		Namespace: "default",
		Reason:    "capacity exceeded",
	})
	if err != nil {
		t.Fatalf("expected reconcile to succeed, got error: %v", err)
	}

	reusedNode, err := client.CoreV1().Nodes().Get(context.TODO(), "node-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected reused node read to succeed, got error: %v", err)
	}
	if _, exists := reusedNode.Labels["role"]; exists {
		t.Fatalf("expected reused node role label to be removed, got labels %#v", reusedNode.Labels)
	}

	controller.scaleInWaitersMu.Lock()
	_, nodeAWaiterActive := controller.scaleInWaiters["node-a"]
	_, nodeBWaiterActive := controller.scaleInWaiters["node-b"]
	_, nodeCWaiterActive := controller.scaleInWaiters["node-c"]
	controller.scaleInWaitersMu.Unlock()
	if nodeAWaiterActive {
		t.Fatalf("expected scale-in waiter to be stopped for lowest-order node-a")
	}
	if !nodeBWaiterActive || !nodeCWaiterActive {
		t.Fatalf("expected higher-order scale-in waiters to continue running")
	}
}

func TestNodeScalingControllerReconcileScaleInShrinksReplicasUsingUnusedNodesOnly(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(5))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 5,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: true},
					{Name: "node-b", Order: 2, Used: false},
					{Name: "node-c", Order: 3, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&v12.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-c"}},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: repoDir,
	}, client, store, time.Hour)

	err := controller.ReconcileScaleIn(ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	if err != nil {
		t.Fatalf("expected scale-in reconcile to succeed, got error: %v", err)
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		t.Fatalf("expected replica read to succeed, got error: %v", err)
	}
	if replicas != 4 {
		t.Fatalf("expected replicas to decrease to 4, got %d", replicas)
	}
	if store.inventory.Spec.MachineDeploymentReplicas != 4 {
		t.Fatalf("expected inventory replicas to be updated to 4, got %d", store.inventory.Spec.MachineDeploymentReplicas)
	}

	node, err := client.CoreV1().Nodes().Get(context.TODO(), "node-c", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected node read to succeed, got error: %v", err)
	}
	if len(node.Labels) != 0 {
		t.Fatalf("expected no additional reservation when unused nodes already exist, got labels %#v", node.Labels)
	}
	if len(node.Spec.Taints) != 0 {
		t.Fatalf("expected no additional reservation taint when unused nodes already exist, got taints %#v", node.Spec.Taints)
	}
}

func TestNodeScalingControllerProcessScaleInWaiterMarksNodeUnusedAndRequeues(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(2))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 2,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule}},
			},
		},
		&v12.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "daemon-pod",
				Namespace: "kube-system",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "DaemonSet", Name: "node-agent"},
				},
			},
			Spec:   v12.PodSpec{NodeName: "node-a"},
			Status: v12.PodStatus{Phase: v12.PodRunning},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: repoDir,
	}, client, store, time.Hour)

	done, err := controller.ProcessScaleInWaiter("node-a", ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	if err != nil {
		t.Fatalf("expected scale-in waiter processing to succeed, got error: %v", err)
	}
	if !done {
		t.Fatalf("expected waiter processing to complete when only daemonset pods remain")
	}
	if store.inventory.Spec.Nodes[0].Used {
		t.Fatalf("expected inventory node to be marked unused")
	}
	select {
	case request := <-controller.scaleInRequests:
		if request.Namespace != "default" {
			t.Fatalf("unexpected requeued request: %#v", request)
		}
	default:
		t.Fatalf("expected scale-in request to be requeued")
	}
}

func TestNodeScalingControllerNodeHasBlockingPodsIgnoresExemptPods(t *testing.T) {
	client := fake.NewSimpleClientset(
		&v12.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "daemon-pod",
				Namespace: "kube-system",
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "DaemonSet", Name: "node-agent"},
				},
			},
			Spec:   v12.PodSpec{NodeName: "node-a"},
			Status: v12.PodStatus{Phase: v12.PodRunning},
		},
		&v12.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "infra-pod",
				Namespace: "monitoring",
			},
			Spec:   v12.PodSpec{NodeName: "node-a"},
			Status: v12.PodStatus{Phase: v12.PodRunning},
		},
		&v12.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "annotated-pod",
				Namespace: "app",
				Annotations: map[string]string{
					defaultScaleInExemptPodKey: "true",
				},
			},
			Spec:   v12.PodSpec{NodeName: "node-a"},
			Status: v12.PodStatus{Phase: v12.PodRunning},
		},
		&v12.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "terminating-pod",
				Namespace:   "app",
				Annotations: map[string]string{"kubernetes.io/config.mirror": "mirror-node-a"},
				DeletionTimestamp: &metav1.Time{
					Time: time.Now(),
				},
			},
			Spec:   v12.PodSpec{NodeName: "node-a"},
			Status: v12.PodStatus{Phase: v12.PodRunning},
		},
	)

	controller := NewNodeScalingController(nil, client, nil, time.Minute)
	controller.SetScaleInExemptNamespaces([]string{"kube-system", "monitoring"})

	hasBlockingPods, err := controller.NodeHasBlockingPods("node-a")
	if err != nil {
		t.Fatalf("expected blocking pod check to succeed, got error: %v", err)
	}
	if hasBlockingPods {
		t.Fatalf("expected only exempt pods to remain on node-a")
	}
}

func TestNodeScalingControllerProcessScaleInWaiterForcesScaleInAfterTimeout(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(2))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 2,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule}},
			},
		},
		&v12.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "middleware-pod",
				Namespace: "app",
			},
			Spec:   v12.PodSpec{NodeName: "node-a"},
			Status: v12.PodStatus{Phase: v12.PodRunning},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: repoDir,
	}, client, store, time.Hour)
	controller.SetScaleInForceDelay(time.Minute)
	controller.StartScaleInWaiter("node-a", ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	controller.scaleInWaitersMu.Lock()
	waiter := controller.scaleInWaiters["node-a"]
	waiter.startedAt = time.Now().Add(-2 * time.Minute)
	controller.scaleInWaiters["node-a"] = waiter
	controller.scaleInWaitersMu.Unlock()

	done, err := controller.ProcessScaleInWaiter("node-a", ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	if err != nil {
		t.Fatalf("expected forced scale-in waiter processing to succeed, got error: %v", err)
	}
	if !done {
		t.Fatalf("expected waiter processing to complete after force timeout")
	}
	if store.inventory.Spec.Nodes[0].Used {
		t.Fatalf("expected timed-out node to be marked unused")
	}
	select {
	case request := <-controller.scaleInRequests:
		if request.Namespace != "default" {
			t.Fatalf("unexpected requeued request: %#v", request)
		}
	default:
		t.Fatalf("expected scale-in request to be requeued after timeout")
	}
}

func TestNodeScalingControllerProcessScaleInWaiterWaitsIndefinitelyWhenForceDisabled(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(2))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 2,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "node-a",
				Labels: map[string]string{
					"role":                            "scaling",
					"node-role.kubernetes.io/scaling": "",
				},
			},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule}},
			},
		},
		&v12.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "middleware-pod",
				Namespace: "app",
			},
			Spec:   v12.PodSpec{NodeName: "node-a"},
			Status: v12.PodStatus{Phase: v12.PodRunning},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: repoDir,
	}, client, store, time.Hour)
	controller.SetScaleInForceEnabled(false)
	controller.SetScaleInForceDelay(time.Minute)
	controller.StartScaleInWaiter("node-a", ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	controller.scaleInWaitersMu.Lock()
	waiter := controller.scaleInWaiters["node-a"]
	waiter.startedAt = time.Now().Add(-24 * time.Hour)
	controller.scaleInWaiters["node-a"] = waiter
	controller.scaleInWaitersMu.Unlock()

	done, err := controller.ProcessScaleInWaiter("node-a", ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	if err != nil {
		t.Fatalf("expected waiter processing to keep waiting cleanly, got error: %v", err)
	}
	if done {
		t.Fatalf("expected waiter processing to keep waiting when force is disabled")
	}
	if !store.inventory.Spec.Nodes[0].Used {
		t.Fatalf("expected node to remain used while blocking pod is present and force is disabled")
	}
	select {
	case request := <-controller.scaleInRequests:
		t.Fatalf("expected no requeued scale-in request when force is disabled, got %#v", request)
	default:
	}
}

func TestNodeScalingControllerReconcileScaleInReturnsDeferredErrorWhenWaitingForDrain(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(2))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 2,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: repoDir,
	}, client, store, time.Hour)

	err := controller.ReconcileScaleIn(ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	if err == nil {
		t.Fatalf("expected deferred error, got nil")
	}

	var deferredErr *NodeScalingDeferredError
	if !errors.As(err, &deferredErr) {
		t.Fatalf("expected deferred error type, got %T: %v", err, err)
	}
}

func TestNodeScalingControllerReconcileScaleInReservesMultipleNodesAtOnce(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(4))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 4,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: true},
					{Name: "node-b", Order: 2, Used: true},
					{Name: "node-c", Order: 3, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		&v12.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
		&v12.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-c"}},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: repoDir,
	}, client, store, time.Hour)

	err := controller.ReconcileScaleIn(ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	})
	if err == nil {
		t.Fatalf("expected deferred error while draining reserved nodes")
	}

	for _, nodeName := range []string{"node-a", "node-b", "node-c"} {
		node, getErr := client.CoreV1().Nodes().Get(context.TODO(), nodeName, metav1.GetOptions{})
		if getErr != nil {
			t.Fatalf("expected node %s read to succeed, got error: %v", nodeName, getErr)
		}
		if node.Labels["role"] != "scaling" {
			t.Fatalf("expected node %s to be reserved for concurrent scale-in, got labels %#v", nodeName, node.Labels)
		}
	}
}

func TestNodeScalingControllerReconcileScaleInSkipsAtMinimumBaseline(t *testing.T) {
	repoDir := t.TempDir()
	writeFile(t, filepath.Join(repoDir, defaultNodeScalingFile), machineDeploymentYAML(1))

	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 1,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "node-a", Order: 1, Used: false},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
	)

	controller := NewNodeScalingController(&NodeScalingRuntime{
		Config: NodeScalingConfig{
			RepoURL:      "http://invalid.example.local/repo.git",
			RepoFilePath: defaultNodeScalingFile,
		},
		RepoDir: repoDir,
	}, client, store, time.Hour)

	if err := controller.ReconcileScaleIn(ScaleInRequest{
		Namespace: "default",
		Reason:    "quota scaled down",
	}); err != nil {
		t.Fatalf("expected scale-in to be skipped cleanly at minimum baseline, got error: %v", err)
	}

	replicas, err := controller.runtime.ReadMachineDeploymentReplicas()
	if err != nil {
		t.Fatalf("expected replica read to succeed, got error: %v", err)
	}
	if replicas != 1 {
		t.Fatalf("expected replicas to stay at baseline 1, got %d", replicas)
	}
}

func TestNodeScalingControllerEvaluateAutomaticScaleInQueuesRequestAfterDelay(t *testing.T) {
	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 2,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "reserved-scaling", Order: 1, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("4"),
					v12.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "reserved-scaling",
				Labels: map[string]string{"role": "scaling"},
			},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("4"),
					v12.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		&v12.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "quota-a",
				Namespace: "default",
			},
			Spec: v12.ResourceQuotaSpec{
				Hard: v12.ResourceList{
					v12.ResourceLimitsCPU:    resource.MustParse("2"),
					v12.ResourceLimitsMemory: resource.MustParse("2Gi"),
				},
			},
		},
	)
	scalerClient := scalerfake.NewSimpleClientset(
		&scalerv1.QuotaAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scaler-a",
				Namespace: "default",
			},
			Spec: scalerv1.QuotaAutoscalerSpec{
				ResourceQuota: "quota-a",
			},
		},
	)

	controller := NewNodeScalingController(nil, client, store, time.Minute)
	controller.SetQuotaAutoscalerClient(scalerClient)
	controller.SetScaleInTriggerDelay(time.Minute)
	controller.runtime = &NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: t.TempDir(),
	}
	writeFile(t, filepath.Join(controller.runtime.RepoDir, defaultNodeScalingFile), machineDeploymentYAML(2))

	if err := controller.EvaluateAutomaticScaleIn(); err != nil {
		t.Fatalf("expected first automatic scale-in evaluation to succeed, got error: %v", err)
	}
	if controller.scaleInEligibleSince.IsZero() {
		t.Fatalf("expected scale-in eligibility timestamp to be recorded")
	}

	controller.scaleInEligibleSince = time.Now().Add(-2 * controller.scaleInTriggerDelay)
	if err := controller.EvaluateAutomaticScaleIn(); err != nil {
		t.Fatalf("expected delayed automatic scale-in evaluation to succeed, got error: %v", err)
	}

	select {
	case request := <-controller.scaleInRequests:
		if request.Reason == "" {
			t.Fatalf("expected automatic scale-in request to include a reason")
		}
	default:
		t.Fatalf("expected automatic scale-in request to be queued")
	}
}

func TestNodeScalingControllerEvaluateAutomaticScaleInSkipsAtMinimumBaseline(t *testing.T) {
	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 1,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "worker-1", Order: 1, Used: false},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("4"),
					v12.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		&v12.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "quota-a",
				Namespace: "default",
			},
			Spec: v12.ResourceQuotaSpec{
				Hard: v12.ResourceList{
					v12.ResourceLimitsCPU:    resource.MustParse("2"),
					v12.ResourceLimitsMemory: resource.MustParse("2Gi"),
				},
			},
		},
	)
	scalerClient := scalerfake.NewSimpleClientset(
		&scalerv1.QuotaAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scaler-a",
				Namespace: "default",
			},
			Spec: scalerv1.QuotaAutoscalerSpec{
				ResourceQuota: "quota-a",
			},
		},
	)

	controller := NewNodeScalingController(nil, client, store, time.Minute)
	controller.SetQuotaAutoscalerClient(scalerClient)
	controller.SetScaleInTriggerDelay(time.Minute)
	controller.runtime = &NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: t.TempDir(),
	}
	writeFile(t, filepath.Join(controller.runtime.RepoDir, defaultNodeScalingFile), machineDeploymentYAML(1))

	controller.scaleInEligibleSince = time.Now().Add(-2 * time.Minute)
	if err := controller.EvaluateAutomaticScaleIn(); err != nil {
		t.Fatalf("expected automatic scale-in evaluation to skip cleanly at minimum baseline, got error: %v", err)
	}
	select {
	case request := <-controller.scaleInRequests:
		t.Fatalf("expected no automatic scale-in request at minimum baseline, got %#v", request)
	default:
	}
}

func TestNodeScalingControllerEvaluateScaleInCapacityAllowsPartialScalingNodeRemoval(t *testing.T) {
	store := &captureNodeScalingInventoryStore{
		inventory: &inventoryv1.NodeScalingInventory{
			Spec: inventoryv1.NodeScalingInventorySpec{
				MachineDeploymentReplicas: 3,
				Nodes: []inventoryv1.NodeScalingInventoryNode{
					{Name: "scaling-a", Order: 1, Used: true},
					{Name: "scaling-b", Order: 2, Used: true},
				},
			},
		},
	}
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1"},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("4"),
					v12.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "scaling-a"},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("2"),
					v12.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "scaling-b"},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("2"),
					v12.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		},
		&v12.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "quota-a",
				Namespace: "default",
			},
			Spec: v12.ResourceQuotaSpec{
				Hard: v12.ResourceList{
					v12.ResourceLimitsCPU:    resource.MustParse("5"),
					v12.ResourceLimitsMemory: resource.MustParse("10Gi"),
				},
			},
		},
	)
	scalerClient := scalerfake.NewSimpleClientset(
		&scalerv1.QuotaAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "scaler-a",
				Namespace: "default",
			},
			Spec: scalerv1.QuotaAutoscalerSpec{
				ResourceQuota: "quota-a",
			},
		},
	)

	controller := NewNodeScalingController(nil, client, store, time.Minute)
	controller.SetQuotaAutoscalerClient(scalerClient)
	controller.runtime = &NodeScalingRuntime{
		Config:  NodeScalingConfig{RepoFilePath: defaultNodeScalingFile},
		RepoDir: t.TempDir(),
	}
	writeFile(t, filepath.Join(controller.runtime.RepoDir, defaultNodeScalingFile), machineDeploymentYAML(3))

	evaluation, err := controller.EvaluateScaleInCapacity()
	if err != nil {
		t.Fatalf("expected scale-in capacity evaluation to succeed, got error: %v", err)
	}
	if !evaluation.Eligible {
		t.Fatalf("expected one scaling node to be removable")
	}
	if len(evaluation.RemovableNodes) != 1 || evaluation.RemovableNodes[0] != "scaling-b" {
		t.Fatalf("expected only highest-order scaling-b to be removable first, got %#v", evaluation.RemovableNodes)
	}
}

func TestNodeScalingControllerNonScalingWorkerNodeCapacityIncludesLabelOnlyScalingNodes(t *testing.T) {
	client := fake.NewSimpleClientset(
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "worker-1",
				Labels: map[string]string{"role": "scaling"},
			},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("4"),
					v12.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
		&v12.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-2"},
			Spec: v12.NodeSpec{
				Taints: []v12.Taint{
					{Key: "node-role.kubernetes.io/scaling", Effect: v12.TaintEffectNoSchedule},
				},
			},
			Status: v12.NodeStatus{
				Allocatable: v12.ResourceList{
					v12.ResourceCPU:    resource.MustParse("4"),
					v12.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
	)

	controller := NewNodeScalingController(nil, client, nil, time.Minute)
	capacity, err := controller.NonScalingWorkerNodeCapacity()
	if err != nil {
		t.Fatalf("expected capacity evaluation to succeed, got error: %v", err)
	}
	if capacity.Cpu != 4000 {
		t.Fatalf("expected label-only scaling node to count toward CPU capacity, got %dm", capacity.Cpu)
	}
	expectedMemoryQuantity := resource.MustParse("8Gi")
	expectedMemory := expectedMemoryQuantity.ScaledValue(resource.Mega)
	if capacity.Memory != expectedMemory {
		t.Fatalf("expected only taint-free node memory to count, got %dM", capacity.Memory)
	}
}

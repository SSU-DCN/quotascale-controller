package nodescaling

import (
	"context"
	"fmt"
	"sort"

	inventoryv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/nodescalinginventory/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	defaultNodeScalingInventoryName = "default"
)

var nodeScalingInventoryGVR = schema.GroupVersionResource{
	Group:    "dcn.ssu.ac.kr",
	Version:  "v1",
	Resource: "nodescalinginventories",
}

type NodeScalingInventoryStore interface {
	Sync(machineDeploymentReplicas int32, nodeNames []string) error
	Get() (*inventoryv1.NodeScalingInventory, error)
	MarkNodeUsed(nodeName string) error
	MarkNodeUnused(nodeName string) error
	UpdateMachineDeploymentReplicas(replicas int32) error
}

type KubernetesNodeScalingInventoryStore struct {
	client        dynamic.Interface
	inventoryName string
}

func NewKubernetesNodeScalingInventoryStore(client dynamic.Interface) *KubernetesNodeScalingInventoryStore {
	return &KubernetesNodeScalingInventoryStore{
		client:        client,
		inventoryName: defaultNodeScalingInventoryName,
	}
}

func (store *KubernetesNodeScalingInventoryStore) Sync(machineDeploymentReplicas int32, nodeNames []string) error {
	inventory, err := store.Get()
	if apierrors.IsNotFound(err) {
		return store.create(machineDeploymentReplicas, nodeNames)
	}
	if err != nil {
		return err
	}

	inventory.TypeMeta = metav1.TypeMeta{
		APIVersion: "dcn.ssu.ac.kr/v1",
		Kind:       "NodeScalingInventory",
	}
	inventory.Spec = BuildNodeScalingInventorySpec(inventory.Spec, machineDeploymentReplicas, nodeNames)

	return store.update(inventory)
}

func (store *KubernetesNodeScalingInventoryStore) Get() (*inventoryv1.NodeScalingInventory, error) {
	existing, err := store.client.Resource(nodeScalingInventoryGVR).Get(context.TODO(), store.inventoryName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var inventory inventoryv1.NodeScalingInventory
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existing.Object, &inventory); err != nil {
		return nil, err
	}
	return &inventory, nil
}

func (store *KubernetesNodeScalingInventoryStore) MarkNodeUsed(nodeName string) error {
	return store.updateNodeUsage(nodeName, true)
}

func (store *KubernetesNodeScalingInventoryStore) MarkNodeUnused(nodeName string) error {
	return store.updateNodeUsage(nodeName, false)
}

func (store *KubernetesNodeScalingInventoryStore) updateNodeUsage(nodeName string, used bool) error {
	inventory, err := store.Get()
	if err != nil {
		return err
	}

	for i := range inventory.Spec.Nodes {
		if inventory.Spec.Nodes[i].Name != nodeName {
			continue
		}
		inventory.Spec.Nodes[i].Used = used
		return store.update(inventory)
	}

	return fmt.Errorf("node scaling inventory node %q not found", nodeName)
}

func (store *KubernetesNodeScalingInventoryStore) UpdateMachineDeploymentReplicas(replicas int32) error {
	inventory, err := store.Get()
	if err != nil {
		return err
	}

	inventory.Spec.MachineDeploymentReplicas = replicas
	return store.update(inventory)
}

func (store *KubernetesNodeScalingInventoryStore) update(inventory *inventoryv1.NodeScalingInventory) error {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inventory)
	if err != nil {
		return err
	}

	_, err = store.client.Resource(nodeScalingInventoryGVR).Update(context.TODO(), &unstructured.Unstructured{Object: object}, metav1.UpdateOptions{})
	return err
}

func (store *KubernetesNodeScalingInventoryStore) create(machineDeploymentReplicas int32, nodeNames []string) error {
	inventory := &inventoryv1.NodeScalingInventory{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "dcn.ssu.ac.kr/v1",
			Kind:       "NodeScalingInventory",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: store.inventoryName,
		},
		Spec: BuildNodeScalingInventorySpec(inventoryv1.NodeScalingInventorySpec{}, machineDeploymentReplicas, nodeNames),
	}

	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inventory)
	if err != nil {
		return err
	}

	_, err = store.client.Resource(nodeScalingInventoryGVR).Create(context.TODO(), &unstructured.Unstructured{Object: object}, metav1.CreateOptions{})
	return err
}

func BuildNodeScalingInventorySpec(existing inventoryv1.NodeScalingInventorySpec, machineDeploymentReplicas int32, nodeNames []string) inventoryv1.NodeScalingInventorySpec {
	spec := inventoryv1.NodeScalingInventorySpec{
		MachineDeploymentReplicas: machineDeploymentReplicas,
	}

	existingNodes := make([]inventoryv1.NodeScalingInventoryNode, 0, len(existing.Nodes))
	existingNodeNames := map[string]inventoryv1.NodeScalingInventoryNode{}
	var maxOrder int32

	for _, node := range existing.Nodes {
		if node.Name == "" {
			continue
		}
		existingNodes = append(existingNodes, node)
		existingNodeNames[node.Name] = node
		if node.Order > maxOrder {
			maxOrder = node.Order
		}
	}

	normalizedNames := normalizeNodeNames(nodeNames)
	for _, nodeName := range normalizedNames {
		if _, exists := existingNodeNames[nodeName]; exists {
			continue
		}
		maxOrder++
		existingNodes = append(existingNodes, inventoryv1.NodeScalingInventoryNode{
			Name:  nodeName,
			Order: maxOrder,
			Used:  false,
		})
	}

	sort.SliceStable(existingNodes, func(i, j int) bool {
		if existingNodes[i].Order == existingNodes[j].Order {
			return existingNodes[i].Name < existingNodes[j].Name
		}
		return existingNodes[i].Order < existingNodes[j].Order
	})

	spec.Nodes = existingNodes
	return spec
}

func normalizeNodeNames(nodeNames []string) []string {
	seen := map[string]struct{}{}
	normalized := make([]string, 0, len(nodeNames))

	for _, nodeName := range nodeNames {
		if nodeName == "" {
			continue
		}
		if _, exists := seen[nodeName]; exists {
			continue
		}
		seen[nodeName] = struct{}{}
		normalized = append(normalized, nodeName)
	}

	sort.Strings(normalized)
	return normalized
}

func FindFirstUnusedInventoryNode(inventory *inventoryv1.NodeScalingInventory) (*inventoryv1.NodeScalingInventoryNode, error) {
	if inventory == nil {
		return nil, fmt.Errorf("node scaling inventory is nil")
	}

	nodes := append([]inventoryv1.NodeScalingInventoryNode(nil), inventory.Spec.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Order == nodes[j].Order {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].Order < nodes[j].Order
	})

	for _, node := range nodes {
		if node.Used {
			continue
		}
		return &node, nil
	}

	return nil, fmt.Errorf("no unused scaling nodes available in inventory")
}

func FindHighestOrderUsedInventoryNode(inventory *inventoryv1.NodeScalingInventory) (*inventoryv1.NodeScalingInventoryNode, error) {
	if inventory == nil {
		return nil, fmt.Errorf("node scaling inventory is nil")
	}

	nodes := append([]inventoryv1.NodeScalingInventoryNode(nil), inventory.Spec.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Order == nodes[j].Order {
			return nodes[i].Name > nodes[j].Name
		}
		return nodes[i].Order > nodes[j].Order
	})

	for _, node := range nodes {
		if !node.Used {
			continue
		}
		return &node, nil
	}

	return nil, fmt.Errorf("no used scaling nodes available in inventory")
}

func CountUnusedInventoryNodes(inventory *inventoryv1.NodeScalingInventory) int {
	if inventory == nil {
		return 0
	}

	count := 0
	for _, node := range inventory.Spec.Nodes {
		if !node.Used {
			count++
		}
	}
	return count
}

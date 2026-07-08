package nodescaling

import (
	"reflect"
	"testing"

	inventoryv1 "github.com/SSU-DCN/quotascale-controller/pkg/scalerclient/apis/nodescalinginventory/v1"
)

func TestBuildNodeScalingInventorySpecAppendsNewNodesAndPreservesExistingState(t *testing.T) {
	existing := inventoryv1.NodeScalingInventorySpec{
		MachineDeploymentReplicas: 2,
		Nodes: []inventoryv1.NodeScalingInventoryNode{
			{Name: "node-b", Order: 2, Used: true},
		},
	}

	got := BuildNodeScalingInventorySpec(existing, 3, []string{"node-c", "node-a", "node-b"})

	if got.MachineDeploymentReplicas != 3 {
		t.Fatalf("expected replicas to be updated to 3, got %d", got.MachineDeploymentReplicas)
	}

	wantNodes := []inventoryv1.NodeScalingInventoryNode{
		{Name: "node-b", Order: 2, Used: true},
		{Name: "node-a", Order: 3, Used: false},
		{Name: "node-c", Order: 4, Used: false},
	}
	if !reflect.DeepEqual(got.Nodes, wantNodes) {
		t.Fatalf("unexpected nodes: %#v", got.Nodes)
	}
}

func TestFindFirstUnusedInventoryNodeReturnsLowestOrderUnusedNode(t *testing.T) {
	inventory := &inventoryv1.NodeScalingInventory{
		Spec: inventoryv1.NodeScalingInventorySpec{
			Nodes: []inventoryv1.NodeScalingInventoryNode{
				{Name: "node-c", Order: 3, Used: false},
				{Name: "node-a", Order: 1, Used: true},
				{Name: "node-b", Order: 2, Used: false},
			},
		},
	}

	got, err := FindFirstUnusedInventoryNode(inventory)
	if err != nil {
		t.Fatalf("expected unused node selection to succeed, got error: %v", err)
	}
	if got.Name != "node-b" {
		t.Fatalf("expected node-b to be selected first, got %s", got.Name)
	}
}

func TestFindHighestOrderUsedInventoryNodeReturnsHighestOrderUsedNode(t *testing.T) {
	inventory := &inventoryv1.NodeScalingInventory{
		Spec: inventoryv1.NodeScalingInventorySpec{
			Nodes: []inventoryv1.NodeScalingInventoryNode{
				{Name: "node-a", Order: 1, Used: true},
				{Name: "node-c", Order: 3, Used: false},
				{Name: "node-b", Order: 2, Used: true},
			},
		},
	}

	got, err := FindHighestOrderUsedInventoryNode(inventory)
	if err != nil {
		t.Fatalf("expected used node selection to succeed, got error: %v", err)
	}
	if got.Name != "node-b" {
		t.Fatalf("expected node-b to be selected, got %s", got.Name)
	}
}

func TestCountUnusedInventoryNodes(t *testing.T) {
	inventory := &inventoryv1.NodeScalingInventory{
		Spec: inventoryv1.NodeScalingInventorySpec{
			Nodes: []inventoryv1.NodeScalingInventoryNode{
				{Name: "node-a", Used: false},
				{Name: "node-b", Used: true},
				{Name: "node-c", Used: false},
			},
		},
	}

	if got := CountUnusedInventoryNodes(inventory); got != 2 {
		t.Fatalf("expected 2 unused nodes, got %d", got)
	}
}

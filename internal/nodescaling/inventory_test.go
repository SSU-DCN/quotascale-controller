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

func TestBuildNodeScalingInventorySpecRemovesNodesThatAreNoLongerObserved(t *testing.T) {
	existing := inventoryv1.NodeScalingInventorySpec{
		MachineDeploymentReplicas: 2,
		Nodes: []inventoryv1.NodeScalingInventoryNode{
			{Name: "node-a", Order: 1, Used: true},
			{Name: "node-b", Order: 2, Used: false},
			{Name: "node-c", Order: 3, Used: false},
		},
	}

	got := BuildNodeScalingInventorySpec(existing, 1, []string{"node-a"})

	wantNodes := []inventoryv1.NodeScalingInventoryNode{
		{Name: "node-a", Order: 1, Used: true},
	}
	if !reflect.DeepEqual(got.Nodes, wantNodes) {
		t.Fatalf("expected only currently observed nodes to remain, got %#v", got.Nodes)
	}
	if got.MachineDeploymentReplicas != 1 {
		t.Fatalf("expected desired replicas to be updated to 1, got %d", got.MachineDeploymentReplicas)
	}
}

func TestBuildNodeScalingInventorySpecKeepsObservedNodesEvenWhenTheyExceedDesiredReplicas(t *testing.T) {
	existing := inventoryv1.NodeScalingInventorySpec{
		MachineDeploymentReplicas: 2,
		Nodes: []inventoryv1.NodeScalingInventoryNode{
			{Name: "node-a", Order: 1, Used: true},
			{Name: "node-b", Order: 2, Used: false},
		},
	}

	got := BuildNodeScalingInventorySpec(existing, 0, []string{"node-a", "node-b"})

	wantNodes := []inventoryv1.NodeScalingInventoryNode{
		{Name: "node-a", Order: 1, Used: true},
		{Name: "node-b", Order: 2, Used: false},
	}
	if !reflect.DeepEqual(got.Nodes, wantNodes) {
		t.Fatalf("expected observed nodes to remain until they actually disappear, got %#v", got.Nodes)
	}
	if got.MachineDeploymentReplicas != 0 {
		t.Fatalf("expected desired replicas to be updated to 0, got %d", got.MachineDeploymentReplicas)
	}
}

func TestBuildNodeScalingInventoryStatusSummarizesDesiredReplicasAndUsage(t *testing.T) {
	spec := inventoryv1.NodeScalingInventorySpec{
		MachineDeploymentReplicas: 3,
		Nodes: []inventoryv1.NodeScalingInventoryNode{
			{Name: "node-a", Order: 1, Used: true},
			{Name: "node-b", Order: 2, Used: false},
			{Name: "node-c", Order: 3, Used: true},
		},
	}

	got := BuildNodeScalingInventoryStatus(spec)

	if got.Desired != 3 {
		t.Fatalf("expected desired replicas 3, got %d", got.Desired)
	}
	if got.Replicas != 3 {
		t.Fatalf("expected observed replicas 3, got %d", got.Replicas)
	}
	if got.Used != 2 {
		t.Fatalf("expected used nodes 2, got %d", got.Used)
	}
	if got.Unused != 1 {
		t.Fatalf("expected unused nodes 1, got %d", got.Unused)
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

func TestFindUnusedInventoryNodesReturnsLowestOrderUnusedNodes(t *testing.T) {
	inventory := &inventoryv1.NodeScalingInventory{
		Spec: inventoryv1.NodeScalingInventorySpec{
			Nodes: []inventoryv1.NodeScalingInventoryNode{
				{Name: "node-d", Order: 4, Used: false},
				{Name: "node-a", Order: 1, Used: true},
				{Name: "node-c", Order: 3, Used: false},
				{Name: "node-b", Order: 2, Used: false},
			},
		},
	}

	got, err := FindUnusedInventoryNodes(inventory, 2)
	if err != nil {
		t.Fatalf("expected unused node selection to succeed, got error: %v", err)
	}

	want := []inventoryv1.NodeScalingInventoryNode{
		{Name: "node-b", Order: 2, Used: false},
		{Name: "node-c", Order: 3, Used: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected unused nodes: %#v", got)
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

func TestFindHighestOrderUsedInventoryNodesReturnsMultipleHighestOrderUsedNodes(t *testing.T) {
	inventory := &inventoryv1.NodeScalingInventory{
		Spec: inventoryv1.NodeScalingInventorySpec{
			Nodes: []inventoryv1.NodeScalingInventoryNode{
				{Name: "node-a", Order: 1, Used: true},
				{Name: "node-d", Order: 4, Used: false},
				{Name: "node-c", Order: 3, Used: true},
				{Name: "node-b", Order: 2, Used: true},
			},
		},
	}

	got, err := FindHighestOrderUsedInventoryNodes(inventory, 2)
	if err != nil {
		t.Fatalf("expected used node selection to succeed, got error: %v", err)
	}

	want := []inventoryv1.NodeScalingInventoryNode{
		{Name: "node-c", Order: 3, Used: true},
		{Name: "node-b", Order: 2, Used: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected highest-order used nodes: %#v", got)
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

package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type NodeScalingInventory struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec NodeScalingInventorySpec `json:"spec,omitempty"`
}

type NodeScalingInventorySpec struct {
	MachineDeploymentReplicas int32                      `json:"machineDeploymentReplicas"`
	Nodes                     []NodeScalingInventoryNode `json:"nodes,omitempty"`
}

type NodeScalingInventoryNode struct {
	Name  string `json:"name"`
	Order int32  `json:"order"`
	Used  bool   `json:"used"`
}

type NodeScalingInventoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeScalingInventory `json:"items"`
}

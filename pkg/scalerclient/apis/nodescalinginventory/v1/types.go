package v1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type NodeScalingInventory struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeScalingInventorySpec   `json:"spec,omitempty"`
	Status NodeScalingInventoryStatus `json:"status,omitempty"`
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

type NodeScalingInventoryStatus struct {
	Desired  int32 `json:"desired,omitempty"`
	Replicas int32 `json:"replicas,omitempty"`
	Used     int32 `json:"used,omitempty"`
	Unused   int32 `json:"unused,omitempty"`
}

type NodeScalingInventoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []NodeScalingInventory `json:"items"`
}

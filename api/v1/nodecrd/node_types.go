/*
Copyright © 2020 Dell Inc. or its subsidiaries. All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package nodecrd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/dell/csi-baremetal/api/generated/v1"
)

// +kubebuilder:object:root=true

// Node is the Schema for the Node API
// +kubebuilder:resource:scope=Cluster,shortName={csibmnode,csibmnodes}
// +kubebuilder:printcolumn:name="UUID",type="string",JSONPath=".spec.UUID",description="Node Id"
// +kubebuilder:printcolumn:name="HOSTNAME",type="string",JSONPath=".spec.Addresses.Hostname",description="Node hostname"
// +kubebuilder:printcolumn:name="NODE_IP",type="string",JSONPath=".spec.Addresses.InternalIP",description="Node ip"
type Node struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              api.Node `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// NodeList contains a list of Node
//+kubebuilder:object:generate=true
type NodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Node `json:"items"`
}

func init() {
	SchemeBuilderACR.Register(&Node{}, &NodeList{})
}

func (in *Node) DeepCopyInto(out *Node) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

/*
Copyright 2022.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
)

// BMCMachineTemplateSpec defines the desired state of BMCMachineTemplate
type BMCMachineTemplateSpec struct {
	Template BMCMachineTemplateResource `json:"template"`
}

// BMCMachineTemplateResource describes the data needed to create am BMCMachine from a template.
type BMCMachineTemplateResource struct {
	// Standard object's metadata.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#metadata
	// +optional
	ObjectMeta clusterv1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the specification of the desired behavior of the machine.
	Spec BMCMachineSpec `json:"spec"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// BMCMachineTemplate is the Schema for the bmcmachinetemplates API
type BMCMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec BMCMachineTemplateSpec `json:"spec,omitempty"`
}

//+kubebuilder:object:root=true

// BMCMachineTemplateList contains a list of BMCMachineTemplate
type BMCMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BMCMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BMCMachineTemplate{}, &BMCMachineTemplateList{})
}

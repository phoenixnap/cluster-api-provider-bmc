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

const (
	// ClusterFinalizer allows ReconcileBMCCluster to clean up resources associated with BMCCluster before
	// removing it from the apiserver.
	ClusterFinalizer = "bmccluster.infrastructure.cluster.x-k8s.io"
)

// BMCClusterSpec defines the desired state of BMCCluster
type BMCClusterSpec struct {
	// The BMC location the cluster lives in. Must be one of PHX, ASH, SGP, NLD,
	// CHI, SEA or AUS.
	Location LocationID `json:"location"`

	// ControlPlaneEndpoint represents the endpoint used to communicate with the
	// control plane.
	// +optional
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint"`
}

// BMCClusterStatus defines the observed state of BMCCluster
type BMCClusterStatus struct {
	// Ready denotes that the cluster (infrastructure) is ready.
	// +optional
	Ready bool `json:"ready"`

	// ErrorMessage indicates that there is a error reconciling the
	// state, and will be set to a descriptive error message.
	// +optional
	ErrorMessage *string `json:"errorMessage,omitempty"`

	// ID is the BMC IP Block ID created for this cluster
	// +optional
	ID string `json:"id,omitempty"`

	// CIDR is the CIDR block in BMC created for this cluster
	// +optional
	CIDR string `json:"cidr,omitempty"`

	// BMCStatus is the status of the IP block as reported by BMC
	// +optional
	BMCStatus string `json:"status,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:resource:path=bmcclusters,scope=Namespaced,categories=cluster-api
//+kubebuilder:storageversion
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".metadata.labels.cluster\\.x-k8s\\.io/cluster-name"
//+kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready"
//+kubebuilder:printcolumn:name="IP-Block",type="string",JSONPath=".status.cidr"

// BMCCluster is the Schema for the bmcclusters API
type BMCCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BMCClusterSpec   `json:"spec,omitempty"`
	Status BMCClusterStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// BMCClusterList contains a list of BMCCluster
type BMCClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BMCCluster `json:"items"`
}

type TagList []Tag
type Tag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func init() {
	SchemeBuilder.Register(&BMCCluster{}, &BMCClusterList{})
}

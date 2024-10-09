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
	corev1 "k8s.io/api/core/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/errors"
)

const (
	// MachineFinalizer allows ReconcileBMCMachine to clean up resources associated with BMCMachine before
	// removing it from the apiserver.
	MachineFinalizer = "bmcmachine.infrastructure.cluster.x-k8s.io"
)

// BMCMachineSpec defines the desired state of BMCMachine
type BMCMachineSpec struct {
	// To be set after the BMC server is created.
	// Required by the cluster API machine controller.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// Description of server.
	// +kubebuilder:validation:MaxLength=250
	Description string `json:"description,omitempty"`

	// OS ID used for server creation.
	// +kubebuilder:validation:Required
	OS ServerOS `json:"os,omitempty"`

	// Server type used for creation.
	// +kubebuilder:validation:Required
	Type ServerType `json:"type,omitempty"`

	// Location ID where the server is created.
	// +kubebuilder:validation:Required
	Location LocationID `json:"location,omitempty"`

	// Whether or not to install SSH Keys marked as default in additionl to any SSH keys speficied on this resource.
	// Defaults to true.
	InstallDefaultSSHKeys *bool `json:"installDefaultSshKeys"`

	// A list of SSH key IDs (BMC resource ID) that will be installed on the server in addition default SSH keys if enabled.
	// +kubebuilder:validation:Optional
	SSHKeyIDs []string `json:"sshKeyIds,omitempty"`

	// The type of networks where this server should be attached.
	// +kubebuilder:validation:Optional
	NetworkType NetworkType `json:"networkType,omitempty"`
}

// NetworkType represents the type of networking configuraiton a server should use.
// Only one of the following network types may be specified.
// If none of the following network types are specified, the default one is PublicAndPrivate.
// +kubebuilder:validation:Enum=PUBLIC_AND_PRIVATE;PRIVATE_ONLY
type NetworkType string

const (
	PublicAndPrivate NetworkType = `PUBLIC_AND_PRIVATE`
	PrivateOnly      NetworkType = `PRIVATE_ONLY`
)

// LocationID identifies a BMC region.
// Only one of the following locations may be specified.
// If none of the following locations are specified, the default one is Phoenix.
// +kubebuilder:validation:Enum=PHX;ASH;SGP;NLD
type LocationID string

const (
	Phoenix   LocationID = `PHX`
	Ashburn   LocationID = `ASH`
	Singapore LocationID = `SGP`
	Amsterdam LocationID = `NLD`
)

// ServerOS describes the operating system image for this server.
// Only one of the following server OSs may be specified.
// If none of the following OSs are specified, the default one is UbuntuBionic.
// +kubebuilder:validation:Enum=ubuntu/bionic;ubuntu/focal;ubuntu/jammy
type ServerOS string

const (
	UbuntuBionic ServerOS = `ubuntu/bionic`
	UbuntuFocal  ServerOS = `ubuntu/focal`
	UbuntuJammy  ServerOS = `ubuntu/jammy`
)

// ServerType describes the hardware to allocate for this server.
// Only one of the following server types may be specified.
// If none of the following types are specified, the default one is S1C1Small.
// +kubebuilder:validation:Enum=s0.d1.small;s0.d1.medium;s1.c1.small;s1.c1.medium;s1.c2.medium;s1.c2.large;s1.e1.small;s1.e1.medium;s1.e1.large;s2.c1.small;s2.c1.medium;s2.c1.large;s2.c2.small;s2.c2.medium;s2.c2.large;d2.c1.medium;d2.c2.medium;d2.c3.medium;d2.c4.medium;d2.c5.medium;d2.c1.large;d2.c2.large;d2.c3.large;d2.c4.large;d2.c4.storage.pliops1;d2.c5.large;d2.m1.medium;d2.m1.large;d2.m2.medium;d2.m2.large;d2.m2.xlarge;d1.c1.small;d1.c2.small;d1.c3.small;d1.c4.small;d1.c1.medium;d1.c2.medium;d1.c3.medium;d1.c4.medium;d1.c1.large;d1.c2.large;d1.c3.large;d1.c4.large;d1.m1.medium;d1.m2.medium;d1.m3.medium;d1.m4.medium
type ServerType string

const (
	S0D1Small          ServerType = `s0.d1.small`
	S0D1Medium         ServerType = `s0.d1.medium`
	S1C1Small          ServerType = `s1.c1.small`
	S1C1Medium         ServerType = `s1.c1.medium`
	S1C2Medium         ServerType = `s1.c2.medium`
	S1C2Large          ServerType = `s1.c2.large`
	S1E1Small          ServerType = `s1.e1.small`
	S1E1Medium         ServerType = `s1.e1.medium`
	S1E1Large          ServerType = `s1.e1.large`
	S2C1Small          ServerType = `s2.c1.small`
	S2C1Medium         ServerType = `s2.c1.medium`
	S2C1Large          ServerType = `s2.c1.large`
	S2C2Small          ServerType = `s2.c2.small`
	S2C2Medium         ServerType = `s2.c2.medium`
	S2C2Large          ServerType = `s2.c2.large`
	D2C1Medium         ServerType = `d2.c1.medium`
	D2C2Medium         ServerType = `d2.c2.medium`
	D2C3Medium         ServerType = `d2.c3.medium`
	D2C4Medium         ServerType = `d2.c4.medium`
	D2C5Medium         ServerType = `d2.c5.medium`
	D2C1Large          ServerType = `d2.c1.large`
	D2C2Large          ServerType = `d2.c2.large`
	D2C3Large          ServerType = `d2.c3.large`
	D2C4Large          ServerType = `d2.c4.large`
	D2C4StoragePliops1 ServerType = `d2.c4.storage.pliops1`
	D2C5Large          ServerType = `d2.c5.large`
	D2M1Medium         ServerType = `d2.m1.medium`
	D2M1Large          ServerType = `d2.m1.large`
	D2M2Medium         ServerType = `d2.m2.medium`
	D2M2Large          ServerType = `d2.m2.large`
	D2M2Xlarge         ServerType = `d2.m2.xlarge`
	D1C1Small          ServerType = `d1.c1.small`
	D1C2Small          ServerType = `d1.c2.small`
	D1C3Small          ServerType = `d1.c3.small`
	D1C4Small          ServerType = `d1.c4.small`
	D1C1Medium         ServerType = `d1.c1.medium`
	D1C2Medium         ServerType = `d1.c2.medium`
	D1C3Medium         ServerType = `d1.c3.medium`
	D1C4Medium         ServerType = `d1.c4.medium`
	D1C1Large          ServerType = `d1.c1.large`
	D1C2Large          ServerType = `d1.c2.large`
	D1C3Large          ServerType = `d1.c3.large`
	D1C4Large          ServerType = `d1.c4.large`
	D1M1Medium         ServerType = `d1.m1.medium`
	D1M2Medium         ServerType = `d1.m2.medium`
	D1M3Medium         ServerType = `d1.m3.medium`
	D1M4Medium         ServerType = `d1.m4.medium`
)

// ServerPricingModel describes the pricing model used for a specific server resource.
// One on of the following pricing models may be specified.
// If none of the following types are specified, the default one is PMHourly.
// +kubebuilder:validation:Enum=HOURLY;ONE_MONTH_RESERVATION;TWELVE_MONTHS_RESERVATION;TWENTY_FOUR_MONTHS_RESERVATION;THIRTY_SIX_MONTHS_RESERVATION
type ServerPricingModel string

const (
	PMHourly                      ServerPricingModel = `HOURLY`
	PMOneMonthReservation         ServerPricingModel = `ONE_MONTH_RESERVATION`
	PMTwelveMonthsReservation     ServerPricingModel = `TWELVE_MONTHS_RESERVATION`
	PMTwentyFourMonthsReservation ServerPricingModel = `TWENTY_FOUR_MONTHS_RESERVATION`
	PMThirtySixMonthsReservation  ServerPricingModel = `THIRTY_SIX_MONTHS_RESERVATION`
)

// ServerStatus defines the observed state of Server
type BMCMachineStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	BMCServerID        string            `json:"id,omitempty"`
	BMCStatus          string            `json:"status,omitempty"`
	CPU                string            `json:"cpu,omitempty"`
	CPUCount           int32             `json:"cpuCount,omitempty"`
	CPUCores           int32             `json:"coresPerCpu,omitempty"`
	CPUFrequency       resource.Quantity `json:"cpuFrequency,omitempty"`
	Ram                string            `json:"ram,omitempty"`
	Storage            string            `json:"storage,omitempty"`
	PrivateIPAddresses []string          `json:"privateIpAddresses,omitempty"`
	PublicIPAddresses  []string          `json:"publicIpAddresses,omitempty"`

	// Cluster API fields
	Ready          bool                       `json:"ready,omitempty"`
	Addresses      []corev1.NodeAddress       `json:"addresses,omitempty"`
	FailureReason  *errors.MachineStatusError `json:"failureReason,omitempty"`
	FailureMessage *string                    `json:"failureMessage,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.ready"
//+kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.status"
//+kubebuilder:printcolumn:name="Address",type="string",JSONPath=".status.publicIpAddresses[0]"

// BMCMachine is the Schema for the bmcmachines API
type BMCMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BMCMachineSpec   `json:"spec,omitempty"`
	Status BMCMachineStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// BMCMachineList contains a list of BMCMachine
type BMCMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BMCMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BMCMachine{}, &BMCMachineList{})
}

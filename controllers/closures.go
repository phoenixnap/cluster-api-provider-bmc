package controllers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	apierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/pkg/errors"
	bmcv1 "github.com/pnap/cluster-api-provider-bmc/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
)

type MachineContext struct {
	BMCClient *http.Client
	K8sClient client.Client

	Recorder record.EventRecorder

	Cluster *clusterv1.Cluster
	Machine *clusterv1.Machine

	BMCCluster *bmcv1.BMCCluster
	BMCMachine *bmcv1.BMCMachine

	PatchHelper *patch.Helper
}

const (
	ANNOTATION_NAME_BMC_SERVER_ID                = `bmc.api.phoenixnap.com/server_id`
	ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID = `bmc.api.phoenixnap.com/cluster_ip_allocation_id`
	ANNOTATION_NAME_BMC_CLUSTER_IP               = `bmc.api.phoenixnap.com/cluster_ip`
)

func (mc *MachineContext) AddFini() {
	controllerutil.AddFinalizer(mc.BMCMachine, bmcv1.MachineFinalizer)
}

func (mc *MachineContext) RemoveFini() {
	controllerutil.RemoveFinalizer(mc.BMCMachine, bmcv1.MachineFinalizer)
}

func (mc MachineContext) HasFini() bool {
	return controllerutil.ContainsFinalizer(mc.BMCMachine, bmcv1.MachineFinalizer)
}

func (mc MachineContext) IsMarkedDeleted() bool {
	if mc.BMCMachine == nil {
		return false
	}
	return mc.BMCMachine.ObjectMeta.DeletionTimestamp != nil ||
		!mc.BMCMachine.ObjectMeta.DeletionTimestamp.IsZero()
}

func (mc MachineContext) GetMachineID() string {
	return mc.BMCMachine.Annotations[ANNOTATION_NAME_BMC_SERVER_ID]
}

func (mc *MachineContext) SetMachineID(id string) {
	nsid := fmt.Sprintf("bmc://servers/%s", id)
	mc.BMCMachine.Spec.ProviderID = &nsid
	mc.BMCMachine.Annotations[ANNOTATION_NAME_BMC_SERVER_ID] = id
}

func (mc MachineContext) IsInfrastructureReady() bool {
	return mc.Cluster.Status.InfrastructureReady
}

func (mc MachineContext) IsDataSecretReady() bool {
	return mc.Machine.Spec.Bootstrap.DataSecretName != nil
}

func (mc MachineContext) GetDataSecretValue(ctx context.Context) (string, error) {
	s := &corev1.Secret{}
	k := types.NamespacedName{
		Namespace: mc.BMCMachine.Namespace,
		Name:      *mc.Machine.Spec.Bootstrap.DataSecretName,
	}
	if err := mc.K8sClient.Get(ctx, k, s); err != nil {
		return ``, errors.Wrapf(err, "failed to retrieve bootstrap data secret for BMCMachine %s/%s", mc.BMCMachine.Namespace, mc.BMCMachine.Name)
	}
	value, ok := s.Data[`value`]
	if !ok {
		return ``, fmt.Errorf("bootstrap value is missing in data secret for BMCMachine %s/%s", mc.BMCMachine.Namespace, mc.BMCMachine.Name)
	}
	return string(value), nil
}

func (mc MachineContext) IsStatusEqual(s string) bool {
	return mc.BMCMachine.Status.BMCStatus == string(s)
}

func (mc *MachineContext) SetStatus(s bmcv1.BMCMachineStatus) {
	mc.BMCMachine.Status = s
}

func (mc *MachineContext) MergeBMCStatusProperties(s bmcv1.BMCMachineStatus) {
	mc.BMCMachine.Status.BMCServerID = s.BMCServerID
	mc.BMCMachine.Status.BMCStatus = s.BMCStatus
	mc.BMCMachine.Status.CPU = s.CPU
	mc.BMCMachine.Status.CPUCount = s.CPUCount
	mc.BMCMachine.Status.CPUCores = s.CPUCores
	mc.BMCMachine.Status.CPUFrequency = s.CPUFrequency
	mc.BMCMachine.Status.Ram = s.Ram
	mc.BMCMachine.Status.Storage = s.Storage
	mc.BMCMachine.Status.PrivateIPAddresses = s.PrivateIPAddresses
	mc.BMCMachine.Status.PublicIPAddresses = s.PublicIPAddresses
}

func (mc *MachineContext) SetBMCStatus(s string) {
	mc.BMCMachine.Status.BMCStatus = string(s)
}
func (mc *MachineContext) SetNodeRef(ctx context.Context, cl client.Client) {
	if mc.BMCMachine.Status.NodeRef == nil {
		clusterKey := types.NamespacedName{
			Namespace: mc.Cluster.Namespace,
			Name:      mc.Cluster.Name,
		}

		remoteClient, err := GetRemoteClient(ctx, cl, clusterKey)
		if err != nil {
			mc.Eventf(`Warning`, "FailureNodeRef", "Getting remote client failed. %s", fmt.Sprintln(err))
			return
		}
		// Retrieve the remote node
		nodeName := mc.GetHostname()
		node := &corev1.Node{}
		nodeKey := types.NamespacedName{
			Namespace: "",
			Name:      nodeName,
		}
		if err := remoteClient.Get(ctx, nodeKey, node); err != nil {
			mc.Eventf(`Warning`, "FailureNodeRef", "Getting node failed. %s", fmt.Sprintln(err))
			return
		}

		if mc.BMCMachine.Status.NodeRef == nil {
			mc.BMCMachine.Status.NodeRef = &corev1.ObjectReference{
				APIVersion: corev1.SchemeGroupVersion.String(),
				Kind:       "Node",
				Name:       node.Name,
				UID:        node.UID,
			}

			mc.Event(`Normal`, "SuccessNodeRef", "Succesfully set node ref "+mc.BMCMachine.Status.NodeRef.Name)
		}
		// Update the node's Spec.ProviderID
		patchHelper, err := patch.NewHelper(node, remoteClient)
		if err != nil {
			mc.Eventf(`Warning`, "FailureNodeRef", "failed to create patchHelper for the workload cluster node %s", fmt.Sprintln(err))
			return
		}

		node.Spec.ProviderID = *mc.BMCMachine.Spec.ProviderID
		err = patchHelper.Patch(ctx, node)
		if err != nil {
			mc.Eventf(`Warning`, "FailureNodeRef", "failed to patch the remote workload cluster node %s", fmt.Sprintln(err))
			return
		}
	}
}

func (mc *MachineContext) GetBMCStatus() string {
	return mc.BMCMachine.Status.BMCStatus
}

func (mc *MachineContext) GetHostname() string {
	r := strings.NewReplacer(".", "-", "/", "-")
	return r.Replace(mc.BMCMachine.Name)
}

func (mc MachineContext) IsControlPlaneMachine() bool {
	return util.IsControlPlaneMachine(mc.Machine)
}

func (mc MachineContext) Event(eventtype, reason, message string) {
	mc.Recorder.Event(mc.BMCMachine, eventtype, reason, message)
}

func (mc MachineContext) Eventf(eventtype, reason, messageFmt string, args ...interface{}) {
	mc.Recorder.Eventf(mc.BMCMachine, eventtype, reason, messageFmt, args...)
}

func (mc MachineContext) GetClusterIPAllocationID() string {
	return mc.BMCCluster.Annotations[ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID]
}

func (mc *MachineContext) SetIrreconcilable(err apierrors.MachineStatusError, msg string) {
	mc.BMCMachine.Status.FailureReason = &err
	mc.BMCMachine.Status.FailureMessage = &msg
	mc.BMCMachine.Status.BMCStatus = StatusIrreconcilable
}

func (mc *MachineContext) SetReady() {
	mc.BMCMachine.Status.Ready = true
	addrs := []corev1.NodeAddress{}
	for _, a := range mc.BMCMachine.Status.PublicIPAddresses {
		addrs = append(addrs, corev1.NodeAddress{
			Type:    corev1.NodeExternalIP,
			Address: a})
	}
	mc.BMCMachine.Status.Addresses = addrs
}

func (mc *MachineContext) IsReady() bool {
	return mc.BMCMachine.Status.Ready
}

type ClusterContext struct {
	BMCClient *http.Client
	K8sClient client.Client

	Cluster    *clusterv1.Cluster
	BMCCluster *bmcv1.BMCCluster

	Recorder    record.EventRecorder
	PatchHelper *patch.Helper
}

func (cc ClusterContext) IsMarkedDeleted() bool {
	if cc.BMCCluster == nil {
		return false
	}
	return cc.BMCCluster.ObjectMeta.DeletionTimestamp != nil ||
		!cc.BMCCluster.ObjectMeta.DeletionTimestamp.IsZero()
}

func (cc *ClusterContext) AddFini() {
	controllerutil.AddFinalizer(cc.BMCCluster, bmcv1.ClusterFinalizer)
}

func (cc *ClusterContext) RemoveFini() {
	controllerutil.RemoveFinalizer(cc.BMCCluster, bmcv1.ClusterFinalizer)
}

func (cc ClusterContext) HasFini() bool {
	return controllerutil.ContainsFinalizer(cc.BMCCluster, bmcv1.ClusterFinalizer)
}

func (cc ClusterContext) GetIPAllocationID() string {
	return cc.BMCCluster.Annotations[ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID]
}

func (cc *ClusterContext) SetIPAllocationID(id string) {
	cc.BMCCluster.Annotations[ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID] = id
}

func (cc *ClusterContext) SetIP(ip string) {
	cc.BMCCluster.Annotations[ANNOTATION_NAME_BMC_CLUSTER_IP] = ip
}

func (cc *ClusterContext) GetIP() string {
	return cc.BMCCluster.Annotations[ANNOTATION_NAME_BMC_CLUSTER_IP]
}

func (cc ClusterContext) Event(eventtype, reason, message string) {
	cc.Recorder.Event(cc.BMCCluster, eventtype, reason, message)
}

func (cc ClusterContext) Eventf(eventtype, reason, messageFmt string, args ...interface{}) {
	cc.Recorder.Eventf(cc.BMCCluster, eventtype, reason, messageFmt, args...)
}

func (cc *ClusterContext) SetStatus(s bmcv1.BMCClusterStatus) {
	cc.BMCCluster.Status.ID = s.ID
	cc.BMCCluster.Status.CIDR = s.CIDR
	cc.BMCCluster.Status.BMCStatus = s.BMCStatus
}

func (cc *ClusterContext) SetBMCStatus(s string) {
	cc.BMCCluster.Status.BMCStatus = string(s)
}

func (cc *ClusterContext) GetBMCStatus() string {
	return cc.BMCCluster.Status.BMCStatus
}

func (cc ClusterContext) IsStatusEqual(s string) bool {
	return cc.BMCCluster.Status.BMCStatus == string(s)
}

func (cc *ClusterContext) SetControlPlaneEndpoint(host string, port int) {
	cc.BMCCluster.Spec.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: host, Port: int32(port)}
}

func (cc *ClusterContext) SetReady() {
	cc.BMCCluster.Status.Ready = true
}

func (cc *ClusterContext) IsReady() bool {
	return cc.BMCCluster.Status.Ready
}

func GetRemoteClient(ctx context.Context, client client.Client, clusterKey client.ObjectKey) (client.Client, error) {

	remoteClient, err := remote.NewClusterClient(ctx, "remote-cluster-cache", client, clusterKey)
	if err != nil {
		return nil, err
	}

	return remoteClient, nil
}

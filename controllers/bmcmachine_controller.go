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

package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/oauth2/clientcredentials"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	capierrors "sigs.k8s.io/cluster-api/errors"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	"github.com/phoenixnap/cluster-api-provider-bmc/api/v1beta1"
	bmcv1 "github.com/phoenixnap/cluster-api-provider-bmc/api/v1beta1"
	"github.com/pkg/errors"
	"sigs.k8s.io/controller-runtime/pkg/builder"
)

// BMCMachineReconciler reconciles a BMCMachine object
type BMCMachineReconciler struct {
	client.Client
	Recorder         record.EventRecorder
	ReconcileTimeout time.Duration
	BMCClientConfig  clientcredentials.Config
	BMCEndpointURL   string

	api apiClient
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=bmcmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=bmcmachines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=bmcmachines/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups="",resources=secrets;,verbs=get;list;watch

type Status string

const (
	ENV_BMC_CLIENT_ID     = `BMC_CLIENT_ID`
	ENV_BMC_CLIENT_SECRET = `BMC_CLIENT_SECRET`
	ENV_BMC_TOKEN_URL     = `BMC_TOKEN_URL`
	ENV_BMC_ENDPOINT_URL  = `BMC_ENDPOINT_URL`
)

var (
	noRequeue        = ctrl.Result{}
	requeueAfter1Min = ctrl.Result{RequeueAfter: 1 * time.Minute}
	requeueAfter2Min = ctrl.Result{RequeueAfter: 2 * time.Minute}
	requeueAfter5Min = ctrl.Result{RequeueAfter: 5 * time.Minute}

	EventTypeNormal  = `Normal`
	EventTypeWarning = `Warning`

	// UpperCamelCase
	EventReasonCleanupError   = `CleanupError`
	EventReasonCleanupSuccess = `CleanupSuccess`

	EventReasonCreated              = `Created`
	EventReasonCreateError          = `CreateError`
	EventReasonCreateErrorPermanent = `CreateErrorPermanent`
	EventReasonCreateErrorInventory = `CreateErrorInventory`
	EventReasonCreateFailure        = `CreateServerFailure`

	EventReasonResourceOrphaned = `ResourceOrphaned`
	EventReasonPollFailure      = `PollingFailure`
	EventReasonStatusChange     = `StatusChange`

	StatusIrreconcilable = `irreconcilable`
	StatusOrphaned       = `orphaned`
	StatusStale          = `stale`
	StatusPoweredOn      = `powered-on`
	StatusError          = `error`
	StatusAssigned       = `assigned`
	StatusNotAssigned    = `unassigned`
)

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// Reconcile function to compare the state specified by
// the BMCMachine object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.1/pkg/reconcile
func (r *BMCMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.ReconcileTimeout)
	defer cancel()

	log := ctrl.LoggerFrom(ctx)

	// 1. Load the BMC client
	bmc := r.api.get(r.BMCClientConfig)

	// 2. get the BMCMachine resource
	var bmcMachine bmcv1.BMCMachine
	if err := r.Get(ctx, req.NamespacedName, &bmcMachine); err != nil {
		return noRequeue, client.IgnoreNotFound(err)
	}

	// 3. Retrieve the ClusterAPI Machine
	machine, err := util.GetOwnerMachine(ctx, r.Client, bmcMachine.ObjectMeta)
	if err != nil {
		return noRequeue, err
	}
	if machine == nil {
		log.Info(`No OwnerRef set`)
		return noRequeue, nil
	}

	// Gather context
	helper, _ := patch.NewHelper(&bmcMachine, r.Client)
	mc := MachineContext{
		BMCClient:   bmc,
		K8sClient:   r.Client,
		Machine:     machine,
		BMCMachine:  &bmcMachine,
		Recorder:    r.Recorder,
		PatchHelper: helper,
	}

	// Always persist changes to the BMCMachine when the reconcile function returns
	defer func() {
		if err := mc.PatchHelper.Patch(ctx, mc.BMCMachine); err != nil {
			log.Error(err, `Unable to patch BMCMachine`)
		}
	}()

	if mc.IsMarkedDeleted() {
		return r.reconcileDelete(ctx, &mc)
	}

	// 4. Retrieve the ClusterAPI Cluster
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("Machine is missing cluster label or cluster does not exist")
		return noRequeue, err
	}

	// 5. Retrieve the BMCCluster (because you can't have a BMCMachine without a cluster)
	var bmcCluster bmcv1.BMCCluster
	cnname := client.ObjectKey{Namespace: bmcMachine.Namespace, Name: cluster.Spec.InfrastructureRef.Name}
	if err := r.Get(ctx, cnname, &bmcCluster); err != nil {
		log.Info("BMCCluster is not available")
		return noRequeue, nil
	}

	// 6. Check to see if reconciliation is paused
	if annotations.IsPaused(cluster, &bmcCluster) {
		log.Info(`Cluster reconciliation is paused`)
		return noRequeue, nil
	}

	mc.Cluster = cluster
	mc.BMCCluster = &bmcCluster

	return r.reconcileLive(ctx, &mc)
}

// reconcileLive determines current state of cluster
func (r *BMCMachineReconciler) reconcileLive(ctx context.Context, mc *MachineContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Add the finalizer if missing
	if !mc.HasFini() {
		mc.AddFini()
	}

	if !mc.IsInfrastructureReady() {
		log.Info(`Cluster infrastructure is not yet ready`)
		return noRequeue, nil
	}

	// Validate that the bootstrap data is available
	if !mc.IsDataSecretReady() {
		log.Info(`Data secret is not yet ready`)
		return noRequeue, nil
	}

	// The bmcServerIDAnnotation is used to store the BMC Server ID assocaited with this machine. If
	// the annotation is not present then the server has yet to be created and so this function
	// should proceed with the creation flow. If it is set, then poll for the current status of the
	// underlying server and make updates where appropriate.
	//
	// Create, poll, or update?
	if len(mc.GetMachineID()) == 0 {
		return r.reconcileCreate(ctx, mc)
	}
	return r.reconcileSynchronize(ctx, mc)
}

// reconcileCreate create BMCMachine
func (r *BMCMachineReconciler) reconcileCreate(ctx context.Context, mc *MachineContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	if mc.IsStatusEqual(StatusIrreconcilable) {
		return noRequeue, nil
	}

	// adopt a server from an earlier create whose ID was never recorded
	if requested := createRequestedAt(&mc.BMCMachine.ObjectMeta); !requested.IsZero() {
		found, err := r.findCreatedServer(ctx, mc, requested)
		if err != nil {
			return requeueAfter1Min, err
		}
		switch len(found) {
		case 0:
		case 1:
			var ss bmcv1.BMCMachineStatus
			if err := json.Unmarshal(found[0], &ss); err != nil {
				return requeueAfter1Min, err
			}
			mc.SetStatus(ss)
			mc.SetMachineID(ss.BMCServerID)
			clearCreateRequested(&mc.BMCMachine.ObjectMeta)
			return requeueAfter1Min, nil
		default:
			msg := fmt.Sprintf("%d BMC servers named %s were created; delete the extras manually", len(found), mc.GetHostname())
			mc.Event(EventTypeWarning, EventReasonCreateErrorPermanent, msg)
			mc.SetIrreconcilable(capierrors.CreateMachineError, msg)
			return noRequeue, nil
		}
	}

	// Retrieve bootstrap data
	bootstrap, err := mc.GetDataSecretValue(ctx)
	if err != nil {
		return noRequeue, err
	}

	encodedBootstrap := make([]byte, base64.StdEncoding.EncodedLen(len(bootstrap)))
	base64.StdEncoding.Encode(encodedBootstrap, []byte(bootstrap))
	request := BMCCreateServerRequest{
		Hostname:              mc.GetHostname(),
		Description:           mc.BMCMachine.Spec.Description,
		OS:                    mc.BMCMachine.Spec.OS,
		Type:                  mc.BMCMachine.Spec.Type,
		Location:              mc.BMCMachine.Spec.Location,
		InstallDefaultSSHKeys: mc.BMCMachine.Spec.InstallDefaultSSHKeys,
		SSHKeyIDs:             mc.BMCMachine.Spec.SSHKeyIDs,
		NetworkType:           mc.BMCMachine.Spec.NetworkType,
		OSConfiguration:       OSConfiguration{CloudInit: CloudInit{UserData: string(encodedBootstrap)}},
	}

	//do not to assign control plane ip to specific server, if using kube-vip and bgp
	if mc.IsControlPlaneMachine() && mc.BMCCluster.Spec.VIPManager == v1beta1.NONE {
		// if its a control plane machine then add the public network configuration
		// if control plane && ipid == `` then we have a problem
		ipid := mc.GetClusterIPAllocationID()
		if len(ipid) <= 0 {
			log.Info(`Cluster address block is not yet ready`)
			return noRequeue, nil
		}
		request.NetworkConfiguration = NetworkConfiguration{
			IPBlocksConfiguration: IPBlocksConfiguration{
				ConfigurationType: `USER_DEFINED`,
				IPBlocks:          []IPBlock{{ID: ipid}},
			},
		}
	}

	// save the marker before creating, so the server can be found if its ID is lost
	setCreateRequested(&mc.BMCMachine.ObjectMeta, time.Now())
	if err := mc.PatchHelper.Patch(ctx, mc.BMCMachine); err != nil {
		return requeueAfter1Min, err
	}
	// diff later patches against the saved object, so clearing the marker is sent
	h, err := patch.NewHelper(mc.BMCMachine, mc.K8sClient)
	if err != nil {
		return requeueAfter1Min, err
	}
	mc.PatchHelper = h

	// Create bmc server
	code, body, err := callAPI(ctx, mc.BMCClient, http.MethodPost, fmt.Sprintf("%s/bmc/v1/servers", r.BMCEndpointURL), request)
	if err != nil {
		mc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
		return requeueAfter1Min, err
	}

	switch code {
	case 400, 401, 403, 409:
		// rejected: nothing was created, and retrying won't help
		log.Info("Unable to reconcile", `code`, code, `body`, string(body))
		clearCreateRequested(&mc.BMCMachine.ObjectMeta)
		if !mc.IsStatusEqual(StatusIrreconcilable) {
			mc.Eventf(EventTypeWarning, EventReasonCreateErrorPermanent, "Code: %v", code)
			mc.SetIrreconcilable(capierrors.InvalidConfigurationMachineError, fmt.Sprintf("BMC create code %v: %s", code, body))
		}
		return noRequeue, nil
	case 406:
		// no inventory, backoff and retry
		log.Info("Temporary no inventory", `code`, 406, `body`, string(body))
		clearCreateRequested(&mc.BMCMachine.ObjectMeta)
		mc.Eventf(EventTypeWarning, EventReasonCreateErrorInventory, `Code: %v`, code)
		return requeueAfter5Min, nil
	case 200, 201, 202, 204:
		// the call was successful, do nothing and continue reconciliation
	default:
		// the server may still have been created; the next reconcile looks for it
		log.Info(`Unexpected response from API`, `statusCode`, code, `body`, string(body))
		mc.Eventf(EventTypeWarning, EventReasonCreateFailure, `Unexpected response from API: %v`, code)
		return requeueAfter2Min, nil
	}

	// Set the resulting server ID in the annotation and set status
	var ss bmcv1.BMCMachineStatus
	if err := json.Unmarshal(body, &ss); err != nil || ss.BMCServerID == "" {
		mc.Eventf(EventTypeWarning, EventReasonCreateError, "unable to read the created server: %v", err)
		return requeueAfter1Min, nil
	}

	mc.Eventf(EventTypeNormal, EventReasonCreated, "created BMC server %s", ss.BMCServerID)
	mc.SetStatus(ss)
	mc.SetMachineID(ss.BMCServerID)
	clearCreateRequested(&mc.BMCMachine.ObjectMeta)
	return requeueAfter1Min, nil
}

// findCreatedServer returns the servers with this machine's hostname and location that
// were provisioned since the create was requested
func (r *BMCMachineReconciler) findCreatedServer(ctx context.Context, mc *MachineContext, requested time.Time) ([]json.RawMessage, error) {
	raw, listed, err := listResources(ctx, mc.BMCClient, fmt.Sprintf("%s/bmc/v1/servers", r.BMCEndpointURL))
	if err != nil {
		return nil, err
	}
	var found []json.RawMessage
	for i, s := range listed {
		if s.Hostname != mc.GetHostname() || s.Location != string(mc.BMCMachine.Spec.Location) {
			continue
		}
		if s.ProvisionedOn != nil && s.ProvisionedOn.Before(requested.Add(-time.Minute)) {
			continue
		}
		found = append(found, raw[i])
	}
	return found, nil
}

// reconcileSynchronize synchronize BMCMachine
func (r *BMCMachineReconciler) reconcileSynchronize(ctx context.Context, mc *MachineContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	code, body, err := callAPI(ctx, mc.BMCClient, http.MethodGet, fmt.Sprintf("%s/bmc/v1/servers/%s", r.BMCEndpointURL, mc.GetMachineID()), nil)
	if err != nil {
		if !mc.IsStatusEqual(StatusStale) {
			mc.Eventf(EventTypeNormal, EventReasonStatusChange, `%v -> %v`, mc.GetBMCStatus(), StatusStale)
			mc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}

	switch code {
	case 400:
		// bad data, or controller/API incompatibility
		log.Info("Unable to reconcile, bad request", `code`, 400, `body`, string(body))
		if !mc.IsStatusEqual(StatusIrreconcilable) {
			mc.Eventf(EventTypeWarning, EventReasonPollFailure, "Code: %v", code)
			mc.SetIrreconcilable(capierrors.InvalidConfigurationMachineError, fmt.Sprintf("BMC read code %v", code))
		}
		return requeueAfter5Min, nil
	case 401:
		// bad credentials
		log.Info("Unable to reconcile", `code`, 401, `body`, string(body))
		if !mc.IsStatusEqual(StatusIrreconcilable) {
			mc.Eventf(EventTypeWarning, EventReasonPollFailure, "Code: %v", code)
			mc.SetIrreconcilable(capierrors.InvalidConfigurationMachineError, fmt.Sprintf("BMC read code %v", code))
		}
		return requeueAfter5Min, nil
	case 403, 404:
		gone, err := confirmGone(ctx, mc.BMCClient, fmt.Sprintf("%s/bmc/v1/servers", r.BMCEndpointURL), mc.GetMachineID())
		if err != nil || !gone {
			log.Info("Unable to read server", `code`, code, `body`, string(body), `listError`, err)
			if !mc.IsStatusEqual(StatusStale) {
				mc.Eventf(EventTypeWarning, EventReasonPollFailure, "Access to BMC server %s was denied: %v", mc.GetMachineID(), code)
				mc.SetBMCStatus(StatusStale)
			}
			return requeueAfter5Min, err
		}
		if !mc.IsStatusEqual(StatusOrphaned) {
			mc.Eventf(EventTypeWarning, EventReasonResourceOrphaned, "BMC server %s no longer exists", mc.GetMachineID())
			mc.SetBMCStatus(StatusOrphaned)
		}
		return noRequeue, nil
	case 200, 201, 202, 204:
		// the call was successful, do nothing and continue synchronizing
	default:
		// temporarily unavailable, backoff and retry
		log.Info(`Unexpected response from API`, `statusCode`, code, `body`, string(body))
		if !mc.IsStatusEqual(StatusStale) {
			mc.Eventf(EventTypeWarning, EventReasonPollFailure, "Code: %v", code)
			mc.SetBMCStatus(StatusStale)
		}
		return requeueAfter5Min, nil
	}

	// Update the status
	var ss bmcv1.BMCMachineStatus
	err = json.Unmarshal(body, &ss)
	if err != nil {
		log.Info(`Unable to unmarshal BMC get server response`, `body`, string(body))
		if !mc.IsStatusEqual(StatusStale) {
			mc.Eventf(EventTypeWarning, EventReasonPollFailure, "error: %v", err)
			mc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}

	// Detect a status delta
	if !mc.IsStatusEqual(ss.BMCStatus) {
		mc.Eventf(`Normal`, EventReasonStatusChange, `%v -> %v`, mc.GetBMCStatus(), ss.BMCStatus)
		mc.MergeBMCStatusProperties(ss)
		switch mc.GetBMCStatus() {
		case StatusPoweredOn:
			{
				mc.SetReady()
			}
		case StatusError:
			mc.SetIrreconcilable(capierrors.CreateMachineError, `unrecoverable error while creating the resource at BMC`)
		}
	}

	// BMC server details are mostly immutable. However servers do have
	// power state and can have SSH and other OS configuration, "reset."
	// A ValidatingWebhook should prevent users or other controllers from
	// changing a server resource.
	// Poll timing based on status and expected change
	switch ss.BMCStatus {
	case StatusPoweredOn:
		{
			mc.SetNodeRef(ctx, r.Client)
			if !mc.IsReady() {
				mc.MergeBMCStatusProperties(ss)
				mc.SetReady()
			}
			return requeueAfter2Min, nil
		}
	case StatusError:
		return noRequeue, nil
	default:
		return requeueAfter1Min, nil
	}
}

// reconcileDelete deprovisions the BMC server, keeping the finalizer until that is accepted
// or the server is confirmed gone
func (r *BMCMachineReconciler) reconcileDelete(ctx context.Context, mc *MachineContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	serversURL := fmt.Sprintf("%s/bmc/v1/servers", r.BMCEndpointURL)

	bmcServerID := mc.GetMachineID()
	if len(bmcServerID) == 0 {
		requested := createRequestedAt(&mc.BMCMachine.ObjectMeta)
		if requested.IsZero() {
			mc.RemoveFini()
			return noRequeue, nil
		}
		found, err := r.findCreatedServer(ctx, mc, requested)
		if err != nil {
			return requeueAfter1Min, err
		}
		if len(found) == 0 {
			mc.RemoveFini()
			return noRequeue, nil
		}
		var ss bmcv1.BMCMachineStatus
		if err := json.Unmarshal(found[0], &ss); err != nil {
			return requeueAfter1Min, err
		}
		bmcServerID = ss.BMCServerID
		mc.SetMachineID(bmcServerID)
	}
	if mc.IsStatusEqual(StatusOrphaned) {
		mc.RemoveFini()
		return noRequeue, nil
	}

	// tear down the BMC server
	code, body, err := callAPI(ctx, mc.BMCClient, http.MethodPost, fmt.Sprintf("%s/%s/actions/deprovision", serversURL, bmcServerID), map[string]bool{"deleteIpBlocks": true})
	if err != nil {
		mc.Event(EventTypeWarning, EventReasonCleanupError, err.Error())
		return requeueAfter1Min, err
	}

	switch code {
	case 200, 201, 202, 204:
		mc.Eventf(EventTypeNormal, EventReasonCleanupSuccess, "Deleted BMC server %s", bmcServerID)
	case 403, 404:
		gone, err := confirmGone(ctx, mc.BMCClient, serversURL, bmcServerID)
		if err != nil {
			mc.Eventf(EventTypeWarning, EventReasonCleanupError, "unable to confirm BMC server %s is gone: %v", bmcServerID, err)
			return requeueAfter2Min, err
		}
		if !gone {
			mc.Eventf(EventTypeWarning, EventReasonCleanupError, "access to BMC server %s was denied and it still exists", bmcServerID)
			return requeueAfter5Min, nil
		}
	default:
		// e.g. 409 while the server is still provisioning
		log.Info("Unable to delete", `code`, code, `body`, string(body))
		mc.Eventf(EventTypeWarning, EventReasonCleanupError, "deprovisioning BMC server %s failed with %v; retrying", bmcServerID, code)
		return requeueAfter2Min, nil
	}

	// At this point the BMC server is being deprovisioned or is gone
	mc.RemoveFini()
	return noRequeue, nil
}

// BMCCreateServerRequest creates a BMC server
type BMCCreateServerRequest struct {
	Hostname              string               `json:"hostname,omitempty"`
	Description           string               `json:"description,omitempty"`
	OS                    bmcv1.ServerOS       `json:"os,omitempty"`
	Type                  bmcv1.ServerType     `json:"type,omitempty"`
	Location              bmcv1.LocationID     `json:"location,omitempty"`
	InstallDefaultSSHKeys *bool                `json:"installDefaultSshKeys"`
	SSHKeyIDs             []string             `json:"sshKeyIds,omitempty"`
	NetworkType           bmcv1.NetworkType    `json:"networkType,omitempty"`
	OSConfiguration       OSConfiguration      `json:"osConfiguration,omitempty"`
	NetworkConfiguration  NetworkConfiguration `json:"networkConfiguration"`
}

// OSConfiguration configurations of server
type OSConfiguration struct {
	CloudInit CloudInit `json:"cloudInit,omitempty"`
}

// CloudInit configurations of server
type CloudInit struct {
	UserData string `json:"userData,omitempty"`
}

// NetworkConfiguration configurations of server
type NetworkConfiguration struct {
	IPBlocksConfiguration IPBlocksConfiguration `json:"ipBlocksConfiguration,omitempty"`
}

// IPBlocksConfiguration configurations of server
type IPBlocksConfiguration struct {
	ConfigurationType string    `json:"configurationType,omitempty"`
	IPBlocks          []IPBlock `json:"ipBlocks,omitempty"`
}

// IPBlock configurations of server
type IPBlock struct {
	ID string `json:"id,omitempty"`
}

// SetupWithManager sets up the controller with the Manager.
func (r *BMCMachineReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {

	objFunc, err := util.ClusterToTypedObjectsMapper(r.Client, &bmcv1.BMCMachineList{}, mgr.GetScheme())
	if err != nil {
		return errors.Wrapf(err, `Failed to create cluster to object mapper func`)
	}

	_, err = ctrl.NewControllerManagedBy(mgr).
		For(&bmcv1.BMCMachine{}).
		WithEventFilter(predicates.ResourceNotPaused(mgr.GetScheme(), ctrl.LoggerFrom(ctx))).
		Watches(&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(bmcv1.GroupVersion.WithKind(`BMCMachine`)))).
		Watches(&bmcv1.BMCCluster{},
			handler.EnqueueRequestsFromMapFunc(r.BMCClusterToBMCMachines(ctx))).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(objFunc), builder.WithPredicates(predicates.ClusterUnpausedAndInfrastructureReady(mgr.GetScheme(), ctrl.LoggerFrom(ctx)))).
		Build(r)
	if err != nil {
		return errors.Wrapf(err, `Failed to create controller`)
	}

	//objFunc, err := util.ClusterToTypedObjectsMapper(r.Client, &bmcv1.BMCMachineList{}, mgr.GetScheme())
	//if err != nil {
	//	return errors.Wrapf(err, `Failed to create cluster to object mapper func`)
	//}

	/* err = c.Watch(&source.Kind{Type: &clusterv1.Cluster{}},
	handler.EnqueueRequestsFromMapFunc(objFunc),
	predicates.ClusterUnpausedAndInfrastructureReady(r.Scheme(), ctrl.LoggerFrom(ctx))) */
	/* if err != nil {
		return errors.Wrapf(err, `Failed to add a watch for unpaused clusters`)
	} */
	return nil
}

// BMCClusterToBMCMachines adds cluster to machine
func (r *BMCMachineReconciler) BMCClusterToBMCMachines(ctx context.Context) handler.MapFunc {
	log := ctrl.LoggerFrom(ctx)
	return func(ctx context.Context, o client.Object) []ctrl.Request {
		result := []ctrl.Request{}
		bmccluster, ok := o.(*bmcv1.BMCCluster)
		if !ok {
			log.Error(fmt.Errorf("Invalid cluster type: %T", o), `invalid BMCCluster`)
			return nil
		}
		cluster, err := util.GetOwnerCluster(ctx, r.Client, bmccluster.ObjectMeta)
		if apierrors.IsNotFound(err) || cluster == nil {
			return result
		} else if err != nil {
			log.Error(err, `Unable to lookup the owner cluster`)
			return result
		}

		ls := map[string]string{clusterv1.ClusterNameLabel: cluster.Name}
		ml := &clusterv1.MachineList{}
		if err := r.List(ctx, ml, client.InNamespace(bmccluster.Namespace), client.MatchingLabels(ls)); err != nil {
			log.Error(err, `Unable to get machine list`)
			return nil
		}
		for _, machine := range ml.Items {
			if len(machine.Spec.InfrastructureRef.Name) > 0 {
				result = append(result, ctrl.Request{
					NamespacedName: client.ObjectKey{
						Namespace: machine.Namespace,
						Name:      machine.Spec.InfrastructureRef.Name}})
			}
		}
		return result
	}
}

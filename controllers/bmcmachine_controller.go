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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
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
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/pkg/errors"
	bmcv1 "github.com/pnap/cluster-api-provider-bmc/api/v1beta1"
)

// BMCMachineReconciler reconciles a BMCMachine object
type BMCMachineReconciler struct {
	client.Client
	Recorder         record.EventRecorder
	ReconcileTimeout time.Duration
	BMCClientConfig  clientcredentials.Config
	BMCEndpointURL   string
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
	bmc := r.BMCClientConfig.Client(ctx)

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

	if mc.IsControlPlaneMachine() {
		// if its a control plane machine then add the public network configuration
		// if control plane && ipid == `` then we have a problem
		ipid := mc.GetClusterIPAllocationID()
		if len(ipid) <= 0 {
			log.Info(`Cluster address block is not yet ready`)
			return noRequeue, nil
		}
		var ii = true
		request.InstallDefaultSSHKeys = &ii
		request.NetworkConfiguration = NetworkConfiguration{
			IPBlocksConfiguration: IPBlocksConfiguration{
				ConfigurationType: `USER_DEFINED`,
				IPBlocks:          []IPBlock{{ID: ipid}},
			},
		}
	}

	createBody, err := json.Marshal(request)
	log.Info("Request object FROM machine controller is   **************" + string(createBody))
	if err != nil {
		return noRequeue, err
	}

	// Create bmc server
	resp, err := mc.BMCClient.Post(fmt.Sprintf("%s/bmc/v1/servers", r.BMCEndpointURL), `application/json`, bytes.NewBuffer(createBody))
	if err != nil {
		mc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
		return noRequeue, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		mc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
		return noRequeue, err
	}

	switch resp.StatusCode {
	case 400, 401, 403:
		// bad data, or controller/API incompatibility
		// bad credentials
		// unauthorized (also 404)
		// everything fallingthrough into this block should not be requeued
		log.Info("Unable to reconcile", `code`, resp.StatusCode, `body`, string(body))
		if !mc.IsStatusEqual(StatusIrreconcilable) {
			mc.Eventf(EventTypeWarning, EventReasonCreateErrorPermanent, "Code: %v", resp.StatusCode)
			mc.SetIrreconcilable(capierrors.InvalidConfigurationMachineError, fmt.Sprintf("BMC create code %v", resp.StatusCode))
		}
		return noRequeue, nil
	case 406:
		// no inventory, backoff and retry
		log.Info("Temporary no inventory", `code`, 406, `body`, string(body))
		mc.Eventf(EventTypeWarning, EventReasonCreateErrorInventory, `Code: %v`, resp.StatusCode)
		return requeueAfter5Min, err
	case 409:
		// something is wrong; incompatible state
		log.Info("Unable to reconcile", `code`, 409, `body`, string(body))
		if !mc.IsStatusEqual(StatusIrreconcilable) {
			mc.Eventf(EventTypeWarning, EventReasonCreateErrorPermanent, "Code: %v", resp.StatusCode)
			mc.SetIrreconcilable(capierrors.InvalidConfigurationMachineError, fmt.Sprintf("BMC create code %v", resp.StatusCode))
		}
		return requeueAfter2Min, nil
	case 500:
		// temporarily unavailable, backoff and retry
		log.Info(`BMC temporarily unavailable`, `body`, string(body))
		mc.Event(EventTypeWarning, EventReasonCreateFailure, `Temporary API failure`)
		return requeueAfter2Min, err
	case 200, 201, 202, 204:
		// the call was successful, do nothing and continue reconciliation
	default:
		log.Info(`Unexpected response from API`, `statusCode`, resp.StatusCode)
		mc.Eventf(EventTypeWarning, EventReasonCreateError, `Unexpected response from API: %v`, resp.StatusCode)
		return noRequeue, fmt.Errorf("Unexpected response during server create: %v", resp.StatusCode)
	}

	// Set the resulting server ID in the annotation and set status
	var ss bmcv1.BMCMachineStatus
	err = json.Unmarshal(body, &ss)
	if err != nil {
		mc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
		return noRequeue, err
	}

	mc.Eventf(EventTypeNormal, EventReasonCreated, "creatd BMC server %s", ss.BMCServerID)
	mc.SetStatus(ss)
	mc.SetMachineID(ss.BMCServerID)
	return requeueAfter1Min, nil
}

// reconcileSynchronize synchronize BMCMachine
func (r *BMCMachineReconciler) reconcileSynchronize(ctx context.Context, mc *MachineContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	resp, err := mc.BMCClient.Get(fmt.Sprintf("%s/bmc/v1/servers/%s", r.BMCEndpointURL, mc.GetMachineID()))
	if err != nil {
		if !mc.IsStatusEqual(StatusStale) {
			mc.Eventf(EventTypeNormal, EventReasonStatusChange, `%v -> %v`, mc.GetBMCStatus(), StatusStale)
			mc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		if !mc.IsStatusEqual(StatusStale) {
			log.Error(err, string(body))
			mc.Eventf(EventTypeNormal, EventReasonStatusChange, `%v -> %v`, mc.GetBMCStatus(), StatusStale)
			mc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}

	switch resp.StatusCode {
	case 400:
		// bad data, or controller/API incompatibility
		log.Info("Unable to reconcile, bad request", `code`, 400, `body`, string(body))
		if !mc.IsStatusEqual(StatusIrreconcilable) {
			mc.Eventf(EventTypeWarning, EventReasonPollFailure, "Code: %v", resp.StatusCode)
			mc.SetIrreconcilable(capierrors.InvalidConfigurationMachineError, fmt.Sprintf("BMC read code %v", resp.StatusCode))
		}
		return requeueAfter5Min, nil
	case 401:
		// bad credentials
		log.Info("Unable to reconcile", `code`, 401, `body`, string(body))
		if !mc.IsStatusEqual(StatusIrreconcilable) {
			mc.Eventf(EventTypeWarning, EventReasonPollFailure, "Code: %v", resp.StatusCode)
			mc.SetIrreconcilable(capierrors.InvalidConfigurationMachineError, fmt.Sprintf("BMC read code %v", resp.StatusCode))
		}
		return requeueAfter5Min, nil
	case 403:
		// unauthorized (also 404)
		log.Info("Unable to reconcile", `code`, 403, `body`, string(body))
		if !mc.IsStatusEqual(StatusOrphaned) {
			mc.Eventf(EventTypeWarning, EventReasonResourceOrphaned, "Access to BMC resource was denied", resp.StatusCode)
			mc.SetBMCStatus(StatusOrphaned)
		}
		return noRequeue, nil
	case 500:
		// temporarily unavailable, backoff and retry
		log.Info(`BMC temporarily unavailable`, `body`, string(body))
		if !mc.IsStatusEqual(StatusStale) {
			mc.Eventf(EventTypeWarning, EventReasonPollFailure, "Code: %v", resp.StatusCode)
			mc.SetBMCStatus(StatusStale)
		}
		return requeueAfter5Min, nil
	case 200, 201, 202, 204:
		// the call was successful, do nothing and continue synchronizing
	default:
		log.Info(`Unexpected response from API`, `statusCode`, resp.StatusCode)
		mc.Eventf(EventTypeWarning, EventReasonPollFailure, `Unexpected response from API: %v`, resp.StatusCode)
		return noRequeue, fmt.Errorf("unexpected response during server poll: %v", resp.StatusCode)
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
				//mc.SetNodeRef(ctx, r.Client)
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
			return requeueAfter2Min, nil
		}
	case StatusError:
		return noRequeue, nil
	default:
		return requeueAfter1Min, nil
	}
}

// reconcileDelete delete BMCMachine
func (r *BMCMachineReconciler) reconcileDelete(ctx context.Context, mc *MachineContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// This function makes a best-effort attempt to cleanup the BMC server
	mc.RemoveFini()

	bmcServerID := mc.GetMachineID()
	if !mc.IsStatusEqual(StatusOrphaned) && len(bmcServerID) > 0 {
		// tear down the BMC server
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			fmt.Sprintf("%s/bmc/v1/servers/%s/actions/deprovision", r.BMCEndpointURL, bmcServerID),
			bytes.NewBufferString(`{"deleteIpBlocks":true}`))
		if err != nil {
			mc.Event(`Warning`, EventReasonCleanupError, err.Error())
			return noRequeue, err
		}
		req.Header.Add(`Content-Type`, `application/json`)
		resp, err := mc.BMCClient.Do(req)
		if err != nil {
			mc.Event(`Warning`, EventReasonCleanupError, err.Error())
			return noRequeue, err
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			mc.Event(`Warning`, EventReasonCleanupError, err.Error())
			return noRequeue, err
		}

		switch resp.StatusCode {
		case 400, 401:
			// bad data, or controller/API incompatibility
			// bad credentials
			log.Info("Unable to delete", `code`, resp.StatusCode, `body`, string(body))
			mc.SetBMCStatus(StatusIrreconcilable)
			return requeueAfter2Min, nil
		case 403:
			// unauthorized (also 404)
			log.Info("Unable to delete", `code`, 403, `body`, string(body))
			mc.SetBMCStatus(StatusOrphaned)
			return requeueAfter2Min, nil
		case 500:
			// temporarily unavailable, backoff and retry
			log.Info(`BMC temporarily unavailable`, `body`, string(body))
			return requeueAfter2Min, nil
		case 200, 201, 202, 204:
			// the call was successful, do nothing and continue reconciliation
			mc.Eventf(`Normal`, EventReasonCleanupSuccess, "Deleted BMC server %s", bmcServerID)
		default:
			mc.Eventf(`Warning`, EventReasonCleanupError, "Unexpected response from API: %v", resp.StatusCode)
			return requeueAfter2Min, fmt.Errorf("Unexpected response during server delete: %v", resp.StatusCode)
		}
	}

	// At this point the BMC server is either orphaned or has been cleaned up
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
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&bmcv1.BMCMachine{}).
		WithEventFilter(predicates.ResourceNotPaused(ctrl.LoggerFrom(ctx))).
		Watches(&source.Kind{Type: &clusterv1.Machine{}},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(bmcv1.GroupVersion.WithKind(`BMCMachine`)))).
		Watches(&source.Kind{Type: &bmcv1.BMCCluster{}},
			handler.EnqueueRequestsFromMapFunc(r.BMCClusterToBMCMachines(ctx))).
		Build(r)
	if err != nil {
		return errors.Wrapf(err, `Failed to create controller`)
	}

	objFunc, err := util.ClusterToObjectsMapper(r.Client, &bmcv1.BMCMachineList{}, mgr.GetScheme())
	if err != nil {
		return errors.Wrapf(err, `Failed to create cluster to object mapper func`)
	}

	err = c.Watch(&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(objFunc),
		predicates.ClusterUnpausedAndInfrastructureReady(ctrl.LoggerFrom(ctx)))
	if err != nil {
		return errors.Wrapf(err, `Failed to add a watch for unpaused clusters`)
	}
	return nil
}

// BMCClusterToBMCMachines adds cluster to machine
func (r *BMCMachineReconciler) BMCClusterToBMCMachines(ctx context.Context) handler.MapFunc {
	log := ctrl.LoggerFrom(ctx)
	return func(o client.Object) []ctrl.Request {
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

		ls := map[string]string{clusterv1.ClusterLabelName: cluster.Name}
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

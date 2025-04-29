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
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2/clientcredentials"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	bmcv1 "github.com/phoenixnap/cluster-api-provider-bmc/api/v1beta1"
	"github.com/pkg/errors"
)

// BMCClusterReconciler reconciles a BMCCluster object
type BMCClusterReconciler struct {
	client.Client
	Recorder         record.EventRecorder
	ReconcileTimeout time.Duration
	BMCClientConfig  clientcredentials.Config
	BMCEndpointURL   string
	Scheme           *runtime.Scheme
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=bmcclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=bmcclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=bmcclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.1/pkg/reconcile
func (r *BMCClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, r.ReconcileTimeout)
	defer cancel()

	log := ctrl.LoggerFrom(ctx)

	// 1. Load the BMC client
	bmc := r.BMCClientConfig.Client(ctx)

	// 2. Get the BMCCluster
	var bmcCluster bmcv1.BMCCluster
	if err := r.Get(ctx, req.NamespacedName, &bmcCluster); err != nil {
		return noRequeue, client.IgnoreNotFound(err)
	}

	// 3. Get the Cluster
	cluster, err := util.GetOwnerCluster(ctx, r.Client, bmcCluster.ObjectMeta)
	if err != nil {
		return noRequeue, err
	}
	if cluster == nil {
		log.Info(`Cluster's OwnerRef has not yet been set.`)
		return noRequeue, nil
	}

	// 4. Halt processing if reconciliation is paused
	if annotations.IsPaused(cluster, &bmcCluster) {
		log.Info(`Reconciliation is paused for this cluster.`)
		return noRequeue, nil
	}

	// 5. Gather context
	helper, _ := patch.NewHelper(&bmcCluster, r.Client)
	cc := ClusterContext{
		BMCClient:   bmc,
		K8sClient:   r.Client,
		Cluster:     cluster,
		BMCCluster:  &bmcCluster,
		Recorder:    r.Recorder,
		PatchHelper: helper,
	}

	// Always persist changes to the BMCCluster when the reconcile function returns
	defer func() {
		if err := helper.Patch(ctx, cc.BMCCluster); err != nil {
			log.Error(err, `Unable to patch`)
		}
	}()

	if cc.IsMarkedDeleted() {
		return r.reconcileDelete(ctx, &cc)
	}
	return r.reconcileLive(ctx, &cc)
}

// reconcileLive determines current state of cluster
func (r *BMCClusterReconciler) reconcileLive(ctx context.Context, cc *ClusterContext) (ctrl.Result, error) {

	// Add the finalizer if missing
	if !cc.HasFini() {
		cc.AddFini()
	}

	if len(cc.GetIP()) == 0 {
		return r.reconcileCreate(ctx, cc)
	}

	return r.reconcileSynchronize(ctx, cc)
}

// reconcileCreate create BMCCluster
func (r *BMCClusterReconciler) reconcileCreate(ctx context.Context, cc *ClusterContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info(`Creating cluster`)

	createBody, err := json.Marshal(IPBlockRequest{
		CIDRBlockSize: `/31`,
		Location:      string(cc.BMCCluster.Spec.Location),
		Description:   fmt.Sprintf("CAPI cluster for BMC %s/%s", cc.BMCCluster.Namespace, cc.BMCCluster.Name),
	})
	if err != nil {
		return noRequeue, err
	}

	resp, err := cc.BMCClient.Post(fmt.Sprintf("%s/ips/v1/ip-blocks", r.BMCEndpointURL), `application/json`, bytes.NewBuffer(createBody))
	if err != nil {
		cc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
		return noRequeue, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		cc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
		return noRequeue, err
	}

	switch resp.StatusCode {
	case 400, 401, 403:
		// bad data, or controller/API incompatibility
		// bad credentials
		// unauthorized (also 404)
		// everything fallingthrough into this block should not be requeued
		log.Info(`Unable to reconcile`, `code`, resp.StatusCode, `body`, string(body))
		if !cc.IsStatusEqual(StatusIrreconcilable) {
			cc.Eventf(EventTypeWarning, EventReasonCreateErrorPermanent, "Code: %v", resp.StatusCode)
			cc.SetBMCStatus(StatusIrreconcilable)
		}
		return noRequeue, nil
	case 500:
		// temporarily unavailable, backoff and retry
		log.Info(`BMC temporarily unavailable`)
		cc.Eventf(EventTypeWarning, EventReasonCreateFailure, "Code: %v", resp.StatusCode)
		return noRequeue, nil
	case 200, 201, 202, 204:
		// the call was successful, do nothing and continue reconciliation
		log.Info(`ip-block successfully created`)
	default:
		cc.Eventf(EventTypeWarning, EventReasonCreateError, "Unexpected response from the API: %s", resp.StatusCode)
		return noRequeue, fmt.Errorf("Unexpected response during server create: %v", resp.StatusCode)
	}

	// Set the resulting IP Block response in the status and set annotation values appropriately.
	var ips bmcv1.BMCClusterStatus
	err = json.Unmarshal(body, &ips)
	if err != nil {
		cc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
		return noRequeue, err
	}

	log.Info(`Setting status to BMC response`, `Response`, string(body), `Parsed Status`, ips.ID)

	cc.Eventf(EventTypeNormal, EventReasonCreated, "Created BMC IP-Block %s", ips.ID)
	cc.SetStatus(ips)

	cc.SetIPAllocationID(ips.ID)

	// This controller created an IP-block of size two, and the first address is reserved
	// for broadcase. This makes the publicly assigned IP address knowable before a control
	// plane machine is created.
	addr := net.ParseIP(strings.TrimSuffix(ips.CIDR, `/31`))
	addr[15] = addr[15] + 1

	cc.SetIP(addr.String())

	// set the control plane endpoint
	// default port for the kube-apiserver is 6443
	cc.SetControlPlaneEndpoint(addr.String(), 6443)

	// mark the cluster ready
	cc.SetReady()

	return noRequeue, nil
}

// reconcileSynchronize synchronize BMCMCluster
func (r *BMCClusterReconciler) reconcileSynchronize(ctx context.Context, cc *ClusterContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	resp, err := cc.BMCClient.Get(fmt.Sprintf("%s/ips/v1/ip-blocks/%s", r.BMCEndpointURL, cc.GetIPAllocationID()))
	if err != nil {
		if !cc.IsStatusEqual(StatusStale) {
			cc.Eventf(EventTypeNormal, EventReasonStatusChange, `%v -> %v`, cc.GetBMCStatus(), StatusStale)
			cc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		if !cc.IsStatusEqual(StatusStale) {
			log.Error(err, string(body))
			cc.Eventf(EventTypeNormal, EventReasonStatusChange, `%v -> %v`, cc.GetBMCStatus(), StatusStale)
			cc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}

	switch resp.StatusCode {
	case 400:
		// bad data, or controller/API incompatibility
		log.Info(`Unable to reconcile, bad request`, `code`, 400, `body`, string(body))
		if !cc.IsStatusEqual(StatusIrreconcilable) {
			cc.Eventf(EventTypeWarning, EventReasonPollFailure, `Code: %v`, resp.StatusCode)
			cc.SetBMCStatus(StatusIrreconcilable)
		}
		return requeueAfter5Min, err
	case 401:
		// bad credentials
		log.Info(`Unable to reconcile`, `code`, 401, `body`, string(body))
		if !cc.IsStatusEqual(StatusIrreconcilable) {
			cc.Eventf(EventTypeWarning, EventReasonPollFailure, `Code: %v`, resp.StatusCode)
			cc.SetBMCStatus(StatusIrreconcilable)
		}
		return requeueAfter5Min, err
	case 403:
		// unauthorized (also 404)
		log.Info(`Unable to reconcile`, `code`, 403, `body`, string(body))
		if !cc.IsStatusEqual(StatusOrphaned) {
			cc.Eventf(EventTypeWarning, EventReasonResourceOrphaned, `Access to BMC resource was denied: %v`, resp.StatusCode)
			cc.SetBMCStatus(StatusOrphaned)
		}
		return noRequeue, err
	case 500:
		// temporarily unavailable, backoff and retry
		log.Info(`BMC is temporarily unavailable`, `code`, 500, `body`, string(body))
		if !cc.IsStatusEqual(StatusStale) {
			cc.Eventf(EventTypeWarning, EventReasonResourceOrphaned, `Access to BMC resource was denied: %v`, resp.StatusCode)
			cc.SetBMCStatus(StatusStale)
		}
		return requeueAfter5Min, err
	case 200, 201, 202, 204:
		// the call was successful, do nothing and continue synchronizing
	default:
		log.Info(`Unexpected response from API`, `statusCode`, resp.StatusCode)
		cc.Eventf(EventTypeWarning, EventReasonPollFailure, `Unexpected response from API: %v`, resp.StatusCode)
		return noRequeue, fmt.Errorf("Unexpected response during server poll: %v", resp.StatusCode)
	}

	var ss bmcv1.BMCClusterStatus
	err = json.Unmarshal(body, &ss)
	if err != nil {
		log.Info(`Unable to unmarshal BMC get ip-block response`, `body`, string(body))
		if !cc.IsStatusEqual(StatusStale) {
			cc.Eventf(EventTypeWarning, EventReasonPollFailure, `error: %v`, err)
			cc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}

	// Detect a status delta
	if !cc.IsStatusEqual(ss.BMCStatus) {
		log.Info(`Status changed`)
		cc.Eventf(EventTypeNormal, EventReasonStatusChange, `%v -> %v`, cc.GetBMCStatus(), ss.BMCStatus)
		cc.SetStatus(ss)
	} else {
		log.Info(`No change in status`)
	}
	log.Info("Status is " + string(cc.BMCCluster.Status.BMCStatus) + ", and vip manager is " + string(cc.BMCCluster.Spec.VIPManager) + " from enum: " + string(bmcv1.KUBEVIP) + "," + string(bmcv1.NONE))
	if cc.IsStatusEqual(StatusAssigned) && cc.BMCCluster.Spec.VIPManager == bmcv1.NONE && !cc.IsReady() {
		log.Info("infrastructure set to ready, vip manager is none.")
		cc.SetReady()
	}

	if cc.IsStatusEqual(StatusNotAssigned) && cc.BMCCluster.Spec.VIPManager == bmcv1.KUBEVIP && !cc.IsReady() {
		log.Info("infrastructure set to ready, vip manager is kube-vip.")
		cc.SetReady()
	}
	return requeueAfter5Min, nil
}

// reconcileDelete delete BMCCluster
func (r *BMCClusterReconciler) reconcileDelete(ctx context.Context, cc *ClusterContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info(`Reconciling delete BMCCluster`)

	// Unless the IP-Block is unassigned, the current implementation of BMCCluster
	// does not support HA configurations and no loadbalancing infrastructure is
	// created. Instead a single address IP is created and the control plane machine
	// is assigned into that block. When the control plane machine is deleted, the
	// IP block will be automatically deleted (TODO actually they won't right now).
	if cc.IsStatusEqual(`unassigned`) {
		req, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/ips/v1/ip-blocks/%s", r.BMCEndpointURL, cc.GetIPAllocationID()), nil)
		resp, err := cc.BMCClient.Do(req)
		if err != nil {
			log.Error(err, `Unable to delete IP-Block with BMC, manual cleanup may be required`)
		}
		resp.Body.Close()
	}

	cc.Eventf(EventTypeNormal, `ClusterDeleted`, "Deleted cluster %s/%s", cc.BMCCluster.Namespace, cc.BMCCluster.Name)
	cc.RemoveFini()
	return noRequeue, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *BMCClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&bmcv1.BMCCluster{}).
		WithEventFilter(predicates.ResourceNotPaused(ctrl.LoggerFrom(ctx))).
		Build(r)
	if err != nil {
		return errors.Wrapf(err, `error creating cluster controller`)
	}

	err = c.Watch(&source.Kind{Type: &clusterv1.Cluster{}},
		handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx, bmcv1.GroupVersion.WithKind(`BMCCluster`), mgr.GetClient(), &bmcv1.BMCCluster{})),
		predicates.ClusterUnpaused(ctrl.LoggerFrom(ctx)))
	if err != nil {
		return errors.Wrapf(err, `err adding watch for unpaused cluster`)
	}
	return nil
}

// IPBlockRequest IPBlock configuration
type IPBlockRequest struct {
	CIDRBlockSize string `json:"cidrBlockSize,omitempty"`
	Location      string `json:"location,omitempty"`
	Description   string `json:"description,omitempty"`
}

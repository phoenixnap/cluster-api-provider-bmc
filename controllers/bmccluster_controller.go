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
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

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

	api apiClient
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
	bmc := r.api.get(r.BMCClientConfig)

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
		if err := cc.PatchHelper.Patch(ctx, cc.BMCCluster); err != nil {
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

// ipBlockDescription identifies the cluster's IP block, so it can be found again
func ipBlockDescription(cc *ClusterContext) string {
	return fmt.Sprintf("CAPI cluster for BMC %s/%s (%s)", cc.BMCCluster.Namespace, cc.BMCCluster.Name, cc.BMCCluster.UID)
}

// reconcileCreate create BMCCluster
func (r *BMCClusterReconciler) reconcileCreate(ctx context.Context, cc *ClusterContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info(`Creating cluster`)
	blocksURL := fmt.Sprintf("%s/ips/v1/ip-blocks", r.BMCEndpointURL)

	if cc.IsStatusEqual(StatusIrreconcilable) {
		return noRequeue, nil
	}

	var body []byte
	// A previous create may have succeeded without the block ID being recorded. Find that
	// block instead of creating a second one.
	if requested := createRequestedAt(&cc.BMCCluster.ObjectMeta); !requested.IsZero() {
		found, err := r.findCreatedBlock(ctx, cc, requested)
		if err != nil {
			cc.Eventf(EventTypeWarning, EventReasonCreateError, "unable to check for an IP block created earlier: %v", err)
			return requeueAfter1Min, err
		}
		if len(found) > 1 {
			cc.Eventf(EventTypeWarning, EventReasonCreateErrorPermanent, "%d IP blocks were created for this cluster; delete the extra blocks manually", len(found))
			cc.SetBMCStatus(StatusIrreconcilable)
			return noRequeue, nil
		}
		if len(found) == 1 {
			body = found[0]
		}
	}

	if body == nil {
		// record the request before making it, so the block can be found if its ID is lost
		setCreateRequested(&cc.BMCCluster.ObjectMeta, time.Now())
		if err := cc.PatchHelper.Patch(ctx, cc.BMCCluster); err != nil {
			return requeueAfter1Min, errors.Wrap(err, "unable to record the IP block create request")
		}
		// later patches must be computed against what was just saved, or removing the
		// marker again would not be sent
		h, err := patch.NewHelper(cc.BMCCluster, cc.K8sClient)
		if err != nil {
			return requeueAfter1Min, err
		}
		cc.PatchHelper = h

		code, respBody, err := callAPI(ctx, cc.BMCClient, http.MethodPost, blocksURL, IPBlockRequest{
			CIDRBlockSize: `/31`,
			Location:      string(cc.BMCCluster.Spec.Location),
			Description:   ipBlockDescription(cc),
		})
		if err != nil {
			cc.Event(EventTypeWarning, EventReasonCreateError, err.Error())
			return requeueAfter1Min, err
		}

		switch code {
		case 400, 401, 403:
			// bad data, or controller/API incompatibility
			// bad credentials
			// unauthorized (also 404)
			// everything fallingthrough into this block should not be requeued
			log.Info(`Unable to reconcile`, `code`, code, `body`, string(respBody))
			clearCreateRequested(&cc.BMCCluster.ObjectMeta)
			if !cc.IsStatusEqual(StatusIrreconcilable) {
				cc.Eventf(EventTypeWarning, EventReasonCreateErrorPermanent, "Code: %v", code)
				cc.SetBMCStatus(StatusIrreconcilable)
			}
			return noRequeue, nil
		case 200, 201, 202, 204:
			// the call was successful, do nothing and continue reconciliation
			log.Info(`ip-block successfully created`)
			body = respBody
		default:
			// temporarily unavailable or unexpected; the block may have been created, so the
			// next reconcile looks for it before creating again
			log.Info(`Unexpected response from API`, `code`, code, `body`, string(respBody))
			cc.Eventf(EventTypeWarning, EventReasonCreateFailure, "Unexpected response from the API: %v", code)
			return requeueAfter2Min, nil
		}
	}

	// Set the resulting IP Block response in the status and set annotation values appropriately.
	var ips bmcv1.BMCClusterStatus
	if err := json.Unmarshal(body, &ips); err != nil || ips.ID == "" {
		cc.Eventf(EventTypeWarning, EventReasonCreateError, "unable to read the created IP block: %v", err)
		return requeueAfter1Min, nil
	}

	// This controller created an IP-block of size two, and the first address is reserved
	// for broadcase. This makes the publicly assigned IP address knowable before a control
	// plane machine is created.
	prefix, err := netip.ParsePrefix(ips.CIDR)
	if err != nil || !prefix.Addr().Is4() {
		cc.Eventf(EventTypeWarning, EventReasonCreateError, "IP block %s has unusable CIDR %q", ips.ID, ips.CIDR)
		return requeueAfter5Min, fmt.Errorf("IP block %s has unusable CIDR %q: %v", ips.ID, ips.CIDR, err)
	}
	addr := prefix.Masked().Addr().Next()

	log.Info(`Setting status to BMC response`, `Response`, string(body), `Parsed Status`, ips.ID)
	cc.Eventf(EventTypeNormal, EventReasonCreated, "Created BMC IP-Block %s", ips.ID)
	cc.SetStatus(ips)
	cc.SetIPAllocationID(ips.ID)
	clearCreateRequested(&cc.BMCCluster.ObjectMeta)

	cc.SetIP(addr.String())

	// set the control plane endpoint
	// default port for the kube-apiserver is 6443
	cc.SetControlPlaneEndpoint(addr.String(), 6443)

	// mark the cluster ready
	cc.SetReady()

	return noRequeue, nil
}

// findCreatedBlock returns the IP blocks with this cluster's description and location that
// were created since the create was requested
func (r *BMCClusterReconciler) findCreatedBlock(ctx context.Context, cc *ClusterContext, requested time.Time) ([]json.RawMessage, error) {
	raw, listed, err := listResources(ctx, cc.BMCClient, fmt.Sprintf("%s/ips/v1/ip-blocks", r.BMCEndpointURL))
	if err != nil {
		return nil, err
	}
	var found []json.RawMessage
	for i, b := range listed {
		if b.Description == ipBlockDescription(cc) && b.Location == string(cc.BMCCluster.Spec.Location) &&
			(b.CreatedOn == nil || !b.CreatedOn.Before(requested.Add(-time.Minute))) {
			found = append(found, raw[i])
		}
	}
	return found, nil
}

// reconcileSynchronize synchronize BMCMCluster
func (r *BMCClusterReconciler) reconcileSynchronize(ctx context.Context, cc *ClusterContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	code, body, err := callAPI(ctx, cc.BMCClient, http.MethodGet, fmt.Sprintf("%s/ips/v1/ip-blocks/%s", r.BMCEndpointURL, cc.GetIPAllocationID()), nil)
	if err != nil {
		if !cc.IsStatusEqual(StatusStale) {
			cc.Eventf(EventTypeNormal, EventReasonStatusChange, `%v -> %v`, cc.GetBMCStatus(), StatusStale)
			cc.SetBMCStatus(StatusStale)
		}
		return requeueAfter2Min, err
	}

	switch code {
	case 400:
		// bad data, or controller/API incompatibility
		log.Info(`Unable to reconcile, bad request`, `code`, 400, `body`, string(body))
		if !cc.IsStatusEqual(StatusIrreconcilable) {
			cc.Eventf(EventTypeWarning, EventReasonPollFailure, `Code: %v`, code)
			cc.SetBMCStatus(StatusIrreconcilable)
		}
		return requeueAfter5Min, nil
	case 401:
		// bad credentials
		log.Info(`Unable to reconcile`, `code`, 401, `body`, string(body))
		if !cc.IsStatusEqual(StatusIrreconcilable) {
			cc.Eventf(EventTypeWarning, EventReasonPollFailure, `Code: %v`, code)
			cc.SetBMCStatus(StatusIrreconcilable)
		}
		return requeueAfter5Min, nil
	case 403, 404:
		// the block is gone, or the credentials may not read it; only the first is orphaned
		gone, err := confirmGone(ctx, cc.BMCClient, fmt.Sprintf("%s/ips/v1/ip-blocks", r.BMCEndpointURL), cc.GetIPAllocationID())
		if err != nil || !gone {
			log.Info(`Unable to read IP block`, `code`, code, `body`, string(body), `listError`, err)
			if !cc.IsStatusEqual(StatusStale) {
				cc.Eventf(EventTypeWarning, EventReasonPollFailure, `Access to BMC IP block %s was denied: %v`, cc.GetIPAllocationID(), code)
				cc.SetBMCStatus(StatusStale)
			}
			return requeueAfter5Min, err
		}
		if !cc.IsStatusEqual(StatusOrphaned) {
			cc.Eventf(EventTypeWarning, EventReasonResourceOrphaned, `BMC IP block %s no longer exists`, cc.GetIPAllocationID())
			cc.SetBMCStatus(StatusOrphaned)
		}
		return noRequeue, nil
	case 200, 201, 202, 204:
		// the call was successful, do nothing and continue synchronizing
	default:
		// temporarily unavailable, backoff and retry
		log.Info(`Unexpected response from API`, `statusCode`, code, `body`, string(body))
		if !cc.IsStatusEqual(StatusStale) {
			cc.Eventf(EventTypeWarning, EventReasonPollFailure, `Unexpected response from API: %v`, code)
			cc.SetBMCStatus(StatusStale)
		}
		return requeueAfter5Min, nil
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

// reconcileDelete deletes the cluster's IP block. The finalizer is only removed once the
// block is deleted or confirmed gone, so a failure is retried instead of leaking it.
func (r *BMCClusterReconciler) reconcileDelete(ctx context.Context, cc *ClusterContext) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	log.Info(`Reconciling delete BMCCluster`)
	blocksURL := fmt.Sprintf("%s/ips/v1/ip-blocks", r.BMCEndpointURL)

	id := cc.GetIPAllocationID()
	if id == "" {
		requested := createRequestedAt(&cc.BMCCluster.ObjectMeta)
		if requested.IsZero() {
			// no block was ever requested
			cc.RemoveFini()
			return noRequeue, nil
		}
		// a block may have been created without its ID being recorded
		found, err := r.findCreatedBlock(ctx, cc, requested)
		if err != nil {
			cc.Eventf(EventTypeWarning, EventReasonCleanupError, "unable to check for an IP block created earlier: %v", err)
			return requeueAfter1Min, err
		}
		if len(found) == 0 {
			cc.RemoveFini()
			return noRequeue, nil
		}
		var block bmcv1.BMCClusterStatus
		if err := json.Unmarshal(found[0], &block); err != nil {
			return requeueAfter1Min, err
		}
		id = block.ID
		cc.SetIPAllocationID(id)
	}

	gone := func() (ctrl.Result, error) {
		cc.Eventf(EventTypeNormal, `ClusterDeleted`, "Deleted cluster %s/%s", cc.BMCCluster.Namespace, cc.BMCCluster.Name)
		cc.RemoveFini()
		return noRequeue, nil
	}
	confirm := func() (ctrl.Result, error) {
		isGone, err := confirmGone(ctx, cc.BMCClient, blocksURL, id)
		if err != nil {
			return requeueAfter2Min, err
		}
		if !isGone {
			cc.Eventf(EventTypeWarning, EventReasonCleanupError, "access to BMC IP block %s was denied and it still exists; not removing the finalizer", id)
			return requeueAfter5Min, nil
		}
		return gone()
	}

	code, body, err := callAPI(ctx, cc.BMCClient, http.MethodGet, blocksURL+"/"+id, nil)
	if err != nil {
		cc.Eventf(EventTypeWarning, EventReasonCleanupError, "unable to read IP block %s: %v", id, err)
		return requeueAfter1Min, err
	}
	switch code {
	case 200:
	case 403, 404:
		return confirm()
	default:
		cc.Eventf(EventTypeWarning, EventReasonCleanupError, "reading IP block %s failed with %v; retrying", id, code)
		return requeueAfter2Min, nil
	}

	var block bmcv1.BMCClusterStatus
	if err := json.Unmarshal(body, &block); err != nil {
		return requeueAfter2Min, err
	}
	if block.BMCStatus != StatusNotAssigned {
		// still assigned to the control plane server, which releases it when deprovisioned
		log.Info(`Waiting for the IP block to be released`, `id`, id, `status`, block.BMCStatus)
		return requeueAfter1Min, nil
	}

	code, body, err = callAPI(ctx, cc.BMCClient, http.MethodDelete, blocksURL+"/"+id, nil)
	if err != nil {
		cc.Eventf(EventTypeWarning, EventReasonCleanupError, "unable to delete IP block %s: %v", id, err)
		return requeueAfter1Min, err
	}
	switch code {
	case 200, 202, 204:
		return gone()
	case 403, 404:
		return confirm()
	default:
		log.Info(`Unable to delete IP block`, `code`, code, `body`, string(body))
		cc.Eventf(EventTypeWarning, EventReasonCleanupError, "deleting IP block %s failed with %v; retrying", id, code)
		return requeueAfter2Min, nil
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *BMCClusterReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&bmcv1.BMCCluster{}).
		WithEventFilter(predicates.ResourceNotPaused(mgr.GetScheme(), ctrl.LoggerFrom(ctx))).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx, bmcv1.GroupVersion.WithKind(`BMCCluster`), mgr.GetClient(), &bmcv1.BMCCluster{})),
			builder.WithPredicates(predicates.ClusterUnpaused(mgr.GetScheme(), ctrl.LoggerFrom(ctx)))).
		Build(r)
	if err != nil {
		return errors.Wrapf(err, `error creating cluster controller`)
	}

	/* err = c.Watch(&clusterv1.Cluster{},
		handler.EnqueueRequestsFromMapFunc(util.ClusterToInfrastructureMapFunc(ctx, bmcv1.GroupVersion.WithKind(`BMCCluster`), mgr.GetClient(), &bmcv1.BMCCluster{})),
		predicates.ClusterUnpaused(r.Scheme, ctrl.LoggerFrom(ctx)))
	if err != nil {
		return errors.Wrapf(err, `err adding watch for unpaused cluster`)
	} */
	return nil
}

// IPBlockRequest IPBlock configuration
type IPBlockRequest struct {
	CIDRBlockSize string `json:"cidrBlockSize,omitempty"`
	Location      string `json:"location,omitempty"`
	Description   string `json:"description,omitempty"`
}

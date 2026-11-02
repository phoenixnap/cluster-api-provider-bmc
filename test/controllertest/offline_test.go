package controllertest

// Offline tests: the real reconcilers against a mock PhoenixNAP API. No credentials,
// no cost; run with
//
//	go test ./test/controllertest -run TestOffline -v

import (
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"

	bmcv1 "github.com/phoenixnap/cluster-api-provider-bmc/api/v1beta1"
	"github.com/phoenixnap/cluster-api-provider-bmc/controllers"
)

// lastApplied is what client-side `kubectl apply` adds; with it the annotation map is non-nil
var lastApplied = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}

// The IP block lifecycle when the BMCCluster already has annotations (client-side apply).
func TestOfflineClusterLifecycle(t *testing.T) {
	api := newMockAPI(t)
	e := newEnv(t, api.URL, api.creds(), testCluster("c1"), testBMCCluster("c1", "PHX", bmcv1.NONE, lastApplied))

	if _, err := e.reconcile(t, e.cluster, "c1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	bc, _ := e.getBMCCluster(t, "c1")
	if api.count("POST", "/ips/v1/ip-blocks") != 1 || bc.Annotations[controllers.ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID] == "" {
		t.Fatalf("expected one IP block recorded in annotations, have %v", bc.Annotations)
	}
	if !bc.Status.Ready || bc.Spec.ControlPlaneEndpoint.Port != 6443 || net.ParseIP(bc.Spec.ControlPlaneEndpoint.Host) == nil {
		t.Errorf("cluster not ready or bad endpoint: ready=%v endpoint=%+v", bc.Status.Ready, bc.Spec.ControlPlaneEndpoint)
	}

	// a second pass synchronizes instead of creating
	if _, err := e.reconcile(t, e.cluster, "c1"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n := api.count("POST", "/ips/v1/ip-blocks"); n != 1 {
		t.Errorf("sync created IP blocks: %d", n)
	}

	e.delete(t, bc)
	if _, err := e.reconcile(t, e.cluster, "c1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, exists := e.getBMCCluster(t, "c1"); exists {
		t.Error("BMCCluster still exists after delete")
	}
	if api.count("DELETE", "/ips/v1/ip-blocks/") != 1 {
		t.Error("IP block was not deleted")
	}
}

// The shipped cluster template creates the BMCCluster without annotations, as do
// `kubectl create`, server-side apply (Flux, Argo CD) and ClusterClass. Writing the
// allocation ID into the nil map panics after the IP block was created, and every
// retry creates another billable block.
func TestOfflineClusterCreateWithoutAnnotations(t *testing.T) {
	api := newMockAPI(t)
	e := newEnv(t, api.URL, api.creds(), testCluster("c1"), testBMCCluster("c1", "PHX", bmcv1.NONE, nil))

	for i := 0; i < 3; i++ {
		_, _ = e.reconcile(t, e.cluster, "c1")
	}
	if n := api.count("POST", "/ips/v1/ip-blocks"); n != 1 || len(e.panics) > 0 {
		t.Errorf("3 reconciles created %d IP blocks; panics: %v", n, e.panics)
	}
}

// Deleting a BMCCluster when the API is unreachable must neither panic nor drop the
// finalizer, or the IP block is never deleted.
func TestOfflineClusterDeleteAPIUnreachable(t *testing.T) {
	api := newMockAPI(t)
	e := newEnv(t, api.URL, api.creds(), testCluster("c1"), testBMCCluster("c1", "PHX", bmcv1.KUBEVIP, lastApplied))
	_, _ = e.reconcile(t, e.cluster, "c1") // create
	_, _ = e.reconcile(t, e.cluster, "c1") // sync: status unassigned
	bc, _ := e.getBMCCluster(t, "c1")
	api.Close()

	e.delete(t, bc)
	_, _ = e.reconcile(t, e.cluster, "c1")
	_, exists := e.getBMCCluster(t, "c1")
	if len(e.panics) > 0 || !exists {
		t.Errorf("panics: %v; BMCCluster kept for retry: %v", e.panics, exists)
	}
}

// machineEnv returns a ready cluster with one worker machine
func machineEnv(t *testing.T, api *mockAPI) *env {
	m, s := testMachine("c1", "c1-worker-0")
	return newEnv(t, api.URL, api.creds(), testCluster("c1"),
		testBMCCluster("c1", "PHX", bmcv1.NONE, map[string]string{controllers.ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID: "block-0", controllers.ANNOTATION_NAME_BMC_CLUSTER_IP: "198.18.0.1"}),
		m, s, testBMCMachine("c1-worker-0", "PHX", "s0.d1.small", "ubuntu/jammy", nil))
}

// A worker machine is created once, becomes ready when powered on, and is deprovisioned on delete.
func TestOfflineMachineLifecycle(t *testing.T) {
	api := newMockAPI(t)
	e := machineEnv(t, api)

	if _, err := e.reconcile(t, e.machine, "c1-worker-0"); err != nil {
		t.Fatalf("create: %v", err)
	}
	bm, _ := e.getBMCMachine(t, "c1-worker-0")
	id := bm.Annotations[controllers.ANNOTATION_NAME_BMC_SERVER_ID]
	if api.count("POST", "/bmc/v1/servers") != 1 || id == "" || bm.Spec.ProviderID == nil || *bm.Spec.ProviderID != "bmc://servers/"+id {
		t.Fatalf("server not recorded: annotations=%v providerID=%v", bm.Annotations, bm.Spec.ProviderID)
	}

	if _, err := e.reconcile(t, e.machine, "c1-worker-0"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	bm, _ = e.getBMCMachine(t, "c1-worker-0")
	if !bm.Status.Ready || len(bm.Status.Addresses) == 0 {
		t.Errorf("machine not ready: ready=%v addresses=%v", bm.Status.Ready, bm.Status.Addresses)
	}
	if n := api.count("POST", "/bmc/v1/servers"); n != 1 {
		t.Errorf("sync created servers: %d", n)
	}

	e.delete(t, bm)
	if _, err := e.reconcile(t, e.machine, "c1-worker-0"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if api.count("POST", "/bmc/v1/servers/"+id+"/actions/deprovision") != 1 {
		t.Error("server was not deprovisioned")
	}
}

// If deprovisioning fails, the finalizer must stay so the deletion is retried; otherwise
// the BMCMachine disappears and the server keeps running.
func TestOfflineMachineDeleteDeprovisionFails(t *testing.T) {
	api := newMockAPI(t)
	e := machineEnv(t, api)
	_, _ = e.reconcile(t, e.machine, "c1-worker-0") // create
	bm, _ := e.getBMCMachine(t, "c1-worker-0")

	api.set(func() { api.DeprovisionStatus = 500 })
	e.delete(t, bm)
	_, _ = e.reconcile(t, e.machine, "c1-worker-0")

	if _, exists := e.getBMCMachine(t, "c1-worker-0"); !exists {
		t.Errorf("BMCMachine removed although deprovisioning failed; server %s is left running", bm.Annotations[controllers.ANNOTATION_NAME_BMC_SERVER_ID])
	}
}

// When a server is created but its ID cannot be recorded (here: an unreadable response;
// equally a crash, timeout or failed patch), the next reconcile must not create a second server.
func TestOfflineMachineCreateNotRecorded(t *testing.T) {
	api := newMockAPI(t)
	api.CreateServerBody = `{"id": 12345}`
	e := machineEnv(t, api)

	_, _ = e.reconcile(t, e.machine, "c1-worker-0")
	_, _ = e.reconcile(t, e.machine, "c1-worker-0")
	if n := api.count("POST", "/bmc/v1/servers"); n != 1 {
		t.Errorf("2 reconciles created %d servers", n)
	}
	// the server created by the first reconcile must be found and recorded
	if bm, _ := e.getBMCMachine(t, "c1-worker-0"); bm.Annotations[controllers.ANNOTATION_NAME_BMC_SERVER_ID] == "" {
		t.Errorf("the created server was not found and recorded")
	}
}

// Server IP blocks are reported as "first - last" ranges; machine addresses must be IPs.
func TestOfflineMachineAddressRange(t *testing.T) {
	api := newMockAPI(t)
	api.PublicIPs = []string{"198.18.1.8 - 198.18.1.15"}
	e := machineEnv(t, api)
	_, _ = e.reconcile(t, e.machine, "c1-worker-0")
	_, _ = e.reconcile(t, e.machine, "c1-worker-0")

	bm, _ := e.getBMCMachine(t, "c1-worker-0")
	for _, a := range bm.Status.Addresses {
		if net.ParseIP(a.Address) == nil {
			t.Errorf("machine address %q (%s) is not an IP", a.Address, a.Type)
		}
	}
	hasInternal := false
	for _, a := range bm.Status.Addresses {
		hasInternal = hasInternal || a.Type == corev1.NodeInternalIP
	}
	if !hasInternal {
		t.Errorf("machine has no InternalIP address although the server has a private IP")
	}
}

// With revoked credentials a 403 from deprovision must not be taken as "already gone".
func TestOfflineMachineDeleteRevokedCredentials(t *testing.T) {
	api := newMockAPI(t)
	e := machineEnv(t, api)
	_, _ = e.reconcile(t, e.machine, "c1-worker-0") // create
	bm, _ := e.getBMCMachine(t, "c1-worker-0")

	api.set(func() { api.Revoked = true })
	e.delete(t, bm)
	_, _ = e.reconcile(t, e.machine, "c1-worker-0")
	if _, exists := e.getBMCMachine(t, "c1-worker-0"); !exists {
		t.Error("BMCMachine removed while the server could not be confirmed gone")
	}
}

// A permanent create failure is not retried on every event.
func TestOfflineMachineCreatePermanentFailure(t *testing.T) {
	api := newMockAPI(t)
	api.set(func() { api.Revoked = true })
	e := machineEnv(t, api)
	for i := 0; i < 3; i++ {
		_, _ = e.reconcile(t, e.machine, "c1-worker-0")
	}
	if n := api.count("POST", "/bmc/v1/servers"); n != 1 {
		t.Errorf("a rejected create was sent %d times", n)
	}
	bm, _ := e.getBMCMachine(t, "c1-worker-0")
	if bm.Status.FailureReason == nil {
		t.Error("machine has no failure reason")
	}
}

// An IP block created without its ID being recorded is found, not created again.
func TestOfflineClusterCreateNotRecorded(t *testing.T) {
	api := newMockAPI(t)
	api.CreateBlockBody = `{"id": 12345}`
	e := newEnv(t, api.URL, api.creds(), testCluster("c1"), testBMCCluster("c1", "PHX", bmcv1.NONE, nil))

	_, _ = e.reconcile(t, e.cluster, "c1")
	_, _ = e.reconcile(t, e.cluster, "c1")
	if n := api.count("POST", "/ips/v1/ip-blocks"); n != 1 {
		t.Errorf("2 reconciles created %d IP blocks", n)
	}
	bc, _ := e.getBMCCluster(t, "c1")
	if bc.Annotations[controllers.ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID] == "" || !bc.Status.Ready {
		t.Errorf("block not adopted: annotations=%v ready=%v", bc.Annotations, bc.Status.Ready)
	}
}

// Deleting the cluster waits while the block is still assigned to a server.
func TestOfflineClusterDeleteWaitsForRelease(t *testing.T) {
	api := newMockAPI(t)
	e := newEnv(t, api.URL, api.creds(), testCluster("c1"), testBMCCluster("c1", "PHX", bmcv1.NONE, nil))
	_, _ = e.reconcile(t, e.cluster, "c1")
	api.SetBlockStatus(controllers.StatusAssigned)
	bc, _ := e.getBMCCluster(t, "c1")

	e.delete(t, bc)
	_, _ = e.reconcile(t, e.cluster, "c1")
	if _, exists := e.getBMCCluster(t, "c1"); !exists || api.count("DELETE", "/ips/v1/ip-blocks/") != 0 {
		t.Fatal("cluster deleted while its IP block was still assigned")
	}

	api.SetBlockStatus(controllers.StatusNotAssigned)
	_, _ = e.reconcile(t, e.cluster, "c1")
	if _, exists := e.getBMCCluster(t, "c1"); exists || api.count("DELETE", "/ips/v1/ip-blocks/") != 1 {
		t.Error("cluster not deleted after its IP block was released")
	}
}

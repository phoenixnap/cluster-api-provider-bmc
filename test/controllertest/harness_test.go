package controllertest

// The tests call the reconcilers directly, with a fake Kubernetes client and a mock
// PhoenixNAP API.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2/clientcredentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bmcv1 "github.com/phoenixnap/cluster-api-provider-bmc/api/v1beta1"
	"github.com/phoenixnap/cluster-api-provider-bmc/controllers"
)

const testNamespace = "default"

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, bmcv1.AddToScheme, clusterv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

type env struct {
	client  client.Client
	cluster *controllers.BMCClusterReconciler
	machine *controllers.BMCMachineReconciler
	panics  []interface{}
}

func newEnv(t *testing.T, endpointURL string, creds clientcredentials.Config, objs ...client.Object) *env {
	t.Helper()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&bmcv1.BMCCluster{}, &bmcv1.BMCMachine{}, &clusterv1.Cluster{}, &clusterv1.Machine{}).
		Build()
	recorder := record.NewFakeRecorder(10000)
	return &env{
		client: c,
		cluster: &controllers.BMCClusterReconciler{Client: c, Recorder: recorder, ReconcileTimeout: 5 * time.Minute,
			BMCClientConfig: creds, BMCEndpointURL: endpointURL},
		machine: &controllers.BMCMachineReconciler{Client: c, Recorder: recorder, ReconcileTimeout: 5 * time.Minute,
			BMCClientConfig: creds, BMCEndpointURL: endpointURL},
	}
}

// reconcile records a panic instead of failing, as controller-runtime recovers them
func (e *env) reconcile(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, name string) (res ctrl.Result, err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			e.panics = append(e.panics, p)
			err = fmt.Errorf("panic: %v", p)
		}
	}()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: name}})
}

func (e *env) getBMCCluster(t *testing.T, name string) (*bmcv1.BMCCluster, bool) {
	t.Helper()
	var bc bmcv1.BMCCluster
	err := e.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &bc)
	if client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	return &bc, err == nil
}

func (e *env) getBMCMachine(t *testing.T, name string) (*bmcv1.BMCMachine, bool) {
	t.Helper()
	var bm bmcv1.BMCMachine
	err := e.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, &bm)
	if client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	return &bm, err == nil
}

func (e *env) delete(t *testing.T, obj client.Object) {
	t.Helper()
	if err := e.client.Delete(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

func testCluster(name string) *clusterv1.Cluster {
	return &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, UID: types.UID(name + "-uid")},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{APIVersion: bmcv1.GroupVersion.String(), Kind: "BMCCluster", Namespace: testNamespace, Name: name},
		},
		Status: clusterv1.ClusterStatus{InfrastructureReady: true},
	}
}

// testBMCCluster returns a BMCCluster owned by the Cluster of the same name
func testBMCCluster(name, location string, vip bmcv1.VIPMANAGER, annotations map[string]string) *bmcv1.BMCCluster {
	return &bmcv1.BMCCluster{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace, Name: name, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Cluster", Name: name, UID: types.UID(name + "-uid")}},
		},
		Spec: bmcv1.BMCClusterSpec{Location: bmcv1.LocationID(location), VIPManager: vip},
	}
}

func testMachine(cluster, name string) (*clusterv1.Machine, *corev1.Secret) {
	secretName := name + "-bootstrap"
	m := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace, Name: name, UID: types.UID(name + "-uid"),
			Labels: map[string]string{clusterv1.ClusterNameLabel: cluster},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName:       cluster,
			Bootstrap:         clusterv1.Bootstrap{DataSecretName: &secretName},
			InfrastructureRef: corev1.ObjectReference{APIVersion: bmcv1.GroupVersion.String(), Kind: "BMCMachine", Namespace: testNamespace, Name: name},
		},
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: secretName},
		Data:       map[string][]byte{"value": []byte("#cloud-config\n{}\n")},
	}
	return m, s
}

func testBMCMachine(name string) *bmcv1.BMCMachine {
	no := false
	// machines cloned from a template always have annotations
	annotations := map[string]string{"cluster.x-k8s.io/cloned-from-name": "template"}
	return &bmcv1.BMCMachine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace, Name: name, Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: name, UID: types.UID(name + "-uid")}},
		},
		Spec: bmcv1.BMCMachineSpec{
			OS: bmcv1.UbuntuNoble, Type: "s0.d1.small", Location: bmcv1.Phoenix,
			InstallDefaultSSHKeys: &no, NetworkType: bmcv1.PublicAndPrivate,
			Description: "cluster-api-provider-bmc test machine",
		},
	}
}

// mockAPI is an in-memory PhoenixNAP API. Like the real one, it answers 403 for unknown IDs.
type mockAPI struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []string
	blocks  map[string]map[string]interface{}
	servers map[string]map[string]interface{}
	next    int

	DeprovisionStatus int
	CreateServerBody  string // replaces the created server in the response, if set
	PublicIPs         []string
	CreateBlockBody   string // replaces the created IP block in the response, if set
	Revoked           bool   // every call fails with 403
}

func (m *mockAPI) SetBlockStatus(status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.blocks {
		b["status"] = status
	}
}

// set changes the mock's settings while it serves requests
func (m *mockAPI) set(f func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f()
}

func newMockAPI(t *testing.T) *mockAPI {
	m := &mockAPI{blocks: map[string]map[string]interface{}{}, servers: map[string]map[string]interface{}{},
		DeprovisionStatus: http.StatusAccepted, PublicIPs: []string{"198.18.1.5"}}
	m.Server = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.Close)
	return m
}

func (m *mockAPI) creds() clientcredentials.Config {
	return clientcredentials.Config{ClientID: "id", ClientSecret: "secret", TokenURL: m.URL + "/token", Scopes: []string{"bmc", "bmc.read"}}
}

func (m *mockAPI) count(method, pathPrefix string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if strings.HasPrefix(c, method+" "+pathPrefix) {
			n++
		}
	}
	return n
}

func writeJSONResponse(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *mockAPI) handle(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, r.Method+" "+r.URL.Path)
	forbidden := map[string]string{"message": "The request failed since this resource cannot be accessed by the provided credentials."}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	now := time.Now().UTC().Format(time.RFC3339)

	switch {
	case r.URL.Path == "/token":
		writeJSONResponse(w, http.StatusOK, map[string]interface{}{"access_token": "token", "token_type": "bearer", "expires_in": 3600})

	case m.Revoked:
		writeJSONResponse(w, http.StatusForbidden, forbidden)

	case r.Method == http.MethodGet && r.URL.Path == "/ips/v1/ip-blocks":
		list := []map[string]interface{}{}
		for _, b := range m.blocks {
			list = append(list, b)
		}
		writeJSONResponse(w, http.StatusOK, list)

	case r.Method == http.MethodGet && r.URL.Path == "/bmc/v1/servers":
		list := []map[string]interface{}{}
		for _, s := range m.servers {
			list = append(list, s)
		}
		writeJSONResponse(w, http.StatusOK, list)

	case r.Method == http.MethodPost && r.URL.Path == "/ips/v1/ip-blocks":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.next++
		id := fmt.Sprintf("block-%d", m.next)
		b := map[string]interface{}{"id": id, "cidr": fmt.Sprintf("198.18.0.%d/31", 2*m.next), "status": controllers.StatusNotAssigned,
			"location": req["location"], "cidrBlockSize": req["cidrBlockSize"], "description": req["description"], "createdOn": now}
		m.blocks[id] = b
		if m.CreateBlockBody != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(m.CreateBlockBody))
			return
		}
		writeJSONResponse(w, http.StatusCreated, b)

	case len(parts) == 4 && parts[0] == "ips" && parts[2] == "ip-blocks":
		b, ok := m.blocks[parts[3]]
		switch {
		case !ok:
			writeJSONResponse(w, http.StatusForbidden, forbidden)
		case r.Method == http.MethodGet:
			writeJSONResponse(w, http.StatusOK, b)
		case r.Method == http.MethodDelete:
			delete(m.blocks, parts[3])
			writeJSONResponse(w, http.StatusOK, map[string]string{"result": "IP Block is being deleted.", "ipBlockId": parts[3]})
		}

	case r.Method == http.MethodPost && r.URL.Path == "/bmc/v1/servers":
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.next++
		id := fmt.Sprintf("server-%d", m.next)
		s := map[string]interface{}{"id": id, "status": "creating", "hostname": req["hostname"], "type": req["type"], "location": req["location"],
			"cpu": "Intel Xeon E-2276G", "cpuCount": 1, "coresPerCpu": 6, "cpuFrequency": 3.8, "ram": "32GB", "storage": "1x 960GB NVMe",
			"privateIpAddresses": []string{"10.0.0.11"}, "publicIpAddresses": m.PublicIPs, "provisionedOn": now}
		m.servers[id] = s
		if m.CreateServerBody != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(m.CreateServerBody))
			return
		}
		writeJSONResponse(w, http.StatusAccepted, s)

	case len(parts) == 4 && parts[1] == "v1" && parts[2] == "servers" && r.Method == http.MethodGet:
		s, ok := m.servers[parts[3]]
		if !ok {
			writeJSONResponse(w, http.StatusForbidden, forbidden)
			return
		}
		s["status"] = controllers.StatusPoweredOn
		writeJSONResponse(w, http.StatusOK, s)

	case len(parts) == 6 && parts[2] == "servers" && parts[5] == "deprovision":
		if _, ok := m.servers[parts[3]]; !ok {
			writeJSONResponse(w, http.StatusForbidden, forbidden)
			return
		}
		if m.DeprovisionStatus/100 == 2 {
			delete(m.servers, parts[3])
		}
		writeJSONResponse(w, m.DeprovisionStatus, map[string]string{"result": "Server deprovisioned"})

	default:
		writeJSONResponse(w, http.StatusNotFound, map[string]string{"message": "not implemented in mock: " + r.Method + " " + r.URL.Path})
	}
}

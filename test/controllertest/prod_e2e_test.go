//go:build prode2e

// Production tests: the real reconcilers against the real PhoenixNAP API, with a fake
// management cluster. Excluded from normal builds by the "prode2e" tag and gated by env:
//
//	PNAP_E2E=1                    required for any test in this file (tier 0 is read-only)
//	PNAP_E2E_ALLOW_SPEND=yes      tier 1: creates and deletes one /31 IP block
//	PNAP_E2E_ALLOW_SERVER=yes     tier 2: also provisions and deprovisions one server (hourly billing)
//	PNAP_E2E_LOCATION=PHX         location for created resources
//	PNAP_E2E_SERVER_TYPE=s0.d1.small, PNAP_E2E_OS=ubuntu/jammy   tier 2 server
//	PNAP_CONFIG=~/.pnap/config.yaml                             clientId / clientSecret
//	PNAP_E2E_LEDGER=<file>        ledger of created resources (default: in the temp directory)
//
// See test/controllertest/README.md.
package controllertest

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2/clientcredentials"

	bmcv1 "github.com/phoenixnap/cluster-api-provider-bmc/api/v1beta1"
	"github.com/phoenixnap/cluster-api-provider-bmc/controllers"
)

const (
	prodEndpoint = "https://api.phoenixnap.com"
	prodTokenURL = "https://auth.phoenixnap.com/auth/realms/BMC/protocol/openid-connect/token"
	// run names start with this, so nothing a test creates can be mistaken for real resources
	runPrefix = "capi-e2e-"
)

func requireProd(t *testing.T) {
	t.Helper()
	if os.Getenv("PNAP_E2E") != "1" {
		t.Skip("set PNAP_E2E=1 to run production tests")
	}
}

func requireSpend(t *testing.T, gate string) {
	t.Helper()
	requireProd(t)
	if os.Getenv(gate) != "yes" {
		t.Skipf("set %s=yes to run this billable test", gate)
	}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// prodCreds reads clientId/clientSecret from the pnapctl config, or BMC_CLIENT_ID /
// BMC_CLIENT_SECRET. The scopes match main.go. Values are never logged.
func prodCreds(t *testing.T) clientcredentials.Config {
	t.Helper()
	id, secret := os.Getenv(controllers.ENV_BMC_CLIENT_ID), os.Getenv(controllers.ENV_BMC_CLIENT_SECRET)
	if id == "" || secret == "" {
		path := os.Getenv("PNAP_CONFIG")
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".pnap", "config.yaml")
			if _, err := os.Stat(path); err != nil {
				path = filepath.Join(home, ".pnap", "config.yml")
			}
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("unable to open PNAP config %s: %v", path, err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			switch strings.TrimSpace(k) {
			case "clientId", "clientID", "client_id":
				id = v
			case "clientSecret", "client_secret":
				secret = v
			}
		}
	}
	if id == "" || secret == "" {
		t.Fatal("no PNAP credentials: set PNAP_CONFIG or BMC_CLIENT_ID/BMC_CLIENT_SECRET")
	}
	return clientcredentials.Config{ClientID: id, ClientSecret: secret, TokenURL: envOr(controllers.ENV_BMC_TOKEN_URL, prodTokenURL), Scopes: []string{"bmc", "bmc.read"}}
}

// prodClient returns an HTTP client that authenticates like the controllers do
func prodClient(t *testing.T) *http.Client {
	t.Helper()
	creds := prodCreds(t)
	return creds.Client(context.Background())
}

func endpoint() string {
	return strings.TrimSuffix(envOr(controllers.ENV_BMC_ENDPOINT_URL, prodEndpoint), "/")
}

// apiGet returns the status code and body of a GET against the API
func apiGet(t *testing.T, c *http.Client, path string) (int, []byte) {
	t.Helper()
	resp, err := c.Get(endpoint() + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func randomRunID(t *testing.T) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Tier 0: read-only
// ---------------------------------------------------------------------------

// The provider's credentials and scopes can read servers and IP blocks.
func TestProdAuth(t *testing.T) {
	requireProd(t)
	c := prodClient(t)
	for _, path := range []string{"/bmc/v1/servers", "/ips/v1/ip-blocks"} {
		if code, body := apiGet(t, c, path); code != http.StatusOK {
			t.Errorf("GET %s: %d %s", path, code, body)
		}
	}
}

// Every server in the account decodes into BMCMachineStatus the way the controller
// decodes it, uses a status the controller understands, and reports usable addresses.
func TestProdServersDecode(t *testing.T) {
	requireProd(t)
	c := prodClient(t)
	code, body := apiGet(t, c, "/bmc/v1/servers")
	if code != http.StatusOK {
		t.Fatalf("GET /bmc/v1/servers: %d", code)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	t.Logf("account has %d servers", len(raw))
	statuses := map[string]int{}
	for _, r := range raw {
		var s bmcv1.BMCMachineStatus
		if err := json.Unmarshal(r, &s); err != nil {
			t.Errorf("server does not decode into BMCMachineStatus: %v", err)
			continue
		}
		statuses[s.BMCStatus]++
		for _, a := range append(append([]string{}, s.PublicIPAddresses...), s.PrivateIPAddresses...) {
			// the controller publishes an address, or the first address of a "first - last"
			// range (see TestOfflineMachineAddressRange); anything else would be dropped
			first, _, isRange := strings.Cut(a, " - ")
			if net.ParseIP(strings.TrimSpace(first)) == nil {
				t.Errorf("server %s reports address %q, which is neither an IP nor a range", s.BMCServerID, a)
			} else if isRange {
				t.Logf("server %s reports range %q; the machine address is %s", s.BMCServerID, a, strings.TrimSpace(first))
			}
		}
	}
	t.Logf("server statuses: %v (the controller acts on %q and %q)", statuses, controllers.StatusPoweredOn, controllers.StatusError)
}

// IP blocks decode into BMCClusterStatus and use the status values the controller compares against.
func TestProdIPBlocksDecode(t *testing.T) {
	requireProd(t)
	c := prodClient(t)
	code, body := apiGet(t, c, "/ips/v1/ip-blocks")
	if code != http.StatusOK {
		t.Fatalf("GET /ips/v1/ip-blocks: %d", code)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]int{}
	for _, r := range raw {
		var s bmcv1.BMCClusterStatus
		if err := json.Unmarshal(r, &s); err != nil {
			t.Errorf("IP block does not decode into BMCClusterStatus: %v", err)
			continue
		}
		statuses[s.BMCStatus]++
	}
	t.Logf("%d IP blocks, statuses: %v (the controller compares against %q and %q)", len(raw), statuses, controllers.StatusAssigned, controllers.StatusNotAssigned)
}

// Finding a server or IP block whose ID was lost (create_requested_at) relies on these
// fields in the list responses; if one is missing the lookup silently finds nothing.
func TestProdListFields(t *testing.T) {
	requireProd(t)
	c := prodClient(t)
	for path, fields := range map[string][]string{
		"/bmc/v1/servers":   {"id", "hostname", "location", "provisionedOn"},
		"/ips/v1/ip-blocks": {"id", "location", "description", "createdOn"},
	} {
		code, body := apiGet(t, c, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s: %d", path, code)
		}
		var items []map[string]interface{}
		if err := json.Unmarshal(body, &items); err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			t.Logf("GET %s: no items to check", path)
			continue
		}
		missing := map[string]int{}
		for _, item := range items {
			for _, f := range fields {
				if _, ok := item[f]; !ok {
					missing[f]++
				}
			}
		}
		for _, f := range fields {
			if missing[f] > 0 {
				// description is optional on IP blocks; the controller always sets it
				if f == "description" {
					t.Logf("GET %s: %d of %d items have no %s", path, missing[f], len(items), f)
					continue
				}
				t.Errorf("GET %s: %d of %d items have no %q field", path, missing[f], len(items), f)
			}
		}
		if ts, ok := items[0][fields[3]].(string); ok {
			if _, err := time.Parse(time.RFC3339, ts); err != nil {
				missing[fields[3]+" format"]++
			}
		}
		if missing[fields[3]+" format"] > 0 {
			t.Errorf("GET %s: %s is not RFC 3339: %v", path, fields[3], items[0][fields[3]])
		}
	}
}

// Records how the API answers for unknown IDs. The controllers map 403 to "orphaned" and
// stop managing the resource, so this must stay 403 (or 404) for deleted resources.
func TestProdUnknownIDs(t *testing.T) {
	requireProd(t)
	c := prodClient(t)
	for _, path := range []string{"/bmc/v1/servers/000000000000000000000000", "/ips/v1/ip-blocks/000000000000000000000000"} {
		code, body := apiGet(t, c, path)
		t.Logf("GET %s -> %d %s", path, code, strings.TrimSpace(string(body)))
		if code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("unexpected status %d for an unknown ID", code)
		}
	}
}

// enumFromSource reads a +kubebuilder:validation:Enum list from the API types
func enumFromSource(t *testing.T, typeName string) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "api", "v1beta1", "bmcmachine_types.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^// \+kubebuilder:validation:Enum=([^\n]+)\ntype ` + typeName + ` `)
	m := re.FindSubmatch(src)
	if m == nil {
		t.Fatalf("no enum for %s", typeName)
	}
	set := map[string]bool{}
	for _, v := range strings.Split(string(m[1]), ";") {
		set[strings.TrimSpace(v)] = true
	}
	return set
}

// patternFromSource returns the +kubebuilder:validation:Pattern of a type, or nil if it has none
func patternFromSource(t *testing.T, typeName string) *regexp.Regexp {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "api", "v1beta1", "bmcmachine_types.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?m)^// \\+kubebuilder:validation:Pattern=`([^`]+)`\ntype " + typeName + " ").FindSubmatch(src)
	if m == nil {
		return nil
	}
	return regexp.MustCompile(string(m[1]))
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The CRD validates server types by format and locations against a list. Every server
// type and location for sale must be accepted.
func TestProdCatalogDrift(t *testing.T) {
	requireProd(t)
	c := prodClient(t)
	code, body := apiGet(t, c, "/billing/v1/products?productCategory=SERVER")
	if code != http.StatusOK {
		t.Skipf("billing products not readable with these credentials: %d", code)
	}
	var products []struct {
		ProductCode string `json:"productCode"`
		Plans       []struct {
			Location string `json:"location"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(body, &products); err != nil {
		t.Fatal(err)
	}
	sold, soldLocations := map[string]bool{}, map[string]bool{}
	for _, p := range products {
		sold[p.ProductCode] = true
		for _, plan := range p.Plans {
			soldLocations[plan.Location] = true
		}
	}

	allowedLocations := enumFromSource(t, "LocationID")
	var notAllowed, notSold, locNotAllowed []string
	if pattern := patternFromSource(t, "ServerType"); pattern != nil {
		// server types are validated by format; every type for sale must match it
		for _, s := range sortedKeys(sold) {
			if !pattern.MatchString(s) {
				notAllowed = append(notAllowed, s)
			}
		}
		if len(notAllowed) > 0 {
			t.Errorf("server types for sale that the CRD pattern rejects: %v", notAllowed)
		}
	} else {
		allowedTypes := enumFromSource(t, "ServerType")
		for _, s := range sortedKeys(sold) {
			if !allowedTypes[s] {
				notAllowed = append(notAllowed, s)
			}
		}
		for _, s := range sortedKeys(allowedTypes) {
			if !sold[s] {
				notSold = append(notSold, s)
			}
		}
	}
	for _, l := range sortedKeys(soldLocations) {
		if !allowedLocations[l] {
			locNotAllowed = append(locNotAllowed, l)
		}
	}
	t.Logf("server types for sale but rejected by the CRD: %v", notAllowed)
	t.Logf("server types the CRD accepts but are not for sale: %v", notSold)
	if len(locNotAllowed) > 0 {
		t.Errorf("locations with servers for sale that the CRD rejects: %v", locNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// Tiers 1 and 2: billable
// ---------------------------------------------------------------------------

// ledger records what a run created, so cleanup only touches those resources
type ledger struct {
	mu       sync.Mutex
	path     string
	RunID    string   `json:"runID"`
	Name     string   `json:"name"`
	IPBlocks []string `json:"ipBlocks"`
	Servers  []string `json:"servers"`
}

func newLedger(t *testing.T) *ledger {
	id := randomRunID(t)
	l := &ledger{RunID: id, Name: runPrefix + id, path: envOr("PNAP_E2E_LEDGER", filepath.Join(os.TempDir(), "capi-e2e-ledger-"+id+".json"))}
	l.save()
	t.Logf("run %s, ledger %s", l.Name, l.path)
	return l
}

func (l *ledger) add(list *[]string, id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, x := range *list {
		if x == id {
			return
		}
	}
	*list = append(*list, id)
	l.saveLocked()
}

func (l *ledger) save() { l.mu.Lock(); defer l.mu.Unlock(); l.saveLocked() }
func (l *ledger) saveLocked() {
	b, _ := json.MarshalIndent(l, "", "  ")
	_ = os.WriteFile(l.path, b, 0o600)
}

// teardown deprovisions the run's servers and deletes its IP blocks. Each resource is
// re-read first and only touched if its hostname or description carries the run name.
func teardown(t *testing.T, c *http.Client, l *ledger) {
	do := func(method, path, body string) (int, []byte) {
		req, _ := http.NewRequest(method, endpoint()+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			return 0, []byte(err.Error())
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	gone := func(code int) bool { return code == http.StatusForbidden || code == http.StatusNotFound }

	for _, id := range l.Servers {
		code, body := do(http.MethodGet, "/bmc/v1/servers/"+id, "")
		if gone(code) {
			continue
		}
		var s struct{ Hostname string }
		_ = json.Unmarshal(body, &s)
		if !strings.HasPrefix(s.Hostname, l.Name) {
			t.Errorf("cleanup: server %s has hostname %q, not from run %s; not touching it", id, s.Hostname, l.Name)
			continue
		}
		if code, body := do(http.MethodPost, "/bmc/v1/servers/"+id+"/actions/deprovision", `{"deleteIpBlocks":true}`); code/100 != 2 && !gone(code) {
			t.Errorf("cleanup: deprovision server %s: %d %s; deprovision it manually", id, code, body)
		}
	}

	deadline := time.Now().Add(10 * time.Minute)
	for _, id := range l.IPBlocks {
		for {
			code, body := do(http.MethodGet, "/ips/v1/ip-blocks/"+id, "")
			if gone(code) {
				break
			}
			var b struct{ Status, Description string }
			_ = json.Unmarshal(body, &b)
			if !strings.Contains(b.Description, l.Name) {
				t.Errorf("cleanup: IP block %s has description %q, not from run %s; not touching it", id, b.Description, l.Name)
				break
			}
			if b.Status == controllers.StatusNotAssigned {
				if code, body := do(http.MethodDelete, "/ips/v1/ip-blocks/"+id, ""); code/100 != 2 && !gone(code) {
					t.Errorf("cleanup: delete IP block %s: %d %s", id, code, body)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Errorf("cleanup: IP block %s still %q; delete it manually", id, b.Status)
				break
			}
			time.Sleep(15 * time.Second) // still assigned to a server that is deprovisioning
		}
	}
}

// Tier 1: the BMCCluster controller creates the control-plane /31, reports it, and deletes it.
// KUBEVIP leaves the block unassigned, so the delete path runs without any server.
func TestProdClusterIPBlockLifecycle(t *testing.T) {
	requireSpend(t, "PNAP_E2E_ALLOW_SPEND")
	creds := prodCreds(t)
	l := newLedger(t)
	t.Cleanup(func() { teardown(t, creds.Client(context.Background()), l) })

	// no annotations, as the shipped cluster template creates it
	e := newEnv(t, endpoint(), creds, testCluster(l.Name), testBMCCluster(l.Name, envOr("PNAP_E2E_LOCATION", "PHX"), bmcv1.KUBEVIP, nil))
	if _, err := e.reconcile(t, e.cluster, l.Name); err != nil {
		t.Fatalf("create: %v", err)
	}
	bc, _ := e.getBMCCluster(t, l.Name)
	blockID := bc.Annotations[controllers.ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID]
	if blockID == "" {
		t.Fatalf("no IP block recorded; status %+v", bc.Status)
	}
	l.add(&l.IPBlocks, blockID)

	_, cidr, err := net.ParseCIDR(bc.Status.CIDR)
	if err != nil || !cidr.Contains(net.ParseIP(bc.Spec.ControlPlaneEndpoint.Host)) || !bc.Status.Ready {
		t.Errorf("endpoint %+v not in block %q, or not ready (%v)", bc.Spec.ControlPlaneEndpoint, bc.Status.CIDR, bc.Status.Ready)
	}

	if _, err := e.reconcile(t, e.cluster, l.Name); err != nil {
		t.Fatalf("sync: %v", err)
	}
	bc, _ = e.getBMCCluster(t, l.Name)
	if bc.Status.BMCStatus != controllers.StatusNotAssigned {
		t.Errorf("block status %q, want %q", bc.Status.BMCStatus, controllers.StatusNotAssigned)
	}

	e.delete(t, bc)
	if _, err := e.reconcile(t, e.cluster, l.Name); err != nil {
		t.Fatalf("delete: %v", err)
	}
	c := creds.Client(context.Background())
	if code, _ := apiGet(t, c, "/ips/v1/ip-blocks/"+blockID); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Errorf("IP block %s still exists after delete (GET %d)", blockID, code)
	}
}

// Tier 2: the BMCMachine controller provisions a worker server, reports it ready with
// addresses, and deprovisions it on delete. Takes 15-45 minutes.
func TestProdMachineLifecycle(t *testing.T) {
	requireSpend(t, "PNAP_E2E_ALLOW_SPEND")
	requireSpend(t, "PNAP_E2E_ALLOW_SERVER")
	creds := prodCreds(t)
	l := newLedger(t)
	t.Cleanup(func() { teardown(t, creds.Client(context.Background()), l) })

	location := envOr("PNAP_E2E_LOCATION", "PHX")
	machineName := l.Name + "-worker"
	m, s := testMachine(l.Name, machineName)
	e := newEnv(t, endpoint(), creds, testCluster(l.Name),
		// workers do not use the cluster IP block; the annotation only has to be present
		testBMCCluster(l.Name, location, bmcv1.KUBEVIP, map[string]string{controllers.ANNOTATION_NAME_BMC_CLUSTER_IP_ALLOCATION_ID: "unused", controllers.ANNOTATION_NAME_BMC_CLUSTER_IP: "192.0.2.1"}),
		m, s, testBMCMachine(machineName, location, envOr("PNAP_E2E_SERVER_TYPE", "s0.d1.small"), envOr("PNAP_E2E_OS", "ubuntu/jammy"), nil))

	if _, err := e.reconcile(t, e.machine, machineName); err != nil {
		t.Fatalf("create: %v", err)
	}
	bm, _ := e.getBMCMachine(t, machineName)
	serverID := bm.Annotations[controllers.ANNOTATION_NAME_BMC_SERVER_ID]
	if serverID == "" {
		t.Fatalf("no server recorded; status %+v", bm.Status)
	}
	l.add(&l.Servers, serverID)
	t.Logf("server %s created", serverID)

	deadline := time.Now().Add(60 * time.Minute)
	for !bm.Status.Ready && time.Now().Before(deadline) {
		time.Sleep(time.Minute)
		_, _ = e.reconcile(t, e.machine, machineName)
		bm, _ = e.getBMCMachine(t, machineName)
		if bm.Status.BMCStatus == controllers.StatusError || bm.Status.BMCStatus == controllers.StatusIrreconcilable {
			t.Fatalf("server failed: %s %v", bm.Status.BMCStatus, bm.Status.FailureMessage)
		}
	}
	if !bm.Status.Ready {
		t.Fatalf("server %s not ready within an hour, status %q", serverID, bm.Status.BMCStatus)
	}
	if len(bm.Status.Addresses) == 0 {
		t.Error("ready machine has no addresses")
	}
	for _, a := range bm.Status.Addresses {
		if net.ParseIP(a.Address) == nil {
			t.Errorf("machine address %q is not an IP", a.Address)
		}
	}

	e.delete(t, bm)
	if _, err := e.reconcile(t, e.machine, machineName); err != nil {
		t.Errorf("delete: %v", err)
	}
	c := creds.Client(context.Background())
	for time.Now().Before(deadline) {
		if code, _ := apiGet(t, c, "/bmc/v1/servers/"+serverID); code == http.StatusForbidden || code == http.StatusNotFound {
			return
		}
		time.Sleep(30 * time.Second)
	}
	t.Errorf("server %s still exists after delete", serverID)
}

// TestProdCleanup removes what an interrupted run left, using its ledger:
// PNAP_E2E=1 PNAP_E2E_LEDGER=<ledger path printed by the run> go test -tags prode2e -run TestProdCleanup ./test/controllertest
func TestProdCleanup(t *testing.T) {
	requireProd(t)
	path := os.Getenv("PNAP_E2E_LEDGER")
	if path == "" {
		t.Skip("set PNAP_E2E_LEDGER to the ledger of the run to clean up")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	l := &ledger{path: path}
	if err := json.Unmarshal(b, l); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.Name, runPrefix) {
		t.Fatalf("ledger run name %q is not a test run; refusing", l.Name)
	}
	teardown(t, prodClient(t), l)
	fmt.Fprintf(os.Stderr, "cleanup of %s done\n", l.Name)
}

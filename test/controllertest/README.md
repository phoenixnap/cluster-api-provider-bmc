# Controller tests

These tests call `BMCClusterReconciler` and `BMCMachineReconciler` directly, with a
fake management cluster (the controller-runtime fake client). There is no envtest, kind
or clusterctl. The PhoenixNAP side is either a mock or the production API.

## Offline tests

```bash
go test ./test/controllertest
```

They run in CI and need no credentials. The mock API in `harness_test.go` behaves like
the production API where the controllers depend on it: 403 for unknown IDs,
`assigned`/`unassigned` IP block statuses, list responses with `provisionedOn` and
`createdOn`, and `"first - last"` address ranges.

They cover:
- the cluster and machine lifecycles;
- finding a server or IP block whose ID was lost instead of creating another;
- keeping the finalizer until a server or IP block is really gone, including with revoked credentials;
- waiting for a server to release the cluster IP block;
- not retrying permanent create failures;
- address conversion.

## Production tests

These are excluded from normal builds by the `prode2e` tag. Credentials come from
`~/.pnap/config.yaml` (`clientId`/`clientSecret`) or from
`BMC_CLIENT_ID`/`BMC_CLIENT_SECRET`, and are never logged. Use a dedicated API
application.

| Tier | Gate | Creates | Duration |
|------|------|---------|----------|
| 0 | `PNAP_E2E=1` | nothing; GET requests only | seconds |
| 1 | `+ PNAP_E2E_ALLOW_SPEND=yes` | one /31 public IP block, deleted by the test | about 1 min |
| 2 | `+ PNAP_E2E_ALLOW_SERVER=yes` | one server (default `s0.d1.small`, hourly) | 15–45 min |

```bash
# tier 0: credentials, status decoding, list fields, 403 for unknown IDs, catalog vs. CRD validation
PNAP_E2E=1 go test -tags prode2e -count=1 -v -run 'TestProd(Auth|ServersDecode|IPBlocksDecode|ListFields|UnknownIDs|CatalogDrift)' ./test/controllertest

# tier 1: BMCCluster create, sync and delete with a real /31
PNAP_E2E=1 PNAP_E2E_ALLOW_SPEND=yes PNAP_E2E_LOCATION=PHX \
  go test -tags prode2e -count=1 -v -run TestProdClusterIPBlockLifecycle ./test/controllertest

# tier 2: BMCMachine provisions a worker server, becomes ready, and deprovisions it
PNAP_E2E=1 PNAP_E2E_ALLOW_SPEND=yes PNAP_E2E_ALLOW_SERVER=yes PNAP_E2E_LOCATION=PHX \
  go test -tags prode2e -count=1 -v -timeout 90m -run TestProdMachineLifecycle ./test/controllertest
```

**Safety:**
- Every created resource is named `capi-e2e-<run>`, in the server hostname or the IP block description.
- The resource is recorded in a ledger file as soon as its ID is known. The file goes in the temp directory, and its path is logged.
- Cleanup re-reads each resource and only touches it if it carries the run name.
- After an interrupted run:

```bash
PNAP_E2E=1 PNAP_E2E_LEDGER=<ledger path> go test -tags prode2e -count=1 -v -run TestProdCleanup ./test/controllertest
```

## Before a release

Run tiers 0–2. Then install the release manifests with `clusterctl init --infrastructure bmc` on a kind management cluster and create a cluster from `templates/cluster-template.yaml`. Check that:
- the machines get `providerID: bmc://servers/<id>`;
- the workload nodes become Ready;
- scaling up and down creates and deprovisions servers;
- deleting the cluster leaves no servers or IP blocks behind.

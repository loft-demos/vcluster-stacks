# gpu-burn-in-recorder

A controller that records [gpu-burn-in](../README.md) results on the servers they qualified. Install
it once on the cluster that runs vCluster Platform. Each finished run then leaves a result label on
the server's inventory record, and NodeType selectors can use that label to keep failed servers
away from customers.

It replaces running [record-qualification.sh](../automation/record-qualification.sh) by hand after
each qualification.

## Contents

- [What it does](#what-it-does)
- [Requirements](#requirements)
- [Install](#install)
- [Configure](#configure)
- [Per-run parameters](#per-run-parameters)
- [QualificationReport](#qualificationreport)
- [Verify](#verify)
- [Permissions](#permissions)
- [Build](#build)
- [Run from a workstation](#run-from-a-workstation)
- [Troubleshooting](#troubleshooting)
- [Development](#development)
- [Tested](#tested)

## What it does

The controller watches StackInstances of the `gpu-burn-in` StackTemplate. For each one:

1. **It waits for the report.** The `report` task must be Healthy, and its outputs must be
   captured in the instance's outputs Secret, `stack-outputs-<instance>`, in the project namespace.
2. **It finds the server the run tested.** The tenant cluster's NodeClaims lead to the inventory
   Machines whose `machines.vcluster.com/node-claim` annotation names one of them. The run needs no
   `assetID`, and the controller cannot record on the wrong server.
3. **It updates NetBox, if the run asks.** The device comes from the Machine's
   `netbox.vcluster.com/url` annotation. On a failure, the device status becomes `failed` and the
   admit tag is removed; on a pass, the qualified tag is added. Either way, a journal entry gets
   the summary.
4. **It creates a QualificationReport**, one per run. See [QualificationReport](#qualificationreport).
5. **It writes the record.** That's the label `<domain>/result=passed|failed`, plus annotations with
   the profile, finish time, failed tests, summary, report URI and the run. It writes them to the
   Platform Machine or to the Metal3 BareMetalHost.
6. **It deletes the tenant cluster, if the run asks.** That returns the server to its node provider,
   which wipes it.

The record survives the teardown. The Metal3 provider removes only its own project and NodeClaim
labels when it releases a host.

Each run is recorded once. The `<domain>/run` annotation on the target names the StackInstance and
its UID, and the controller skips a target that already carries it. NetBox is written before the
target, so a run that fails partway is retried until the target records it. A retry can add a
second NetBox journal entry.

The controller must see the run before the tenant cluster is deleted, because the NodeClaim, and
the Machine's annotation naming it, go away with the tenant cluster. With `releaseServer`, the
controller does the deletion itself, after recording.

## QualificationReport

Every recorded run also becomes a `QualificationReport`
(`qualification.vcluster-stacks.io/v1alpha1`). It's a cluster-scoped record of one qualification
of one machine, kept beside the vCluster Platform Machine it names. The CRD is in
[chart/crds/](chart/crds/qualificationreports.yaml).

The report doesn't depend on how the tests ran. The gpu-burn-in Stack fills it through this
controller, and NICo Machine Validation or another test suite could fill the same fields with no
tenant cluster at all. That makes it the one source for dashboards, gating and reporting across
providers.

```bash
kubectl get qualificationreports
NAME                                                          MACHINE                                             RESULT   FAILED   PROFILE                    SOURCE   FINISHED
metal3-us-va-blacksburg-dc1.rack-b-u18-xlarge-6ae3ece2   metal3-us-va-blacksburg-dc1.rack-b-u18-xlarge   failed   [...]    nvml-mock-ecc              Stack    3m
```

| Field | Content |
| --- | --- |
| `spec.machineRef`, `providerRef`, `assetID` | The machine qualified |
| `spec.source` | What ran it: `kind` (`Stack`, `NICoValidation`, `vMetalAgent`, `Other`) and, for a Stack, the StackInstance and template |
| `spec.result`, `failedTests`, `summary`, `profile` | The verdict and the thresholds it was judged against |
| `spec.startedAt`, `finishedAt`, `reportURI` | When, and where the full report is stored |
| `spec.hardware` | Node, GPU model, count, expected count, driver, each GPU's UUID and serial, and DMI |
| `spec.tests[]` | Each test's name, status, summary, times and `metrics` |

The spec is a record. The API server rejects any change to it, and the labels
`qualification.vcluster-stacks.io/machine`, `/result`, `/provider`, `/profile` and `/source-kind`
make reports easy to select. The controller keeps the newest `keepReports` (20) per machine. It
reads the full report from the report task's `reportjson` output; a Stack without that output
gives a report without hardware and tests.

Each test's `metrics` is a flat set of named numbers, the same names whatever ran the test:

| Test | Metrics |
| --- | --- |
| `gpuready` | `allocatable_gpus` |
| `inventory` | `gpus`, `allocatable_gpus`, `uncorrected_ecc_errors`, `inactive_nvlinks` |
| `dcgmdiag` | `level`, `failed_tests`, `skipped_tests`, `warned_tests` |
| `nccl` | `all_reduce_bus_bandwidth_gbps`, `all_gather_bus_bandwidth_gbps`, `reduce_scatter_bus_bandwidth_gbps`, `gpus` |
| `soak` | `peak_temp_c`, `peak_power_w`, `gpus_with_xid`, `throttle_samples`, `samples`, `cycles`, `minutes` |

### Metrics

The controller computes Prometheus metrics from the reports on every scrape, at `:8080/metrics`.
The chart creates a Service for this, and with `metrics.serviceMonitor.enabled` a ServiceMonitor.
All metrics describe each machine's latest report, except `gpu_qualification_reports`:

| Metric | Labels | Value |
| --- | --- | --- |
| `gpu_qualification_result` | `machine`, `provider`, `profile`, `source`, `result` | 1 |
| `gpu_qualification_passed` | `machine`, `provider`, `profile` | 1 passed, 0 not |
| `gpu_qualification_finished_timestamp_seconds` | `machine`, `provider` | Finish time |
| `gpu_qualification_gpus` | `machine`, `kind` (`found`, `expected`) | GPU count |
| `gpu_qualification_test_status` | `machine`, `test`, `status` | 1 |
| `gpu_qualification_test_metric` | `machine`, `test`, `metric` | The measurement |
| `gpu_qualification_reports` | `machine`, `result` | Reports kept, for history |

For example, servers that are not qualified: `gpu_qualification_passed == 0`. Peak soak temperature
per server: `gpu_qualification_test_metric{test="soak",metric="peak_temp_c"}`.

## Requirements

- vCluster Platform 4.12.2 or later, which stores Stack outputs in the outputs Secret.
- The gpu-burn-in Stack from this repository, with the `recordResult`, `netboxUpdate` and
  `releaseServer` parameters.
- Servers from a node provider with an inventory, such as Metal3 or NICo, so each server has a
  Machine. A KubeVirt or cloud node has no Machine, and the controller records nothing for it.
- For the `machine` target: a Platform build that lets users label Machines. Earlier builds,
  including 4.13.0-alpha.19, reject every Machine write; use `bmh` there.
- For the `bmh` target: access to the cluster that runs Metal3. See
  [Configure](#configure).

## Install

From the repository root, on the vCluster Platform cluster:

```bash
# Platform builds that let users label Machines:
helm upgrade --install gpu-burn-in-recorder community-stacks/gpu-burn-in/recorder/chart \
  --namespace gpu-burn-in-recorder --create-namespace \
  --set labelDomain=gpu-qualification.example.com \
  --set image.tag=gpu-burn-in
```

On 4.13 and earlier, record on the BareMetalHost. If the Metal3 provider's `clusterRef` names the
Platform cluster itself, set `localCluster` to its Platform name. For any other cluster, store a
kubeconfig in a Secret whose key is the cluster's Platform name:

```bash
kubectl create namespace gpu-burn-in-recorder
kubectl -n gpu-burn-in-recorder create secret generic recorder-clusters \
  --from-file=spark-cluster=spark-cluster.kubeconfig
helm upgrade --install gpu-burn-in-recorder community-stacks/gpu-burn-in/recorder/chart \
  --namespace gpu-burn-in-recorder \
  --set labelDomain=gpu-qualification.example.com \
  --set image.tag=gpu-burn-in \
  --set target=bmh --set localCluster=loft-cluster \
  --set clusterKubeconfigs.existingSecret=recorder-clusters
```

Start with `--set dryRun=true`. The controller then logs every write it would make, including the
tenant cluster deletion, and makes none.

The image is `ghcr.io/loft-demos/gpu-burn-in-recorder`, built by
[the image workflow](../../../.github/workflows/gpu-burn-in-recorder-image.yml). Until a release
tag publishes the chart's `appVersion`, set `--set image.tag=gpu-burn-in` (the branch's latest
build) or `--set image.tag=sha-<short sha>`.

## Configure

Chart values hold the settings shared by every run.

| Value | Default | Purpose |
| --- | --- | --- |
| `labelDomain` | `gpu-qualification.example.com` | Domain of the label and annotations. Use one your organization owns. `vcluster.com` and `loft.sh` domains are refused: newer Platform builds keep labels in those domains out of NodeType selection |
| `target` | `machine` | `machine` writes the Platform Machine; `bmh` writes the Metal3 BareMetalHost |
| `templates` | `[gpu-burn-in]` | StackTemplates whose instances are recorded |
| `localCluster` | empty | `bmh` only. Platform name of the cluster the controller runs in. Hosts on it are written with the controller's own credentials, and the chart grants it `baremetalhosts` access |
| `clusterKubeconfigs.existingSecret` | empty | `bmh` only. Secret with one kubeconfig per other Platform cluster, keyed by cluster name |
| `netbox.tokenSecret.name`, `.key` | empty, `token` | Secret with the NetBox API token. Without it, runs that set `netboxUpdate` skip NetBox |
| `netbox.admitTag` | empty | Tag removed from a device that fails: the tag that brings it into vCluster Platform |
| `netbox.qualifiedTag` | empty | Tag added to a device that passes |
| `keepReports` | `20` | QualificationReports kept per machine. `0` keeps all |
| `metrics.service.enabled`, `metrics.serviceMonitor.enabled` | `true`, `false` | Service and ServiceMonitor for `/metrics` |
| `dryRun` | `false` | Log writes instead of making them |
| `rbac.releaseServer` | `true` | Grant delete on VirtualClusterInstances, for runs that set `releaseServer` |
| `replicas`, `leaderElection` | `1`, `false` | More than one replica needs leader election |
| `logLevel` | `info` | `debug` logs every reconcile |

**Kubeconfigs need a static credential.** A cluster connected to the Platform through its agent,
with `networkPeer: true`, has no stored kubeconfig the controller could reuse. A kubeconfig from
`vcluster platform connect cluster` authenticates through the `vcluster` CLI, which the image does
not have. Use a token instead. A Platform access key works through the cluster proxy, with server
`https://<platform host>/kubernetes/cluster/<cluster>`. So does a ServiceAccount token on that
cluster. Either needs `get` and `patch` on `baremetalhosts.metal3.io` in the provider's namespace.
The `machine` target needs none of this.

**NetBox tokens:** v1 tokens are sent as `Token <token>`, and NetBox 4.5 v2 tokens (`nbt_...`) as
`Bearer <token>`. The token needs to read and change devices and create journal entries. Check
what your NetBox import does when a device loses its admit tag before you set `netbox.admitTag`.

## Per-run parameters

Three Stack parameters choose what happens to each run. They're set on the tenant cluster
template or the StackInstance. A parameter the instance leaves unset takes the StackTemplate
default.

| Parameter | Default | Effect |
| --- | --- | --- |
| `recordResult` | `true` | Record the result. `false` makes the controller ignore the run |
| `netboxUpdate` | `false` | Also update the NetBox device |
| `releaseServer` | `false` | Delete the tenant cluster once the result is recorded |

## Verify

Watch the controller's decisions:

```bash
kubectl -n gpu-burn-in-recorder logs deploy/gpu-burn-in-recorder -f
```

Each record produces a `Recorded` event on the StackInstance. A run with no inventory Machine gets a
`NoMachine` warning, and a failed write gets `RecordFailed`:

```bash
kubectl -n <project-namespace> get events --field-selector involvedObject.name=<stack-instance>
```

Then read the record:

```bash
# machine target
kubectl get machines.management.loft.sh <provider>.<host> --show-labels
# bmh target, on the cluster that runs Metal3
kubectl -n metal3-system get bmh <host> \
  -o jsonpath='{.metadata.labels}{"\n"}{.metadata.annotations}{"\n"}'
```

## Permissions

The chart's ClusterRole gives the controller only what it uses:

| Access | Why |
| --- | --- |
| `get`, `list`, `watch` StackInstances, StackTemplates, NodeClaims, Machines and NodeProviders (`storage.loft.sh`) | The run, its parameter defaults, the claim, the server and its provider |
| `get`, `list`, `watch`, `create`, `delete` QualificationReports | Create one per run, prune past `keepReports`, compute metrics |
| `get` Secrets | The run's outputs Secret, by name. The controller never lists Secrets |
| `get`, `patch` Machines (`management.loft.sh`) | `machine` target |
| `get`, `patch` BareMetalHosts (`metal3.io`) | `bmh` target with `localCluster` |
| `delete` VirtualClusterInstances (`management.loft.sh`) | `rbac.releaseServer` |
| `create`, `patch` Events | Recorded, NoMachine and RecordFailed events |

Secrets `get` is cluster-wide, because Stack outputs Secrets live in every project namespace.

Writes go through the management API, which authorizes the controller's ServiceAccount. That has not
been verified in-cluster yet; see [Tested](#tested).

## Build

[.github/workflows/gpu-burn-in-recorder-image.yml](../../../.github/workflows/gpu-burn-in-recorder-image.yml)
tests the controller and publishes `ghcr.io/<repository owner>/gpu-burn-in-recorder` for
`linux/amd64` and `linux/arm64`. It runs on every push to `main` or `gpu-burn-in` that changes
`recorder/`, and on demand. Pull requests build the image without publishing it. It tags:

| Tag | Published for |
| --- | --- |
| `sha-<short sha>` | Every build |
| `main`, `gpu-burn-in` | The latest build of that branch |
| `<version>` and `latest` | A git tag `gpu-burn-in-recorder-v<version>`, for example `gpu-burn-in-recorder-v0.1.0` |

The first push creates the package as private. Make it public in the package's settings, or give the
chart an `imagePullSecret`.

To build locally:

```bash
cd community-stacks/gpu-burn-in/recorder
docker build -t <registry>/gpu-burn-in-recorder:dev .
```

The image is a static binary on `gcr.io/distroless/static:nonroot`, running as UID 65532 with a
read-only root filesystem.

## Run from a workstation

The controller runs anywhere it can reach the Platform cluster, which is useful for a dry run before
you install it:

```bash
cd community-stacks/gpu-burn-in/recorder
KUBECONFIG=<kubeconfig for the Platform cluster> go run . --dry-run \
  --target=bmh --local-cluster=loft-cluster --cluster-kubeconfig-dir=<dir> \
  --label-domain=gpu-qualification.example.com \
  --metrics-bind-address=0 --health-probe-bind-address=:18081
```

Here a kubeconfig that authenticates through the `vcluster` CLI works, because the CLI is on your
machine. `--cluster-kubeconfig-dir` holds one kubeconfig file per other cluster, named after the
cluster. Run `go run . --help` for every flag.

## Troubleshooting

- **Nothing recorded, no event.** The `report` task is not Healthy yet, the run sets
  `recordResult: false`, or the StackInstance's template is not in `templates`. With
  `logLevel: debug`, the controller logs every reconcile.
- **`NoMachine` warning.** No inventory Machine holds a NodeClaim of the run's tenant cluster.
  Either the server is not from an inventory provider, or the tenant cluster was deleted before the
  controller saw the finished run.
- **`RecordFailed ... Forbidden` on a Machine.** The Platform build does not let users label
  Machines. Use `target: bmh`.
- **`RecordFailed ... no kubeconfig directory configured` or `cluster <name>: open ...`.** The Metal3
  provider's `clusterRef` names a cluster with no kubeconfig in `clusterKubeconfigs.existingSecret`,
  and it is not `localCluster`.
- **`label domain ... ignores labels in vcluster.com and loft.sh domains`.** Choose another
  `labelDomain`.
- **The tenant cluster was not deleted.** Check that the run sets `releaseServer: true` and that
  `rbac.releaseServer` is on.

## Development

```bash
cd community-stacks/gpu-burn-in/recorder
gofmt -l . && go vet ./... && go test ./...
helm lint chart
```

The unit tests run the reconciler against a fake API server and a fake NetBox, covering:

- Both targets.
- Waiting for the report and for captured outputs.
- Per-run parameters against StackTemplate defaults.
- Dry run.
- NetBox tag handling.
- Recording each run only once.

`../test-manifests.sh` runs these and also lints and renders the chart.

The controller uses unstructured objects for Platform and Metal3 types, so it does not depend on a
particular Platform API module version.

## Tested

**2026-10-20, vCluster Platform 4.13.0-alpha.19:** run from a workstation against a live gpu-burn-in
run on an RTX 2000 Ada, with `target: bmh`:

- **Dry run:** within seconds of the report being captured, it found the right server. It went from
  NodeClaim `gpu-burn-in-rtx2000-qm92s` to Machine
  `metal3-us-va-blacksburg-dc1.rack-c-u12-xlarge-gpu`, then to BareMetalHost
  `metal3-system/rack-c-u12-xlarge-gpu` on `spark-cluster`, without an `assetID`.
- **For real:** it wrote `result=passed` and the annotations to that BareMetalHost, once.
- **Two more runs:** an RTX run with telemetry and an nvml-mock run with an injected uncorrectable
  ECC error. It recorded `passed` and `failed: inventory,soak` on their BareMetalHosts and, with
  `releaseServer: true`, deleted both tenant clusters.

Not tested yet:

- In-cluster from the image, including how the management API authorizes the ServiceAccount for the
  tenant cluster delete. The workstation runs used a Platform admin's credentials.
- The `machine` target on a Platform build that allows Machine labels.
- NetBox updates. The unit tests cover them.

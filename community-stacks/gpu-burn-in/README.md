# GPU Burn-In Community Stack

This Stack qualifies one GPU server before it joins tenant capacity. A temporary tenant cluster
claims the server as its only private node. The Stack installs the NVIDIA GPU Operator and runs
these steps in order:

1. **GPU ready.** Waits for the device plugin to advertise GPUs.
2. **Inventory.** `nvidia-smi` checks the GPU count, model, uncorrected ECC errors and NVLink link
   state. It also records the server's DMI identity.
3. **DCGM diagnostics.** `dcgmi diag` at level 1 to 4, plus optional named tests such as
   `nvbandwidth` and `pulse_test`.
4. **NCCL.** `nccl-tests` all-reduce, all-gather and reduce-scatter across every GPU, with
   correctness checks and an optional bandwidth floor.
5. **Soak.** Sustained DCGM `targeted_stress` and `targeted_power` load, for up to 24 hours. It
   samples temperature, power, XID errors and throttle reasons the whole time.
6. **Report.** Writes a structured pass or fail report to a ConfigMap and, optionally, to S3.
   The report is also published as Stack outputs.

The Stack reports Healthy only when the server qualified. When it does not, the Stack goes Degraded
and the message names the failed tests. Either way the report stays readable.
[automation/record-qualification.sh](automation/record-qualification.sh) then writes the result to
the server's inventory record: a label and annotations on its Platform Machine or BareMetalHost, and optionally NetBox.

Everything is built from existing vCluster Platform features: private nodes, Stacks, Argo CD and
Kubernetes Jobs. The tests run public NVIDIA images. The step logic is a small Helm chart in this
directory that Argo CD reads from Git.

> [!IMPORTANT]
> This Stack is not marked `vcluster.com/certified`, and vCluster Platform does not bundle it.

## Contents

- [Architecture](#architecture)
- [Qualify and quarantine servers](#qualify-and-quarantine-servers)
- [Requirements](#requirements)
- [Files](#files)
- [Configure parameters](#configure-parameters)
- [Install](#install)
- [Run a qualification](#run-a-qualification)
- [Read the report](#read-the-report)
- [Test on simulated GPUs (nvml-mock)](#test-on-simulated-gpus-nvml-mock)
- [Test on a single real GPU](#test-on-a-single-real-gpu)
- [Upgrade](#upgrade)
- [Remove](#remove)
- [Known limitations](#known-limitations)
- [Troubleshooting](#troubleshooting)
- [Test changes](#test-changes)

## Architecture

```text
gpu-operator -> gpuready -> inventory -> dcgmdiag -> nccl -> soak -> report -> verdict
                   |                                                     |
                   +-- output gpus: the GPU request of every test         +-- outputs: result, failedtests, ...
```

| Task | Runs | Image | Timeout | Fails the server when |
| --- | --- | --- | --- | --- |
| `gpu-operator` | NVIDIA GPU Operator, without DCGM or dcgm-exporter | NGC chart | `45m0s` | |
| `gpuready` | Waits up to 40 minutes for allocatable `nvidia.com/gpu` | `alpine/k8s` | `50m0s` | No GPU appears |
| `inventory` | `nvidia-smi -L`, `--query-gpu`, `topo -m`, `nvlink -s`, DMI | DCGM | `20m0s` | Count differs from `expectedGPUs`, wrong model, uncorrected ECC, inactive NVLink |
| `dcgmdiag` | `dcgmi diag -r <level> -j`, then `-r <extra tests>` | DCGM | `4h30m0s` | Any DCGM result is Fail; a Skip too with `failOnSkip` |
| `nccl` | `all_reduce`, `all_gather` and `reduce_scatter` with `-c 1` | NGC PyTorch | `1h30m0s` | Non-zero exit, wrong results, bus bandwidth below the floor |
| `soak` | DCGM `targeted_stress` and `targeted_power` cycles, `dcgmi dmon` every 5s | DCGM | `25h0m0s` | A DCGM Fail, any GPU above `maxGPUTempC`, an XID, or no load test actually ran |
| `report` | Collects the results, writes `gpu-burn-in-report`, uploads to S3 | `alpine/k8s` | `15m0s` | |
| `verdict` | Exits 1 unless the report says `passed` | `alpine/k8s` | `10m0s` | |

### Why Argo CD tasks

A Platform App deploy stops after 30 minutes. A DCGM level 4 run takes hours, and a soak takes as
long as you ask for. An `argoCDApplication` task waits as long as its own timeout. Argo CD reports
a running Job as Progressing, a completed Job as Healthy and a failed Job as Degraded. A Degraded
Application fails its task immediately, and the message names the failed resource.

Every step is the same chart, [chart/](chart/), deployed by the same template,
[apps/02-test.yaml](apps/02-test.yaml), with `test` choosing the step. The chart renders one Job per
step, plus its ServiceAccount, RBAC and scripts. The Job name carries a hash of its values. A Job
spec is immutable, so a changed setting creates a new Job, which runs the step again.

### How a test reports

Each GPU test pod has three containers:

| Container | Image | Does |
| --- | --- | --- |
| `gate` (init) | tools | With `stopOnFailure`, records a skip when an earlier step failed |
| `run` (init) | DCGM or NGC PyTorch | Runs `test-<step>.sh` under a deadline and keeps its raw output. Always exits 0 |
| `publish` | tools | Judges the output with [evaluate.sh](chart/files/evaluate.sh) and writes ConfigMap `gpu-burn-in-result-<step>` |

A result is `passed`, `failed` (the hardware missed the bar), `error` (the test could not run, for
example a timeout), `skipped` (an earlier step failed) or `disabled` (turned off by a parameter).
Every outcome still completes the Job, so a bad server always gets a report. Only `verdict` fails
its Job, after `report` has captured its outputs.

The NVIDIA images have no `kubectl` or `jq`, and the tools image has no NVIDIA tools. Splitting the
work between `run` and `publish` keeps the judging in one tested script and the NVIDIA images
unmodified.

### GPU requests

The GPU tests request the GPUs that `gpuready` observed, which the Stack passes as
`{{ .Outputs.gpuready.gpus }}`, not `expectedGPUs`. Suppose a server has a GPU that fell off the bus,
so the device plugin advertises 7 GPUs. A pod requesting 8 would stay Pending until the task timed
out, and there would be no report. Instead the tests run on 7, `inventory` fails with `7 of 8
GPUs`, and the report says why.

The tenant cluster has the server to itself, so the tests take every advertised GPU and run one at
a time.

### DCGM

The GPU Operator's own DCGM and dcgm-exporter are disabled. DCGM active diagnostics need the GPUs
to themselves. The exporter's profiling metrics also hold the profiling module the diagnostics use.
Each DCGM step starts its own `nv-hostengine` inside its pod, on the GPUs the pod was given. The
`run` container of `dcgmdiag` and `soak` adds `SYS_ADMIN`, as NVIDIA's DCGM container instructions
do.

## Qualify and quarantine servers

The Stack produces evidence. It does not decide which servers customers get. That needs two more
pieces, both outside the Stack.

**1. Record the result on the inventory.** The tenant cluster is temporary, so its Node object is
not a durable record. [automation/record-qualification.sh](automation/record-qualification.sh)
reads the report and writes it where the provisioning path looks:

- **The label:** `gpu-qualification.example.com/result=passed|failed`, plus annotations with the
  profile, time, failed tests, summary and report URI. Set `LABEL_DOMAIN` to a domain your
  organization owns. Never use a `vcluster.com` or `loft.sh` domain: newer Platform builds keep
  labels in those domains out of NodeType selection, and the script refuses them.
- **`--machine`, the vCluster Platform Machine:** the preferred target where it works. The Machine
  is provider-neutral, covering Metal3 and NICo, and lives on the management API, so the
  automation needs no credentials for the cluster that runs Metal3. NodeType selection merges
  Machine labels with the host's labels. This needs a Platform build that lets users label
  Machines. Earlier builds, including 4.13.0-alpha.19, reject every Machine write, so use `--bmh`
  there.
- **`--bmh`, the Metal3 BareMetalHost:** works on any build, and the Platform mirrors its labels to
  the Machine.
- **Either way the record survives the teardown.** When the tenant cluster is deleted, the Metal3
  provider removes only its own project and NodeClaim labels. The qualification label and
  annotations stay, even after the host is wiped and returned to `available`.
- **NetBox, optionally:**
  - On a failure, the script sets the device status to `failed` and removes the tag that brings
    the device into vCluster Platform.
  - On a pass, it adds a qualified tag.
  - Either way, it adds a journal entry with the summary.

It is a dry run unless you pass `--apply`.

**2. Enforce it.** A label alone does not stop allocation. Require it in the BareMetalHost selector
of every production NodeType, and point the qualification NodeType at hosts without it:

```yaml
nodeTypes:
  - name: rack-a-b300                 # production: qualified servers only
    bareMetalHosts:
      selector:
        matchLabels:
          netbox.vcluster.com/device-type: hgx-b300
          gpu-qualification.example.com/result: passed
  - name: rack-a-b300-qualification   # qualification: never-tested or failed servers
    bareMetalHosts:
      selector:
        matchLabels:
          netbox.vcluster.com/device-type: hgx-b300
        matchExpressions:
          - key: gpu-qualification.example.com/result
            operator: NotIn
            values: [passed]
    properties:
      vcluster.com/profile: b300-qualification
```

Verify that your node provider honors `matchExpressions` before you rely on the second selector.
If the NetBox import manages BareMetalHost labels, also check that it leaves this label alone.

The lifecycle is:

1. A new or repaired server is unlabeled, so only the qualification NodeType selects it.
2. Create a tenant cluster from **GPU Burn-In - Private Node** with `nodeTypeValue:
   b300-qualification`.
3. Wait for the Stack. Then run `record-qualification.sh --save-report <file> --apply`.
4. Delete the tenant cluster. The server is wiped and becomes `available`.
5. A passed server is now visible to production NodeTypes. A failed one stays out until it is
   repaired and passes again.

## Requirements

- vCluster Platform 4.12 or later. The Stack uses only APIs available in 4.12.
- vCluster 0.37 or later in the tenant cluster, for `deploy.stacks`. The template pins `0.37.2`.
- An Argo CD connector. You also need an Argo CD project that permits these sources and
  destinations:
  - The GPU Operator chart, `https://helm.ngc.nvidia.com/nvidia`.
  - This repository (or your fork) as a Git source.
  - The namespaces `gpu-operator` and `gpu-burn-in`.
- A node provider with the server, and a node type that selects only servers to qualify.
- Platform private nodes, with the VPN reachable by the server.
- A node OS and kernel the GPU Operator supports. Either the driver is preinstalled
  (`driverPreinstalled: true`), or the GPU Operator can pull and build it.
- Network access from the server to these sources:

  | Source | Used by |
  | --- | --- |
  | `nvcr.io` | GPU Operator images, `nvcr.io/nvidia/cloud-native/dcgm`, `nvcr.io/nvidia/pytorch` (about 10 GB) |
  | Docker Hub | `alpine/k8s`, the tools image |
  | The S3 endpoint | `report`, when `reportS3URI` is set |

## Files

| File | Purpose |
| --- | --- |
| [stacktemplate.yaml](stacktemplate.yaml) | StackTemplate `gpu-burn-in`: eight tasks, parameters and published outputs |
| [apps/01-gpu-operator.yaml](apps/01-gpu-operator.yaml) | ArgoCDApplicationTemplate `gpu-burn-in-gpu-operator` |
| [apps/02-test.yaml](apps/02-test.yaml) | ArgoCDApplicationTemplate `gpu-burn-in-test`: one step of the chart |
| [chart/](chart/) | The step chart: Job, RBAC and scripts. `files/evaluate.sh` holds every pass or fail rule |
| [virtualclustertemplate.yaml](virtualclustertemplate.yaml) | VirtualClusterTemplate `gpu-burn-in-private-nodes`: one server, the Argo CD integration and the Stack |
| [example/stackinstance.yaml](example/stackinstance.yaml) | StackInstance with an HGX B300 starting profile |
| [example/stackinstance-nvml-mock.yaml](example/stackinstance-nvml-mock.yaml) | StackInstance for simulated GPUs |
| [example/virtualclusterinstance-single-gpu.yaml](example/virtualclusterinstance-single-gpu.yaml) | Tenant cluster qualifying one real workstation GPU |
| [automation/record-qualification.sh](automation/record-qualification.sh) | Writes a report to a Machine, a BareMetalHost and NetBox |
| [tests/evaluate-test.sh](tests/evaluate-test.sh) | Unit tests of the pass or fail rules, against [tests/fixtures/](tests/fixtures/) |
| [tests/nvml-mock-e2e.sh](tests/nvml-mock-e2e.sh) | End-to-end scenarios with injected faults on nvml-mock |
| [test-manifests.sh](test-manifests.sh) | Static and render checks, then the unit tests |

Every Platform object carries the label `app.kubernetes.io/part-of: gpu-burn-in-stack`.

## Configure parameters

Set these on the tenant cluster template, which passes the Stack parameters through
`deploy.stacks`. The tenant cluster template has every Stack parameter, plus `kubernetesVersion`,
`argoConnector`, `gpuNodeProvider`, `nodeTypeProperty` and `nodeTypeValue`.

| Parameter | Default | Purpose |
| --- | --- | --- |
| `expectedGPUs` | `8` | GPUs the server must have |
| `gpuModel` | empty | Substring every GPU name must contain, for example `B300` |
| `assetID` | empty | The server's inventory name, recorded in the report |
| `profile` | `default` | Name for this set of thresholds, for example `b300-burnin-v1` |
| `stopOnFailure` | `true` | Skip the remaining tests after a failure |
| `runDCGMDiag` | `true` | Run `dcgmdiag`. Needs CUDA |
| `dcgmDiagLevel` | `3` | 1 quick, 2 medium, 3 long (about 30 minutes), 4 extended (hours) |
| `dcgmExtraTests` | empty | Named DCGM tests after the level, for example `nvbandwidth,pulse_test` |
| `failOnSkip` | `false` | Fail when DCGM skips a subtest |
| `runNCCL` | `true` | Run `nccl`. Needs CUDA |
| `ncclMaxBytes` | `1G` | Largest NCCL message size |
| `ncclMinBusBandwidthGBps` | `0` | Bus bandwidth floor per collective. `0` records without judging |
| `soakMinutes` | `60` | Soak length, 0 to 1440. `0` disables the soak |
| `soakLoad` | `dcgm` | `dcgm` loads the GPUs; `none` only records telemetry |
| `maxGPUTempC` | `90` | The soak fails above this |
| `reportS3URI`, `s3CredentialsSecret`, `s3Endpoint` | empty | Optional S3 copy of the report |
| `driverPreinstalled` | `false` | `true` when the node image has the NVIDIA driver |
| `argoProject`, `chartRepoURL`, `chartRevision` | `default`, this repository, `main` | Where Argo CD reads the step chart |
| `gpuOperatorVersion`, `toolsImage`, `dcgmImage`, `ncclImage` | see [stacktemplate.yaml](stacktemplate.yaml) | Versions. Renovate tracks them |

Notes:

- **Thresholds come from known-good hardware.** `ncclMinBusBandwidthGBps` and `maxGPUTempC` have
  no universal right value. Run the Stack on servers you trust, record their numbers, and set the
  profile from those.
- **The DCGM image must match the driver.** DCGM 4.5 supports the 535, 570, 580 and 590 driver
  branches. The NGC PyTorch image needs a driver its CUDA version supports.
- **`soakMinutes` tops out at 1440.** The soak task's timeout is 25 hours, and the timeout is not
  a parameter.
- **Ownership.** The templates are owned by the `loft-admins` team. Change `spec.owner` if your
  installation uses another team.

## Install

All commands below go to the Platform **management API**. Get a kubeconfig with
`vcluster platform connect management`.

```bash
kubectl apply -f community-stacks/gpu-burn-in/apps/ \
  -f community-stacks/gpu-burn-in/stacktemplate.yaml \
  -f community-stacks/gpu-burn-in/virtualclustertemplate.yaml
```

Apply the templates before creating a tenant cluster, never during a run. A task renders its
template when it starts, so a mid-run change reaches only the tasks that have not started yet.

To test changes to the chart, push them to a fork and set `chartRepoURL` and `chartRevision`. Argo
CD reads the chart from Git, not from your checkout.

## Run a qualification

Create a tenant cluster from **GPU Burn-In - Private Node** under **Tenant Clusters > New Tenant
Cluster**, and select the server with `nodeTypeProperty` and `nodeTypeValue`. Or apply a
VirtualClusterInstance like
[example/virtualclusterinstance-single-gpu.yaml](example/virtualclusterinstance-single-gpu.yaml).

To run the Stack in an existing tenant cluster, apply a StackInstance in the project namespace,
such as [example/stackinstance.yaml](example/stackinstance.yaml). The tenant cluster needs the
Argo CD integration and exactly one GPU node, with nothing else using its GPUs and no other GPU
Operator installed.

Follow the tasks on the management API:

```bash
kubectl -n <project-namespace> get stackinstance <name> \
  -o jsonpath='{.status.phase}{"\n"}{range .status.tasks[*]}{.name}{"\t"}{.phase}{"\t"}{.message}{"\n"}{end}'
```

With the tenant cluster kubeconfig, follow a step:

```bash
kubectl -n gpu-burn-in get jobs,pods
kubectl -n gpu-burn-in logs -l app.kubernetes.io/component=soak -c run -f
kubectl -n gpu-burn-in get configmaps -l gpu-burn-in.vcluster.com/result=true \
  -o custom-columns=TEST:.data.test,STATUS:.data.status,SUMMARY:.data.summary
```

## Read the report

The StackTemplate publishes `result`, `failedtests`, `summary`, `host`, `assetid`, `gpucount`,
`reporturi` and `finishedat`. Platform builds that serve the `stackinstances/outputs`
subresource return them from the StackInstance:

```bash
kubectl get --raw "/apis/management.loft.sh/v1/namespaces/<project-namespace>/stackinstances/<name>/outputs"
```

The full report is in the tenant cluster:

```bash
kubectl -n gpu-burn-in get configmap gpu-burn-in-report -o jsonpath='{.data.report\.json}' | jq
```

```json
{
  "schema": "gpu-burn-in.vcluster.com/v1",
  "result": "failed",
  "failedTests": ["nccl", "soak"],
  "summary": "nccl failed: all_gather 98.12 GB/s below 300; soak skipped: an earlier test failed: nccl",
  "assetID": "b300-rack-a-u12",
  "profile": "b300-burnin-v1",
  "host": {"name": "...", "labels": {"...": "..."}, "dmi": {"product_serial": "..."}},
  "expectedGPUs": 8,
  "detectedGPUs": 8,
  "gpus": [{"index": 0, "uuid": "GPU-...", "name": "NVIDIA B300 ...", "serial": "...", "uncorrectedECC": 0}],
  "tests": [{"test": "inventory", "status": "passed", "summary": "8 x NVIDIA B300 ...", "details": {}}]
}
```

Each test's `details` holds its measurements. These include the DCGM results by test name, NCCL bus
bandwidth per collective, and per-GPU peak temperature, peak power, last XID and throttle samples
from the soak. Each result ConfigMap also keeps the last 256 KiB of its test's log.

## Test on simulated GPUs (nvml-mock)

NVIDIA's [nvml-mock](https://github.com/NVIDIA/k8s-test-infra) simulates the driver on a node:
`nvidia-smi`, NVML, the device plugin and DCGM telemetry, for any GPU count and model. It has no
CUDA, so only part of the Stack can run on it.

| Step | On nvml-mock | Why |
| --- | --- | --- |
| `gpu-operator`, `gpuready` | Runs | The device plugin advertises the simulated GPUs. Driver and toolkit are skipped by node label |
| `inventory` | Runs | `nvidia-smi` reports count, model and ECC from the simulation |
| `dcgmdiag` | Disable (`runDCGMDiag: false`) | Needs CUDA |
| `nccl` | Disable (`runNCCL: false`) | Needs CUDA |
| `soak` | Telemetry only (`soakLoad: none`) | `dcgmi dmon` reads the simulation; there is no load to apply |
| `report`, `verdict`, outputs | Runs | |

That covers the task graph, the `gpuready` output and GPU requests, stop-on-failure, the soak's
thermal and XID checks, the report and its outputs, and the verdict. Use
[example/stackinstance-nvml-mock.yaml](example/stackinstance-nvml-mock.yaml), or run the scenarios
with injected faults:

```bash
community-stacks/gpu-burn-in/tests/nvml-mock-e2e.sh --project default \
  --provider <metal3-provider> --argo-connector <connector> \
  --node-type-property vcluster.com/accelerator --node-type-value b200 \
  --chart-repo https://github.com/<fork>/vcluster-stacks.git --chart-revision <branch>
```

| Scenario | Fault | Expected |
| --- | --- | --- |
| `pass` | none | `passed`, Stack Healthy |
| `missing-gpu` | `expectedGPUs` one more than the server has | `failed: inventory,soak` (soak skipped by stop-on-failure) |
| `wrong-model` | `gpuModel: H100` on B200 simulations | `failed: inventory,soak` |
| `thermal` | `nvml-mock-ctl temp --gpu 0 142` during the soak | `failed: soak`, above `maxGPUTempC` |
| `xid` | `nvml-mock-ctl fail --gpu 0 --mode ecc_uncorrectable --xid 48` during the soak | `failed: soak`, Xid 48 |

The thermal path matches one verified with NVSentinel on nvml-mock, where DCGM reported the spike
capped at 95 C. Whether DCGM's last-XID field reflects an nvml-mock Xid has not been verified.

## Test on a single real GPU

A single workstation GPU, such as an RTX 2000 Ada, runs every step with real CUDA:

- `nccl` runs with one rank. That proves the image, CUDA and NCCL work, but the bandwidth is not
  meaningful.
- DCGM diagnostics target data center GPUs, so some tests may report Skip. The report records
  every skip. The soak fails if DCGM skips every load test, because then it never loaded the GPU.
- There is no NVLink to check.

See [example/virtualclusterinstance-single-gpu.yaml](example/virtualclusterinstance-single-gpu.yaml).

## Upgrade

- **Versions.** Change `gpuOperatorVersion`, `dcgmImage`, `ncclImage` or `toolsImage`, or update
  the defaults. Renovate tracks the defaults in this directory.
- **The step chart.** Argo CD reads `chartRevision`. Pin it to a tag or commit for reproducible
  qualifications, and record the revision in `profile`.
- **Template parameters on existing tenant clusters.** These tenant clusters are meant to be
  short-lived. If you keep one, a new template parameter reaches it only after **Sync Template**.

## Remove

Delete the tenant cluster when its result is recorded. That removes the Stack and releases the
server to its node provider, which wipes it. To remove the Stack from a tenant cluster you keep:

```bash
kubectl -n <project-namespace> delete stackinstance <name>
```

Argo CD prunes the GPU Operator and every step's Job and RBAC. These remain in the tenant cluster:

- The namespaces `gpu-operator` and `gpu-burn-in`.
- The result ConfigMaps and `gpu-burn-in-report`. The steps create them, not Argo CD, so the
  evidence outlives the Stack.

Remove the templates when nothing references them:

```bash
kubectl delete argocdapplicationtemplates,stacktemplates,virtualclustertemplates \
  -l app.kubernetes.io/part-of=gpu-burn-in-stack
```

## Known limitations

- **One server per tenant cluster.** Multi-node qualification (inter-node NCCL over RDMA, fabric
  tests, or NVIDIA Cluster Readiness Engine) is not covered. An NVL72 rack's NVLink domain spans
  trays, so per-tray qualification cannot validate it.
- **It starts from a booted OS.** A fault that stops the kernel, kubelet or driver from coming up
  shows up as a `gpuready` failure or a tenant cluster that never gets a node. It is not diagnosed
  further. Out-of-band checks such as BMC, Redfish and power-on self-test are outside the Stack.
- **Re-running in the same tenant cluster.** The stop-on-failure gate reads every result
  ConfigMap in the namespace. Use a fresh tenant cluster per qualification, or delete the
  `gpu-burn-in-result-*` ConfigMaps first.
- **Thresholds are not built in.** The defaults catch broken hardware. Telling a marginal server
  from a good one needs baselines from known-good servers of the same model.
- **Telemetry is coarse.** `dcgmi dmon` samples every 5 seconds. The soak judges peak temperature
  and the last XID. Throttle samples are recorded but do not fail the soak, because power capping
  under full load is normal.
- **Privilege.** `dcgmdiag` and `soak` add `SYS_ADMIN` to their test container. The tenant
  cluster exists only for the qualification.
- **Enforcement is yours.** The Stack and the recording script produce a result and a label. Only a
  NodeType selector, or an equivalent gate in your provisioning path, keeps a failed server away
  from customers.

## Troubleshooting

- **`gpuready` failed: no `nvidia.com/gpu` advertised.** The driver, toolkit or device plugin did
  not come up. Check `kubectl -n gpu-operator get pods` and the driver pod's log. That is a
  finding about the server and its image, and the report records it.
- **A test is `error`, not `failed`.** The test could not run: a timeout, a host engine that did
  not start, or a missing binary. Read the `run` container log, or the `log` key of its result
  ConfigMap.
- **`dcgmdiag` reports no JSON.** The DCGM version may not support the GPU or the requested level.
  Check `dcgmi diag` output in the log, and whether the DCGM image supports the driver branch.
- **`nccl` exits non-zero right away.** The PyTorch image's CUDA version needs a newer driver, or
  `/usr/local/bin` has no nccl-tests binaries in the image you chose.
- **A task waits with `CapturingOutputs`.** The step's ConfigMap is missing a key the output reads.
  This usually means the task rendered from a stale template. See [Install](#install).
- **`verdict` Degraded.** This is the expected outcome for a server that did not qualify. Its
  message lists the failed tests, and the report has the details.
- **Argo CD `ComparisonError` on a step.** The Argo CD project does not permit `chartRepoURL`, or
  the revision does not exist in that repository.

## Test changes

From the repository root, run the static checks. They need bash 4 or later, python3 with PyYAML,
helm, jq and sh.

```bash
./community-stacks/gpu-burn-in/test-manifests.sh
```

The checks render every task for the default parameters, the nvml-mock parameters and a variant
with the soak off and S3 on. They render each StackTemplate task, then its ArgoCDApplicationTemplate, then
the chart with the resulting Helm values. They assert:

- Names carry the `gpu-burn-in` prefix and the `part-of` label, and match their files. Every chart
  value is a template parameter, and defaults agree across the StackTemplate, the templates and
  the chart.
- The tasks form one chain in the expected order, all as Argo CD tasks. Timeouts are in canonical
  Go duration form, and each exceeds its step's own deadline by 5 minutes, up to a maximum-length
  soak.
- Outputs come from the `gpu-burn-in` namespace, from keys the steps write. Every `.Outputs`
  reference reads a declared output of a dependency.
- Each step renders exactly one Job that runs once. GPU tests request the `gpuready` count rather
  than `expectedGPUs`. Disabled tests start no NVIDIA container. The GPU Operator keeps DCGM and
  dcgm-exporter off. `verdict` receives the report's outputs.
- The tenant cluster template claims exactly one server and passes every Stack parameter.

They then run [tests/evaluate-test.sh](tests/evaluate-test.sh). It runs the pass or fail rules
against fixtures for each test and failure mode, and the report against combinations of results.
The fixtures are hand-written in the formats `nvidia-smi`, `dcgmi diag -j`, nccl-tests and
`dcgmi dmon` print. Replace them with captured output when a real run is available.

CI also runs a server-side dry run of every manifest against vCluster Platform.

### Tested

Not yet run on a live cluster. The static checks and the unit tests pass. These have not been
exercised:

- Any step on real or simulated GPUs, including the DCGM 4.5 JSON layout the fixtures assume.
- The `nvml-mock-e2e.sh` scenarios.
- `record-qualification.sh --apply` against a Machine, a BareMetalHost and NetBox.
- The S3 upload.

## License

This Stack is part of this repository and is licensed under the
[Apache License 2.0](../../LICENSE).

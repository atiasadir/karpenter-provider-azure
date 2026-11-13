# Experimental Flex functional evidence

**Same-region and cross-region functional gates passed, with a policy limitation.**
These were bounded live experiments, not the complete repository E2E suite or a
production-support/security qualification. They require a separate AKS cluster
created with `networkPlugin=none`; they do not establish managed Azure CNI or
Cilium compatibility.

## Observed results

The experiments ran on Kubernetes 1.35.9 with two fixed system nodes and at most
one dynamic on-demand `Standard_D4ads_v5` worker (four vCPUs, no GPUs).
The Flex NodeClass and logical pool both used `maxPods=250`.

| Gate | Actual placement | Result |
|---|---|---|
| Native regression, Flex disabled | Primary worker subnet, East US 2 | Ready, workload/network checks, automatic cleanup |
| Flex, second VNet | East US 2, privately peered secondary VNet | Functional PASS with policy limitation |
| Flex, third VNet | Central US zone 2, globally peered to the East US 2 VNets | Functional PASS with the same policy limitation |

Same-region source: `656f10d8cc9d1cf888533682c16ff1e1693cd22a`.
Cross-region source: `8c25e3cc5d750f664b46d77881f924ed1bde12b6`.
The Linux/amd64 controller images were built from committed archives using the
repository's `.ko.yaml` and `hack/go-crossbuild.sh`, not a directory containing
local credentials. The host image was
`Canonical:ubuntu-24_04-lts:server:24.04.202610250`.

For the remote worker, ARM reported the VM and NIC in Central US and the enrolled
Machine as `Succeeded`. The canonical backing VM provider ID exactly matched
both `Node.spec.providerID` and `NodeClaim.status.providerID`. Node and NodeClaim
region/zone labels matched ARM placement. `Launched`, `Registered`, `Initialized`
and `Ready` were all true; the Node received a pod CIDR from its remote Site.

Ordinary non-hostNetwork pods on a fixed system node and the external worker
passed both directions of direct-pod HTTP, ClusterIP HTTP, Service DNS HTTP,
cluster DNS resolution and peer-node kubelet TCP connectivity. Logs and exec
worked. Deleting the synthetic worker Pod triggered `WhenEmpty` consolidation;
subsequent ARM VM, NIC and Machine lists and Kubernetes NodeClaim lists were empty.
The worker Node disappeared, while two fixed system Nodes stayed Ready.
All synthetic probe Pods and Services were removed. Both dynamic pools were left
with CPU limits of zero; shared pools, Sites, VNets, peerings and identities remained.

**NetworkPolicy was not enforced in the tested Unbounded-only stack.** A matching
deny-all ingress policy was accepted, but a fresh connection after 30 seconds
still returned HTTP 200. The source was a normal non-hostNetwork pod; source and
target were in the same namespace, the selector matched the target, and no other
selecting policy granted ingress. The synthetic policy was removed after its
workload. No policy engine was installed or substituted. The remote gate retained
this limitation rather than reporting policy isolation as PASS.

## Reproduction contract

Follow the [Flex prerequisites and NodeClass configuration](../README.md#experimental-external-vnet-workers).
Do not convert an existing managed-CNI cluster: the AKS service returned
`InvalidParameter: Flex node pools require networkProfile.networkPlugin to be 'none'.`
An independent stock native second-VNet probe on managed Overlay/Cilium also
failed to obtain the secondary VNet's overlay registration and never became Ready.

Use the pinned [AKS Flex Node v0.3.0 source](https://github.com/Azure/AKSFlexNode/tree/4741b41e4feaaca86d0dd53014fd4a141731ea34)
and [Unbounded v0.21.0 source](https://github.com/Azure/unbounded/tree/2854ad4ddd6c05b567719fe7bea00086a32d9305).
Register `AKSFlexNodePreview` and `PutMachinePreview` and refresh the resource
provider before creating the logical `FlexNodes`/`User` pool. Keep managed NAP
manual and cluster autoscaler disabled for the isolated experiment.

Precreate disjoint private VNets/subnets, NSGs, explicit egress and bidirectional
peering, including global peering for the remote VNet. Add each egress address to
the API allowlist without removing existing entries. VNet peering supplies only
the underlay, not pod or Service routing. Allocate disjoint Site pod ranges inside
an explicit covering ARM `podCidr`, separate from service and VNet ranges, as
required by [AKS BYO-CNI documentation](https://learn.microsoft.com/azure/aks/use-byo-cni).

Use these immutable Unbounded image references:

```text
ghcr.io/azure/unbounded-operator:v0.21.0@sha256:fc0132897ddbdfc9f3a0287f6d869d2ae577da553921bd6433ce31a7f70bd5fc
ghcr.io/azure/unbounded-net-controller:v0.21.0@sha256:fe20a54ecdf98344f383120de8a4d70c4cd55a20e79c1c96280bf38848e24ab0
ghcr.io/azure/unbounded-net-node:v0.21.0@sha256:9a64041b5a839141301fd8a28e8094e9d134b33c091177a8304851744e4e4fdd
```

The v0.21.0 `kubectl-unbounded install` command accepts a dedicated `--kubeconfig`,
`--namespace unbounded-system`, `--image-registry ghcr.io/azure`, the pinned
`--operator-image` and an explicit `--api-server-endpoint`. For system nodes with
`CriticalAddonsOnly=true:NoSchedule`, install with `--wait=false` and append that
exact toleration to the operator if absent, preserving its NotReady toleration.
Do not remove the node taint. The pinned net-controller already tolerates it and
NotReady; net-node tolerates all taints. Pin both the net-node main and CNI installer
containers through the documented workload overrides before applying Sites.

Each Site uses `unbounded-cloud.io/v1alpha3` with `spec.nodeCidrs` for membership,
`manageCniPlugin: true`, and a `podCidrAssignments` entry containing `cidrBlocks`,
`assignmentEnabled: true`, and `nodeBlockSizes.ipv4: 24`.
Disable `machina`, `metalman`, `gantry` and `tokenRefresher` in `spec.components`.
Scheduling labels are not Site selectors. Create Sites before admitting the
`net.unbounded-cloud.io/v1alpha1` SitePeering, whose spec contains
`enabled: true`, the Site names in `sites`, `meshNodes: true` and
`tunnelProtocol: Auto`. Do not create GatewayPool assignments for this private mesh.

Preserve the controller's home `LOCATION`. Enable the selected remote location
in `FLEX_LOCATIONS` and set `spec.flex.location` on only the remote NodeClass.
Verify actual SKU/image availability, quota, policy and ARM prerequisites first.
The tested controller used the purpose-built Machine Manager plus Reader roles
at the exact Flex pool scope, not pool Contributor or subscription-wide writes.

Keep all other dynamic pools closed. Start one small pending synthetic Pod with
an exact pool selector and one four-vCPU eligible SKU; close the pool CPU limit
to zero immediately after the first NodeClaim appears. NodePool limits alone are
not an atomic allocation cap. Verify real enrollment, placement, labels, traffic
and cleanup before opening the next gate. A Ready NodeClass or passing unit test
does not establish these live guarantees.

# High-Level Requirements & Design: NVIDIA vGPU Support in DRA Driver for NVIDIA GPUs

| Field | Value |
| --- | --- |
| **Status** | Draft (design proposal) |
| **Target project** | [kubernetes-sigs/dra-driver-nvidia-gpu](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu) |
| **Driver** | `gpu.nvidia.com` (GPU kubelet plugin) |
| **Resource model** | **Partitionable devices (KEP-4815)** — SharedCounters per PF + vGPU partition devices with `consumesCounters` |
| **Primary consumers** | KubeVirt VMs (via DRA / `GPUsWithDRA`), future VM runtimes that consume MDEV/VFIO vGPU |
| **Related work** | Dynamic MIG (same partitionable substrate), VFIO passthrough (`PassthroughSupport`), Device Metadata |
| **Lifecycle reference** | `kubevirt-dynamic-gpu-device-plugin` (mdev/vdev create/destroy, SR-IOV, MIG-backed vGPU, bin packing) |
| **Cluster prerequisite** | Kubernetes with `DRAPartitionableDevices` available (see version matrix below) |

---

## 1. Motivation

The DRA Driver for NVIDIA GPUs already exposes three device kinds under one driver (`gpu.nvidia.com`):

| `type` | DeviceClass | Workload model |
| --- | --- | --- |
| `gpu` | `gpu.nvidia.com` | Full GPU to containers (optional CUDA time-slicing / MPS / consumable shares) |
| `mig` | `mig.nvidia.com` | Hardware MIG slice to containers (static discovery **or** Dynamic MIG via partitionable devices) |
| `vfio` | `vfio.gpu.nvidia.com` | Full-GPU VFIO passthrough (KubeVirt / raw VFIO) |

**NVIDIA vGPU** (mediated / vendor-VFIO virtual GPUs backed by the host NVIDIA vGPU Manager) is not represented today. Container time-slicing and MPS are **not** substitutes for vGPU:

| | CUDA time-slicing / MPS | NVIDIA vGPU |
| --- | --- | --- |
| Isolation | Soft (CUDA-level) | Hypervisor + vGPU Manager mediated device |
| Consumer | Containers with NVIDIA container runtime | VMs (KubeVirt virt-launcher) needing MDEV UUID or VFIO-bound VF |
| Host driver | Remains `nvidia` for containers | Remains `nvidia` on PF; creates mdev or sets VF `current_vgpu_type` |
| Profiles | N/A | Named profiles with fixed max instances / FB; **overlapping layouts** on one PF |

Without DRA vGPU support, KubeVirt multi-tenant GPU density still depends on the older **Device Plugin** path (`kubevirt-dynamic-gpu-device-plugin`), while full-GPU passthrough and Dynamic MIG are moving to this DRA driver.

### Why partitionable devices

A single PF can typically host **exactly one vGPU profile family at a time**, with `maxInstances` slots, and optionally **MIG-backed** placements. Those layouts **overlap**: advertising both `12Q×4` and `24Q×2` without a shared budget lets the scheduler double-book the PF.

That is the same problem **Dynamic MIG** already solves with **KEP-4815 partitionable devices**:

1. Publish a **CounterSet per PF** (framebuffer, instance slots, MIG memory slices, …).
2. Advertise every **possible vGPU partition** as a device with `consumesCounters`.
3. Scheduler allocates only combinations that fit remaining counters.
4. **NodePrepare** creates the concrete mdev/vdev (and MIG GI/CI if required).

Adopting the same substrate keeps one mental model for “carve the GPU,” reuses `partitions.go` / ResourceSlice publishing patterns, and gives correct multi-profile packing **before** bind—without reimplementing device-plugin `GetPreferredAllocation`.

---

## 2. Goals

1. Advertise vGPU via **partitionable devices**: SharedCounters + partition `devices[]` under driver `gpu.nvidia.com`.
2. Ship DeviceClass `vgpu.gpu.nvidia.com` selecting `type == vgpu` partitions.
3. Allow optional opaque **`VgpuDeviceConfig`** for Prepare-time knobs (params, typeID overrides); profile **identity is primarily carried by the allocated partition device**.
4. On **NodePrepareResources**, create the concrete vGPU (mdev and/or vendor-VFIO path), optional MIG GI/CI, CDI + Device Metadata for KubeVirt.
5. On **NodeUnprepareResources**, destroy vGPU (then MIG if owned), update checkpoint; counters naturally free as claims release.
6. Express **exclusivity** with full `gpu` and `vfio` siblings by having those devices **consume the entire PF CounterSet** (preferred) and/or sibling republish fallback.
7. Achieve **bin packing / multi-profile safety in the scheduler** via counters—not post-hoc preferred allocation.
8. Feature gate **`VGPUSupport`** (Alpha, default false); require cluster **`DRAPartitionableDevices`** (document version matrix).
9. Align implementation with Dynamic MIG publishing (separate counters vs devices slices on k8s ≥ 1.35).
10. Docs, demos, e2e on real vGPU Manager hardware.

## 3. Non-goals (initial releases)

1. Replacing NVIDIA vGPU **license** infrastructure or guest driver install.
2. CUDA **container** workloads on created mdev/vdev (VM/passthrough consumers first).
3. Full parity with every dynamic DP knob in v1 (PVMRL/power/clock as follow-ons).
4. Cross-node live migration of vGPU (beyond stable metadata).
5. Changing ComputeDomain / IMEX.
6. Co-installing classic GPU device plugin on the same GPUs (same restriction as VFIO guide).
7. Advertising **live mdev UUIDs** as stable ResourceSlice device names (UUIDs are Prepare artifacts).

---

## 4. Actors and use cases

### 4.1 Primary (must)

| ID | Use case | Partitionable role |
| --- | --- | --- |
| UC-1 | Multi-instance vGPU for KubeVirt | N slot devices per profile; each consumes instance + FB counters |
| UC-2 | Coexist with VFIO / full GPU | `gpu` / `vfio` devices consume **all** PF counters → exclusive |
| UC-3 | mdev framework | Prepare creates mdev for allocated partition |
| UC-4 | vdev framework | Prepare sets VF `current_vgpu_type` for allocated partition |
| UC-5 | Crash-safe lifecycle | Checkpoint concrete ids; counters driven by claim allocation |
| UC-6 | KubeVirt attachment | CDI + DeviceMetadata from Prepare |
| UC-7 | Multi-profile on one PF | Overlapping partition devices; counters prevent invalid mixes |

### 4.2 Secondary (should)

| ID | Use case | Partitionable role |
| --- | --- | --- |
| UC-8 | SR-IOV-backed vGPU | Attrs + Prepare VF enable; counters still on PF |
| UC-9 | MIG-backed vGPU | Counters include memory slices / GI costs (like Dynamic MIG) |
| UC-10 | Packing density | Scheduler spends counters on already-partial PFs when possible |
| UC-11 | Host `vgpu_params` / scheduler knobs | Opaque config at Prepare |
| UC-12 | Admin profile catalog | Drives which partition devices are enumerated |

### 4.3 Out of scope personas

- Pure container CUDA sharing → `gpu` + TimeSlicing/MPS/ConsumableShares.
- Full PF to VM without vGPU Manager → `vfio` DeviceClass.

---

## 5. Requirements

### 5.1 Functional

| ID | Requirement | Priority |
| --- | --- | --- |
| FR-1 | Publish **SharedCounters** (one CounterSet per vGPU-capable PF) in the node resource pool. | P0 |
| FR-2 | Publish **vGPU partition devices** (`type=vgpu`) each with `consumesCounters` against that PF CounterSet. | P0 |
| FR-3 | Optionally publish `gpu` / `vfio` devices that consume the **full** CounterSet for exclusivity (when those features enabled). | P0 |
| FR-4 | Use pool layout compatible with KEP-4815: on k8s ≥ 1.35, **separate** ResourceSlices for counters vs devices (reuse Dynamic MIG publisher). | P0 |
| FR-5 | DeviceClass `vgpu.gpu.nvidia.com` selects `type == vgpu`. | P0 |
| FR-6 | Opaque `VgpuDeviceConfig` supported and webhook-validated (optional profile override / params); identity defaults from allocated device attrs. | P0 |
| FR-7 | Prepare creates exactly one concrete vGPU per allocated partition device; Unprepare destroys it. | P0 |
| FR-8 | Detect mdev vs vdev framework; branch host ops. | P0 |
| FR-9 | CDI + DeviceMetadata for KubeVirt. | P0 |
| FR-10 | Checkpoint `PreparedVgpuDevice` (mdev UUID or VF PCI, profile, optional MIG GI id). | P0 |
| FR-11 | Feature gate `VGPUSupport` (Alpha, default false). | P0 |
| FR-12 | Fail fast / clear errors if cluster lacks partitionable-device support when gate on. | P0 |
| FR-13 | MIG-backed partitions: consume GI placement counters; Prepare = CreateMigInstance then vGPU (atomic). | P1 |
| FR-14 | SR-IOV enable/slice in Prepare when needed; unsliced PF required for whole-PF VFIO (existing check). | P1 |
| FR-15 | Metrics for prepare/unprepare and active partitions per PF. | P2 |
| FR-16 | Helm DeviceClass, demos, site docs (concept + KubeVirt guide + ResourceSlice attributes). | P0 |

### 5.2 Non-functional

| ID | Requirement |
| --- | --- |
| NFR-1 | Bound Prepare/Unprepare timeouts (align with VFIO/Dynamic MIG). |
| NFR-2 | `VGPUSupport=false` → no vGPU partitions/counters; no regression to gpu/mig/vfio. |
| NFR-3 | API additive (`resource.nvidia.com/v1beta1`). |
| NFR-4 | Counter model must match vGPU Manager rules (wrong counters ⇒ scheduler oversubscribe). |
| NFR-5 | Bound ResourceSlice size (profile × slots × GPUs); document scaling limits / aggregation strategies. |
| NFR-6 | Checkpoint JSON: new fields `omitempty` (schema stability). |

### 5.3 Compatibility

| ID | Requirement |
| --- | --- |
| CR-1 | Host **vGPU Manager** (KVM); PFs on `nvidia` with supported types. |
| CR-2 | Kubernetes with DRA + **`DRAPartitionableDevices`** (see §6.1.1 version matrix). |
| CR-3 | KubeVirt with `GPUsWithDRA` (+ agreed metadata for mdev/vdev). |
| CR-4 | IOMMU as required by vdev/VFIO path. |
| CR-5 | No classic GPU DP on same GPUs. |
| CR-6 | Optional `sriov-manage` on SR-IOV nodes. |

---

## 6. Design

### 6.1 Architecture placement

vGPU is a **fourth resource kind** on `gpu-kubelet-plugin`, published with the **same partitionable-devices pipeline** Dynamic MIG uses:

```text
Pod / VMI
  → ResourceClaim (DeviceClass vgpu.gpu.nvidia.com ± VgpuDeviceConfig)
  → webhook validate
  → scheduler selects a vGPU partition device whose consumesCounters fit SharedCounters
  → kubelet NodePrepareResources
       → create MIG GI/CI if partition is MIG-backed
       → create mdev OR set VF vGPU type
       → CDI + DeviceMetadata
  → virt-launcher attaches device to QEMU
  → Unprepare: destroy vGPU then MIG; checkpoint clear
```

| Area | Change |
| --- | --- |
| `api/.../v1beta1` | `VgpuDeviceConfig` |
| `pkg/featuregates` | `VGPUSupport` (+ validation vs other gates) |
| `cmd/gpu-kubelet-plugin/partitions.go` (or sibling) | vGPU CounterSet + `consumesCounters` helpers |
| `driver.go` | Include vGPU counter sets + partition devices in KEP-4815 publish path |
| `deviceinfo.go` / new `vgpu*.go` | `VgpuPartitionInfo`, discovery, host ops |
| `device_state.go` | Prepare/Unprepare / applyConfig |
| CDI | `vgpu-cdi.go` |
| Helm / webhook / site / demo | DeviceClass, validation, docs |

### 6.1.1 Cluster version matrix

| Kubernetes | Partitionable devices | Driver publish mode |
| --- | --- | --- |
| 1.34–1.35 | Enable `DRAPartitionableDevices` on apiserver + scheduler | Follow existing Dynamic MIG branch (combined vs split slices) |
| ≥ 1.36 | Typically default/beta per upstream | **Separate** ResourceSlices: counters vs devices (driver already detects ≥ 1.35) |

`VGPUSupport` MUST document and ideally **detect** missing scheduler support (fail plugin start or mark unhealthy with explicit log/metric).

### 6.2 Resource model (decision: partitionable devices only)

**Decision:** vGPU support is built **only** on KEP-4815 partitionable devices. A simplified “single device + capacity” model is **not** the product path (may exist only as a rejected alternative in appendix).

#### 6.2.1 CounterSet per physical GPU

For each vGPU-capable PF, publish one CounterSet named consistently with Dynamic MIG, e.g. `gpu-<minor>-counter-set` (RFC1123 via existing helpers).

**Baseline counters (time-sliced / non-MIG vGPU):**

| Counter name | Meaning | Typical value |
| --- | --- | --- |
| `framebuffer` | Framebuffer budget (Mi/Gi) | PF FB total |
| `vgpuInstances` | Coarse instance slots | max concurrent vGPUs under admin policy (often max of profile maxInstances, or a configured cap) |

**Optional counters:**

| Counter | When |
| --- | --- |
| `encoder` / `decoder` | If profiles are limited by encode engines |
| `memorySliceN` | MIG-backed (same as Dynamic MIG) |
| `multiprocessors`, copy engines, … | MIG-backed GI costs |

**Counter accounting rules (normative for Alpha):**

1. Every vGPU partition device consumes `framebuffer` equal to that profile’s FB requirement and `vgpuInstances: 1` (unless profile defines otherwise).
2. Full `gpu` and `vfio` devices for the same PF, when advertised, consume **100%** of the CounterSet (exclusive with any vGPU partition).
3. If hardware allows only **one profile family** at a time, either:
   - **(Preferred)** encode family exclusivity purely in counters (e.g. additional mutually exclusive counters / profile-group tokens), **or**
   - **(Fallback)** prepare-time reject + republish dropping incompatible partitions when first profile locks (document as transitional if counters alone cannot express vendor rules).

> Implementing accurate FB and exclusivity is the hard product requirement; incorrect counters are considered defects.

#### 6.2.2 Partition devices

Enumerate **abstract** devices **before** creation:

```text
name: vgpu-gpu-<minor>-<profileSlug>-<slot>
# MIG-backed example:
# vgpu-gpu-<minor>-<profileSlug>-p<placementStart>
```

Required attributes (bare keys in slice; CEL via `gpu.nvidia.com`):

| Attribute | Value |
| --- | --- |
| `type` | `vgpu` |
| `profile` | Full vGPU type name (e.g. `NVIDIA L40S-12Q`) |
| `profileSlug` | Stable short id for device names |
| `slot` | Integer 0..maxInstances-1 (non-MIG) |
| `uuid` | Parent GPU UUID |
| `productName` | PF product |
| `resource.kubernetes.io/pciBusID` | PF BDF |
| `vgpuFramework` | `mdev` \| `vdev` |
| `sriovCapable` | bool |
| `typeID` | int (optional attr if useful for CEL; also in config) |

MIG-backed extras: `migProfile`, `placementStart`, `placementSize`, `gpuProfileId`, `computeProfileId`.

Each device sets `consumesCounters` → parent `gpu-<minor>-counter-set`.

**Do not** put live mdev UUIDs or VF PCIs in the advertised device **name**. Those appear only after Prepare in checkpoint/CDI/metadata.

#### 6.2.3 Example ResourceSlices (non-MIG, one L40S)

**Counters slice:**

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: worker-1-gpu.nvidia.com-counters
spec:
  driver: gpu.nvidia.com
  nodeName: worker-1
  pool:
    name: worker-1
    generation: 7
    resourceSliceCount: 2
  sharedCounters:
  - name: gpu-0-counter-set
    counters:
      framebuffer: { value: 48Gi }
      vgpuInstances: { value: "8" }
```

**Devices slice:**

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: worker-1-gpu.nvidia.com-devices
spec:
  driver: gpu.nvidia.com
  nodeName: worker-1
  pool:
    name: worker-1
    generation: 7
    resourceSliceCount: 2
  devices:
  # Exclusive full GPU (if advertised alongside vGPU)
  - name: gpu-0
    attributes:
      type: { string: gpu }
      uuid: { string: GPU-aaaa }
      productName: { string: NVIDIA L40S }
      resource.kubernetes.io/pciBusID: { string: "0000:41:00.0" }
    consumesCounters:
    - counterSet: gpu-0-counter-set
      counters:
        framebuffer: { value: 48Gi }
        vgpuInstances: { value: "8" }

  # Exclusive VFIO sibling (if PassthroughSupport)
  - name: gpu-vfio-0
    attributes:
      type: { string: vfio }
      uuid: { string: GPU-aaaa }
      resource.kubernetes.io/pciBusID: { string: "0000:41:00.0" }
    consumesCounters:
    - counterSet: gpu-0-counter-set
      counters:
        framebuffer: { value: 48Gi }
        vgpuInstances: { value: "8" }

  # vGPU partitions — profile 12Q, 4 slots
  - name: vgpu-gpu-0-12q-0
    attributes:
      type: { string: vgpu }
      profile: { string: "NVIDIA L40S-12Q" }
      profileSlug: { string: "12q" }
      slot: { int: 0 }
      uuid: { string: GPU-aaaa }
      productName: { string: NVIDIA L40S }
      resource.kubernetes.io/pciBusID: { string: "0000:41:00.0" }
      vgpuFramework: { string: mdev }
      sriovCapable: { bool: true }
    consumesCounters:
    - counterSet: gpu-0-counter-set
      counters:
        framebuffer: { value: 12Gi }
        vgpuInstances: { value: "1" }

  - name: vgpu-gpu-0-12q-1
    attributes: { type: { string: vgpu }, profile: { string: "NVIDIA L40S-12Q" }, slot: { int: 1 }, ... }
    consumesCounters:
    - counterSet: gpu-0-counter-set
      counters:
        framebuffer: { value: 12Gi }
        vgpuInstances: { value: "1" }
  # ... 12q-2, 12q-3 ...

  # Overlapping alternate profile 24Q (2 slots)
  - name: vgpu-gpu-0-24q-0
    attributes:
      type: { string: vgpu }
      profile: { string: "NVIDIA L40S-24Q" }
      profileSlug: { string: "24q" }
      slot: { int: 0 }
      uuid: { string: GPU-aaaa }
      vgpuFramework: { string: mdev }
    consumesCounters:
    - counterSet: gpu-0-counter-set
      counters:
        framebuffer: { value: 24Gi }
        vgpuInstances: { value: "1" }
  # ... 24q-1 ...
```

Scheduler examples:

| Allocation | Remaining (conceptually) | Blocked |
| --- | --- | --- |
| `vgpu-gpu-0-12q-0` | FB 36Gi, inst 7 | none that need >36Gi FB on this set |
| four `12q-*` | FB 0 if 12×4=48 | further vGPU; full gpu/vfio |
| two `24q-*` | FB 0 | further vGPU; full gpu/vfio |
| `gpu-0` or `gpu-vfio-0` | 0 | all vGPU on PF |

#### 6.2.4 MIG-backed partition example

Reuse Dynamic MIG counter dimensions; device type remains `vgpu`:

```yaml
# consumesCounters for placement-0 1g.5gb-backed vGPU
- name: vgpu-gpu-0-a100-1g5-p0
  attributes:
    type: { string: vgpu }
    profile: { string: "NVIDIA A100-1-5C" }
    migProfile: { string: "1g.5gb" }
    placementStart: { int: 0 }
    placementSize: { int: 1 }
    uuid: { string: GPU-... }
    vgpuFramework: { string: mdev }
  consumesCounters:
  - counterSet: gpu-0-counter-set
    counters:
      memory: { value: 5Gi }
      multiprocessors: { value: "14" }
      memorySlice0: { value: "1" }
```

Prepare: `CreateMigInstance` (GI+CI) → create mdev/vdev → verify MIG instance id (vdev path) → checkpoint both.

#### 6.2.5 DeviceClass

```yaml
apiVersion: resource.k8s.io/v1
kind: DeviceClass
metadata:
  name: vgpu.gpu.nvidia.com
spec:
  selectors:
  - cel:
      expression: >
        device.driver == 'gpu.nvidia.com' &&
        device.attributes['gpu.nvidia.com'].type == 'vgpu'
```

Admins may ship **narrower classes** (CEL on `profile` or `productName`) so claim authors omit selectors.

### 6.3 ResourceClaim shapes

#### 6.3.1 Profile selected by device attributes (preferred with partitionable)

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: l40s-12q
spec:
  spec:
    devices:
      requests:
      - name: vgpu
        exactly:
          deviceClassName: vgpu.gpu.nvidia.com
          selectors:
          - cel:
              expression: >
                device.attributes['gpu.nvidia.com'].profile == "NVIDIA L40S-12Q"
```

Scheduler picks any free slot partition matching the profile (e.g. `vgpu-gpu-0-12q-2`).

#### 6.3.2 With opaque Prepare config

```yaml
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  name: l40s-12q-tuned
spec:
  spec:
    devices:
      config:
      - opaque:
          driver: gpu.nvidia.com
          parameters:
            apiVersion: resource.nvidia.com/v1beta1
            kind: VgpuDeviceConfig
            # profile optional if selector already pinned it; if set must match device
            params:
              override_bar1_size: "1"
              staging_buf_size_mb: "2"
        requests: ["vgpu"]
      requests:
      - name: vgpu
        exactly:
          deviceClassName: vgpu.gpu.nvidia.com
          selectors:
          - cel:
              expression: >
                device.attributes['gpu.nvidia.com'].profile == "NVIDIA L40S-12Q"
```

#### 6.3.3 MIG-backed

```yaml
requests:
- name: vgpu
  exactly:
    deviceClassName: vgpu.gpu.nvidia.com
    selectors:
    - cel:
        expression: >
          device.attributes['gpu.nvidia.com'].profile == "NVIDIA A100-1-5C" &&
          device.attributes['gpu.nvidia.com'].migProfile == "1g.5gb"
```

#### 6.3.4 Pod wiring

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: virt-launcher-example
spec:
  resourceClaims:
  - name: vgpu
    resourceClaimTemplateName: l40s-12q
  containers:
  - name: compute
    resources:
      claims:
      - name: vgpu
```

KubeVirt: attach claim to virt-launcher via `GPUsWithDRA` (exact VMI fields coordinated with KubeVirt).

### 6.4 API: `VgpuDeviceConfig`

```go
// resource.nvidia.com/v1beta1
type VgpuDeviceConfig struct {
    metav1.TypeMeta `json:",inline"`

    // Profile optional when the allocated device already carries profile.
    // If set, Prepare MUST verify equality with device attribute.
    Profile string `json:"profile,omitempty"`

    TypeID *int `json:"typeID,omitempty"`

    Mig *VgpuMigConfig `json:"mig,omitempty"` // optional override; partition usually encodes MIG

    Sriov *VgpuSriovConfig `json:"sriov,omitempty"`

    Params map[string]string `json:"params,omitempty"`
}
```

Validation:

- Unknown fields rejected (strict decoder).
- If both claim config profile and device attr profile set → must match.
- At most one vGPU config group per claim (Alpha).
- Incompatible with GpuConfig sharing strategies on same group.

### 6.5 Discovery and enumeration

On plugin start (and when republishing after mode changes):

1. Enumerate PFs via existing `deviceLib` / NVML.
2. If `VGPUSupport`:
   - Detect framework (`mdev` / `vdev`).
   - Load supported types (NVML + sysfs) merged with **admin catalog** (TypeID, maxInstances, FB cost, MIG flags).
   - Skip PFs unsuitable (no vGPU Manager types, wrong driver bind, etc.).
3. Build per-PF CounterSet from PF FB + policy.
4. For each profile: emit `maxInstances` slot devices (or MIG placements × profile).
5. Attach `consumesCounters` costs from catalog.
6. If `gpu`/`vfio` also published for PF, set their consumption to full CounterSet.
7. Publish via existing KEP-4815 path in `driver.go` (split/combined by k8s version).

**Inventory size control:** see **§6.5.1** (required for production).

### 6.5.1 Controlling which vGPU partitions are published

Naive enumeration is:

```text
devices ≈ (#PFs) × (Σ profiles maxInstances)   [+ MIG placements × profiles]
```

That can flood ResourceSlices. **Admins MUST be able to bound what is published.** The driver should treat *discovered* types as a candidate set and *published* partitions as an **admin-filtered** subset.

#### Configuration surface (proposed)

Driver node config (ConfigMap / Helm values), analogous to dynamic GPU DP `config.json` profile lists — **explicit catalog, not “publish everything NVML returns.”**

```yaml
# Example: /etc/nvidia-dra-vgpu/config.yaml  (or Helm values under vgpu:)
vgpu:
  # Default deny: only listed profiles are published.
  publicationMode: Allowlist  # Allowlist | Denylist | All (All = dev-only)

  # Global caps
  maxProfilesPerGPU: 4
  maxSlotsPerProfile: 8          # hard cap even if profile maxInstances is higher
  maxPartitionsPerNode: 256      # fail start or truncate with error if exceeded

  # Optional: do not emit one device per slot (see strategies below)
  slotPublication: PerSlot       # PerSlot | SingleDevicePerProfile

  gpuModels:
  - model: "NVIDIA L40S"         # match productName / PCI device
    # Only these profiles appear in ResourceSlices for this model
    profiles:
    - name: "NVIDIA L40S-12Q"
      typeID: 1177
      maxInstances: 4            # advertise at most 4 slots (≤ hardware max)
      framebuffer: 12Gi          # counter cost
      enabled: true
    - name: "NVIDIA L40S-24Q"
      typeID: 1178
      maxInstances: 2
      framebuffer: 24Gi
      enabled: true
    # Everything else supported by the host is NOT published

  - model: "NVIDIA A100-SXM4-40GB"
    profiles:
    - name: "NVIDIA A100-1-5C"
      maxInstances: 7
      mig:
        enabled: true
        # Optional: limit placements instead of full geometry
        placements:
          strategy: FirstN       # FirstN | All | Explicit
          firstN: 4              # only p0..p3, not every geometric placement
```

Helm sketch:

```yaml
featureGates:
  VGPUSupport: true
vgpu:
  publicationMode: Allowlist
  maxPartitionsPerNode: 256
  configMap: nvidia-dra-vgpu-catalog   # mounted into gpu-kubelet-plugin
```

#### Strategies (pick one or compose)

| Strategy | What gets published | Devices per PF (example) | Pros | Cons |
| --- | --- | --- | --- | --- |
| **1. Profile allowlist** | Only admin-listed profiles | `profiles × slots` | Biggest win; matches how dynamic DP config already works | New profiles need config change |
| **2. Cap `maxInstances` advertised** | `min(hardwareMax, adminMax)` slots | fewer slots | Simple density control | Doesn't reduce profile count |
| **3. Single device per profile** (not per slot) | One partition device per profile; capacity/multi-alloc or `count` via counters differently | `≈ #profiles` | **Fewest devices** | Needs careful multi-allocation story; weaker fit for “one device name per claim copy” unless using claim `count` + multi-allocate semantics |
| **4. MIG placement filter** | `FirstN` / explicit placement list | drops geometric explosion | Required on large MIG maps | Less placement flexibility |
| **5. Per-node / per-GPU model overlay** | Different catalogs per SKU or node pool | targeted | Good for heterogeneous fleets | More config |
| **6. On-demand profiles** (later) | Publish only profiles referenced by existing non-scheduled claims / class defaults | dynamic | Minimal idle footprint | Complex; not Alpha |

**Alpha recommendation:** **(1) + (2) + (4)** with `publicationMode: Allowlist` and `slotPublication: PerSlot`.  
**(3)** as optional mode when customers insist on minimal slice size and accept multi-instance via counters on one device name per profile.

#### Strategy 3 detail (few devices)

```yaml
# Published (1 PF, 2 profiles) — only 2 vGPU devices total
- name: vgpu-gpu-0-12q
  attributes: { type: vgpu, profile: "NVIDIA L40S-12Q" }
  # AllowMultipleAllocations + consumable capacity OR
  # partitionable: still one device that consumes per-allocation via claim count
  consumesCounters: ...
```

Versus PerSlot (`12q-0`…`12q-3`). Prefer PerSlot for Alpha clarity unless slice size is proven painful.

#### Operational controls

| Control | Effect |
| --- | --- |
| Empty allowlist + Allowlist mode | **Zero** vGPU devices (safe default) |
| `enabled: false` on a profile | Drop without deleting catalog entry |
| Node label / pool-specific ConfigMap | Only some node pools expose vGPU |
| `maxPartitionsPerNode` | Plugin refuses to start or logs fatal if enumeration exceeds cap |
| Metrics `vgpu_partitions_advertised` | Alert on bloat |

#### What users should *not* rely on

- Publishing **all** NVML/`mdev_supported_types` entries by default.
- CEL DeviceClasses alone (they filter claims, **not** what is stored in etcd ResourceSlices).
- Hiding devices only after first allocation (helps exclusivity, not initial enumerate size).

#### Worked example

Host supports 20 L40S vGPU types. Admin allowlists 2:

```text
Without control:  8 GPUs × 20 profiles × ~avg 4 slots ≈ 640 devices
With allowlist:   8 × (4×12Q + 2×24Q) = 48 devices
+ maxSlotsPerProfile: 4 on a type with hardware 8 → further cut
```

### 6.6 Prepare / Unprepare

#### Prepare

```text
Input: allocated device name (e.g. vgpu-gpu-0-12q-2) + optional VgpuDeviceConfig
  → resolve VgpuPartitionInfo (parent PF, profile, slot/placement, framework, costs)
  → validate counters already allocated by apiserver/scheduler (trust but verify host free)
  → PF on nvidia driver
  → SR-IOV: ensure VFs if required
  → if MIG-backed and GI not present for placement:
        CreateMigInstance (GI+CI); record giId
  → if mdev: create mdev UUID on PF or VF
    if vdev: SetCurrentVgpuType(vf, typeID); verify MIG id if needed
  → SetVgpuParams
  → checkpoint PreparedVgpuDevice { partitionName, concreteId, giId, profile }
  → CDI + DeviceMetadata (mdev UUID or VF PCI, parent UUID, profile, type=vgpu)
```

Partial failure: rollback mdev/vdev and MIG GI created by this attempt (Dynamic MIG style).

#### Unprepare

```text
  → destroy vGPU (mdev delete / clear current_vgpu_type)
  → if MIG owned by claim: DeleteMigInstance
  → clear checkpoint
  → counters freed by claim release (scheduler); optional republish generation bump
```

**Order:** create MIG → vGPU; destroy vGPU → MIG (matches kubevirt-dynamic-gpu-device-plugin).

### 6.7 Exclusivity strategy

| Mechanism | Role |
| --- | --- |
| **Primary:** full CounterSet consumption by `gpu` / `vfio` | Scheduler never dual-allocates PF modes |
| **Secondary:** prepare-time checks | Host state drift, foreign mdevs |
| **Tertiary:** sibling remove/republish | Optional UX; less critical than today if counters correct |

Feature-gate interactions (Alpha proposal):

| Gate combo | Behavior |
| --- | --- |
| `VGPUSupport` alone | vGPU counters+partitions; may omit gpu/vfio or publish them with full consume |
| + `PassthroughSupport` | vfio devices full-consume same CounterSet |
| + `DynamicMIG` | **Decide in implementation spike:** shared CounterSet namespace vs separate; MIG-backed vGPU likely **composes** with Dynamic MIG counters on same PF — requires one unified counter model per PF |
| vs `TimeSlicing`/`MPS` on same PF | Disallow concurrent mode while vGPU counters partially spent (prepare reject / don’t advertise sharing configs against vGPU-locked PF) |

> Open design spike: unified PF CounterSet when DynamicMIG + VGPUSupport both on (recommended long-term) vs mutual exclusion of gates (simpler Alpha).

### 6.8 CDI and KubeVirt contract

Parallel to `vfio-cdi.go`:

- `NVIDIA_VISIBLE_DEVICES=void` where needed.
- mdev: mediated device nodes / env with mdev UUID.
- vdev: `/dev/vfio/<group>` (+ optional IOMMU API device policy if shared with VFIO stack).
- DeviceMetadata attributes: `type=vgpu`, parent UUID, mdev UUID or VF PCI, profile, pciBusID.

Success criterion: KubeVirt VMI reaches Running with guest vGPU visible.

### 6.9 Checkpoint

```go
type PreparedVgpuDevice struct {
    Info     *VgpuPartitionInfo
    Device   *CheckpointedDevice
    Concrete *VgpuConcrete // mdev UUID | vfPCI + typeID + optional GI id
}
```

Startup reconcile: destroy checkpoint-orphaned mdev/vdev/MIG created by this driver; do not destroy unknown admin mdevs without policy flag.

### 6.10 Bin packing

Emergent from counters:

- Partly used PFs have remaining counters; identical profile slots still fit → natural pack.
- Empty PF remains fully available for large profiles or full gpu/vfio.
- No kubelet `GetPreferredAllocation` equivalent required for correctness.

Optional later: scoring attributes if scheduler policies need “prefer denser PF.”

### 6.11 SR-IOV

- Attribute `sriovCapable` on partitions.
- Prepare enables/slices VFs when framework needs them (dynamic DP `createVF` parity).
- Whole-PF VFIO path keeps `verifyDisabledVFs`.
- Admin pre-slice still supported.

### 6.12 Feature gate and Helm

```go
VGPUSupport featuregate.Feature = "VGPUSupport" // Alpha, default false
```

Helm:

```yaml
featureGates:
  VGPUSupport: false
```

Ship `deviceclass-vgpu-gpu.yaml` when GPUs enabled (class present even if gate off is OK if prepare fails clearly—or only render when gate on; match project VFIO convention).

### 6.13 Code extension map

| Concern | Location |
| --- | --- |
| CounterSet + consumesCounters | extend `partitions.go` / new `vgpu_partitions.go` |
| Publish path | `driver.go` KEP-4815 branch |
| Partition info | `deviceinfo.go`, `allocatable.go`, `prepared.go` |
| Host ops | `vgpu.go`, `vgpu_mdev.go`, `vgpu_vdev.go` (port from kubevirt DP util) |
| MIG compose | `util`-equivalent create/delete GI; mirror dynamic DP order |
| Lifecycle | `device_state.go` applyConfig / prepare / unprepare |
| CDI | `vgpu-cdi.go` |
| API / webhook | `vgpudeviceconfig.go`, webhook switch |
| Types | `VgpuDeviceType = "vgpu"` |
| Tests | unit fixtures for counter math; e2e lab |

---

## 7. Phased delivery

### Phase 0 — Spikes

- Counter model for 1–2 SKUs (FB + instances + profile exclusivity).
- Gate interaction with DynamicMIG and PassthroughSupport.
- KubeVirt metadata contract for mdev vs vdev.
- Slice cardinality estimates (profiles × slots × GPUs per node).

### Phase 1 — Alpha MVP (partitionable, non-MIG)

- `VGPUSupport` + SharedCounters + slot partitions for allowlisted profiles.
- mdev **or** vdev Prepare/Unprepare (host framework detect).
- `gpu`/`vfio` full-counter exclusivity when those devices published.
- CDI + DeviceMetadata + checkpoint.
- DeviceClass + claim examples with CEL profile selector.
- Unit tests for consumption matrix; lab e2e.
- Docs: architecture note, ResourceSlice attributes, KubeVirt guide.

### Phase 2 — Density

- MIG-backed partitions (compose Dynamic MIG counters).
- SR-IOV in-Prepare.
- Richer exclusivity counters / remove prepare-time family lock if possible.
- `vgpu_params` + optional host scheduler knobs.
- Metrics; slice size controls.

### Phase 3 — Beta hardening

- Multi-SKU soak; failure injection; foreign mdev policy.
- KubeVirt continuous e2e.
- API freeze; scoring polish; gate matricies finalized.

---

## 8. Testing strategy

| Layer | Focus |
| --- | --- |
| Unit | Counter costs per profile; exclusivity with full consume; name stability; config validate; checkpoint |
| Integration | Fake publish of counters+devices slices; allocate two overlapping profiles rejected by scheduler mock |
| E2E lab | Multi-VM same profile pack; cross-profile blocked; full gpu/vfio blocked while vGPU held; unprepare restores; plugin restart |
| Negative | Oversubscribe FB; missing vGPU Manager; wrong typeID; MIG create fail rollback |
| Regression | Dynamic MIG + VFIO demos with `VGPUSupport=false` |

---

## 9. Risks and mitigations

| Risk | Mitigation |
| --- | --- |
| Inaccurate counters → silent oversubscribe | SKU catalog review; prepare-time host verify; e2e pack tests |
| ResourceSlice explosion | Allowlist profiles; slot caps; monitor apiserver size |
| DynamicMIG + VGPU counter clash | Phase 0 spike; prefer unified PF CounterSet or gate mutex for Alpha |
| KubeVirt metadata gaps | Early liaison; unstable env contract documented |
| Orphan mdevs | Checkpoint + conservative startup cleanup |
| Double plugin with kubevirt-dynamic-gpu-dp | Docs + taints; detect foreign mdevs |
| k8s without DRAPartitionableDevices | Gate check at startup; docs prerequisites |
| Profile family rules not expressible in counters | Temporary prepare lock + republish; fix counter model |

---

## 10. Open questions

1. Unified CounterSet with DynamicMIG on same PF vs mutual exclusion of gates (Alpha).
2. Exact counter names and FB sources (NVML vs admin catalog).
3. How to encode “single profile family” purely in counters (token counter vs publish filter).
4. Slot device naming stability across plugin restarts (must be deterministic).
5. KubeVirt DeviceMetadata schema for mdev vs vdev.
6. Whether opaque `profile` remains required anywhere once CEL selectors are standard.
7. Max partitions per node before split pools / filtering needed.
8. Should `vgpuInstances` global cap be sum of max profile instances or a separate admin knob?

---

## 11. Success metrics

- Alpha: multi-profile ResourceSlices published; scheduler packs N identical profile VMs on one PF up to counter limits; rejects oversubscribe and gpu/vfio collision.
- KubeVirt VM gets working vGPU via DRA only (no classic GPU DP).
- Unprepare returns counters; subsequent full gpu or other profile works.
- 100 claim churn cycles without mdev/MIG leak.
- Gate off: zero impact on existing paths.

---

## 12. References

- KEP-4815: https://github.com/kubernetes/enhancements/tree/master/keps/sig-scheduling/4815-dra-partitionable-devices
- Kubernetes DRA partitionable devices: https://kubernetes.io/docs/concepts/resource-management/dynamic-resource-allocation/dra-features/
- Driver architecture: https://dra-driver-nvidia-gpu.sigs.k8s.io/docs/concepts/architecture/
- GPU allocation / Dynamic MIG: https://dra-driver-nvidia-gpu.sigs.k8s.io/docs/concepts/gpu-allocation/
- KubeVirt VFIO guide: https://dra-driver-nvidia-gpu.sigs.k8s.io/docs/guides/gpu-allocation/kubevirt-vfio-gpu-passthrough/
- ResourceSlice attributes: https://dra-driver-nvidia-gpu.sigs.k8s.io/docs/reference/resourceslice-attributes/
- Implementation reference: `cmd/gpu-kubelet-plugin/partitions.go`, `driver.go` (SharedCounters publish)
- Lifecycle reference: `kubevirt-dynamic-gpu-device-plugin` MIG+vGPU allocate order

---

## 13. Appendix A — Mapping from kubevirt-dynamic-gpu-device-plugin

| Dynamic GPU DP | Partitionable DRA |
| --- | --- |
| Per-type plugins + synthetic UUIDs in ListAndWatch | Partition devices in ResourceSlice (`type=vgpu`) |
| `GetPreferredAllocation` bin pack | SharedCounters remaining (pre-bind) |
| `maxInstances` per profile | Slot partitions 0..N-1 each consuming counters |
| Profile exclusivity by marking unavailable | Counters (+ optional republish) |
| `allocateMdevVgpu` / `allocateVdevVgpu` | NodePrepare on allocated partition |
| `CreateMigInstance` then vGPU | Prepare MIG-backed partition |
| Deallocate vGPU then MIG | NodeUnprepare |
| `config.json` profiles | Admin catalog → enumeration + counter costs |
| Env MDEV/PCI | CDI + DeviceMetadata |
| gpus-in-use files | Checkpoint + claim UID |

## 14. Appendix B — Rejected alternative: single device + capacity

Earlier drafts considered one `vgpu-gpu-N` device with `capacity.vgpu.instances`. **Rejected as the product path** because it does not correctly schedule **overlapping multi-profile** layouts without driver-side hide/show races, and diverges from Dynamic MIG. Capacity-only may still appear in internal prototypes but is not the design target.

## 15. Appendix C — Comparison: Dynamic MIG vs vGPU partitions

| | Dynamic MIG | vGPU (this design) |
| --- | --- | --- |
| Device `type` | `mig` (API) | `vgpu` |
| Partition meaning | GI placement | Profile slot or MIG placement + vGPU type |
| Prepare creates | GI + CI | optional GI+CI + **mdev/vdev** |
| Consumer | CUDA container | KubeVirt / VFIO-mdev guest |
| CounterSet | per PF | per PF (ideally unified if both enabled) |
| Publish path | `partitions.go` + `driver.go` | same path extended |

---

## Document history

| Date | Author | Notes |
| --- | --- | --- |
| 2026-09-01 | Draft | Initial design (capacity-oriented Phase 1) |
| 2026-09-01 | Draft rev | **Pivot to partitionable devices (KEP-4815) as sole resource model**; ResourceSlice/claim examples; Dynamic MIG alignment |

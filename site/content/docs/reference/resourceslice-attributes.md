---
title: ResourceSlice device attributes
linkTitle: ResourceSlice attributes
weight: 40
description: NVIDIA GPU, MIG, and VFIO device attributes and capacity published by the DRA Driver for NVIDIA GPUs in ResourceSlices.
---

The DRA Driver for NVIDIA GPUs publishes each allocatable device — full GPUs, MIG
slices, and VFIO passthrough devices — as an entry under `spec.devices` in a
node's `ResourceSlice`. This page is a reference for the NVIDIA-specific
attributes and capacity on those entries: what each field means and the exact
names you reference in CEL selectors.

For the generic `ResourceSlice` type schema — every field on `ResourceSlice`,
`Device`, `DeviceAttribute`, `DeviceCapacity`, and related types — see the
[Kubernetes API reference for ResourceSlice v1](https://kubernetes.io/docs/reference/kubernetes-api/resource/resource-slice-v1/).
For how the driver publishes slices, see
[Publishing GPUs in ResourceSlices](../concepts/gpu-allocation.md#publishing-gpus-in-resourceslices).
To inspect the ResourceSlices on your own cluster, see
[View available GPU resources](../guides/gpu-allocation/view-resources.md).

## Device types

The DRA Driver for NVIDIA GPUs publishes all devices under a single driver,
`gpu.nvidia.com`, with one pool per node. The NVIDIA-specific `type` attribute on
each device identifies the kind of device, and the built-in DeviceClasses select
on it:

| `type` | DeviceClass | Device |
|---|---|---|
| `gpu` | `gpu.nvidia.com` | Full physical GPU |
| `mig` | `mig.nvidia.com` | MIG slice |
| `vfio` | `vfio.gpu.nvidia.com` | VFIO passthrough device |
| `vgpu` | `vgpu.gpu.nvidia.com` | vGPU partition (abstract; created at Prepare time), requires the `VGPUSupport` feature gate |

The sections below show a representative `spec.devices[]` entry for each type.
`kubectl` prints map keys alphabetically (so `type` and `uuid` appear last), the
`#` comments are annotations rather than part of the real output, and the values
are illustrative — confirm them on your own cluster.

## Full GPU (type: gpu)

<!-- gpuModuleID with ID, cmd/gpu-kubelet-plugin/deviceinfo.go:236 -->

```yaml
- attributes:
    addressingMode:
      string: HMM                   # memory addressing mode, when available
    architecture:
      string: Ampere                # GPU architecture
    brand:
      string: Nvidia                # GPU brand
    cudaComputeCapability:
      version: 8.0.0                # CUDA compute capability
    cudaDriverVersion:
      version: 13.0.0              # CUDA driver version
    driverVersion:
      version: 580.126.20          # NVIDIA driver version
    gpuModuleID:
      int: 1                        # NVML GPU module ID, when FM partitioning is enabled
    partition2:
      int: 4                        # ID of a reported size-2 FM partition
    productName:
      string: NVIDIA A100-PCIE-40GB # product name reported by NVML
    resource.kubernetes.io/pciBusID:
      string: 0000:65:00.0          # PCI bus address in BDF notation, when available
    resource.kubernetes.io/pcieRoot:
      string: pci0000:64            # PCIe root complex identifier, when available
    resource.kubernetes.io/numaNode:
      int: 0                         # NUMA node, when available
    type:
      string: gpu                   # device kind: gpu, mig, or vfio
    uuid:
      string: GPU-2fa81118-5a5f-aa66-7660-471eed407181
  capacity:
    memory:
      value: 40Gi                   # total GPU memory
    # On MIG-capable GPUs with partition metadata, additional capacities
    # (multiprocessors, copyEngines, decoders, encoders, jpegEngines, ofaEngines)
    # may also appear here.
  name: gpu-0
```

## MIG slice (type: mig)

```yaml
- attributes:
    addressingMode:
      string: HMM
    architecture:
      string: Ampere                # inherited from the parent GPU
    brand:
      string: Nvidia
    cudaComputeCapability:
      version: 8.0.0
    cudaDriverVersion:
      version: 13.0.0
    driverVersion:
      version: 580.126.20
    parentUUID:
      string: GPU-2fa81118-5a5f-aa66-7660-471eed407181  # physical GPU hosting this instance
    productName:
      string: NVIDIA A100-PCIE-40GB # inherited from the parent GPU
    profile:
      string: 1g.5gb                # MIG profile, e.g. 1g.5gb or 3g.20gb
    resource.kubernetes.io/pciBusID:
      string: 0000:65:00.0
    resource.kubernetes.io/pcieRoot:
      string: pci0000:64
    resource.kubernetes.io/numaNode:
      int: 0                         # inherited from the parent GPU, when available
    type:
      string: mig                   # device kind
    uuid:
      string: MIG-1a2b3c4d-5e6f-7a8b-9c0d-1e2f3a4b5c6d
  capacity:
    copyEngines:
      value: "1"                    # dedicated copy engines
    decoders:
      value: "0"                    # dedicated video decoders
    encoders:
      value: "0"                    # dedicated video encoders
    jpegEngines:
      value: "0"                    # dedicated JPEG engines
    memory:
      value: 4864Mi                 # dedicated slice memory (usable amount, below the "5gb" label)
    multiprocessors:
      value: "14"                   # streaming multiprocessors dedicated to the slice
    ofaEngines:
      value: "0"                    # dedicated optical-flow accelerators
  name: gpu-0-mig-1g.5gb-0
```

## VFIO passthrough (type: vfio)

```yaml
- attributes:
    deviceID:
      string: "0x20b0"              # PCI device ID
    gpuModuleID:
      int: 1                         # NVML GPU module ID, when FM partitioning is enabled
    iommuFDEnabled:
      bool: true                    # whether the IOMMUFD backend is enabled
    partition1:
      int: 8                         # ID of the size-1 Fabric Manager partition
    partition2:
      int: 4                         # ID of the size-2 Fabric Manager partition
    partition4:
      int: 2                         # ID of the size-4 Fabric Manager partition
    partition8:
      int: 1                         # ID of the size-8 Fabric Manager partition
    productName:
      string: NVIDIA A100-PCIE-40GB # product name reported by NVML
    resource.kubernetes.io/numaNode:
      int: 0                         # NUMA node, when available
    resource.kubernetes.io/pciBusID:
      string: 0000:65:00.0          # PCI bus address in BDF notation, when available
    resource.kubernetes.io/pcieRoot:
      string: pci0000:64            # PCIe root complex identifier, when available
    type:
      string: vfio                  # device kind
    uuid:
      string: GPU-2fa81118-5a5f-aa66-7660-471eed407181
    vendorID:
      string: "0x10de"             # PCI vendor ID (0x10de = NVIDIA)
  capacity:
    addressableMemory:
      value: 40Gi                   # addressable device memory
  name: gpu-vfio-0
```

## vGPU partition (type: vgpu)

> **Note:** vGPU support is alpha and requires the `VGPUSupport` feature
> gate, cluster-side `DRAPartitionableDevices`, and the NVIDIA vGPU Manager
> on the host. At Prepare time the driver creates the concrete mediated
> device (`mdev` framework) or programs the vGPU type of an SR-IOV VF
> (`vdev` framework, VFs pre-enabled by the administrator), and exposes the
> result via CDI and Device Metadata (`DeviceMetadata` gate) for VM
> consumers such as KubeVirt.

vGPU partitions are abstract devices: one entry per (profile, slot) exists in
the ResourceSlice before any concrete vGPU device has been created. Selection
is done via shared counters on the parent GPU's CounterSet, exactly as for
dynamic MIG. The device name never contains a live mdev UUID or VF PCI
address; those are produced at Prepare time.

```yaml
- attributes:
    profile:
      string: NVIDIA L40S-12Q       # full vGPU type name
    shortProfileName:
      string: 12q                   # short, stable ID: derived from the profile name; keys the device name slot and the "vgpu-<shortProfileName>" compatibility group
    productName:
      string: NVIDIA L40S           # inherited from the parent GPU
    resource.kubernetes.io/pciBusID:
      string: 0000:65:00.0          # PCI bus address of the parent GPU
    resource.kubernetes.io/pcieRoot:
      string: pci0000:64            # inherited from the parent GPU
    eccMode:
      bool: true                    # parent GPU ECC enabled (NVML; omitted when NVML cannot report)

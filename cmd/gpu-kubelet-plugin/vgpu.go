/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// Name of the counter published in the per-GPU CounterSet for the vGPU
// partitioning scheme (design: docs/design/vgpu-support.md, section 6.2.1).
// There is deliberately no instance-slot counter: enumerating exactly
// MaxInstances slot devices per profile bounds the instance count already.
const (
	vgpuFramebufferCounterName = "framebuffer"
)

// Opaque compatibility group names (KEP-5963) this driver declares on
// consumesCounters entries. Group names are meaningful only within this
// driver's pool; the taxonomy below is the contract proposed in
// docs/design/vgpu-support.md, section 6.2.3.
const (
	// vgpuCompatibilityGroupPrefix prefixes per-profile-family groups: slots
	// of the same vGPU profile family are co-allocatable on one physical GPU,
	// slots of different families are not.
	vgpuCompatibilityGroupPrefix = "vgpu-"

	// Full-GPU devices (and whole-PF VFIO devices, once representable in the
	// partitionable publish path) carry these disjoint-from-everything groups.
	// Capacity exclusion is provided on top by consuming the entire CounterSet.
	gpuFullCompatibilityGroup = "gpu-full"
	vfioCompatibilityGroup    = "vfio"

	// Container MIG partitions carry this group; it is disjoint from all
	// vgpu-* groups so MIG and vGPU partitions can never be co-allocated on
	// the same CounterSet.
	migCompatibilityGroup = "mig"
)

// vgpuFramework identifies how concrete vGPU devices are managed on the host
// (design: docs/design/vgpu-support.md, FR-8).
const (
	vgpuFrameworkMdev = "mdev"
	vgpuFrameworkVdev = "vdev"
)

// VgpuProfileSpec describes one vGPU profile (type) advertised on a physical
// GPU: the intersection of the vGPU types the host reports as supported and
// the administrator's profile allowlist (--vgpu-profiles).
type VgpuProfileSpec struct {
	// Full vGPU type name as reported by NVML, e.g. "NVIDIA L40S-12Q".
	Name string
	// Numeric vGPU type ID as reported by NVML.
	TypeID uint32
	// Framebuffer budget this profile consumes from the parent GPU, in bytes.
	FramebufferBytes uint64
	// Number of slot partitions advertised for this profile. Equals the
	// hardware maximum instance count until admin-side caps are implemented
	// (docs/design/vgpu-support.md, section 6.5.1, strategy 2).
	MaxInstances int
}

// Slug returns a stable, RFC1123-compliant short identifier for the profile,
// used in device names (vgpu-gpu-<minor>-<slug>-<slot>) and in the
// compatibility group name. The convention is the last hyphen-separated token
// of the profile name ("NVIDIA L40S-12Q" -> "12q"), falling back to the
// sanitized full name. Deriving it purely from the profile name keeps device
// names stable across plugin restarts.
func (s *VgpuProfileSpec) Slug() string {
	if idx := strings.LastIndex(s.Name, "-"); idx >= 0 && idx+1 < len(s.Name) {
		if slug := toRFC1123Compliant(s.Name[idx+1:]); slug != "" {
			return slug
		}
	}
	return toRFC1123Compliant(s.Name)
}

// VgpuPartitionInfo describes an abstract, not-yet-incarnated vGPU partition
// of a physical GPU: one slot of one profile. The concrete device (an mdev
// UUID or a VF with a programmed vGPU type) is created at NodePrepare time.
type VgpuPartitionInfo struct {
	Parent  *GpuInfo
	Profile *VgpuProfileSpec
	// Slot indexes the advertised instances of this profile on the parent,
	// 0..MaxInstances-1. It makes each abstract partition individually
	// allocatable; it does not imply placement on hardware.
	Slot int
	// Framework is "mdev" or "vdev" (vgpuFramework* constants).
	Framework string
	// SriovCapable reports whether the parent PCI function is SR-IOV capable.
	SriovCapable bool
}

func (i *VgpuPartitionInfo) CanonicalName() string {
	return fmt.Sprintf("vgpu-gpu-%d-%s-%d", i.Parent.minor, i.Profile.Slug(), i.Slot)
}

func (i *VgpuPartitionInfo) Attributes() map[resourceapi.QualifiedName]resourceapi.DeviceAttribute {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"type": {
			StringValue: ptr.To(VgpuDeviceType),
		},
		"profile": {
			StringValue: ptr.To(i.Profile.Name),
		},
		"profileSlug": {
			StringValue: ptr.To(i.Profile.Slug()),
		},
		"slot": {
			IntValue: ptr.To(int64(i.Slot)),
		},
		// Parent GPU identity, per docs/design/vgpu-support.md section 6.2.2.
		"uuid": {
			StringValue: &i.Parent.UUID,
		},
		"productName": {
			StringValue: &i.Parent.productName,
		},
		"vgpuFramework": {
			StringValue: ptr.To(i.Framework),
		},
		"sriovCapable": {
			BoolValue: ptr.To(i.SriovCapable),
		},
		"typeID": {
			IntValue: ptr.To(int64(i.Profile.TypeID)),
		},
	}

	addDeviceAttribute(attrs, i.Parent.pciBusIDAttr)
	addDeviceAttribute(attrs, i.Parent.pcieRootAttr)

	return attrs
}

// compatibilityGroup returns the KEP-5963 group name for this partition's
// profile family: slots of the same family are co-allocatable, and any
// different scheme/family (other vgpu-* families, mig, gpu-full, vfio) is
// rejected at schedule time.
func (i *VgpuPartitionInfo) compatibilityGroup() string {
	return vgpuCompatibilityGroupPrefix + i.Profile.Slug()
}

// PartConsumesCounters returns the KEP-4815 counter consumption of this
// abstract partition against the parent GPU's CounterSet: the profile's
// framebuffer cost. The partition device itself is the instance-slot bound:
// only MaxInstances slot devices per profile are advertised.
//
// CompatibilityGroups (KEP-5963) is declared only when the driver's
// DRADeviceCompatibilityGroups gate is enabled; publishing groups while the
// cluster-side gate is off would make kube-scheduler ignore these devices
// entirely (docs/design/vgpu-support.md, section 6.1.1 skew rule 2).
func (i *VgpuPartitionInfo) PartConsumesCounters() []resourceapi.DeviceCounterConsumption {
	consumption := resourceapi.DeviceCounterConsumption{
		CounterSet: i.Parent.GetSharedCounterSetName(),
		Counters: map[string]resourceapi.Counter{
			vgpuFramebufferCounterName: {Value: *resource.NewQuantity(int64(i.Profile.FramebufferBytes), resource.BinarySI)},
		},
	}
	if featuregates.Enabled(featuregates.DRADeviceCompatibilityGroups) {
		consumption.CompatibilityGroups = []string{i.compatibilityGroup()}
	}
	return []resourceapi.DeviceCounterConsumption{consumption}
}

// Return the full KEP-4815 representation of an abstract vGPU partition.
func (i *VgpuPartitionInfo) PartGetDevice() resourceapi.Device {
	return resourceapi.Device{
		Name:             i.CanonicalName(),
		Attributes:       i.Attributes(),
		ConsumesCounters: i.PartConsumesCounters(),
	}
}

// parseVgpuProfileAllowlist parses the --vgpu-profiles flag: a comma-separated
// allowlist of full vGPU type names (e.g. "NVIDIA L40S-12Q,NVIDIA L40S-24Q").
func parseVgpuProfileAllowlist(raw string) map[string]bool {
	allowed := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			allowed[name] = true
		}
	}
	return allowed
}

// detectVgpuFramework determines whether the host manages vGPU devices for
// the physical function identified by pciBusID through the classic mdev
// framework or through SR-IOV VFs with a programmable vGPU type ("vdev"),
// by probing sysfs below sysfsRoot. SR-IOV presence wins over
// mdev_supported_types: on hosts exposing both, vGPU Manager assigns types
// per VF. Best-effort: absence of both does not fail advertisement, because
// the framework is only exercised at Prepare time.
func detectVgpuFramework(sysfsRoot string, pciBusID string) (framework string, sriovCapable bool) {
	deviceDir := filepath.Join(sysfsRoot, "sys", "bus", "pci", "devices", pciBusID)

	if _, err := os.Stat(filepath.Join(deviceDir, "sriov_totalvfs")); err == nil {
		sriovCapable = true
	}
	if sriovCapable {
		return vgpuFrameworkVdev, true
	}
	if entries, err := os.ReadDir(filepath.Join(deviceDir, "mdev_supported_types")); err == nil && len(entries) > 0 {
		return vgpuFrameworkMdev, false
	}
	return vgpuFrameworkMdev, false
}

/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func setVGPUSupportGate(t *testing.T, enabled bool) {
	t.Helper()
	original := featuregates.Enabled(featuregates.VGPUSupport)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(
		map[string]bool{string(featuregates.VGPUSupport): enabled}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(
			map[string]bool{string(featuregates.VGPUSupport): original}))
	})
}

func setCompatibilityGroupsGate(t *testing.T, enabled bool) {
	t.Helper()
	original := featuregates.Enabled(featuregates.DRADeviceCompatibilityGroups)
	require.NoError(t, featuregates.FeatureGates().SetFromMap(
		map[string]bool{string(featuregates.DRADeviceCompatibilityGroups): enabled}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(
			map[string]bool{string(featuregates.DRADeviceCompatibilityGroups): original}))
	})
}

func newVgpuTestProfile(name string, profileID uint32, fbBytes uint64, maxInstances int) *VgpuProfileSpec {
	return &VgpuProfileSpec{
		Name:             name,
		ProfileID:        profileID,
		FramebufferBytes: fbBytes,
		MaxInstances:     maxInstances,
	}
}

func newVgpuTestGpu(profiles ...*VgpuProfileSpec) *GpuInfo {
	gpu := newTestGpuInfo(newScalarNumaNodeAttribute(0))
	gpu.memoryBytes = ptr.To(uint64(48 << 30))
	gpu.pciBusID = "0000:41:00.0"
	gpu.vgpuProfiles = profiles
	return gpu
}

func TestVgpuProfileSlug(t *testing.T) {
	tests := map[string]struct {
		profileName string
		want        string
	}{
		"q-series profile":                {profileName: "NVIDIA L40S-12Q", want: "12q"},
		"MIG-backed profile":              {profileName: "NVIDIA A100-1-5C", want: "5c"},
		"lowercase passthrough unchanged": {profileName: "h100-nvidia-1q", want: "1q"},
		"no hyphen falls back to sanitized full name": {
			profileName: "Just A Profile",
			want:        "just-a-profile",
		},
		"trailing hyphen falls back to sanitized full name": {
			profileName: "something-",
			want:        "something",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec := &VgpuProfileSpec{Name: tc.profileName}
			require.Equal(t, tc.want, spec.Slug())
		})
	}
}

func TestVgpuPartitionCanonicalName(t *testing.T) {
	gpu := newVgpuTestGpu()
	partition := &VgpuPartitionInfo{
		Parent:  gpu,
		Profile: newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4),
		Slot:    2,
	}

	// docs/design/vgpu-support.md: vgpu-gpu-<minor>-<shortProfileName>-<slot>.
	// The name must be deterministic across plugin restarts: it only depends
	// on the parent minor, the profile slug, and the slot index.
	require.Equal(t, "vgpu-gpu-0-12q-2", partition.CanonicalName())
}

func TestVgpuPartitionGetDeviceAttributes(t *testing.T) {
	gpu := newVgpuTestGpu()
	spec := newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4)
	partition := &VgpuPartitionInfo{
		Parent:    gpu,
		Profile:   spec,
		Slot:      1,
		Framework: vgpuFrameworkMdev,
	}

	dev := partition.PartGetDevice()

	require.Equal(t, "vgpu-gpu-0-12q-1", dev.Name)

	attrs := dev.Attributes
	require.Equal(t, VgpuDeviceType, *attrs["type"].StringValue)
	require.Equal(t, "NVIDIA L40S-12Q", *attrs["profile"].StringValue)
	require.Equal(t, "12q", *attrs["shortProfileName"].StringValue)
	require.Equal(t, int64(1), *attrs["slot"].IntValue)
	// `uuid` carries the parent GPU UUID by design.
	require.Equal(t, gpu.UUID, *attrs["uuid"].StringValue)
	require.Equal(t, gpu.productName, *attrs["productName"].StringValue)
	require.Equal(t, vgpuFrameworkMdev, *attrs["vgpuFramework"].StringValue)
	require.Equal(t, int64(1177), *attrs["profileID"].IntValue)
}

func TestVgpuPartitionConsumesCounters(t *testing.T) {
	gpu := newVgpuTestGpu()
	spec := newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4)
	partition := &VgpuPartitionInfo{Parent: gpu, Profile: spec, Slot: 0}

	t.Run("framebuffer only, groups stripped without gate", func(t *testing.T) {
		setCompatibilityGroupsGate(t, false)

		cc := partition.PartConsumesCounters()
		require.Len(t, cc, 1)
		require.Equal(t, "vgpu-0-counter-set", cc[0].CounterSet)
		require.Equal(t, int64(12<<30), counterValue(t, cc[0].Counters, vgpuFramebufferCounterName))
		// One instance slot per slot device is implied by enumeration.
		require.Len(t, cc[0].Counters, 1)
		// Skew rule: with the cluster gate off, devices must not declare
		// groups or the scheduler would ignore them (design 6.1.1 rule 2).
		require.Empty(t, cc[0].CompatibilityGroups)
	})

	t.Run("profile family group with gate on", func(t *testing.T) {
		setCompatibilityGroupsGate(t, true)

		cc := partition.PartConsumesCounters()
		require.Equal(t, []string{"vgpu-12q"}, cc[0].CompatibilityGroups)
	})
}

func TestVgpuGroupIntersectionMatrix(t *testing.T) {
	// The scheduler rule (KEP-5963): devices consuming from the same CounterSet
	// may be co-allocated iff their group lists intersect. Assert the group
	// taxonomy in docs/design/vgpu-support.md section 6.2.3 produces the
	// required intersection matrix.
	setCompatibilityGroupsGate(t, true)

	intersect := func(a, b []string) bool {
		for _, x := range a {
			for _, y := range b {
				if x == y {
					return true
				}
			}
		}
		return false
	}

	gpu := newVgpuTestGpu()
	spec12q := newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4)
	spec24q := newVgpuTestProfile("NVIDIA L40S-24Q", 1178, 24<<30, 2)
	gpu.vgpuProfiles = []*VgpuProfileSpec{spec12q, spec24q}

	slotOf := func(spec *VgpuProfileSpec, slot int) []string {
		p := &VgpuPartitionInfo{Parent: gpu, Profile: spec, Slot: slot}
		return p.PartConsumesCounters()[0].CompatibilityGroups
	}
	fullGPU := gpu.PartConsumesCounters()[0].CompatibilityGroups
	migPartition := []string{migCompatibilityGroup}

	vfio := []string{vfioCompatibilityGroup}

	assert.True(t, intersect(slotOf(spec12q, 0), slotOf(spec12q, 1)), "same family packs")
	assert.False(t, intersect(slotOf(spec12q, 0), slotOf(spec24q, 0)), "cross-family blocked")
	assert.False(t, intersect(slotOf(spec12q, 0), migPartition), "mig vs vgpu blocked")
	assert.False(t, intersect(slotOf(spec12q, 0), fullGPU), "full GPU vs vgpu blocked")
	assert.False(t, intersect(migPartition, fullGPU), "mig vs full GPU blocked")
	assert.False(t, intersect(vfio, slotOf(spec12q, 0)), "vfio vs vgpu blocked")
	assert.False(t, intersect(vfio, fullGPU), "vfio vs full GPU blocked")
	assert.False(t, intersect(vfio, migPartition), "vfio vs mig blocked")
}

func TestVgpuGpuCounterSets(t *testing.T) {
	gpu := newVgpuTestGpu(
		newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4),
		newVgpuTestProfile("NVIDIA L40S-24Q", 1178, 24<<30, 2),
	)

	sets := gpu.PartSharedCounterSets()
	require.Len(t, sets, 1)
	require.Equal(t, "vgpu-0-counter-set", sets[0].Name)

	shared := sets[0].Counters
	require.Equal(t, int64(48<<30), counterValue(t, shared, vgpuFramebufferCounterName))
	// Framebuffer is the only counter; slot counts are bounded by the
	// enumerated slot devices themselves.
	require.Len(t, shared, 1)

	// The full GPU consumes the entire set, so co-allocating it with any
	// partition is capacity-impossible even with the groups gate off.
	consumes := gpu.PartConsumesCounters()
	require.Len(t, consumes, 1)
	require.Len(t, consumes[0].Counters, len(shared))
	for name, c := range shared {
		require.Equal(t, c.Value.Value(), counterValue(t, consumes[0].Counters, name), name)
	}
}

func TestVgpuGpuCounterSetsEdgeCases(t *testing.T) {
	t.Run("no vGPU profiles falls back to MIG/nil behavior", func(t *testing.T) {
		gpu := newVgpuTestGpu()
		require.Nil(t, gpu.PartSharedCounterSets())
		require.Nil(t, gpu.PartConsumesCounters())
	})

	t.Run("unknown framebuffer publishes no CounterSet", func(t *testing.T) {
		gpu := newVgpuTestGpu(newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4))
		gpu.memoryBytes = nil

		// The framebuffer budget is the only counter; an empty Counters map
		// would be rejected by the API ("spec.sharedCounters[0].counters:
		// Required value").
		require.Nil(t, gpu.PartSharedCounterSets())
		require.Nil(t, gpu.PartConsumesCounters())
	})

	t.Run("full GPU carries gpu-full group only with gate on", func(t *testing.T) {
		gpu := newVgpuTestGpu(newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4))

		setCompatibilityGroupsGate(t, false)
		require.Empty(t, gpu.PartConsumesCounters()[0].CompatibilityGroups)

		setCompatibilityGroupsGate(t, true)
		require.Equal(t, []string{gpuFullCompatibilityGroup}, gpu.PartConsumesCounters()[0].CompatibilityGroups)
	})
}

func TestMigPartitionCompatibilityGroup(t *testing.T) {
	gpu := newVgpuTestGpu()
	spec := newPartTestMigSpec(gpu, 0, 1)

	setCompatibilityGroupsGate(t, false)
	require.Empty(t, spec.PartConsumesCounters()[0].CompatibilityGroups)

	setCompatibilityGroupsGate(t, true)
	require.Equal(t, []string{migCompatibilityGroup}, spec.PartConsumesCounters()[0].CompatibilityGroups)
}

func TestParseVgpuProfileAllowlist(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want map[string]bool
	}{
		"empty":            {raw: "", want: map[string]bool{}},
		"single":           {raw: "NVIDIA L40S-12Q", want: map[string]bool{"NVIDIA L40S-12Q": true}},
		"multiple trimmed": {raw: " NVIDIA L40S-12Q ,NVIDIA L40S-24Q,", want: map[string]bool{"NVIDIA L40S-12Q": true, "NVIDIA L40S-24Q": true}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, parseVgpuProfileAllowlist(tc.raw))
		})
	}
}

func TestDetectVgpuFramework(t *testing.T) {
	t.Run("SR-IOV host vGPU mode means vf framework", func(t *testing.T) {
		framework := detectVgpuFramework(func() (nvml.HostVgpuMode, nvml.Return) {
			return nvml.HOST_VGPU_MODE_SRIOV, nvml.SUCCESS
		})
		assert.Equal(t, vgpuFrameworkVf, framework)
	})

	t.Run("non-SR-IOV host vGPU mode means mdev", func(t *testing.T) {
		framework := detectVgpuFramework(func() (nvml.HostVgpuMode, nvml.Return) {
			return nvml.HOST_VGPU_MODE_NON_SRIOV, nvml.SUCCESS
		})
		assert.Equal(t, vgpuFrameworkMdev, framework)
	})

	t.Run("NVML NOT_SUPPORTED falls back to mdev", func(t *testing.T) {
		framework := detectVgpuFramework(func() (nvml.HostVgpuMode, nvml.Return) {
			return 0, nvml.ERROR_NOT_SUPPORTED
		})
		assert.Equal(t, vgpuFrameworkMdev, framework)
	})
}

func TestApplyVgpuDeviceConfig(t *testing.T) {
	setVGPUSupportGate(t, true)

	gpu := newVgpuTestGpu()
	spec := newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4)
	gpu.vgpuProfiles = []*VgpuProfileSpec{spec}
	partition := &VgpuPartitionInfo{Parent: gpu, Profile: spec, Slot: 0}

	vfioCDIHandler, err := NewVfioCDIHandler(&deviceLib{hostRoot: t.TempDir()})
	require.NoError(t, err)

	state := &DeviceState{
		cdi: &CDIHandler{vfiocdi: vfioCDIHandler},
		perGPUAllocatable: &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{
			gpu.pciBusID: {partition.CanonicalName(): {Vgpu: partition}},
		}},
	}

	result := func() *resourceapi.DeviceRequestAllocationResult {
		return &resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Pool: "pool", Device: partition.CanonicalName(), Request: "r"}
	}

	t.Run("valid empty config", func(t *testing.T) {
		cfg := &configapi.VgpuDeviceConfig{}
		cs, err := state.applyVgpuDeviceConfig(cfg, []*resourceapi.DeviceRequestAllocationResult{result()})
		require.NoError(t, err)
		require.NotNil(t, cs.containerEdits)
		require.Contains(t, cs.containerEdits.Env, "NVIDIA_VISIBLE_DEVICES=void")
		// libvirt (virt-launcher) requires the VFIO control device for
		// mediated host-device assignment; the per-device spec only carries
		// the VF's group node.
		var nodePaths []string
		for _, n := range cs.containerEdits.DeviceNodes {
			nodePaths = append(nodePaths, n.Path)
		}
		require.Contains(t, nodePaths, "/dev/vfio/vfio")
	})

	t.Run("matching profile and profileID", func(t *testing.T) {
		cfg := &configapi.VgpuDeviceConfig{Profile: "NVIDIA L40S-12Q", ProfileID: ptr.To(1177)}
		_, err := state.applyVgpuDeviceConfig(cfg, []*resourceapi.DeviceRequestAllocationResult{result()})
		require.NoError(t, err)
	})

	t.Run("profile mismatch", func(t *testing.T) {
		cfg := &configapi.VgpuDeviceConfig{Profile: "NVIDIA L40S-24Q"}
		_, err := state.applyVgpuDeviceConfig(cfg, []*resourceapi.DeviceRequestAllocationResult{result()})
		require.ErrorContains(t, err, "does not match profile")
	})

	t.Run("profileID mismatch", func(t *testing.T) {
		cfg := &configapi.VgpuDeviceConfig{ProfileID: ptr.To(1178)}
		_, err := state.applyVgpuDeviceConfig(cfg, []*resourceapi.DeviceRequestAllocationResult{result()})
		require.ErrorContains(t, err, "does not match profile ID")
	})

	t.Run("config on non-vgpu device rejected", func(t *testing.T) {
		state.perGPUAllocatable.allocatablesMap[gpu.pciBusID]["gpu-0"] = &AllocatableDevice{Gpu: gpu}
		cfg := &configapi.VgpuDeviceConfig{}
		res := &resourceapi.DeviceRequestAllocationResult{Driver: DriverName, Pool: "pool", Device: "gpu-0", Request: "r"}
		_, err := state.applyVgpuDeviceConfig(cfg, []*resourceapi.DeviceRequestAllocationResult{res})
		require.ErrorContains(t, err, "cannot apply VgpuDeviceConfig to device")
	})
}

func TestVgpuAllocatableDevice(t *testing.T) {
	gpu := newVgpuTestGpu()
	dev := &AllocatableDevice{
		Vgpu: &VgpuPartitionInfo{
			Parent:  gpu,
			Profile: newVgpuTestProfile("NVIDIA L40S-12Q", 1177, 12<<30, 4),
			Slot:    3,
		},
	}

	require.Equal(t, VgpuDeviceType, dev.Type())
	require.Equal(t, "vgpu-gpu-0-12q-3", dev.CanonicalName())
	require.Equal(t, "0000:41:00.0", dev.GetGPUPCIBusID())

	// Abstract partitions have no UUID before actualization (same contract
	// as MigDynamic).
	require.Panics(t, func() { dev.UUID() })

	// The legacy publish path (GetDevice) must never render vGPU partitions;
	// they only exist in the partitionable-devices publish path.
	require.Panics(t, func() { dev.GetDevice(nil) })

	require.Equal(t, "vgpu", *dev.PartGetDevice(nil).Attributes["type"].StringValue)
}

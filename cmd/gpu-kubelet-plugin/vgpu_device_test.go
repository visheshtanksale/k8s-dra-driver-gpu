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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/ktesting"
)

const (
	vgpuTestPCIBusID = "0000:41:00.0"
	vgpuTestTypeID   = 1177
)

// newMdevTestPartition returns a vGPU partition on an mdev-framework parent.
func newMdevTestPartition() *VgpuPartitionInfo {
	return &VgpuPartitionInfo{
		Parent: &GpuInfo{
			UUID:        "GPU-test",
			minor:       0,
			pciBusID:    "0000:41:00.0",
			productName: "NVIDIA Test GPU",
		},
		Profile: &VgpuProfileSpec{
			Name:             "NVIDIA L40S-12Q",
			TypeID:           vgpuTestTypeID,
			FramebufferBytes: 12 << 30,
			MaxInstances:     4,
		},
		Slot:      0,
		Framework: vgpuFrameworkMdev,
	}
}

// setupFakeMdevHost builds a fake sysfs tree for a legacy vGPU-manager host
// (type dir on the PF) and installs the write seam.
func setupFakeMdevHost(t *testing.T, availableInstances string) string {
	t.Helper()
	root := t.TempDir()

	typeDir := filepath.Join(root, "sys", "bus", "pci", "devices", vgpuTestPCIBusID,
		"mdev_supported_types", vgpuMdevSysfsTypeName(vgpuTestTypeID))
	require.NoError(t, os.MkdirAll(typeDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(typeDir, "available_instances"), []byte(availableInstances), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(typeDir, "create"), nil, 0644))

	require.NoError(t, os.MkdirAll(filepath.Join(root, "sys", "bus", "mdev", "devices"), 0755))

	origWrite := vgpuWriteFile
	t.Cleanup(func() { vgpuWriteFile = origWrite })
	vgpuWriteFile = fakeMdevKernelWrites(root, map[string][]string{})

	return root
}

func vgpuTestDeviceLib(sysfsRoot string) *deviceLib {
	return &deviceLib{sysfsRoot: sysfsRoot}
}

func TestCreateVgpuDeviceMdev(t *testing.T) {
	root := setupFakeMdevHost(t, "4")
	lib := vgpuTestDeviceLib(root)

	concrete, err := lib.createVgpuDevice(newMdevTestPartition(), map[string]string{
		"frame_rate_limiter": "0",
	})
	require.NoError(t, err)

	if assert.NotNil(t, concrete) {
		assert.Equal(t, vgpuFrameworkMdev, concrete.Framework)
		assert.Equal(t, uint32(vgpuTestTypeID), concrete.TypeID)
		assert.Equal(t, "NVIDIA L40S-12Q", concrete.Profile)
		assert.Equal(t, vgpuTestPCIBusID, concrete.ParentPCIBusID)
		assert.NotEmpty(t, concrete.MdevUUID)
	}

	// The mdev instance exists on the host, including its removable handle.
	devDir := filepath.Join(root, "sys", "bus", "mdev", "devices", concrete.MdevUUID)
	require.DirExists(t, devDir)
}

// SR-IOV mdev hosts expose mdev_supported_types on the VFs, not the PF; at
// most one vGPU per VF. The driver must create on a free VF and record its
// PCI bus ID in the concrete device identity.
// (NVIDIA vGPU user guide: "Creating an NVIDIA vGPU that Supports SR-IOV".)
func TestCreateVgpuDeviceMdevOnFreeVF(t *testing.T) {
	root := t.TempDir()
	mdevRoot := filepath.Join(root, "sys", "bus", "mdev", "devices")
	require.NoError(t, os.MkdirAll(mdevRoot, 0755))

	devicesDir := filepath.Join(root, "sys", "bus", "pci", "devices")
	pfDir := filepath.Join(devicesDir, vgpuTestPCIBusID)
	require.NoError(t, os.MkdirAll(pfDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(pfDir, "sriov_numvfs"), []byte("2"), 0644))

	// virtfn0: occupied (0 available instances); virtfn1: free (1).
	freeVF, busyVF := "0000:41:01.0", "0000:41:01.1"
	for i, vf := range []string{freeVF, busyVF} {
		typeDir := filepath.Join(devicesDir, vf, "mdev_supported_types", vgpuMdevSysfsTypeName(vgpuTestTypeID))
		require.NoError(t, os.MkdirAll(typeDir, 0755))
		avail := "1"
		if i == 1 {
			avail = "0"
		}
		require.NoError(t, os.WriteFile(filepath.Join(typeDir, "available_instances"), []byte(avail), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(typeDir, "create"), nil, 0644))
		require.NoError(t, os.Symlink(filepath.Join(devicesDir, vf), filepath.Join(pfDir, fmt.Sprintf("virtfn%d", i))))
	}

	origWrite := vgpuWriteFile
	t.Cleanup(func() { vgpuWriteFile = origWrite })
	vgpuWriteFile = fakeMdevKernelWrites(root, nil)

	lib := vgpuTestDeviceLib(root)
	concrete, err := lib.createVgpuDevice(newMdevTestPartition(), nil)
	require.NoError(t, err)

	// Created on the free VF, recording both identities.
	assert.Equal(t, freeVF, concrete.VFPCIBusID)
	assert.NotEmpty(t, concrete.MdevUUID)
	require.DirExists(t, filepath.Join(mdevRoot, concrete.MdevUUID))

	// The busy VF's create file was never touched (content still empty).
	content, err := os.ReadFile(filepath.Join(devicesDir, busyVF, "mdev_supported_types", vgpuMdevSysfsTypeName(vgpuTestTypeID), "create"))
	require.NoError(t, err)
	assert.Empty(t, content)
}

// fakeMdevKernelWrites installs the kernel-emulating write seam: writing a
// UUID to a "create" file instantiates the mdev device dir; writing "1" to an
// instance's "remove" file destroys it. If not nil, createdParams records
// vgpu_params writes per device UUID.
func fakeMdevKernelWrites(root string, createdParams map[string][]string) func(string, []byte, os.FileMode) error {
	mdevRoot := filepath.Join(root, "sys", "bus", "mdev", "devices")
	return func(path string, data []byte, perm os.FileMode) error {
		switch {
		case strings.HasSuffix(path, "/create"):
			devDir := filepath.Join(mdevRoot, string(data))
			if err := os.MkdirAll(filepath.Join(devDir, "nvidia"), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(devDir, "remove"), nil, 0644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(devDir, "nvidia", "vgpu_params"), nil, 0644); err != nil {
				return err
			}
			return nil
		case strings.HasSuffix(path, "/remove"):
			return os.RemoveAll(filepath.Dir(path))
		case strings.HasSuffix(path, "/vgpu_params"):
			if createdParams != nil {
				devUUID := filepath.Base(filepath.Dir(filepath.Dir(path)))
				createdParams[devUUID] = append(createdParams[devUUID], string(data))
			}
			return nil
		}
		return os.WriteFile(path, data, perm)
	}
}

func TestCreateVgpuDeviceMdevNoInstances(t *testing.T) {
	root := setupFakeMdevHost(t, "0")
	lib := vgpuTestDeviceLib(root)

	_, err := lib.createVgpuDevice(newMdevTestPartition(), nil)
	require.ErrorContains(t, err, "no available instances")
}

func TestCreateVgpuDeviceMdevUnknownType(t *testing.T) {
	root := setupFakeMdevHost(t, "4")
	lib := vgpuTestDeviceLib(root)

	partition := newMdevTestPartition()
	partition.Profile.TypeID = 9999

	_, err := lib.createVgpuDevice(partition, nil)
	require.ErrorContains(t, err, "not found or busy on GPU")
}

func TestDeleteVgpuDeviceMdev(t *testing.T) {
	root := setupFakeMdevHost(t, "4")
	lib := vgpuTestDeviceLib(root)

	concrete, err := lib.createVgpuDevice(newMdevTestPartition(), nil)
	require.NoError(t, err)

	require.NoError(t, lib.deleteVgpuDevice(concrete))
	require.NoDirExists(t, filepath.Join(root, "sys", "bus", "mdev", "devices", concrete.MdevUUID))

	// Idempotent: deleting again must succeed (retried Unprepare).
	require.NoError(t, lib.deleteVgpuDevice(concrete))
}

// setupFakeVdevHost builds a fake SR-IOV sysfs tree for the vendor-specific
// VFIO framework: a PF with sriov_numvfs enabled, and two VFs whose
// nvidia/ directories carry the vGPU management attributes from the NVIDIA
// docs (current_vgpu_type, creatable_vgpu_types). One VF is already
// programmed with another vGPU type.
func setupFakeVdevHost(t *testing.T) (root string, freeVF string, busyVF string) {
	t.Helper()
	root = t.TempDir()

	freeVF = "0000:41:01.0"
	busyVF = "0000:41:01.1"

	devicesDir := filepath.Join(root, "sys", "bus", "pci", "devices")
	pfDir := filepath.Join(devicesDir, vgpuTestPCIBusID)
	require.NoError(t, os.MkdirAll(pfDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(pfDir, "sriov_numvfs"), []byte("2"), 0644))

	for i, vf := range []string{freeVF, busyVF} {
		nvidiaDir := filepath.Join(devicesDir, vf, "nvidia")
		require.NoError(t, os.MkdirAll(nvidiaDir, 0755))
		current := "0"
		if i == 1 {
			current = "1178"
		}
		require.NoError(t, os.WriteFile(filepath.Join(nvidiaDir, vgpuCurrentTypeFile), []byte(current), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(nvidiaDir, vgpuCreatableTypesFile), []byte("NVIDIA L40S-12Q   1177\n"), 0644))
	}

	require.NoError(t, os.Symlink(filepath.Join(devicesDir, freeVF), filepath.Join(pfDir, "virtfn0")))
	require.NoError(t, os.Symlink(filepath.Join(devicesDir, busyVF), filepath.Join(pfDir, "virtfn1")))

	return root, freeVF, busyVF
}

func newVdevTestPartition() *VgpuPartitionInfo {
	p := newMdevTestPartition()
	p.Framework = vgpuFrameworkVdev
	p.SriovCapable = true
	return p
}

func TestCreateVgpuDeviceVdev(t *testing.T) {
	root, freeVF, busyVF := setupFakeVdevHost(t)
	lib := vgpuTestDeviceLib(root)

	concrete, err := lib.createVgpuDevice(newVdevTestPartition(), nil)
	require.NoError(t, err)

	// The busy VF must have been skipped, the free one programmed.
	assert.Equal(t, freeVF, concrete.VFPCIBusID)

	readBack, err := os.ReadFile(filepath.Join(root, "sys", "bus", "pci", "devices", freeVF, "nvidia", vgpuCurrentTypeFile))
	require.NoError(t, err)
	assert.Equal(t, "1177", strings.TrimSpace(string(readBack)))

	busyReadBack, err := os.ReadFile(filepath.Join(root, "sys", "bus", "pci", "devices", busyVF, "nvidia", vgpuCurrentTypeFile))
	require.NoError(t, err)
	assert.Equal(t, "1178", strings.TrimSpace(string(busyReadBack)))
}

// createVgpuDevice must reject an unknown sysfs type ID before writing
// anything (documented consequence of an invalid write: the write fails and
// the VF's current_vgpu_type resets to 0).
func TestCreateVgpuDeviceVdevUncreatableType(t *testing.T) {
	root, freeVF, _ := setupFakeVdevHost(t)
	lib := vgpuTestDeviceLib(root)

	partition := newVdevTestPartition()
	partition.Profile.TypeID = 9999

	_, err := lib.createVgpuDevice(partition, nil)
	require.ErrorContains(t, err, "no free VF")

	current, err := os.ReadFile(filepath.Join(root, "sys", "bus", "pci", "devices", freeVF, "nvidia", vgpuCurrentTypeFile))
	require.NoError(t, err)
	assert.Equal(t, "0", strings.TrimSpace(string(current)), "uncreatable type must not have been programmed")
}

func TestDeleteVgpuDeviceVdev(t *testing.T) {
	root, _, _ := setupFakeVdevHost(t)
	lib := vgpuTestDeviceLib(root)

	concrete, err := lib.createVgpuDevice(newVdevTestPartition(), nil)
	require.NoError(t, err)

	require.NoError(t, lib.deleteVgpuDevice(concrete))

	readBack, err := os.ReadFile(filepath.Join(root, "sys", "bus", "pci", "devices", concrete.VFPCIBusID, "nvidia", vgpuCurrentTypeFile))
	require.NoError(t, err)
	assert.Equal(t, "0", strings.TrimSpace(string(readBack)))

	// Idempotent.
	require.NoError(t, lib.deleteVgpuDevice(concrete))
}

func TestCreateVgpuDeviceVdevNoVFs(t *testing.T) {
	root, _, _ := setupFakeVdevHost(t)
	lib := vgpuTestDeviceLib(root)

	require.NoError(t, os.WriteFile(
		filepath.Join(root, "sys", "bus", "pci", "devices", vgpuTestPCIBusID, "sriov_numvfs"),
		[]byte("0"), 0644))

	_, err := lib.createVgpuDevice(newVdevTestPartition(), nil)
	require.ErrorContains(t, err, "no SR-IOV VFs enabled")
}

func TestCreateVgpuDeviceVdevParamsRejected(t *testing.T) {
	root, _, _ := setupFakeVdevHost(t)
	lib := vgpuTestDeviceLib(root)

	_, err := lib.createVgpuDevice(newVdevTestPartition(), map[string]string{"x": "y"})
	require.ErrorContains(t, err, "not supported for the vdev framework")
}

func newVgpuLifecycleTestState(t *testing.T, sysfsRoot string) *DeviceState {
	t.Helper()
	state := newCleanupTestDeviceState(t, &Checkpoint{V2: &CheckpointV2{NodeBootID: "boot-vgpu-test"}})
	state.nvdevlib = vgpuTestDeviceLib(sysfsRoot)
	state.config = &Config{flags: &Flags{kubeletPluginsDirectoryPath: t.TempDir()}}
	return state
}

func TestPrepareUnprepareVgpuDeviceMarkers(t *testing.T) {
	root := setupFakeMdevHost(t, "4")
	state := newVgpuLifecycleTestState(t, root)

	concrete, err := state.prepareVgpuDevice("claim-1", newMdevTestPartition(), nil)
	require.NoError(t, err)

	// Ownership marker exists alongside the device.
	require.FileExists(t, filepath.Join(state.config.DriverPluginPath(), "vgpu-concrete", concrete.id()))

	devices := PreparedDeviceList{{
		Vgpu: &PreparedVgpuDevice{
			Concrete: concrete,
			Device:   &CheckpointedDevice{DeviceName: "vgpu-gpu-0-12q-0"},
		},
	}}
	require.NoError(t, state.unprepareVgpuDevices(devices))

	require.NoDirExists(t, filepath.Join(root, "sys", "bus", "mdev", "devices", concrete.MdevUUID))
	require.NoFileExists(t, filepath.Join(state.config.DriverPluginPath(), "vgpu-concrete", concrete.id()))
}

func TestCleanupVgpuMarkersForClaim(t *testing.T) {
	root := setupFakeMdevHost(t, "4")
	state := newVgpuLifecycleTestState(t, root)

	// Two claims each created a concrete device; the second claim's prepare
	// failed before the checkpoint commit.
	concrete1, err := state.prepareVgpuDevice("claim-1", newMdevTestPartition(), nil)
	require.NoError(t, err)
	concrete2, err := state.prepareVgpuDevice("claim-2", newMdevTestPartition(), nil)
	require.NoError(t, err)

	require.NoError(t, state.cleanupVgpuMarkersForClaim("claim-2"))

	// Only claim-2's device was destroyed.
	require.NoDirExists(t, filepath.Join(root, "sys", "bus", "mdev", "devices", concrete2.MdevUUID))
	require.DirExists(t, filepath.Join(root, "sys", "bus", "mdev", "devices", concrete1.MdevUUID))
	require.FileExists(t, filepath.Join(state.config.DriverPluginPath(), "vgpu-concrete", concrete1.id()))
}

func TestDestroyOrphanVgpuDevices(t *testing.T) {
	root := setupFakeMdevHost(t, "4")
	state := newVgpuLifecycleTestState(t, root)

	orphan, err := state.prepareVgpuDevice("claim-orphan", newMdevTestPartition(), nil)
	require.NoError(t, err)
	alive, err := state.prepareVgpuDevice("claim-alive", newMdevTestPartition(), nil)
	require.NoError(t, err)

	_, ctx := ktesting.NewTestContext(t)

	// Only claim-alive is checkpointed as fully prepared.
	require.NoError(t, state.updateCheckpoint(ctx, func(cp *Checkpoint) {
		if cp.V2.PreparedClaims == nil {
			cp.V2.PreparedClaims = PreparedClaimsByUID{}
		}
		cp.V2.PreparedClaims["claim-alive"] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			PreparedDevices: PreparedDevices{{
				Devices: PreparedDeviceList{{
					Vgpu: &PreparedVgpuDevice{
						Concrete: alive,
						Device:   &CheckpointedDevice{DeviceName: "vgpu-gpu-0-12q-1"},
					},
				}},
			}},
		}
	}))

	state.DestroyOrphanVgpuDevices(ctx)

	require.NoDirExists(t, filepath.Join(root, "sys", "bus", "mdev", "devices", orphan.MdevUUID))
	require.DirExists(t, filepath.Join(root, "sys", "bus", "mdev", "devices", alive.MdevUUID))
}

// TestPrepareVgpuDeviceClaimWithoutConfig exercises the full prepareDevices
// path for a vGPU claim that carries no opaque config: the default
// VgpuDeviceConfig must be picked up, and normalization/validation must
// recognize it. Regression test for "runtime object is not a recognized
// configuration".
func TestPrepareVgpuDeviceClaimWithoutConfig(t *testing.T) {
	setVGPUSupportGate(t, true)

	root := setupFakeMdevHost(t, "4")
	state := newVgpuLifecycleTestState(t, root)

	vfioCDIHandler, err := NewVfioCDIHandler(&deviceLib{hostRoot: t.TempDir()})
	require.NoError(t, err)
	state.cdi = &CDIHandler{vfiocdi: vfioCDIHandler}

	partition := newMdevTestPartition()
	state.perGPUAllocatable = &PerGPUAllocatableDevices{allocatablesMap: map[PCIBusID]AllocatableDevices{
		vgpuTestPCIBusID: {partition.CanonicalName(): {Vgpu: partition}},
	}}

	claim := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{UID: "uid-vgpu-1", Namespace: "default", Name: "claim-vgpu"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Driver: DriverName, Pool: "pool", Device: partition.CanonicalName(), Request: "r0",
					}},
				},
			},
		},
	}

	_, ctx := ktesting.NewTestContext(t)
	prepared, err := state.prepareDevices(ctx, claim, &Checkpoint{V2: &CheckpointV2{}})
	require.NoError(t, err)

	devs := prepared.GetDevices()
	require.Len(t, devs, 1)
	require.Equal(t, partition.CanonicalName(), devs[0].DeviceName)

	preparedGroupVgpu := prepared[0].Devices[0].Vgpu
	require.NotNil(t, preparedGroupVgpu)
	require.NotEmpty(t, preparedGroupVgpu.Concrete.MdevUUID)
	require.DirExists(t, filepath.Join(root, "sys", "bus", "mdev", "devices", preparedGroupVgpu.Concrete.MdevUUID))
}

func TestVgpuConcreteID(t *testing.T) {
	mdev := &VgpuConcrete{Framework: vgpuFrameworkMdev, MdevUUID: "uuid-1"}
	assert.Equal(t, "uuid-1", mdev.id())

	vdev := &VgpuConcrete{Framework: vgpuFrameworkVdev, VFPCIBusID: "0000:41:01.0"}
	assert.Equal(t, "0000:41:01.0", vdev.id())
}

func TestVgpuMetadataAttributes(t *testing.T) {
	partition := newMdevTestPartition()
	concrete := &VgpuConcrete{Framework: vgpuFrameworkMdev, MdevUUID: "uuid-x", TypeID: vgpuTestTypeID, Profile: "NVIDIA L40S-12Q"}

	attrs := vgpuMetadataAttributes(partition, concrete)
	require.Equal(t, "vgpu", *attrs["type"].StringValue)
	require.Equal(t, "NVIDIA L40S-12Q", *attrs["profile"].StringValue)
	require.Equal(t, "uuid-x", *attrs["mdevUUID"].StringValue)
	require.NotContains(t, attrs, "vfPCIBusID")

	concrete = &VgpuConcrete{Framework: vgpuFrameworkVdev, VFPCIBusID: "0000:41:01.3"}
	attrs = vgpuMetadataAttributes(partition, concrete)
	// KubeVirt expects `pciBusID` (the VF's address) for the vdev framework.
	require.Equal(t, "0000:41:01.3", *attrs["pciBusID"].StringValue)
	require.NotContains(t, attrs, "mdevUUID")
}

func TestPreparedVgpuCheckpointRoundtrip(t *testing.T) {
	status, devices := nonEmptyClaimPayload()

	devices = append(devices, &PreparedDeviceGroup{
		Devices: PreparedDeviceList{{
			Vgpu: &PreparedVgpuDevice{
				Concrete: &VgpuConcrete{
					Framework:      vgpuFrameworkMdev,
					TypeID:         vgpuTestTypeID,
					Profile:        "NVIDIA L40S-12Q",
					ParentPCIBusID: vgpuTestPCIBusID,
					MdevUUID:       "uuid-roundtrip",
				},
				Device: &CheckpointedDevice{DeviceName: "vgpu-gpu-0-12q-0"},
			},
		}},
	})

	state := newVgpuLifecycleTestState(t, "")
	_, ctx := ktesting.NewTestContext(t)

	require.NoError(t, state.updateCheckpoint(ctx, func(cp *Checkpoint) {
		if cp.V2.PreparedClaims == nil {
			cp.V2.PreparedClaims = PreparedClaimsByUID{}
		}
		cp.V2.PreparedClaims["uid-vgpu"] = PreparedClaim{
			CheckpointState: ClaimCheckpointStatePrepareCompleted,
			Status:          status,
			PreparedDevices: devices,
		}
	}))

	loaded, err := state.getCheckpoint(ctx)
	require.NoError(t, err)

	pc := loaded.V2.PreparedClaims["uid-vgpu"]
	require.Len(t, pc.PreparedDevices, 2)
	vgpu := pc.PreparedDevices[1].Devices[0].Vgpu
	require.NotNil(t, vgpu)
	require.Equal(t, "uuid-roundtrip", vgpu.Concrete.MdevUUID)
	require.Equal(t, uint32(vgpuTestTypeID), vgpu.Concrete.TypeID)
}

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
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"

	configapi "sigs.k8s.io/dra-driver-nvidia-gpu/api/nvidia.com/resource/v1beta1"
)

// VgpuConcrete identifies the concrete vGPU device created for a claim at
// Prepare time: either a mediated device (mdev UUID) or, for SR-IOV-backed
// vGPU, a VF whose vGPU type was programmed. Serialized to the checkpoint.
type VgpuConcrete struct {
	// Framework is "mdev" or "vdev" (the vgpuFramework* constants).
	Framework string `json:"framework"`
	// TypeID and Profile describe what was created (for diagnostics and for
	// validating checkpointed state against republished partitions).
	TypeID  uint32 `json:"typeID"`
	Profile string `json:"profile"`

	// ParentPCIBusID is the BDF of the physical GPU PF hosting this device.
	ParentPCIBusID string `json:"parentPCIBusID"`

	// MdevUUID is the mediated device UUID (mdev framework only).
	MdevUUID string `json:"mdevUUID,omitempty"`
	// VFPCIBusID is the BDF of the programmed VF (vdev framework only).
	VFPCIBusID string `json:"vfPCIBusID,omitempty"`
}

// id returns the stable host-level identifier of the concrete device, used
// for ownership marker file names.
func (c *VgpuConcrete) id() string {
	if c.Framework == vgpuFrameworkVdev {
		return c.VFPCIBusID
	}
	return c.MdevUUID
}

// vgpuWriteFile is a seam for tests: on real hosts, writing a UUID to an
// mdev type's sysfs "create" file makes the kernel instantiate device state
// (a side effect a plain temp filesystem cannot emulate); likewise writing
// to "remove" tears it down.
var vgpuWriteFile = os.WriteFile

// vgpuMdevSysfsTypeName matches the NVIDIA vGPU Manager's mdev type naming
// convention: mdev_supported_types directory entries are named
// "nvidia-<typeID>".
func vgpuMdevSysfsTypeName(typeID uint32) string {
	return fmt.Sprintf("nvidia-%d", typeID)
}

// sysfsPciDeviceDir returns the sysfs directory of the PCI device with the
// given BDF, rooted at sysfsRoot (the host's sysfs).
func sysfsPciDeviceDir(sysfsRoot string, bdf string) string {
	return filepath.Join(sysfsRoot, "sys", "bus", "pci", "devices", bdf)
}

// sysfsMdevDeviceDir returns the sysfs directory of a potential mediated
// device instance.
func sysfsMdevDeviceDir(sysfsRoot string, devUUID string) string {
	return filepath.Join(sysfsRoot, "sys", "bus", "mdev", "devices", devUUID)
}

// Sysfs attribute names used by the NVIDIA vGPU Manager. Under the
// mediated ("mdev") framework: mdev_supported_types/nvidia-<typeID>/{create,
// available_instances} on the PF (legacy GPUs) or on a VF (SR-IOV GPUs), and
// one instance dir per device under /sys/bus/mdev/devices/<uuid>. Under the
// vendor-specific VFIO ("vdev") framework: per-VF attributes in
// <VF>/nvidia/{current_vgpu_type, creatable_vgpu_types}.
const (
	// vgpuCurrentTypeFile holds the vGPU type ID currently created on a VF;
	// 0 means "no vGPU". Naming follows NVIDIA's vendor-specific VFIO docs.
	vgpuCurrentTypeFile = "current_vgpu_type"
	// vgpuCreatableTypesFile lists the vGPU type names and sysfs type IDs
	// creatable on a VF ("NVIDIA A40-2Q    558"); it becomes empty once the
	// one permitted vGPU has been created on that VF.
	vgpuCreatableTypesFile = "creatable_vgpu_types"
)

// createVgpuDevice creates the concrete vGPU device for an allocated
// partition: an mdev instance under the mdev framework, or a programmed VF
// under the vdev (SR-IOV) framework. Returns the concrete identity for
// checkpointing. Callers must serialize create/delete against Unprepare
// (the driver's global prepare/unprepare lock guarantees this).
func (l deviceLib) createVgpuDevice(partition *VgpuPartitionInfo, params map[string]string) (*VgpuConcrete, error) {
	concrete := &VgpuConcrete{
		Framework:      partition.Framework,
		TypeID:         partition.Profile.TypeID,
		Profile:        partition.Profile.Name,
		ParentPCIBusID: partition.Parent.pciBusID,
	}

	sysfsRoot := l.sysfsRoot
	if sysfsRoot == "" {
		sysfsRoot = "/"
	}

	var err error
	switch partition.Framework {
	case vgpuFrameworkMdev:
		err = l.createMdevVgpu(sysfsRoot, concrete, params)
	case vgpuFrameworkVdev:
		err = l.setVFVgpuType(sysfsRoot, concrete, params)
	default:
		err = fmt.Errorf("unknown vGPU framework %q", partition.Framework)
	}
	if err != nil {
		return nil, err
	}

	return concrete, nil
}

// deleteVgpuDevice destroys a concrete vGPU device previously created by
// createVgpuDevice. Idempotent: a device that no longer exists is already
// gone, which is the correct outcome for retried Unprepare calls.
func (l deviceLib) deleteVgpuDevice(concrete *VgpuConcrete) error {
	sysfsRoot := l.sysfsRoot
	if sysfsRoot == "" {
		sysfsRoot = "/"
	}

	switch concrete.Framework {
	case vgpuFrameworkMdev:
		return l.deleteMdevVgpu(sysfsRoot, concrete)
	case vgpuFrameworkVdev:
		return l.clearVFVgpuType(sysfsRoot, concrete)
	}
	return fmt.Errorf("unknown vGPU framework %q", concrete.Framework)
}

// createMdevVgpu creates a mediated device of the given vGPU type on the
// parent physical GPU. On legacy (non-SR-IOV) GPUs the vGPU Manager exposes
// mdev_supported_types on the PF; on SR-IOV GPUs using the mdev framework it
// exposes them per VF, with at most one vGPU per VF. Creation is the single
// write of a fresh UUID to the type's sysfs "create" file; params are then
// applied to the instance's vgpu_params file when the vGPU Manager exposes
// one.
func (l deviceLib) createMdevVgpu(sysfsRoot string, concrete *VgpuConcrete, params map[string]string) error {
	typeDir, vfBusID, err := l.findFreeMdevTypeDir(sysfsRoot, concrete)
	if err != nil {
		return err
	}
	concrete.VFPCIBusID = vfBusID // empty for PF-hosted mdev

	devUUID := uuid.New().String()
	createPath := filepath.Join(typeDir, "create")
	klog.V(4).Infof("Creating vGPU instance (type %q, mdev UUID %s) on GPU %q (type dir %s)",
		concrete.Profile, devUUID, concrete.ParentPCIBusID, typeDir)
	if err := vgpuWriteFile(createPath, []byte(devUUID), 0200); err != nil {
		return fmt.Errorf("error creating vGPU instance of type %q on GPU %q: %w",
			concrete.Profile, concrete.ParentPCIBusID, err)
	}

	deviceDir := sysfsMdevDeviceDir(sysfsRoot, devUUID)
	if _, err := os.Lstat(deviceDir); err != nil {
		return fmt.Errorf("created vGPU instance %s not visible in sysfs at %q: %w", devUUID, deviceDir, err)
	}
	concrete.MdevUUID = devUUID

	if err := applyVgpuParams(deviceDir, params); err != nil {
		// Roll back the just-created instance: a partition created with
		// partial parameters must not escape Prepare.
		if derr := l.deleteMdevVgpu(sysfsRoot, concrete); derr != nil {
			klog.Warningf("failed to roll back vGPU instance %s after param application failure: %v", devUUID, derr)
		}
		return err
	}

	return nil
}

// findFreeMdevTypeDir locates the mdev_supported_types/<type> directory on
// which to create the vGPU: the PF's directory for legacy GPUs, or a free
// VF's directory for SR-IOV mdev hosts (a VF is free exactly when its type
// dir reports one available instance, per the NVIDIA vGPU docs). Returns the
// directory and, for VF-hosted mdev, the VF's PCI bus ID.
func (l deviceLib) findFreeMdevTypeDir(sysfsRoot string, concrete *VgpuConcrete) (string, string, error) {
	typeName := vgpuMdevSysfsTypeName(concrete.TypeID)
	pfDir := sysfsPciDeviceDir(sysfsRoot, concrete.ParentPCIBusID)

	pfTypeDir := filepath.Join(pfDir, "mdev_supported_types", typeName)
	if avail, err := l.mdevAvailableInstances(pfTypeDir); err == nil {
		if avail < 1 {
			return "", "", fmt.Errorf("no available instances of vGPU type %q on GPU %q", concrete.Profile, concrete.ParentPCIBusID)
		}
		return pfTypeDir, "", nil
	}

	// Not on the PF: SR-IOV mdev. Find a VF whose type dir reports the type
	// as creatable with its single allowed instance still available.
	for _, vf := range vgpuVFsOfPF(pfDir) {
		vfTypeDir := filepath.Join(sysfsPciDeviceDir(sysfsRoot, vf), "mdev_supported_types", typeName)
		avail, err := l.mdevAvailableInstances(vfTypeDir)
		if err != nil {
			continue // this VF cannot host this type
		}
		if avail == 1 {
			return vfTypeDir, vf, nil
		}
	}

	return "", "", fmt.Errorf("vGPU type %q (%s) not found or busy on GPU %q and its VFs (vGPU Manager may not be running, the type may not be creatable, or all VFs are occupied)",
		concrete.Profile, typeName, concrete.ParentPCIBusID)
}

// deleteMdevVgpu removes a mediated device instance. Writing "1" to the
// instance's "remove" file is sufficient on all vGPU Manager versions.
func (l deviceLib) deleteMdevVgpu(sysfsRoot string, concrete *VgpuConcrete) error {
	if concrete.MdevUUID == "" {
		return fmt.Errorf("cannot delete mdev-based vGPU device: no mdev UUID recorded")
	}
	removePath := filepath.Join(sysfsMdevDeviceDir(sysfsRoot, concrete.MdevUUID), "remove")
	if _, err := os.Lstat(removePath); err != nil {
		if os.IsNotExist(err) {
			klog.V(4).Infof("vGPU instance %s already gone; nothing to delete", concrete.MdevUUID)
			return nil
		}
		return err
	}
	return vgpuWriteFile(removePath, []byte("1"), 0200)
}

func (l deviceLib) mdevAvailableInstances(typeDir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(typeDir, "available_instances"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// applyVgpuParams writes parameters into the instance's vgpu_params sysfs
// file (one "key=value" per write), if the vGPU Manager exposes one.
func applyVgpuParams(deviceDir string, params map[string]string) error {
	if len(params) == 0 {
		return nil
	}
	paramsPath := filepath.Join(deviceDir, "nvidia", "vgpu_params")
	if _, err := os.Lstat(paramsPath); err != nil {
		return fmt.Errorf("vgpu_params file not available for vGPU instance %q: %w", deviceDir, err)
	}
	for key, value := range params {
		klog.V(4).Infof("Setting vGPU param %q=%q on %s", key, value, deviceDir)
		if err := os.WriteFile(paramsPath, []byte(fmt.Sprintf("%s=%s", key, value)), 0200); err != nil {
			return fmt.Errorf("error setting vGPU param %q on %s: %w", key, deviceDir, err)
		}
	}
	return nil
}

// setVFVgpuType programs the given vGPU type onto a free VF of the parent
// GPU's PF (vendor-specific VFIO, or "vdev", framework; NVIDIA vGPU Manager
// on RHEL 10 KVM / Ubuntu 24.04+ hypervisors). Per the NVIDIA vGPU user
// guide, a VF is free when its <VF>/nvidia/current_vgpu_type reads 0, the
// requested type must appear in the VF's creatable_vgpu_types, and VFs must
// be enabled beforehand with NVIDIA's sriov-manage script (Alpha: admin
// pre-slice, docs/design/vgpu-support.md FR-14).
func (l deviceLib) setVFVgpuType(sysfsRoot string, concrete *VgpuConcrete, params map[string]string) error {
	if len(params) > 0 {
		return fmt.Errorf("vgpu params are not supported for the vdev framework (profile %q)", concrete.Profile)
	}

	pfDir := sysfsPciDeviceDir(sysfsRoot, concrete.ParentPCIBusID)

	numvfs, err := l.sriovNumVFs(pfDir)
	if err != nil {
		return err
	}
	if numvfs == 0 {
		return fmt.Errorf("no SR-IOV VFs enabled on GPU %q; enable them on the host with NVIDIA's sriov-manage script (/usr/lib/nvidia/sriov-manage -e %s)", concrete.ParentPCIBusID, concrete.ParentPCIBusID)
	}

	vfs, err := l.listVFs(pfDir)
	if err != nil {
		return err
	}

	for _, vf := range vfs {
		nvidiaDir := vgpuVFNVidiaDir(sysfsRoot, vf)
		typePath := filepath.Join(nvidiaDir, vgpuCurrentTypeFile)

		current, err := readTrimmed(typePath)
		if err != nil {
			// The vGPU Manager only exposes these attributes on VFs it
			// manages; skip VFs without them.
			continue
		}
		if current != "" && current != "0" {
			continue // a vGPU already lives on this VF
		}

		// The sysfs type ID must be creatable on this VF; writing an invalid
		// or uncreatable ID fails and resets current_vgpu_type to 0
		// (documented behavior), so validate first for a clear error.
		creatable, err := l.vgpuTypeCreatableOnVF(nvidiaDir, concrete)
		if err != nil || !creatable {
			continue
		}

		if err := vgpuWriteFile(typePath, []byte(fmt.Sprintf("%d", concrete.TypeID)), 0200); err != nil {
			return fmt.Errorf("error setting vGPU type %d (%q) on VF %q: %w", concrete.TypeID, concrete.Profile, vf, err)
		}
		got, err := readTrimmed(typePath)
		if err != nil || got != strconv.Itoa(int(concrete.TypeID)) {
			return fmt.Errorf("verify failed: VF %q reports %s=%q after programming type %d (%q)", vf, vgpuCurrentTypeFile, got, concrete.TypeID, concrete.Profile)
		}

		klog.V(4).Infof("Programmed vGPU type %q (id %d) on VF %s of GPU %q", concrete.Profile, concrete.TypeID, vf, concrete.ParentPCIBusID)
		concrete.VFPCIBusID = vf
		return nil
	}

	return fmt.Errorf("no free VF with a creatable vGPU type %q (id %d) found on GPU %q", concrete.Profile, concrete.TypeID, concrete.ParentPCIBusID)
}

// vgpuTypeCreatableOnVF reads the VF's creatable_vgpu_types file and reports
// whether the concrete's vGPU type is creatable there. The file contains
// lines of the form "NVIDIA A40-2Q         558"; match on the profile name
// AND the integer ID to guard against name/ID skew between NVML and sysfs.
func (l deviceLib) vgpuTypeCreatableOnVF(nvidiaDir string, concrete *VgpuConcrete) (bool, error) {
	data, err := os.ReadFile(filepath.Join(nvidiaDir, vgpuCreatableTypesFile))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		id, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		name := strings.Join(fields[:len(fields)-1], " ")
		if uint32(id) == concrete.TypeID && name == concrete.Profile {
			return true, nil
		}
	}
	return false, nil
}

// clearVFVgpuType resets a VF's vGPU type to 0 (no vGPU), the documented
// means of deleting a vGPU under the vendor-specific VFIO framework.
// Idempotent so retried Unprepare calls are safe.
func (l deviceLib) clearVFVgpuType(sysfsRoot string, concrete *VgpuConcrete) error {
	if concrete.VFPCIBusID == "" {
		return fmt.Errorf("cannot clear vdev-based vGPU device: no VF PCI bus ID recorded")
	}
	typePath := filepath.Join(vgpuVFNVidiaDir(sysfsRoot, concrete.VFPCIBusID), vgpuCurrentTypeFile)
	if _, err := os.Lstat(typePath); err != nil {
		if os.IsNotExist(err) {
			klog.V(4).Infof("VF %s has no %s attribute; vGPU already cleared", concrete.VFPCIBusID, vgpuCurrentTypeFile)
			return nil
		}
		return err
	}
	return vgpuWriteFile(typePath, []byte("0"), 0200)
}

func (l deviceLib) sriovNumVFs(pfDir string) (int, error) {
	data, err := os.ReadFile(filepath.Join(pfDir, "sriov_numvfs"))
	if err != nil {
		return 0, fmt.Errorf("cannot read SR-IOV state of %q: %w", pfDir, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// listVFs returns the PCI bus IDs of all VFs of the PF, by resolving the
// PF's virtfn* symlinks.
func (l deviceLib) listVFs(pfDir string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(pfDir, "virtfn*"))
	if err != nil {
		return nil, err
	}
	var vfs []string
	for _, symlink := range entries {
		target, err := filepath.EvalSymlinks(symlink)
		if err != nil {
			return nil, fmt.Errorf("error resolving VF symlink %q: %w", symlink, err)
		}
		vfs = append(vfs, filepath.Base(target))
	}
	return vfs, nil
}

func readTrimmed(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// prepareVgpuDevice creates the concrete vGPU device for an allocated
// partition and records it in the ownership-marker registry. Return values
// are checkpointed by the caller.
func (s *DeviceState) prepareVgpuDevice(claimUID string, partition *VgpuPartitionInfo, config *configapi.VgpuDeviceConfig) (*VgpuConcrete, error) {
	var params map[string]string
	if config != nil {
		params = config.Params
	}

	concrete, err := s.nvdevlib.createVgpuDevice(partition, params)
	if err != nil {
		return nil, err
	}
	if err := s.writeVgpuConcreteMarker(claimUID, concrete); err != nil {
		// The device exists but would be untraceable across restarts; roll
		// it back rather than leak it.
		if derr := s.nvdevlib.deleteVgpuDevice(concrete); derr != nil {
			klog.Warningf("failed to roll back vGPU device %s after marker write failure: %v", concrete.id(), derr)
		}
		return nil, fmt.Errorf("error recording vGPU device %q: %w", concrete.id(), err)
	}
	return concrete, nil
}

// unprepareVgpuDevices destroys the concrete vGPU devices created for a
// claim. Retried Unprepare is safe: deletions are idempotent and markers
// are only removed after the host operation succeeded.
func (s *DeviceState) unprepareVgpuDevices(devices PreparedDeviceList) error {
	for _, device := range devices {
		concrete := device.Vgpu.Concrete
		if err := s.nvdevlib.deleteVgpuDevice(concrete); err != nil {
			return fmt.Errorf("error deleting vGPU device %s: %w", concrete.id(), err)
		}
		if err := s.removeVgpuConcreteMarker(concrete); err != nil {
			return fmt.Errorf("error removing vGPU marker for %s: %w", concrete.id(), err)
		}
	}
	return nil
}

// vgpuMetadataAttributes builds the Device Metadata attributes (KEP-5304)
// for a prepared vGPU device: the partition's published attributes plus the
// concrete identity that KubeVirt binds. Key names follow KubeVirt's
// convention: `mdevUUID` for the mdev framework and `pciBusID` (the VF's
// address, not the parent's) for the vendor-VFIO framework.
func vgpuMetadataAttributes(partition *VgpuPartitionInfo, concrete *VgpuConcrete) map[string]resourceapi.DeviceAttribute {
	attrs := make(map[string]resourceapi.DeviceAttribute)
	for k, v := range partition.Attributes() {
		attrs[string(k)] = v
	}
	if concrete.MdevUUID != "" {
		attrs["mdevUUID"] = resourceapi.DeviceAttribute{StringValue: &concrete.MdevUUID}
	}
	if concrete.VFPCIBusID != "" {
		attrs["pciBusID"] = resourceapi.DeviceAttribute{StringValue: &concrete.VFPCIBusID}
	}
	return attrs
}

// --- ownership markers ----------------------------------------------------
//
// The concrete vGPU device (mdev UUID / programmed VF) exists before the
// checkpoint transitions to PrepareCompleted. A crash or failed Prepare in
// between leaves an orphan that the empty checkpoint cannot name. Marker
// files under the plugin directory record concrete devices the moment they
// exist, keyed by claim UID, so that a restarted plugin (or a Prepare retry
// for the same claim) can destroy devices it created but never committed to
// the checkpoint. Unknown mdevs created out-of-band by an administrator are
// untouched: they simply have no marker file.

type vgpuConcreteMarker struct {
	ClaimUID string        `json:"claimUID"`
	Concrete *VgpuConcrete `json:"concrete"`
}

func (s *DeviceState) vgpuMarkerDir() string {
	return filepath.Join(s.config.DriverPluginPath(), "vgpu-concrete")
}

func (s *DeviceState) writeVgpuConcreteMarker(claimUID string, concrete *VgpuConcrete) error {
	if err := os.MkdirAll(s.vgpuMarkerDir(), 0750); err != nil {
		return err
	}
	marker := vgpuConcreteMarker{ClaimUID: claimUID, Concrete: concrete}
	data, err := json.Marshal(&marker)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.vgpuMarkerDir(), concrete.id()), data, 0600)
}

func (s *DeviceState) removeVgpuConcreteMarker(concrete *VgpuConcrete) error {
	err := os.Remove(filepath.Join(s.vgpuMarkerDir(), concrete.id()))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *DeviceState) readVgpuConcreteMarkers() ([]*vgpuConcreteMarker, error) {
	entries, err := os.ReadDir(s.vgpuMarkerDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var markers []*vgpuConcreteMarker
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.vgpuMarkerDir(), e.Name()))
		if err != nil {
			return nil, fmt.Errorf("error reading vGPU marker %q: %w", e.Name(), err)
		}
		var m vgpuConcreteMarker
		if err := json.Unmarshal(data, &m); err != nil {
			klog.Warningf("Ignoring unreadable vGPU marker %q: %v", e.Name(), err)
			continue
		}
		markers = append(markers, &m)
	}
	return markers, nil
}

// cleanupVgpuMarkersForClaim deletes concrete vGPU devices recorded for the
// given claim by a previous, failed Prepare attempt. Only devices created by
// a previous attempt of THIS claim are freed; nothing else on the host is
// touched.
func (s *DeviceState) cleanupVgpuMarkersForClaim(claimUID string) error {
	markers, err := s.readVgpuConcreteMarkers()
	if err != nil {
		return err
	}
	for _, m := range markers {
		if m.ClaimUID != claimUID {
			continue
		}
		if err := s.nvdevlib.deleteVgpuDevice(m.Concrete); err != nil {
			return fmt.Errorf("error deleting leftover vGPU device %s of claim %s: %w", m.Concrete.id(), claimUID, err)
		}
		if err := s.removeVgpuConcreteMarker(m.Concrete); err != nil {
			return fmt.Errorf("error removing vGPU marker for %s: %w", m.Concrete.id(), err)
		}
	}
	return nil
}

// DestroyOrphanVgpuDevices destroys concrete vGPU devices that this driver
// created (proven by a marker file) but that are not referenced by any
// checkpointed claim in PrepareCompleted state. It runs once at startup,
// mirroring the MIG orphan cleanup. Out-of-band (admin-created) mdevs have
// no marker and are deliberately never touched.
func (s *DeviceState) DestroyOrphanVgpuDevices(ctx context.Context) {
	logpfx := "Destroy orphan vGPU devices"
	markers, err := s.readVgpuConcreteMarkers()
	if err != nil {
		klog.Errorf("%s: unable to read markers: %s", logpfx, err)
		return
	}
	if len(markers) == 0 {
		return
	}

	cp, err := s.getCheckpoint(ctx)
	if err != nil {
		klog.Errorf("%s: unable to get checkpoint: %s", logpfx, err)
		return
	}

	// Collect concrete device IDs owned by completed claims.
	alive := make(map[string]bool)
	for _, claim := range cp.V2.PreparedClaims {
		if claim.CheckpointState != ClaimCheckpointStatePrepareCompleted {
			continue
		}
		for _, group := range claim.PreparedDevices {
			for _, dev := range group.Devices {
				if dev.Vgpu != nil && dev.Vgpu.Concrete != nil {
					alive[dev.Vgpu.Concrete.id()] = true
				}
			}
		}
	}

	for _, m := range markers {
		if alive[m.Concrete.id()] {
			continue
		}
		klog.Infof("%s: destroying %s (marker claim %s)", logpfx, m.Concrete.id(), m.ClaimUID)
		if err := s.nvdevlib.deleteVgpuDevice(m.Concrete); err != nil {
			klog.Errorf("%s: failed to delete %s (leaving marker for retry): %s", logpfx, m.Concrete.id(), err)
			continue
		}
		if err := s.removeVgpuConcreteMarker(m.Concrete); err != nil {
			klog.Errorf("%s: failed to remove marker for %s: %s", logpfx, m.Concrete.id(), err)
		}
	}
}

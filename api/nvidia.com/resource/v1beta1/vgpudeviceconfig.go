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

package v1beta1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// VgpuDeviceConfig holds the set of parameters for configuring a vGPU
// partition device at Prepare time. Profile identity primarily comes from
// the allocated partition device itself; this config carries optional
// overrides and host knobs (see docs/design/vgpu-support.md, section 6.4).
type VgpuDeviceConfig struct {
	metav1.TypeMeta `json:",inline"`

	// Profile optionally pins the expected vGPU type name (e.g.
	// "NVIDIA L40S-12Q"). When set, it must match the profile of every
	// allocated device this config applies to; identity-by-selector
	// (CEL on the device `profile` attribute) is the preferred mechanism.
	Profile string `json:"profile,omitempty"`

	// ProfileID optionally pins the numeric vGPU type ID. When set, it must
	// match the type ID of every allocated device this config applies to.
	ProfileID *int `json:"profileID,omitempty"`

	// Params are opaque host vGPU parameters (vgpu_params), applied to the
	// concrete device created at Prepare time, e.g. frame rate limiter or
	// staging buffer knobs accepted by the NVIDIA vGPU Manager.
	Params map[string]string `json:"params,omitempty"`
}

// DefaultVgpuDeviceConfig provides the default configuration of a vGPU
// device. Returns nil when the VGPUSupport feature gate is disabled.
func DefaultVgpuDeviceConfig() *VgpuDeviceConfig {
	if !featuregates.Enabled(featuregates.VGPUSupport) {
		return nil
	}
	return &VgpuDeviceConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: GroupName + "/" + Version,
			Kind:       VgpuDeviceConfigKind,
		},
	}
}

// Normalize updates a VgpuDeviceConfig with implied default values based on
// other settings. There are currently no defaults to apply.
func (c *VgpuDeviceConfig) Normalize() error {
	return nil
}

// Validate ensures that VgpuDeviceConfig has a valid set of values.
func (c *VgpuDeviceConfig) Validate() error {
	if c.ProfileID != nil && *c.ProfileID < 0 {
		return fmt.Errorf("profileID must be non-negative, got %d", *c.ProfileID)
	}
	for key := range c.Params {
		if key == "" {
			return fmt.Errorf("params must not contain empty keys")
		}
	}
	return nil
}

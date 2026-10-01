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

package v1beta1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/dra-driver-nvidia-gpu/pkg/featuregates"
)

func TestVgpuDeviceConfigValidate(t *testing.T) {
	one := 1
	negative := -3

	tests := map[string]struct {
		config      *VgpuDeviceConfig
		expectError bool
	}{
		"empty config is valid": {
			config: &VgpuDeviceConfig{},
		},
		"full config is valid": {
			config: &VgpuDeviceConfig{
				Profile: "NVIDIA L40S-12Q",
				TypeID:  ptr.To(1177),
				Params:  map[string]string{"frame_rate_limiter": "0"},
			},
		},
		"zero typeID allowed": {
			config: &VgpuDeviceConfig{TypeID: &[]int{0}[0]},
		},
		"negative typeID rejected": {
			config:      &VgpuDeviceConfig{TypeID: &negative},
			expectError: true,
		},
		"empty param key rejected": {
			config:      &VgpuDeviceConfig{Params: map[string]string{"": "0"}},
			expectError: true,
		},
		"one-pass sanity": {
			config: &VgpuDeviceConfig{TypeID: &one},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, tc.config.Normalize())
			err := tc.config.Validate()
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestVgpuDeviceConfigStrictDecoding(t *testing.T) {
	raw := map[string]interface{}{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind":       "VgpuDeviceConfig",
		"profile":    "NVIDIA L40S-12Q",
		"typeID":     1177,
		"params":     map[string]string{"frame_rate_limiter": "0"},
	}
	data, err := json.Marshal(raw)
	require.NoError(t, err)

	decoded, err := runtime.Decode(StrictDecoder, data)
	require.NoError(t, err)
	config, ok := decoded.(*VgpuDeviceConfig)
	require.True(t, ok)
	assert.Equal(t, "NVIDIA L40S-12Q", config.Profile)
	assert.Equal(t, 1177, *config.TypeID)
	assert.Equal(t, "0", config.Params["frame_rate_limiter"])

	// Unknown fields must be rejected by the strict decoder.
	raw["unknownField"] = "x"
	data, err = json.Marshal(raw)
	require.NoError(t, err)
	_, err = runtime.Decode(StrictDecoder, data)
	require.Error(t, err)

	// ...and tolerated by the non-strict (checkpoint compat) decoder.
	decoded, err = runtime.Decode(NonstrictDecoder, data)
	require.NoError(t, err)
	_, ok = decoded.(*VgpuDeviceConfig)
	assert.True(t, ok)
}

func TestDefaultVgpuDeviceConfigGateBehavior(t *testing.T) {
	require.Nil(t, DefaultVgpuDeviceConfig(), "disabled by default")

	require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
		string(featuregates.VGPUSupport): true,
	}))
	t.Cleanup(func() {
		require.NoError(t, featuregates.FeatureGates().SetFromMap(map[string]bool{
			string(featuregates.VGPUSupport): false,
		}))
	})

	cfg := DefaultVgpuDeviceConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, GroupName+"/"+Version, cfg.APIVersion)
	assert.Equal(t, VgpuDeviceConfigKind, cfg.Kind)
}

/*
Copyright 2026.

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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Design note — Spec vs Status:
// Spec is *intent* (what the fleet operator wants), Status is *fact* (what the
// device actually reports). Nobody except the device agent may write Status,
// and nobody except users/rollout logic may write Spec. Reconcile is the
// engine that drives fact toward intent; because the two are separate
// subresources, their writers never race each other on the same fields.

// DevicePhase is a coarse, at-a-glance lifecycle summary. Fine-grained detail
// (why, since when, which step) lives in Conditions; Phase exists so that
// `kubectl get edgedevices` tells a story in one column.
type DevicePhase string

const (
	// PhaseProvisioning: the CR exists but the device has never reported in.
	PhaseProvisioning DevicePhase = "Provisioning"
	// PhaseReady: device heartbeat is fresh and firmware matches intent.
	PhaseReady DevicePhase = "Ready"
	// PhaseUpgrading: device is executing a firmware transaction.
	PhaseUpgrading DevicePhase = "Upgrading"
	// PhaseDegraded: heartbeat timed out, or the last upgrade failed.
	PhaseDegraded DevicePhase = "Degraded"
)

// Condition types recorded in Status.Conditions.
const (
	// ConditionHealthy tracks heartbeat freshness (set by the operator).
	ConditionHealthy = "Healthy"
	// ConditionFirmwareSynced tracks whether current == desired firmware
	// (set by the agent after a completed/failed upgrade transaction).
	ConditionFirmwareSynced = "FirmwareSynced"
)

// Slot names for the A/B partition scheme.
const (
	SlotA = "A"
	SlotB = "B"
)

// EdgeDeviceSpec is the desired state: pure intent, no runtime facts.
type EdgeDeviceSpec struct {
	// desiredFirmwareVersion is the firmware the device should converge to.
	// +kubebuilder:validation:MinLength=1
	DesiredFirmwareVersion string `json:"desiredFirmwareVersion"`

	// firmwareURL is where the agent downloads the firmware image from.
	// +kubebuilder:validation:Pattern=`^https?://`
	FirmwareURL string `json:"firmwareURL"`

	// checksumSHA256 is the expected digest of the firmware image. The agent
	// refuses to flash anything that does not match (brick safety).
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	ChecksumSHA256 string `json:"checksumSHA256"`

	// region is a placement/config fragment used by rollout ordering.
	// +optional
	Region string `json:"region,omitempty"`

	// deviceLabels are opaque config key/values pushed to the device
	// (kept separate from metadata.labels, which belong to K8s tooling).
	// +optional
	DeviceLabels map[string]string `json:"deviceLabels,omitempty"`

	// rolloutPaused freezes upgrades for this device. The agent must not
	// start a new firmware transaction while true; an in-flight transaction
	// is allowed to finish (pausing mid-flash is how you brick hardware).
	// +optional
	RolloutPaused bool `json:"rolloutPaused,omitempty"`
}

// EdgeDeviceStatus is observed fact, written by the agent (firmware fields)
// and the operator (health fields). It must be reconstructible: wiping Status
// and letting the device report in again should converge to the same values.
type EdgeDeviceStatus struct {
	// phase is the coarse lifecycle summary; details live in conditions.
	// +kubebuilder:validation:Enum=Provisioning;Ready;Upgrading;Degraded
	// +optional
	Phase DevicePhase `json:"phase,omitempty"`

	// conditions carry the fine-grained "why" behind phase transitions.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// currentFirmwareVersion is what the device is actually running.
	// +optional
	CurrentFirmwareVersion string `json:"currentFirmwareVersion,omitempty"`

	// activeSlot is the A/B partition currently booted ("A" or "B").
	// +optional
	ActiveSlot string `json:"activeSlot,omitempty"`

	// lastHeartbeat is the last time the agent reported in. The operator
	// compares this against a timeout to flip the device to Degraded.
	// +optional
	LastHeartbeat *metav1.Time `json:"lastHeartbeat,omitempty"`

	// observedGeneration is the metadata.generation the agent last acted on.
	// It answers "is this Status about the *current* Spec, or a stale one?" —
	// generation bumps only on Spec changes, so a consumer can tell whether
	// the device has even seen the latest intent yet.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredFirmwareVersion`
// +kubebuilder:printcolumn:name="Current",type=string,JSONPath=`.status.currentFirmwareVersion`
// +kubebuilder:printcolumn:name="Slot",type=string,JSONPath=`.status.activeSlot`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.rolloutPaused`
// +kubebuilder:printcolumn:name="Heartbeat",type=date,JSONPath=`.status.lastHeartbeat`

// EdgeDevice is the Schema for the edgedevices API
type EdgeDevice struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of EdgeDevice
	// +required
	Spec EdgeDeviceSpec `json:"spec"`

	// status defines the observed state of EdgeDevice
	// +optional
	Status EdgeDeviceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// EdgeDeviceList contains a list of EdgeDevice
type EdgeDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []EdgeDevice `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &EdgeDevice{}, &EdgeDeviceList{})
		return nil
	})
}

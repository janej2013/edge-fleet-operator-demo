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

// RolloutPhase summarizes a fleet-wide firmware campaign.
type RolloutPhase string

const (
	RolloutProgressing RolloutPhase = "Progressing"
	// RolloutPaused means the circuit breaker tripped: the failure rate
	// crossed the threshold and the rollout stopped itself. Resuming is a
	// human decision (fix the cause, unpause the devices) — a controller
	// that un-trips its own breaker has no breaker at all.
	RolloutPaused   RolloutPhase = "Paused"
	RolloutComplete RolloutPhase = "Complete"
)

// Condition types on FleetRollout.
const (
	ConditionBreakerTripped = "BreakerTripped"
)

// FleetRolloutSpec describes a staged firmware campaign over a device group.
//
// The rollout controller writes the *device* Spec (desired version); the
// device operator + agent do the actual work. This layering mirrors
// Deployment → ReplicaSet → Pod: each controller owns exactly one level of
// abstraction and talks only to the next one down.
type FleetRolloutSpec struct {
	// selector picks target devices by exact-match metadata labels.
	// (Production would use metav1.LabelSelector for set expressions and
	// region ordering; exact match keeps the demo's moving parts visible.)
	// +kubebuilder:validation:MinProperties=1
	Selector map[string]string `json:"selector"`

	// targetVersion / firmwareURL / checksumSHA256 are copied verbatim into
	// each targeted device's Spec, batch by batch.
	// +kubebuilder:validation:MinLength=1
	TargetVersion string `json:"targetVersion"`
	// +kubebuilder:validation:Pattern=`^https?://`
	FirmwareURL string `json:"firmwareURL"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	ChecksumSHA256 string `json:"checksumSHA256"`

	// maxUnavailable caps how many devices may be mid-upgrade at once —
	// the blast-radius knob. A device counts as unavailable from the moment
	// it is targeted until it reports success or failure.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	MaxUnavailable int32 `json:"maxUnavailable,omitempty"`

	// failureThresholdPercent trips the circuit breaker: when
	// failed/targeted reaches this percentage, the rollout pauses itself
	// and sets rolloutPaused on every selected device.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=50
	FailureThresholdPercent int32 `json:"failureThresholdPercent,omitempty"`
}

// FleetRolloutStatus is derived entirely from listing the selected devices —
// there is no hidden campaign state. Wipe it and one reconcile rebuilds it,
// which is what makes the rollout controller restart-safe for free.
type FleetRolloutStatus struct {
	// +kubebuilder:validation:Enum=Progressing;Paused;Complete
	// +optional
	Phase RolloutPhase `json:"phase,omitempty"`

	// Fleet arithmetic, recomputed every reconcile:
	// selected ⊇ targeted ⊇ (succeeded ∪ failed); in-flight = targeted − succeeded − failed.
	// +optional
	Selected int32 `json:"selected,omitempty"`
	// +optional
	Targeted int32 `json:"targeted,omitempty"`
	// +optional
	Succeeded int32 `json:"succeeded,omitempty"`
	// +optional
	Failed int32 `json:"failed,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetVersion`
// +kubebuilder:printcolumn:name="Selected",type=integer,JSONPath=`.status.selected`
// +kubebuilder:printcolumn:name="Targeted",type=integer,JSONPath=`.status.targeted`
// +kubebuilder:printcolumn:name="Succeeded",type=integer,JSONPath=`.status.succeeded`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failed`

// FleetRollout is the Schema for the fleetrollouts API
type FleetRollout struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of FleetRollout
	// +required
	Spec FleetRolloutSpec `json:"spec"`

	// status defines the observed state of FleetRollout
	// +optional
	Status FleetRolloutStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FleetRolloutList contains a list of FleetRollout
type FleetRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []FleetRollout `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &FleetRollout{}, &FleetRolloutList{})
		return nil
	})
}

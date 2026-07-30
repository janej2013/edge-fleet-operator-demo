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

package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	edgev1alpha1 "github.com/janej2013/edge-fleet-operator-demo/api/v1alpha1"
)

var _ = Describe("FleetRollout Controller", func() {
	const (
		ns           = "default"
		rolloutName  = "pilot-rollout"
		fleetLabel   = "pilot-fleet"
		deviceCount  = 5
		targetVer    = "2.0.0"
		oldVer       = "1.0.0"
		testChecksum = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	)

	ctx := context.Background()
	roKey := types.NamespacedName{Name: rolloutName, Namespace: ns}
	var reconciler *FleetRolloutReconciler

	devName := func(i int) string { return fmt.Sprintf("roll-dev-%d", i) }

	getDev := func(i int) *edgev1alpha1.EdgeDevice {
		dev := &edgev1alpha1.EdgeDevice{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: devName(i), Namespace: ns}, dev)).To(Succeed())
		return dev
	}

	getRollout := func() *edgev1alpha1.FleetRollout {
		ro := &edgev1alpha1.FleetRollout{}
		Expect(k8sClient.Get(ctx, roKey, ro)).To(Succeed())
		return ro
	}

	reconcileRollout := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: roKey})
		Expect(err).NotTo(HaveOccurred())
	}

	// markSucceeded simulates the agent finishing an upgrade.
	markSucceeded := func(i int) {
		dev := getDev(i)
		dev.Status.CurrentFirmwareVersion = targetVer
		Expect(k8sClient.Status().Update(ctx, dev)).To(Succeed())
	}

	// markFailed simulates a terminal transaction failure reported by the agent.
	markFailed := func(i int, reason string) {
		dev := getDev(i)
		dev.Status.Conditions = []metav1.Condition{{
			Type: edgev1alpha1.ConditionFirmwareSynced, Status: metav1.ConditionFalse,
			Reason: reason, Message: "injected by test", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, dev)).To(Succeed())
	}

	targetedDevices := func() []int {
		var out []int
		for i := 0; i < deviceCount; i++ {
			if getDev(i).Spec.DesiredFirmwareVersion == targetVer {
				out = append(out, i)
			}
		}
		return out
	}

	BeforeEach(func() {
		reconciler = &FleetRolloutReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for i := 0; i < deviceCount; i++ {
			dev := &edgev1alpha1.EdgeDevice{
				ObjectMeta: metav1.ObjectMeta{
					Name: devName(i), Namespace: ns,
					Labels: map[string]string{"fleet": fleetLabel},
				},
				Spec: edgev1alpha1.EdgeDeviceSpec{
					DesiredFirmwareVersion: oldVer,
					FirmwareURL:            "http://127.0.0.1:8000/firmware-1.0.0.bin",
					ChecksumSHA256:         testChecksum,
				},
			}
			Expect(k8sClient.Create(ctx, dev)).To(Succeed())
		}
		ro := &edgev1alpha1.FleetRollout{
			ObjectMeta: metav1.ObjectMeta{Name: rolloutName, Namespace: ns},
			Spec: edgev1alpha1.FleetRolloutSpec{
				Selector:                map[string]string{"fleet": fleetLabel},
				TargetVersion:           targetVer,
				FirmwareURL:             "http://127.0.0.1:8000/firmware-2.0.0.bin",
				ChecksumSHA256:          testChecksum,
				MaxUnavailable:          2,
				FailureThresholdPercent: 50,
			},
		}
		Expect(k8sClient.Create(ctx, ro)).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.Delete(ctx, getRollout())).To(Succeed())
		for i := 0; i < deviceCount; i++ {
			Expect(k8sClient.Delete(ctx, getDev(i))).To(Succeed())
		}
	})

	It("targets at most maxUnavailable devices per batch and advances as they finish", func() {
		reconcileRollout()
		Expect(targetedDevices()).To(Equal([]int{0, 1}), "first batch must be exactly maxUnavailable, in name order")

		// Reconcile again without any progress: no double-push (idempotency).
		reconcileRollout()
		Expect(targetedDevices()).To(Equal([]int{0, 1}))

		st := getRollout().Status
		Expect(st.Phase).To(Equal(edgev1alpha1.RolloutProgressing))
		Expect(st.Selected).To(Equal(int32(5)))
		Expect(st.Targeted).To(Equal(int32(2)))

		// Batch 1 completes → capacity frees → batch 2 is pushed.
		markSucceeded(0)
		markSucceeded(1)
		reconcileRollout()
		Expect(targetedDevices()).To(Equal([]int{0, 1, 2, 3}))
		Expect(getRollout().Status.Succeeded).To(Equal(int32(2)))

		// Everything finishes → Complete.
		for i := 2; i < deviceCount; i++ {
			markSucceeded(i)
		}
		reconcileRollout() // observes 4 done, pushes the last device
		reconcileRollout() // observes all done
		Expect(getRollout().Status.Phase).To(Equal(edgev1alpha1.RolloutComplete))
		Expect(getRollout().Status.Succeeded).To(Equal(int32(5)))
	})

	It("trips the circuit breaker and freezes the fleet when the failure rate crosses the threshold", func() {
		reconcileRollout() // batch 1 → devices 0,1
		markSucceeded(0)
		markFailed(1, "ChecksumMismatch") // 1 failed / 2 targeted = 50% ≥ threshold

		reconcileRollout()

		ro := getRollout()
		Expect(ro.Status.Phase).To(Equal(edgev1alpha1.RolloutPaused))
		Expect(ro.Status.Failed).To(Equal(int32(1)))
		Expect(ro.Status.Conditions).ToNot(BeEmpty())
		Expect(ro.Status.Conditions[0].Type).To(Equal(edgev1alpha1.ConditionBreakerTripped))
		Expect(ro.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))

		// The breaker's blast-radius guarantee: nobody new gets targeted,
		// and every selected device is visibly frozen.
		Expect(targetedDevices()).To(Equal([]int{0, 1}))
		for i := 0; i < deviceCount; i++ {
			Expect(getDev(i).Spec.RolloutPaused).To(BeTrue(), "device %d must be paused", i)
		}

		// Breaker does not self-reset: further reconciles stay Paused.
		reconcileRollout()
		Expect(getRollout().Status.Phase).To(Equal(edgev1alpha1.RolloutPaused))
		Expect(targetedDevices()).To(Equal([]int{0, 1}))
	})

	It("counts a device that goes Degraded mid-rollout as failed", func() {
		reconcileRollout()
		markSucceeded(0)
		// Device 1 goes dark: the *device* operator marks it Degraded via
		// heartbeat timeout; the rollout controller must treat that as a
		// failure even though the agent never reported anything.
		dev := getDev(1)
		dev.Status.Phase = edgev1alpha1.PhaseDegraded
		Expect(k8sClient.Status().Update(ctx, dev)).To(Succeed())

		reconcileRollout()
		Expect(getRollout().Status.Phase).To(Equal(edgev1alpha1.RolloutPaused))
		Expect(getRollout().Status.Failed).To(Equal(int32(1)))
	})
})

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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	edgev1alpha1 "github.com/janej2013/edge-fleet-operator-demo/api/v1alpha1"
)

var _ = Describe("EdgeDevice Controller", func() {
	const (
		resourceName      = "test-device"
		resourceNamespace = "default"
		heartbeatTimeout  = 30 * time.Second
	)

	ctx := context.Background()
	devKey := types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
	secretKey := types.NamespacedName{Name: credentialSecretName(resourceName), Namespace: resourceNamespace}

	// fakeNow lets tests move time forward instead of sleeping.
	var fakeNow time.Time
	var reconciler *EdgeDeviceReconciler

	reconcileOnce := func() (reconcile.Result, error) {
		return reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: devKey})
	}

	// reconcileSettled runs reconcile until a full pass makes no writes
	// (finalizer pass, secret+status pass, then steady state).
	reconcileSettled := func() {
		for i := 0; i < 5; i++ {
			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
		}
	}

	getDevice := func() *edgev1alpha1.EdgeDevice {
		dev := &edgev1alpha1.EdgeDevice{}
		Expect(k8sClient.Get(ctx, devKey, dev)).To(Succeed())
		return dev
	}

	heartbeat := func(hbAge time.Duration, currentVersion string) {
		dev := getDevice()
		t := metav1.NewTime(fakeNow.Add(-hbAge))
		dev.Status.LastHeartbeat = &t
		dev.Status.CurrentFirmwareVersion = currentVersion
		Expect(k8sClient.Status().Update(ctx, dev)).To(Succeed())
	}

	BeforeEach(func() {
		fakeNow = time.Now()
		reconciler = &EdgeDeviceReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			HeartbeatTimeout: heartbeatTimeout,
			Now:              func() time.Time { return fakeNow },
		}
		resource := &edgev1alpha1.EdgeDevice{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
			Spec: edgev1alpha1.EdgeDeviceSpec{
				DesiredFirmwareVersion: "1.0.0",
				FirmwareURL:            "http://127.0.0.1:8000/firmware-1.0.0.bin",
				ChecksumSHA256:         "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	})

	AfterEach(func() {
		dev := &edgev1alpha1.EdgeDevice{}
		if err := k8sClient.Get(ctx, devKey, dev); err == nil {
			_ = k8sClient.Delete(ctx, dev)
			// Drive the finalizer path so the object actually goes away.
			for i := 0; i < 3; i++ {
				_, _ = reconcileOnce()
			}
		}
		Expect(errors.IsNotFound(k8sClient.Get(ctx, devKey, &edgev1alpha1.EdgeDevice{}))).To(BeTrue())
	})

	Context("idempotency (the two gates)", func() {
		It("creates the credential secret exactly once across repeated reconciles", func() {
			reconcileSettled()

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			tokenAfterFirst := string(secret.Data["token"])
			secretRV := secret.ResourceVersion
			deviceRV := getDevice().ResourceVersion

			// Three more reconciles: no side effect may repeat, no write may
			// happen — resourceVersions are the proof.
			for i := 0; i < 3; i++ {
				res, err := reconcileOnce()
				Expect(err).NotTo(HaveOccurred())
				Expect(res.RequeueAfter).To(BeNumerically(">", 0), "steady state must be a planned re-check, not an error retry")
			}

			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			Expect(secret.ResourceVersion).To(Equal(secretRV), "secret was rewritten by a redundant reconcile")
			Expect(string(secret.Data["token"])).To(Equal(tokenAfterFirst))
			Expect(getDevice().ResourceVersion).To(Equal(deviceRV), "status was rewritten with no change (gate 1 leak)")
		})

		It("rotates the credential if a same-name device is recreated (fingerprint mismatch)", func() {
			reconcileSettled()
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())

			// Simulate a leftover secret from a previous device incarnation.
			secret.Annotations[credFingerprintKey] = "stale-uid"
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			oldToken := string(secret.Data["token"])

			reconcileSettled()
			Expect(k8sClient.Get(ctx, secretKey, secret)).To(Succeed())
			Expect(secret.Annotations[credFingerprintKey]).To(Equal(string(getDevice().UID)))
			Expect(string(secret.Data["token"])).NotTo(Equal(oldToken), "stale credential must be rotated, not reused")
		})
	})

	Context("finalizer", func() {
		It("revokes the credential before allowing deletion", func() {
			reconcileSettled()
			Expect(getDevice().Finalizers).To(ContainElement(deviceFinalizer))
			Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed())

			Expect(k8sClient.Delete(ctx, getDevice())).To(Succeed())
			// Finalizer must hold the object until cleanup runs.
			Expect(getDevice().DeletionTimestamp.IsZero()).To(BeFalse())

			_, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())

			Expect(errors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue(), "credential must be revoked")
			Expect(errors.IsNotFound(k8sClient.Get(ctx, devKey, &edgev1alpha1.EdgeDevice{}))).To(BeTrue(), "device must be released")
		})
	})

	Context("phase projection", func() {
		It("starts as Provisioning before any heartbeat", func() {
			reconcileSettled()
			Expect(getDevice().Status.Phase).To(Equal(edgev1alpha1.PhaseProvisioning))
		})

		It("goes Ready on fresh heartbeat with matching firmware", func() {
			reconcileSettled()
			heartbeat(time.Second, "1.0.0")
			reconcileSettled()
			Expect(getDevice().Status.Phase).To(Equal(edgev1alpha1.PhaseReady))
		})

		It("goes Upgrading on fresh heartbeat with firmware drift", func() {
			reconcileSettled()
			heartbeat(time.Second, "0.9.0")
			reconcileSettled()
			Expect(getDevice().Status.Phase).To(Equal(edgev1alpha1.PhaseUpgrading))
		})

		It("goes Degraded when the heartbeat times out, and recovers", func() {
			reconcileSettled()
			heartbeat(time.Second, "1.0.0")
			reconcileSettled()
			Expect(getDevice().Status.Phase).To(Equal(edgev1alpha1.PhaseReady))

			// Time-travel past the timeout instead of sleeping.
			fakeNow = fakeNow.Add(heartbeatTimeout + 2*time.Second)
			res, err := reconcileOnce()
			Expect(err).NotTo(HaveOccurred())
			Expect(getDevice().Status.Phase).To(Equal(edgev1alpha1.PhaseDegraded))
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			// Device comes back: fresh heartbeat flips it out of Degraded.
			heartbeat(time.Second, "1.0.0")
			reconcileSettled()
			Expect(getDevice().Status.Phase).To(Equal(edgev1alpha1.PhaseReady))
		})
	})
})

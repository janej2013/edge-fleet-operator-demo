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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	edgev1alpha1 "github.com/janej2013/edge-fleet-operator-demo/api/v1alpha1"
)

const (
	// deviceFinalizer gates deletion: the API server will not remove the
	// object until we strip this, which is our window to revoke credentials.
	deviceFinalizer = "edge.example.com/device-cleanup"

	// credFingerprintKey ties a credential Secret to the device *instance*
	// (UID, not name): if a device is deleted and recreated under the same
	// name, the stale secret is detected and rotated instead of reused.
	credFingerprintKey = "edge.example.com/device-uid"

	// defaultHeartbeatTimeout: how long without an agent report before the
	// operator declares the device Degraded. Short because the demo agents
	// heartbeat every few seconds; production would be minutes.
	defaultHeartbeatTimeout = 30 * time.Second
)

// EdgeDeviceReconciler reconciles a EdgeDevice object
type EdgeDeviceReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// HeartbeatTimeout is injectable so tests don't sleep.
	HeartbeatTimeout time.Duration
	// Now is injectable so tests can time-travel instead of sleeping.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=edge.example.com,resources=edgedevices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=edge.example.com,resources=edgedevices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=edge.example.com,resources=edgedevices/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete

// Reconcile drives one device toward its Spec. It is level-triggered: we get
// "something changed", never *what* changed, so every step below must be safe
// to run any number of times. Idempotency is layered as two gates:
//
//	gate 1 — diff & early-return: compute the status we *would* write and
//	         skip the write entirely if it matches what is already there;
//	gate 2 — re-entrant side effects: before creating anything external
//	         (the credential Secret), check existence + fingerprint first.
func (r *EdgeDeviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dev edgev1alpha1.EdgeDevice
	if err := r.Get(ctx, req.NamespacedName, &dev); err != nil {
		// NotFound after a finalizer-less delete is normal: nothing to do.
		// Any other error is transient — return it and let the workqueue
		// retry with exponential backoff.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Deletion path (finalizer) ---------------------------------------
	// DeletionTimestamp set means "someone asked to delete, but finalizers
	// are holding the object". We revoke the credential *synchronously*
	// here, then strip the finalizer to let the delete proceed. We do NOT
	// rely on ownerReference GC for this: GC is asynchronous best-effort,
	// while credential revocation must be ordered *before* the device
	// disappears from the inventory.
	if !dev.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&dev, deviceFinalizer) {
			if err := r.revokeCredential(ctx, &dev); err != nil {
				return ctrl.Result{}, err // retryable: keep holding deletion
			}
			controllerutil.RemoveFinalizer(&dev, deviceFinalizer)
			if err := r.Update(ctx, &dev); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("finalizer cleanup done, device released for deletion")
		}
		return ctrl.Result{}, nil
	}

	// Install the finalizer before doing anything else, so there is no
	// window where a credential exists but deletion could skip cleanup.
	if !controllerutil.ContainsFinalizer(&dev, deviceFinalizer) {
		controllerutil.AddFinalizer(&dev, deviceFinalizer)
		if err := r.Update(ctx, &dev); err != nil {
			return ctrl.Result{}, err
		}
		// The Update bumped resourceVersion; the watch will trigger a fresh
		// reconcile on the new revision. Returning here keeps each pass
		// single-purpose instead of continuing on a stale in-memory copy.
		return ctrl.Result{}, nil
	}

	// --- Gate 2: re-entrant side effect ----------------------------------
	if err := r.ensureCredentialSecret(ctx, &dev); err != nil {
		// Transient API failure → return err → exponential backoff retry.
		return ctrl.Result{}, err
	}

	// --- Compute observed truth → phase ----------------------------------
	newStatus, requeueAfter := r.projectStatus(&dev)

	// --- Gate 1: diff & early-return --------------------------------------
	// If the projection changes nothing, skip the write. This is what makes
	// hot reconcile loops cheap: N redundant events → N no-op comparisons,
	// not N status PUTs (which would themselves trigger more events).
	if statusEqual(&dev.Status, &newStatus) {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}

	oldPhase := dev.Status.Phase
	dev.Status.Phase = newStatus.Phase
	dev.Status.Conditions = newStatus.Conditions
	if err := r.Status().Update(ctx, &dev); err != nil {
		// This includes 409 Conflict when the agent wrote status between our
		// read and this write. Conflict is not damage — it just means our
		// snapshot is stale. Returning err requeues; the next pass re-reads
		// the latest object and re-projects. Never retry a conflict with the
		// same stale object in a loop.
		return ctrl.Result{}, err
	}
	log.Info("phase transition", "from", oldPhase, "to", newStatus.Phase)

	// RequeueAfter vs return err — the distinction that matters:
	//   return err            → "something went wrong, retry with backoff"
	//                           (backoff grows, counts as reconcile errors)
	//   Result{RequeueAfter}  → "all good, but wake me at T to re-check"
	//                           (planned, fixed schedule, not an error)
	// Heartbeat expiry is a *planned future event*, not a failure, so it
	// must be RequeueAfter: abusing `return err` here would pollute error
	// metrics and give us random backoff timing instead of a deadline.
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// projectStatus derives phase + operator-owned conditions from observed facts.
// Pure function of (device, now) — trivially unit-testable, no API calls.
//
// Writer discipline: the operator owns Phase and the Healthy condition; the
// agent owns firmware facts (currentFirmwareVersion, activeSlot,
// lastHeartbeat, observedGeneration) and the FirmwareSynced condition. Two
// writers never fight over one field, and optimistic concurrency handles the
// races on the shared status object as a whole.
func (r *EdgeDeviceReconciler) projectStatus(dev *edgev1alpha1.EdgeDevice) (edgev1alpha1.EdgeDeviceStatus, time.Duration) {
	timeout := r.HeartbeatTimeout
	if timeout == 0 {
		timeout = defaultHeartbeatTimeout
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	st := *dev.Status.DeepCopy() // start from agent-reported facts, never wipe them

	healthy := metav1.Condition{
		Type:               edgev1alpha1.ConditionHealthy,
		ObservedGeneration: dev.Generation,
	}
	requeue := timeout // default re-check cadence

	switch {
	case dev.Status.LastHeartbeat == nil:
		// Never reported in: this is enrollment, not failure.
		st.Phase = edgev1alpha1.PhaseProvisioning
		healthy.Status = metav1.ConditionUnknown
		healthy.Reason = "AwaitingFirstHeartbeat"
		healthy.Message = "device has not reported in yet"

	case now().Sub(dev.Status.LastHeartbeat.Time) > timeout:
		st.Phase = edgev1alpha1.PhaseDegraded
		healthy.Status = metav1.ConditionFalse
		healthy.Reason = "HeartbeatTimeout"
		healthy.Message = fmt.Sprintf("no heartbeat for more than %s", timeout)

	default:
		healthy.Status = metav1.ConditionTrue
		healthy.Reason = "HeartbeatFresh"
		healthy.Message = "agent is reporting"
		// Upgrade failures are reported by the agent via FirmwareSynced;
		// a live device with a failed transaction is Degraded, not Upgrading,
		// so the fleet view surfaces it instead of showing eternal progress.
		if c := meta.FindStatusCondition(dev.Status.Conditions, edgev1alpha1.ConditionFirmwareSynced); c != nil &&
			c.Status == metav1.ConditionFalse && c.Reason != "UpgradeInProgress" {
			st.Phase = edgev1alpha1.PhaseDegraded
		} else if dev.Status.CurrentFirmwareVersion != dev.Spec.DesiredFirmwareVersion {
			st.Phase = edgev1alpha1.PhaseUpgrading
		} else {
			st.Phase = edgev1alpha1.PhaseReady
		}
		// Wake up exactly when this heartbeat would expire (+ epsilon), so
		// Degraded detection lags the deadline by ~1s, not by a poll period.
		age := now().Sub(dev.Status.LastHeartbeat.Time)
		if remaining := timeout - age + time.Second; remaining > 0 {
			requeue = remaining
		}
	}

	meta.SetStatusCondition(&st.Conditions, healthy)
	return st, requeue
}

// statusEqual compares only fields this controller writes. Timestamps inside
// conditions are deliberately ignored: SetStatusCondition preserves
// LastTransitionTime when nothing flipped, so flip-free projections compare
// equal and gate 1 holds.
func statusEqual(a, b *edgev1alpha1.EdgeDeviceStatus) bool {
	if a.Phase != b.Phase {
		return false
	}
	ca := meta.FindStatusCondition(a.Conditions, edgev1alpha1.ConditionHealthy)
	cb := meta.FindStatusCondition(b.Conditions, edgev1alpha1.ConditionHealthy)
	if (ca == nil) != (cb == nil) {
		return false
	}
	if ca == nil {
		return true
	}
	return ca.Status == cb.Status && ca.Reason == cb.Reason && ca.ObservedGeneration == cb.ObservedGeneration
}

// ensureCredentialSecret is idempotency gate 2 in action: look before you
// leap. Reconcile may run 100 times; the side effect must happen once.
func (r *EdgeDeviceReconciler) ensureCredentialSecret(ctx context.Context, dev *edgev1alpha1.EdgeDevice) error {
	name := credentialSecretName(dev.Name)
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: dev.Namespace, Name: name}, &existing)
	if err == nil {
		// Exists — but is it *ours*? Fingerprint on UID catches the
		// delete-and-recreate-same-name case, where reusing the old
		// credential would be a security hole.
		if existing.Annotations[credFingerprintKey] == string(dev.UID) {
			return nil // second gate closed: nothing to do
		}
		existing.Annotations[credFingerprintKey] = string(dev.UID)
		existing.Data = map[string][]byte{"token": []byte(newToken())}
		return r.Update(ctx, &existing)
	}
	if !apierrors.IsNotFound(err) {
		return err // transient read failure: let backoff handle it
	}

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   dev.Namespace,
			Annotations: map[string]string{credFingerprintKey: string(dev.UID)},
			Labels:      map[string]string{"edge.example.com/device": dev.Name},
		},
		Data: map[string][]byte{"token": []byte(newToken())},
	}
	if err := r.Create(ctx, &secret); err != nil {
		// AlreadyExists means we lost a create race with a concurrent
		// reconcile of the same object — the outcome we wanted exists, so
		// treat it as success on the next pass.
		return client.IgnoreAlreadyExists(err)
	}
	return nil
}

func (r *EdgeDeviceReconciler) revokeCredential(ctx context.Context, dev *edgev1alpha1.EdgeDevice) error {
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: dev.Namespace,
		Name:      credentialSecretName(dev.Name),
	}}
	// Deleting something already gone is success, not failure — cleanup
	// must be idempotent because the deletion path also reruns on error.
	return client.IgnoreNotFound(r.Delete(ctx, &secret))
}

func credentialSecretName(deviceName string) string {
	return "device-cred-" + deviceName
}

func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SetupWithManager sets up the controller with the Manager.
// Note: no GenerationChangedPredicate here on purpose — heartbeats arrive as
// *status* updates, which don't bump metadata.generation, and we need those
// events to re-evaluate health.
func (r *EdgeDeviceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&edgev1alpha1.EdgeDevice{}).
		Named("edgedevice").
		Complete(r)
}

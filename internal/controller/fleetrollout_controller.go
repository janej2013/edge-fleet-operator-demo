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
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	edgev1alpha1 "github.com/janej2013/edge-fleet-operator-demo/api/v1alpha1"
)

// FleetRolloutReconciler drives a staged firmware campaign.
//
// It owns exactly one lever: the Spec of selected EdgeDevices. It never talks
// to agents and holds no campaign state in memory — every reconcile re-derives
// "where are we" by listing devices and counting. Batches are therefore not a
// stored cursor but an emergent property: "top up until maxUnavailable
// devices are in flight". Production adds canary steps and region ordering on
// top of this same skeleton.
type FleetRolloutReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=edge.example.com,resources=fleetrollouts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=edge.example.com,resources=fleetrollouts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=edge.example.com,resources=fleetrollouts/finalizers,verbs=update

func (r *FleetRolloutReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ro edgev1alpha1.FleetRollout
	if err := r.Get(ctx, req.NamespacedName, &ro); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var devs edgev1alpha1.EdgeDeviceList
	if err := r.List(ctx, &devs, client.InNamespace(ro.Namespace),
		client.MatchingLabels(ro.Spec.Selector)); err != nil {
		return ctrl.Result{}, err
	}
	// Deterministic batch order: same fleet state → same next batch, no
	// matter which replica reconciles or how the list was paged.
	sort.Slice(devs.Items, func(i, j int) bool { return devs.Items[i].Name < devs.Items[j].Name })

	// --- Fleet arithmetic (pure observation, no writes) -------------------
	var targeted, succeeded, failed int32
	var untargeted []*edgev1alpha1.EdgeDevice
	for i := range devs.Items {
		dev := &devs.Items[i]
		if dev.Status.CurrentFirmwareVersion == ro.Spec.TargetVersion {
			// Reached the target — counts as targeted+succeeded no matter
			// how it got there (idempotent w.r.t. pre-upgraded devices).
			targeted++
			succeeded++
			continue
		}
		if dev.Spec.DesiredFirmwareVersion != ro.Spec.TargetVersion {
			untargeted = append(untargeted, dev)
			continue
		}
		targeted++
		if deviceFailed(dev) {
			failed++
		}
	}
	inFlight := targeted - succeeded - failed

	// --- Circuit breaker ---------------------------------------------------
	// Rate is failed/targeted so far — a cheap proxy for "per batch".
	// Production would window per batch and per region; the demo keeps the
	// arithmetic small enough to verify by eye in kubectl output.
	tripped := targeted > 0 && failed*100 >= ro.Spec.FailureThresholdPercent*targeted

	phase := edgev1alpha1.RolloutProgressing
	switch {
	case tripped:
		phase = edgev1alpha1.RolloutPaused
		// Freeze the whole group, not just the failing devices: paused
		// devices refuse *new* transactions (in-flight ones may finish),
		// and the flag is a kubectl-visible mark that a breaker fired.
		for i := range devs.Items {
			dev := &devs.Items[i]
			if !dev.Spec.RolloutPaused {
				dev.Spec.RolloutPaused = true
				if err := r.Update(ctx, dev); err != nil {
					return ctrl.Result{}, err // conflict-safe: rerun recomputes everything
				}
			}
		}
		log.Info("circuit breaker tripped", "failed", failed, "targeted", targeted,
			"thresholdPercent", ro.Spec.FailureThresholdPercent)

	case len(devs.Items) > 0 && succeeded == int32(len(devs.Items)):
		phase = edgev1alpha1.RolloutComplete

	default:
		// --- Push the next slice of the batch -----------------------------
		// "Batch" = keep at most maxUnavailable devices in flight. Targeting
		// is writing the device Spec; a re-run can't double-push because a
		// targeted device never appears in `untargeted` again.
		capacity := ro.Spec.MaxUnavailable - inFlight
		for _, dev := range untargeted {
			if capacity <= 0 {
				break
			}
			if dev.Spec.RolloutPaused {
				continue // a human explicitly froze this device; not ours to thaw
			}
			dev.Spec.DesiredFirmwareVersion = ro.Spec.TargetVersion
			dev.Spec.FirmwareURL = ro.Spec.FirmwareURL
			dev.Spec.ChecksumSHA256 = ro.Spec.ChecksumSHA256
			if err := r.Update(ctx, dev); err != nil {
				return ctrl.Result{}, err
			}
			log.Info("device targeted", "device", dev.Name, "target", ro.Spec.TargetVersion)
			capacity--
			targeted++
			inFlight++
		}
	}

	// --- Status write (diff-gated, same discipline as the device controller)
	newStatus := *ro.Status.DeepCopy()
	newStatus.Phase = phase
	newStatus.Selected = int32(len(devs.Items))
	newStatus.Targeted = targeted
	newStatus.Succeeded = succeeded
	newStatus.Failed = failed
	newStatus.ObservedGeneration = ro.Generation
	cond := metav1.Condition{
		Type: edgev1alpha1.ConditionBreakerTripped, Status: metav1.ConditionFalse,
		Reason: "Healthy", Message: "failure rate below threshold", ObservedGeneration: ro.Generation,
	}
	if tripped {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "FailureRateExceeded"
		cond.Message = "rollout halted; fix the cause and unpause devices to resume"
	}
	meta.SetStatusCondition(&newStatus.Conditions, cond)

	if !rolloutStatusEqual(&ro.Status, &newStatus) {
		ro.Status = newStatus
		if err := r.Status().Update(ctx, &ro); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Device status changes re-trigger us via the watch in SetupWithManager;
	// the requeue is only a safety net against missed events.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// deviceFailed decides whether a targeted device burns breaker budget:
// either its upgrade transaction reported a terminal failure, or the device
// itself went dark mid-rollout (Degraded via heartbeat timeout).
func deviceFailed(dev *edgev1alpha1.EdgeDevice) bool {
	if dev.Status.Phase == edgev1alpha1.PhaseDegraded {
		return true
	}
	c := meta.FindStatusCondition(dev.Status.Conditions, edgev1alpha1.ConditionFirmwareSynced)
	return c != nil && c.Status == metav1.ConditionFalse && c.Reason != "UpgradeInProgress"
}

func rolloutStatusEqual(a, b *edgev1alpha1.FleetRolloutStatus) bool {
	if a.Phase != b.Phase || a.Selected != b.Selected || a.Targeted != b.Targeted ||
		a.Succeeded != b.Succeeded || a.Failed != b.Failed || a.ObservedGeneration != b.ObservedGeneration {
		return false
	}
	ca := meta.FindStatusCondition(a.Conditions, edgev1alpha1.ConditionBreakerTripped)
	cb := meta.FindStatusCondition(b.Conditions, edgev1alpha1.ConditionBreakerTripped)
	if (ca == nil) != (cb == nil) {
		return false
	}
	return ca == nil || (ca.Status == cb.Status && ca.Reason == cb.Reason)
}

// rolloutsForDevice maps a device event to the rollouts selecting it, so a
// device flipping to failed/succeeded advances its campaign immediately
// instead of waiting for a resync tick.
func (r *FleetRolloutReconciler) rolloutsForDevice(ctx context.Context, obj client.Object) []reconcile.Request {
	var ros edgev1alpha1.FleetRolloutList
	if err := r.List(ctx, &ros, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	labels := obj.GetLabels()
	var reqs []reconcile.Request
	for i := range ros.Items {
		ro := &ros.Items[i]
		match := len(ro.Spec.Selector) > 0
		for k, v := range ro.Spec.Selector {
			if labels[k] != v {
				match = false
				break
			}
		}
		if match {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(ro)})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *FleetRolloutReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&edgev1alpha1.FleetRollout{}).
		Watches(&edgev1alpha1.EdgeDevice{}, handler.EnqueueRequestsFromMapFunc(r.rolloutsForDevice)).
		Named("fleetrollout").
		Complete(r)
}

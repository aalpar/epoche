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
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	decisionsv1alpha1 "github.com/aalpar/epoche/api/v1alpha1"
)

// Freezer abstracts container freeze/unfreeze operations.
type Freezer interface {
	Freeze(ctx context.Context, namespace, podName, containerName string) error
	Unfreeze(ctx context.Context, namespace, podName, containerName string) error
}

// Notifier sends notifications to escalation channels.
type Notifier interface {
	Notify(ctx context.Context, gate *decisionsv1alpha1.DecisionGate) error
}

// DecisionGateReconciler reconciles a DecisionGate object.
type DecisionGateReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	Freezer           Freezer
	Notifier          Notifier
	Now               func() time.Time // injectable for testing
	SidecarManagePort int              // port for sidecar management API (0 = disabled)
}

// +kubebuilder:rbac:groups=decisions.epoche.dev,resources=decisiongates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=decisions.epoche.dev,resources=decisiongates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=decisions.epoche.dev,resources=decisiongates/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create

func (r *DecisionGateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var gate decisionsv1alpha1.DecisionGate
	if err := r.Get(ctx, req.NamespacedName, &gate); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	switch gate.Status.Phase {
	case "":
		return r.initialize(ctx, &gate)
	case decisionsv1alpha1.GatePhasePending:
		return r.reconcilePending(ctx, &gate)
	case decisionsv1alpha1.GatePhaseDecided:
		return r.reconcileDecided(ctx, &gate)
	case decisionsv1alpha1.GatePhaseExecuted,
		decisionsv1alpha1.GatePhaseTimedOut,
		decisionsv1alpha1.GatePhaseFailed:
		return ctrl.Result{}, nil
	default:
		log.Info("Unknown phase, skipping", "phase", gate.Status.Phase)
		return ctrl.Result{}, nil
	}
}

// initialize validates the target, freezes the container, sends notifications,
// and transitions to Pending.
func (r *DecisionGateReconciler) initialize(ctx context.Context, gate *decisionsv1alpha1.DecisionGate) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Validate target pod exists
	var pod corev1.Pod
	podKey := types.NamespacedName{Namespace: gate.Namespace, Name: gate.Spec.TargetRef.Name}
	if err := r.Get(ctx, podKey, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Target pod not found", "pod", podKey)
			r.setFailed(gate, "TargetNotFound", fmt.Sprintf("Pod %s not found", podKey.Name))
			return ctrl.Result{}, r.Status().Update(ctx, gate)
		}
		return ctrl.Result{}, err
	}

	// Validate container exists in pod
	if !hasContainer(&pod, gate.Spec.TargetRef.Container) {
		r.setFailed(gate, "ContainerNotFound",
			fmt.Sprintf("Container %s not found in pod %s", gate.Spec.TargetRef.Container, podKey.Name))
		return ctrl.Result{}, r.Status().Update(ctx, gate)
	}

	// Freeze the container
	if err := r.Freezer.Freeze(ctx, gate.Namespace, gate.Spec.TargetRef.Name, gate.Spec.TargetRef.Container); err != nil {
		log.Error(err, "Failed to freeze container")
		r.setFailed(gate, "FreezeFailed", err.Error())
		return ctrl.Result{}, r.Status().Update(ctx, gate)
	}

	// Notify sidecar to stop proxying after freezing.
	r.notifySidecar(ctx, &pod, "freeze")

	// Set frozen label on target pod (best-effort).
	if err := r.setPodFrozenLabel(ctx, gate.Namespace, gate.Spec.TargetRef.Name, true); err != nil {
		log.Error(err, "Failed to set frozen label on pod")
	}

	now := metav1.NewTime(r.now())
	gate.Status.Phase = decisionsv1alpha1.GatePhasePending
	gate.Status.FreezeTime = &now
	gate.Status.Events = append(gate.Status.Events, decisionsv1alpha1.GateEvent{
		Type:   decisionsv1alpha1.GateEventFrozen,
		Time:   &now,
		Detail: fmt.Sprintf("Froze container %s in pod %s", gate.Spec.TargetRef.Container, gate.Spec.TargetRef.Name),
	})

	// Persist freeze before sending notifications — freeze is the critical path.
	if err := r.Status().Update(ctx, gate); err != nil {
		return ctrl.Result{}, err
	}

	// Notify (best-effort — don't fail the gate if notifications fail)
	if err := r.Notifier.Notify(ctx, gate); err != nil {
		log.Error(err, "Failed to send notifications")
	}

	timeout := r.parseTimeout(gate)
	return ctrl.Result{RequeueAfter: timeout}, nil
}

// reconcilePending checks for a response or timeout.
func (r *DecisionGateReconciler) reconcilePending(ctx context.Context, gate *decisionsv1alpha1.DecisionGate) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// A responder patched spec.response — validate and transition.
	if gate.Spec.Response != nil {
		return r.applyResponse(ctx, gate)
	}

	// Guard: pending gate must have a freeze time.
	if gate.Status.FreezeTime == nil {
		r.setFailed(gate, "InvalidState", "Pending gate with no freeze time")
		return ctrl.Result{}, r.Status().Update(ctx, gate)
	}

	timeout := r.parseTimeout(gate)
	deadline := gate.Status.FreezeTime.Add(timeout)
	now := r.now()

	if now.After(deadline) {
		log.Info("Gate timed out, applying default action", "action", gate.Spec.Timeout.DefaultAction)
		nowMeta := metav1.NewTime(now)
		gate.Status.Decision = &decisionsv1alpha1.Decision{
			Action:    gate.Spec.Timeout.DefaultAction,
			DecidedBy: "system/timeout",
			DecidedAt: &nowMeta,
			Reason:    fmt.Sprintf("No response within %s", gate.Spec.Timeout.Duration),
		}
		gate.Status.Events = append(gate.Status.Events, decisionsv1alpha1.GateEvent{
			Type:   decisionsv1alpha1.GateEventTimedOut,
			Time:   &nowMeta,
			Detail: fmt.Sprintf("Timeout after %s, default action: %s", gate.Spec.Timeout.Duration, gate.Spec.Timeout.DefaultAction),
		})
		gate.Status.Phase = decisionsv1alpha1.GatePhaseDecided
		if err := r.Status().Update(ctx, gate); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	remaining := deadline.Sub(now)
	log.Info("Waiting for response", "remaining", remaining.Round(time.Second))
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// applyResponse validates a spec.response and transitions to Decided.
func (r *DecisionGateReconciler) applyResponse(ctx context.Context, gate *decisionsv1alpha1.DecisionGate) (ctrl.Result, error) {
	resp := gate.Spec.Response

	if !isValidOption(gate.Spec.Options, resp.Action) {
		meta.SetStatusCondition(&gate.Status.Conditions, metav1.Condition{
			Type:               "ResponseValid",
			Status:             metav1.ConditionFalse,
			Reason:             "InvalidAction",
			Message:            fmt.Sprintf("Action %q not in options list", resp.Action),
			ObservedGeneration: gate.Generation,
		})
		return ctrl.Result{}, r.Status().Update(ctx, gate)
	}

	now := metav1.NewTime(r.now())
	gate.Status.Decision = &decisionsv1alpha1.Decision{
		Action:    resp.Action,
		DecidedBy: resp.RespondedBy,
		DecidedAt: &now,
		Reason:    resp.Reason,
	}
	gate.Status.Events = append(gate.Status.Events, decisionsv1alpha1.GateEvent{
		Type:   decisionsv1alpha1.GateEventDecided,
		Time:   &now,
		Detail: fmt.Sprintf("Decision: %s (by %s)", resp.Action, resp.RespondedBy),
	})
	gate.Status.Phase = decisionsv1alpha1.GatePhaseDecided

	if err := r.Status().Update(ctx, gate); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// reconcileDecided unfreezes the container and marks the gate as executed.
func (r *DecisionGateReconciler) reconcileDecided(ctx context.Context, gate *decisionsv1alpha1.DecisionGate) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if gate.Status.Decision == nil {
		r.setFailed(gate, "NoDecision", "Decided phase with no decision recorded")
		return ctrl.Result{}, r.Status().Update(ctx, gate)
	}

	// Notify sidecar to resume proxying before unfreezing.
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: gate.Namespace, Name: gate.Spec.TargetRef.Name,
	}, &pod); err == nil {
		r.notifySidecar(ctx, &pod, "unfreeze")
	}

	// Remove frozen label from target pod (best-effort).
	if err := r.setPodFrozenLabel(ctx, gate.Namespace, gate.Spec.TargetRef.Name, false); err != nil {
		log.Error(err, "Failed to remove frozen label from pod")
	}

	// Unfreeze the container.
	if err := r.Freezer.Unfreeze(ctx, gate.Namespace, gate.Spec.TargetRef.Name, gate.Spec.TargetRef.Container); err != nil {
		log.Error(err, "Failed to unfreeze container")
		// Continue — some actions (Kill) don't need an unfreeze.
	}

	now := metav1.NewTime(r.now())
	gate.Status.Events = append(gate.Status.Events,
		decisionsv1alpha1.GateEvent{
			Type:   decisionsv1alpha1.GateEventUnfrozen,
			Time:   &now,
			Detail: fmt.Sprintf("Unfroze container %s in pod %s", gate.Spec.TargetRef.Container, gate.Spec.TargetRef.Name),
		},
		decisionsv1alpha1.GateEvent{
			Type:   decisionsv1alpha1.GateEventExecuted,
			Time:   &now,
			Detail: fmt.Sprintf("Executed action: %s", gate.Status.Decision.Action),
		},
	)

	log.Info("Executed action", "action", gate.Status.Decision.Action,
		"decidedBy", gate.Status.Decision.DecidedBy)

	gate.Status.Phase = decisionsv1alpha1.GatePhaseExecuted
	meta.SetStatusCondition(&gate.Status.Conditions, metav1.Condition{
		Type:               "Complete",
		Status:             metav1.ConditionTrue,
		Reason:             "ActionExecuted",
		Message:            fmt.Sprintf("Action %s executed", gate.Status.Decision.Action),
		ObservedGeneration: gate.Generation,
	})

	return ctrl.Result{}, r.Status().Update(ctx, gate)
}

func (r *DecisionGateReconciler) setFailed(gate *decisionsv1alpha1.DecisionGate, reason, message string) {
	now := metav1.NewTime(r.now())
	gate.Status.Phase = decisionsv1alpha1.GatePhaseFailed
	gate.Status.Events = append(gate.Status.Events, decisionsv1alpha1.GateEvent{
		Type:   decisionsv1alpha1.GateEventFailed,
		Time:   &now,
		Detail: message,
	})
	meta.SetStatusCondition(&gate.Status.Conditions, metav1.Condition{
		Type:               "Failed",
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: gate.Generation,
	})
}

func (r *DecisionGateReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *DecisionGateReconciler) notifySidecar(ctx context.Context, pod *corev1.Pod, action string) {
	if r.SidecarManagePort == 0 || pod.Status.PodIP == "" {
		return
	}
	log := logf.FromContext(ctx)
	url := fmt.Sprintf("http://%s:%d/manage/%s", pod.Status.PodIP, r.SidecarManagePort, action)

	httpClient := &http.Client{Timeout: 3 * time.Second}
	resp, err := httpClient.Post(url, "", nil)
	if err != nil {
		log.Error(err, "Failed to notify sidecar", "action", action, "pod", pod.Name)
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Info("Sidecar returned non-OK status", "action", action, "status", resp.StatusCode)
	}
}

func (r *DecisionGateReconciler) parseTimeout(gate *decisionsv1alpha1.DecisionGate) time.Duration {
	d, err := time.ParseDuration(gate.Spec.Timeout.Duration)
	if err != nil {
		return 5 * time.Minute
	}
	return d
}

func hasContainer(pod *corev1.Pod, name string) bool {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return true
		}
	}
	return false
}

func isValidOption(options []decisionsv1alpha1.Option, action string) bool {
	for i := range options {
		if options[i].Name == action {
			return true
		}
	}
	return false
}

// setPodFrozenLabel sets or removes the epoche.dev/frozen label on a pod.
func (r *DecisionGateReconciler) setPodFrozenLabel(ctx context.Context, namespace, podName string, frozen bool) error {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err != nil {
		return err
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	if frozen {
		pod.Labels["epoche.dev/frozen"] = "true"
	} else {
		delete(pod.Labels, "epoche.dev/frozen")
	}
	return r.Patch(ctx, &pod, patch)
}

// SetupWithManager sets up the controller with the Manager.
func (r *DecisionGateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&decisionsv1alpha1.DecisionGate{}).
		Named("decisiongate").
		Complete(r)
}

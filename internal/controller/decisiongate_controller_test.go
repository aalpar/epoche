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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	decisionsv1alpha1 "github.com/aalpar/epoche/api/v1alpha1"
)

// --- Test doubles ---

type freezeCall struct {
	Namespace, PodName, ContainerName string
}

type recordingFreezer struct {
	freezeCalls   []freezeCall
	unfreezeCalls []freezeCall
	freezeErr     error
	unfreezeErr   error
}

func (f *recordingFreezer) Freeze(_ context.Context, namespace, podName, containerName string) error {
	f.freezeCalls = append(f.freezeCalls, freezeCall{namespace, podName, containerName})
	return f.freezeErr
}

func (f *recordingFreezer) Unfreeze(_ context.Context, namespace, podName, containerName string) error {
	f.unfreezeCalls = append(f.unfreezeCalls, freezeCall{namespace, podName, containerName})
	return f.unfreezeErr
}

type recordingNotifier struct {
	calls int
	err   error
}

func (n *recordingNotifier) Notify(_ context.Context, _ *decisionsv1alpha1.DecisionGate) error {
	n.calls++
	return n.err
}

// --- Helpers ---

var testCounter int

func uniqueName(prefix string) string {
	testCounter++
	return fmt.Sprintf("%s-%d", prefix, testCounter)
}

func createPod(ctx context.Context, name string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "busybox"},
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, pod)).To(Succeed())
}

func createGate(ctx context.Context, name, podName string) {
	gate := &decisionsv1alpha1.DecisionGate{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: decisionsv1alpha1.DecisionGateSpec{
			TargetRef: decisionsv1alpha1.TargetReference{
				Kind: "Pod", Name: podName, Container: "app",
			},
			Condition: decisionsv1alpha1.Condition{
				Type: "ResourcePressure", Summary: "Test condition", Severity: "Warning",
			},
			Escalation: decisionsv1alpha1.Escalation{
				Channels: []decisionsv1alpha1.Channel{
					{Type: "Slack", Properties: map[string]string{"channel": "#test"}},
				},
				AllowedResponders: []decisionsv1alpha1.ResponderRef{
					{User: "test-user@example.com"},
				},
			},
			Timeout: decisionsv1alpha1.Timeout{Duration: "5m", DefaultAction: "Kill"},
			Options: []decisionsv1alpha1.Option{
				{Name: "Continue", Description: "Let it run"},
				{Name: "Kill", Description: "Kill the process"},
				{Name: "HeapDump", Description: "Take heap dump then restart"},
			},
		},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, gate)).To(Succeed())
}

func getGate(ctx context.Context, name string) *decisionsv1alpha1.DecisionGate {
	gate := &decisionsv1alpha1.DecisionGate{}
	ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{
		Name: name, Namespace: "default",
	}, gate)).To(Succeed())
	return gate
}

// --- Tests ---

var _ = Describe("DecisionGate Controller", func() {
	var (
		reconciler  *DecisionGateReconciler
		freezer     *recordingFreezer
		notifier    *recordingNotifier
		currentTime time.Time
	)

	BeforeEach(func() {
		currentTime = time.Date(2026, 3, 17, 12, 0, 0, 0, time.UTC)
		freezer = &recordingFreezer{}
		notifier = &recordingNotifier{}
		reconciler = &DecisionGateReconciler{
			Client:   k8sClient,
			Scheme:   k8sClient.Scheme(),
			Freezer:  freezer,
			Notifier: notifier,
			Now:      func() time.Time { return currentTime },
		}
	})

	doReconcile := func(name string) (reconcile.Result, error) {
		return reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
	}

	Context("initialization", func() {
		It("should transition to Pending when target pod and container exist", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			gate := getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhasePending))
			Expect(gate.Status.FreezeTime).NotTo(BeNil())
			Expect(gate.Status.FreezeTime.Time.Unix()).To(Equal(currentTime.Unix()))
			Expect(gate.Status.Events).To(HaveLen(1))
			Expect(gate.Status.Events[0].Type).To(Equal(decisionsv1alpha1.GateEventFrozen))

			Expect(freezer.freezeCalls).To(HaveLen(1))
			Expect(freezer.freezeCalls[0].PodName).To(Equal(podName))
			Expect(freezer.freezeCalls[0].ContainerName).To(Equal("app"))
			Expect(notifier.calls).To(Equal(1))
		})

		It("should set the frozen label on the target pod", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			_, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())

			var pod corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: podName, Namespace: "default",
			}, &pod)).To(Succeed())
			Expect(pod.Labels).To(HaveKeyWithValue("epoche.dev/frozen", "true"))
		})

		It("should fail when target pod does not exist", func() {
			gateName := uniqueName("gate")
			createGate(ctx, gateName, "no-such-pod")

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			gate := getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseFailed))
			Expect(gate.Status.Events).To(HaveLen(1))
			Expect(gate.Status.Events[0].Type).To(Equal(decisionsv1alpha1.GateEventFailed))
			Expect(gate.Status.Events[0].Detail).To(ContainSubstring("not found"))

			Expect(freezer.freezeCalls).To(BeEmpty())
		})

		It("should fail when target container does not exist in pod", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)

			// Create gate targeting a container name that doesn't exist.
			gate := &decisionsv1alpha1.DecisionGate{
				ObjectMeta: metav1.ObjectMeta{Name: gateName, Namespace: "default"},
				Spec: decisionsv1alpha1.DecisionGateSpec{
					TargetRef: decisionsv1alpha1.TargetReference{
						Kind: "Pod", Name: podName, Container: "wrong-container",
					},
					Condition: decisionsv1alpha1.Condition{
						Type: "Test", Summary: "test", Severity: "Info",
					},
					Escalation: decisionsv1alpha1.Escalation{
						AllowedResponders: []decisionsv1alpha1.ResponderRef{{User: "u"}},
					},
					Timeout: decisionsv1alpha1.Timeout{Duration: "1m", DefaultAction: "Kill"},
					Options: []decisionsv1alpha1.Option{{Name: "Kill", Description: "k"}},
				},
			}
			Expect(k8sClient.Create(ctx, gate)).To(Succeed())

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			got := getGate(ctx, gateName)
			Expect(got.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseFailed))
			Expect(got.Status.Events[0].Detail).To(ContainSubstring("wrong-container"))
		})

		It("should fail when the freezer returns an error", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			freezer.freezeErr = fmt.Errorf("cgroup not available")

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			gate := getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseFailed))
			Expect(gate.Status.Events[0].Detail).To(ContainSubstring("cgroup not available"))
		})
	})

	Context("pending phase", func() {
		// Each test starts by initializing a gate to Pending.
		var gateName, podName string

		JustBeforeEach(func() {
			podName = uniqueName("pod")
			gateName = uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			_, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(getGate(ctx, gateName).Status.Phase).To(Equal(decisionsv1alpha1.GatePhasePending))
		})

		It("should transition to Decided when a valid response is provided", func() {
			gate := getGate(ctx, gateName)
			gate.Spec.Response = &decisionsv1alpha1.Response{
				Action:      "HeapDump",
				RespondedBy: "user:alice@example.com",
				Reason:      "suspected cache leak",
			}
			Expect(k8sClient.Update(ctx, gate)).To(Succeed())

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeTrue())

			gate = getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseDecided))
			Expect(gate.Status.Decision).NotTo(BeNil())
			Expect(gate.Status.Decision.Action).To(Equal("HeapDump"))
			Expect(gate.Status.Decision.DecidedBy).To(Equal("user:alice@example.com"))
			Expect(gate.Status.Decision.Reason).To(Equal("suspected cache leak"))
		})

		It("should reject an invalid action in the response", func() {
			gate := getGate(ctx, gateName)
			gate.Spec.Response = &decisionsv1alpha1.Response{
				Action:      "NonexistentAction",
				RespondedBy: "user:bob@example.com",
			}
			Expect(k8sClient.Update(ctx, gate)).To(Succeed())

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			gate = getGate(ctx, gateName)
			// Phase should stay Pending — invalid response doesn't transition.
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhasePending))
			Expect(gate.Status.Decision).To(BeNil())

			// Should have a condition explaining the rejection.
			Expect(gate.Status.Conditions).NotTo(BeEmpty())
			var found bool
			for _, c := range gate.Status.Conditions {
				if c.Type == "ResponseValid" && c.Reason == "InvalidAction" {
					found = true
					Expect(c.Message).To(ContainSubstring("NonexistentAction"))
				}
			}
			Expect(found).To(BeTrue(), "expected ResponseValid condition with InvalidAction reason")
		})

		It("should apply default action on timeout", func() {
			// Advance clock past the 5m timeout.
			currentTime = currentTime.Add(6 * time.Minute)

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeTrue())

			gate := getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseDecided))
			Expect(gate.Status.Decision).NotTo(BeNil())
			Expect(gate.Status.Decision.Action).To(Equal("Kill"))
			Expect(gate.Status.Decision.DecidedBy).To(Equal("system/timeout"))

			// Should have a TimedOut event.
			var timedOut bool
			for _, e := range gate.Status.Events {
				if e.Type == decisionsv1alpha1.GateEventTimedOut {
					timedOut = true
				}
			}
			Expect(timedOut).To(BeTrue(), "expected TimedOut event")
		})

		It("should requeue with remaining time when not yet timed out", func() {
			// Advance clock 2 minutes into the 5-minute timeout.
			currentTime = currentTime.Add(2 * time.Minute)

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(3 * time.Minute))
		})
	})

	Context("decided phase", func() {
		It("should unfreeze and transition to Executed", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			// Initialize → Pending
			_, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())

			// Provide response → Decided
			gate := getGate(ctx, gateName)
			gate.Spec.Response = &decisionsv1alpha1.Response{
				Action:      "Continue",
				RespondedBy: "user:alice@example.com",
			}
			Expect(k8sClient.Update(ctx, gate)).To(Succeed())
			_, err = doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(getGate(ctx, gateName).Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseDecided))

			// Execute → Executed
			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			gate = getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseExecuted))

			// Verify frozen label was removed from pod.
			var updatedPod corev1.Pod
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: podName, Namespace: "default",
			}, &updatedPod)).To(Succeed())
			Expect(updatedPod.Labels).NotTo(HaveKey("epoche.dev/frozen"))

			// Verify unfreeze was called.
			Expect(freezer.unfreezeCalls).To(HaveLen(1))
			Expect(freezer.unfreezeCalls[0].PodName).To(Equal(podName))

			// Verify event log has the full lifecycle.
			types := make([]decisionsv1alpha1.GateEventType, len(gate.Status.Events))
			for i, e := range gate.Status.Events {
				types[i] = e.Type
			}
			Expect(types).To(Equal([]decisionsv1alpha1.GateEventType{
				decisionsv1alpha1.GateEventFrozen,
				decisionsv1alpha1.GateEventDecided,
				decisionsv1alpha1.GateEventUnfrozen,
				decisionsv1alpha1.GateEventExecuted,
			}))

			// Verify Complete condition.
			var complete bool
			for _, c := range gate.Status.Conditions {
				if c.Type == "Complete" {
					complete = true
					Expect(c.Status).To(Equal(metav1.ConditionTrue))
				}
			}
			Expect(complete).To(BeTrue(), "expected Complete condition")
		})

		It("should complete the full lifecycle through timeout", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			// Initialize → Pending
			_, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())

			// Timeout → Decided
			currentTime = currentTime.Add(6 * time.Minute)
			_, err = doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(getGate(ctx, gateName).Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseDecided))

			// Execute → Executed
			_, err = doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())

			gate := getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseExecuted))
			Expect(gate.Status.Decision.Action).To(Equal("Kill"))
			Expect(gate.Status.Decision.DecidedBy).To(Equal("system/timeout"))
		})
	})

	Context("terminal phases", func() {
		It("should do nothing for an Executed gate", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			// Drive to Executed.
			_, _ = doReconcile(gateName)
			gate := getGate(ctx, gateName)
			gate.Spec.Response = &decisionsv1alpha1.Response{
				Action: "Kill", RespondedBy: "user:test",
			}
			Expect(k8sClient.Update(ctx, gate)).To(Succeed())
			_, _ = doReconcile(gateName)
			_, _ = doReconcile(gateName)
			Expect(getGate(ctx, gateName).Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseExecuted))

			// Reconcile again — should be a no-op.
			freezer.freezeCalls = nil
			freezer.unfreezeCalls = nil
			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(freezer.freezeCalls).To(BeEmpty())
			Expect(freezer.unfreezeCalls).To(BeEmpty())
		})

		It("should do nothing for a Failed gate", func() {
			gateName := uniqueName("gate")
			createGate(ctx, gateName, "no-such-pod")

			_, _ = doReconcile(gateName)
			Expect(getGate(ctx, gateName).Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseFailed))

			// Reconcile again — should be a no-op.
			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})
	})

	Context("edge cases", func() {
		It("should return no error for a non-existent gate", func() {
			result, err := doReconcile("does-not-exist")
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
		})

		It("should notify even if notification fails", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			notifier.err = fmt.Errorf("slack is down")

			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(5 * time.Minute))

			// Gate should still be Pending — notification failure is not fatal.
			gate := getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhasePending))
			Expect(notifier.calls).To(Equal(1))
		})

		It("should handle unfreeze failure gracefully in decided phase", func() {
			podName := uniqueName("pod")
			gateName := uniqueName("gate")
			createPod(ctx, podName)
			createGate(ctx, gateName, podName)

			// Initialize → Pending → Decided
			_, _ = doReconcile(gateName)
			gate := getGate(ctx, gateName)
			gate.Spec.Response = &decisionsv1alpha1.Response{
				Action: "Kill", RespondedBy: "user:test",
			}
			Expect(k8sClient.Update(ctx, gate)).To(Succeed())
			_, _ = doReconcile(gateName)

			// Make unfreeze fail.
			freezer.unfreezeErr = fmt.Errorf("container already dead")

			// Should still complete — Kill doesn't need an unfreeze.
			result, err := doReconcile(gateName)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			gate = getGate(ctx, gateName)
			Expect(gate.Status.Phase).To(Equal(decisionsv1alpha1.GatePhaseExecuted))
		})
	})
})

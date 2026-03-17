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
)

// DecisionGateSpec defines the desired state of a DecisionGate.
type DecisionGateSpec struct {
	// targetRef identifies the pod and container to freeze.
	// +required
	TargetRef TargetReference `json:"targetRef"`

	// condition describes why the gate was triggered.
	// +required
	Condition Condition `json:"condition"`

	// escalation defines how and to whom to escalate.
	// +required
	Escalation Escalation `json:"escalation"`

	// timeout defines what happens if no decision arrives.
	// +required
	Timeout Timeout `json:"timeout"`

	// options lists the available decisions a responder can choose.
	// +required
	// +kubebuilder:validation:MinItems=1
	Options []Option `json:"options"`
}

// TargetReference identifies a specific container in a pod.
type TargetReference struct {
	// kind of the target resource. Currently only "Pod" is supported.
	// +kubebuilder:validation:Enum=Pod
	// +kubebuilder:default=Pod
	Kind string `json:"kind"`

	// name of the target pod.
	// +required
	Name string `json:"name"`

	// container is the name of the container within the pod to freeze.
	// +required
	Container string `json:"container"`
}

// Condition describes the triggering condition with structured context.
type Condition struct {
	// type categorizes the condition (e.g. ResourcePressure, Deadlock, CircuitOpen).
	// +required
	Type string `json:"type"`

	// summary is a human-readable one-line description of the condition.
	// +required
	Summary string `json:"summary"`

	// detail provides additional context about the condition.
	// +optional
	Detail string `json:"detail,omitempty"`

	// severity indicates the urgency of the decision.
	// +kubebuilder:validation:Enum=Info;Warning;Critical
	// +required
	Severity string `json:"severity"`

	// metrics contains key-value pairs of relevant measurements at trigger time.
	// +optional
	Metrics map[string]string `json:"metrics,omitempty"`
}

// Escalation defines notification channels and authorized responders.
type Escalation struct {
	// channels lists notification targets to alert when the gate is created.
	// +optional
	Channels []Channel `json:"channels,omitempty"`

	// allowedResponders lists entities authorized to resolve this gate.
	// +required
	// +kubebuilder:validation:MinItems=1
	AllowedResponders []ResponderRef `json:"allowedResponders"`
}

// Channel defines a notification target.
type Channel struct {
	// type identifies the notification system (e.g. PagerDuty, Slack, Webhook).
	// +required
	Type string `json:"type"`

	// properties contains channel-specific configuration (e.g. serviceKey, channel, url).
	// +optional
	Properties map[string]string `json:"properties,omitempty"`
}

// ResponderRef identifies an entity authorized to make the decision.
// At least one of group, user, or serviceAccount must be set.
type ResponderRef struct {
	// group is a Kubernetes group name (e.g. "sre-oncall").
	// +optional
	Group string `json:"group,omitempty"`

	// user is a specific user identity (e.g. "alice@company.com").
	// +optional
	User string `json:"user,omitempty"`

	// serviceAccount is a namespace/name reference to a Kubernetes service account
	// authorized to resolve this gate programmatically.
	// +optional
	ServiceAccount string `json:"serviceAccount,omitempty"`
}

// Timeout defines the behavior when no decision arrives in time.
type Timeout struct {
	// duration is how long to wait before applying the default action.
	// Uses standard Kubernetes duration format (e.g. "5m", "1h").
	// +required
	Duration string `json:"duration"`

	// defaultAction is the option name to execute on timeout.
	// Must match one of the names in spec.options.
	// +required
	DefaultAction string `json:"defaultAction"`
}

// Option defines one possible decision a responder can choose.
type Option struct {
	// name is the identifier for this option, referenced by timeout.defaultAction
	// and decision.action.
	// +required
	Name string `json:"name"`

	// description explains what this option does, shown to responders.
	// +required
	Description string `json:"description"`
}

// GatePhase represents the lifecycle phase of a DecisionGate.
// +kubebuilder:validation:Enum=Pending;Decided;Executing;Executed;TimedOut;Failed
type GatePhase string

const (
	GatePhasePending   GatePhase = "Pending"
	GatePhaseDecided   GatePhase = "Decided"
	GatePhaseExecuting GatePhase = "Executing"
	GatePhaseExecuted  GatePhase = "Executed"
	GatePhaseTimedOut  GatePhase = "TimedOut"
	GatePhaseFailed    GatePhase = "Failed"
)

// DecisionGateStatus defines the observed state of a DecisionGate.
type DecisionGateStatus struct {
	// phase is the current lifecycle phase.
	// +optional
	Phase GatePhase `json:"phase,omitempty"`

	// freezeTime is when the container was frozen.
	// +optional
	FreezeTime *metav1.Time `json:"freezeTime,omitempty"`

	// decision records the chosen action and who chose it.
	// +optional
	Decision *Decision `json:"decision,omitempty"`

	// events records the lifecycle events of this gate.
	// +optional
	Events []GateEvent `json:"events,omitempty"`

	// conditions represent standard Kubernetes conditions for the resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Decision records the chosen action and who chose it.
type Decision struct {
	// action is the name of the chosen option.
	// +required
	Action string `json:"action"`

	// decidedBy identifies who made the decision.
	// +required
	DecidedBy string `json:"decidedBy"`

	// decidedAt is when the decision was made.
	// +required
	DecidedAt *metav1.Time `json:"decidedAt"`

	// reason is an optional human-provided justification for the choice.
	// +optional
	Reason string `json:"reason,omitempty"`
}

// GateEventType categorizes lifecycle events.
// +kubebuilder:validation:Enum=Frozen;Notified;Decided;Executing;Executed;TimedOut;Failed;Unfrozen
type GateEventType string

const (
	GateEventFrozen    GateEventType = "Frozen"
	GateEventNotified  GateEventType = "Notified"
	GateEventDecided   GateEventType = "Decided"
	GateEventExecuting GateEventType = "Executing"
	GateEventExecuted  GateEventType = "Executed"
	GateEventTimedOut  GateEventType = "TimedOut"
	GateEventFailed    GateEventType = "Failed"
	GateEventUnfrozen  GateEventType = "Unfrozen"
)

// GateEvent records a lifecycle event on the DecisionGate.
type GateEvent struct {
	// type categorizes this event.
	// +required
	Type GateEventType `json:"type"`

	// time is when the event occurred.
	// +required
	Time *metav1.Time `json:"time"`

	// detail provides additional context for this event.
	// +optional
	Detail string `json:"detail,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.spec.condition.severity`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DecisionGate is the Schema for the decisiongates API.
// It represents a deliberation point where a container is frozen and
// a decision is escalated to an authorized responder.
type DecisionGate struct {
	metav1.TypeMeta `json:",inline"`

	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// +required
	Spec DecisionGateSpec `json:"spec"`

	// +optional
	Status DecisionGateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DecisionGateList contains a list of DecisionGate.
type DecisionGateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DecisionGate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DecisionGate{}, &DecisionGateList{})
}

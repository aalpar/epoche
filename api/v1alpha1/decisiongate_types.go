// Package v1alpha1 defines the DecisionGate custom resource.
//
// A DecisionGate represents a deliberation point: a running container has
// reached a condition where the system should pause and escalate rather
// than act autonomously. The operator freezes the container, notifies
// responders, and waits for a decision before proceeding.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DecisionGateSpec defines the desired state of a DecisionGate.
type DecisionGateSpec struct {
	// TargetRef identifies the pod and container to freeze.
	TargetRef TargetReference `json:"targetRef"`

	// Condition describes why the gate was triggered.
	Condition Condition `json:"condition"`

	// Escalation defines how and to whom to escalate.
	Escalation Escalation `json:"escalation"`

	// Timeout defines what happens if no decision arrives.
	Timeout Timeout `json:"timeout"`

	// Options lists the available decisions a responder can choose.
	Options []Option `json:"options"`
}

// TargetReference identifies a specific container in a pod.
type TargetReference struct {
	Kind      string `json:"kind"`      // "Pod"
	Name      string `json:"name"`
	Container string `json:"container"`
}

// Condition describes the triggering condition with structured context.
type Condition struct {
	Type     string            `json:"type"`
	Summary  string            `json:"summary"`
	Detail   string            `json:"detail,omitempty"`
	Severity string            `json:"severity"` // Info, Warning, Critical
	Metrics  map[string]string `json:"metrics,omitempty"`
}

// Escalation defines notification channels and authorized responders.
type Escalation struct {
	Channels         []Channel         `json:"channels,omitempty"`
	AllowedResponders []ResponderRef   `json:"allowedResponders"`
}

// Channel defines a notification target.
type Channel struct {
	Type       string            `json:"type"` // PagerDuty, Slack, Webhook
	Properties map[string]string `json:"properties"`
}

// ResponderRef identifies an entity authorized to make the decision.
type ResponderRef struct {
	Group          string `json:"group,omitempty"`
	User           string `json:"user,omitempty"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
}

// Timeout defines the behavior when no decision arrives in time.
type Timeout struct {
	Duration      string `json:"duration"`
	DefaultAction string `json:"defaultAction"` // must match an Option name
}

// Option defines one possible decision a responder can choose.
type Option struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// DecisionGateStatus defines the observed state of a DecisionGate.
type DecisionGateStatus struct {
	Phase      string          `json:"phase"`                // Pending, Decided, Executed, TimedOut
	FreezeTime *metav1.Time    `json:"freezeTime,omitempty"`
	Decision   *Decision       `json:"decision,omitempty"`
	Events     []GateEvent     `json:"events,omitempty"`
}

// Decision records the chosen action and who chose it.
type Decision struct {
	Action    string       `json:"action"`
	DecidedBy string       `json:"decidedBy"`
	DecidedAt *metav1.Time `json:"decidedAt"`
	Reason    string       `json:"reason,omitempty"`
}

// GateEvent records a lifecycle event on the DecisionGate.
type GateEvent struct {
	Type   string       `json:"type"` // Frozen, Notified, Decided, Executed, TimedOut
	Time   *metav1.Time `json:"time"`
	Detail string       `json:"detail,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Severity",type=string,JSONPath=`.spec.condition.severity`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DecisionGate is the Schema for the decisiongates API.
type DecisionGate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DecisionGateSpec   `json:"spec,omitempty"`
	Status DecisionGateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DecisionGateList contains a list of DecisionGate.
type DecisionGateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DecisionGate `json:"items"`
}

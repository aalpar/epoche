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

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	decisionsv1alpha1 "github.com/aalpar/epoche/api/v1alpha1"
)

// LogNotifier logs notifications without sending them.
// Used for development and testing until real channel integrations are available.
type LogNotifier struct{}

func (n *LogNotifier) Notify(ctx context.Context, gate *decisionsv1alpha1.DecisionGate) error {
	log := logf.FromContext(ctx)
	for _, ch := range gate.Spec.Escalation.Channels {
		log.Info("Would send notification", "type", ch.Type, "properties", ch.Properties)
	}
	return nil
}

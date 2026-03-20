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
	"testing"
)

type recordingExecutor struct {
	calls []execCall
	err   error
}

type execCall struct {
	Namespace, PodName, ContainerName string
	Command                           []string
}

func (e *recordingExecutor) Exec(_ context.Context, namespace, podName, containerName string, command []string) error {
	e.calls = append(e.calls, execCall{namespace, podName, containerName, command})
	return e.err
}

func TestExecFreezer_Freeze(t *testing.T) {
	executor := &recordingExecutor{}
	freezer := &ExecFreezer{Exec: executor}

	err := freezer.Freeze(context.Background(), "default", "my-pod", "app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(executor.calls) != 1 {
		t.Fatalf("expected 1 exec call, got %d", len(executor.calls))
	}
	call := executor.calls[0]
	if call.Namespace != "default" || call.PodName != "my-pod" || call.ContainerName != "app" {
		t.Errorf("wrong target: %+v", call)
	}
	// Must send SIGSTOP (signal 19) to PID 1
	expected := []string{"kill", "-STOP", "1"}
	for i, v := range expected {
		if call.Command[i] != v {
			t.Errorf("command[%d] = %q, want %q", i, call.Command[i], v)
		}
	}
}

func TestExecFreezer_Unfreeze(t *testing.T) {
	executor := &recordingExecutor{}
	freezer := &ExecFreezer{Exec: executor}

	err := freezer.Unfreeze(context.Background(), "default", "my-pod", "app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	call := executor.calls[0]
	expected := []string{"kill", "-CONT", "1"}
	for i, v := range expected {
		if call.Command[i] != v {
			t.Errorf("command[%d] = %q, want %q", i, call.Command[i], v)
		}
	}
}

func TestExecFreezer_PropagatesError(t *testing.T) {
	executor := &recordingExecutor{err: fmt.Errorf("exec failed")}
	freezer := &ExecFreezer{Exec: executor}

	err := freezer.Freeze(context.Background(), "default", "my-pod", "app")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "freeze container app in pod default/my-pod: exec failed" {
		t.Errorf("unexpected error message: %v", err)
	}
}

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
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// Executor abstracts running a command inside a container.
type Executor interface {
	Exec(ctx context.Context, namespace, podName, containerName string, command []string) error
}

// ExecFreezer freezes and unfreezes containers by exec'ing kill signals.
type ExecFreezer struct {
	Exec Executor
}

func (f *ExecFreezer) Freeze(ctx context.Context, namespace, podName, containerName string) error {
	if err := f.Exec.Exec(ctx, namespace, podName, containerName, []string{"kill", "-STOP", "1"}); err != nil {
		return fmt.Errorf("freeze container %s in pod %s/%s: %w", containerName, namespace, podName, err)
	}
	return nil
}

func (f *ExecFreezer) Unfreeze(ctx context.Context, namespace, podName, containerName string) error {
	if err := f.Exec.Exec(ctx, namespace, podName, containerName, []string{"kill", "-CONT", "1"}); err != nil {
		return fmt.Errorf("unfreeze container %s in pod %s/%s: %w", containerName, namespace, podName, err)
	}
	return nil
}

// KubeExecutor implements Executor using the Kubernetes exec API.
type KubeExecutor struct {
	Client kubernetes.Interface
	Config *rest.Config
}

func (e *KubeExecutor) Exec(ctx context.Context, namespace, podName, containerName string, command []string) error {
	req := e.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.Config, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("exec %v: %s: %w", command, stderr.String(), err)
	}
	return nil
}

package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	sandboxLabelKey      = "agent-sandbox.dev/sandbox-id"
	sandboxContainerName = "sandbox"

	podReadyWaitTimeout = 60 * time.Second
)

// Executor is a spike: instead of spinning up a fresh Job (and therefore a
// fresh Pod, with its scheduling and image-pull latency) for every exec, it
// keeps one long-lived Pod per sandbox, created alongside the sandbox and
// torn down with it, and runs each command against that Pod via the
// pods/exec subresource. This exists to measure whether that removes the
// per-exec latency seen with the Job-per-exec approach.
//
// Executor itself has no notion of locking; concurrent Execs against the
// same sandbox's Pod race inside the container unless something above it
// serializes them. Service does that with an in-process, per-sandbox
// mutex (see Service.execLock), which only holds up as long as sandboxd
// runs as a single instance — see that doc comment before scaling it out.
type Executor struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

func NewExecutor(clientset kubernetes.Interface, restConfig *rest.Config, namespace string) *Executor {
	return &Executor{clientset: clientset, restConfig: restConfig, namespace: namespace}
}

func podName(sandboxID string) string {
	return fmt.Sprintf("sandbox-%s", sandboxID)
}

// CreatePod starts the sandbox's long-lived Pod and waits for it to reach
// Running before returning, so scheduling and image pull happen once, up
// front, instead of on every exec.
func (e *Executor) CreatePod(ctx context.Context, sandboxID, image string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(sandboxID),
			Namespace: e.namespace,
			Labels:    map[string]string{sandboxLabelKey: sandboxID},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{
					Name:    sandboxContainerName,
					Image:   image,
					Command: []string{"/bin/sh", "-c", "sleep infinity"},
				},
			},
		},
	}

	_, err := e.clientset.CoreV1().Pods(e.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	return e.waitForRunning(ctx, sandboxID)
}

// waitForRunning blocks until the sandbox's Pod reaches the Running phase.
func (e *Executor) waitForRunning(ctx context.Context, sandboxID string) error {
	waitCtx, cancel := context.WithTimeout(ctx, podReadyWaitTimeout)
	defer cancel()

	watcher, err := e.clientset.CoreV1().Pods(e.namespace).Watch(waitCtx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", sandboxLabelKey, sandboxID),
	})
	if err != nil {
		return fmt.Errorf("watch pod for sandbox %s: %w", sandboxID, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timed out waiting for sandbox pod to start: %w", waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return fmt.Errorf("pod watch closed before sandbox pod started")
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodRunning:
				return nil
			case corev1.PodFailed:
				return fmt.Errorf("sandbox pod failed to start")
			}
		}
	}
}

// Exec runs command inside the sandbox's already-running Pod and streams
// its combined stdout/stderr to out.
func (e *Executor) Exec(ctx context.Context, sandboxID, command string, out io.Writer) error {
	req := e.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(e.namespace).
		Name(podName(sandboxID)).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: sandboxContainerName,
			Command:   []string{"/bin/sh", "-c", command},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build exec stream for sandbox %s: %w", sandboxID, err)
	}

	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: out,
		Stderr: out,
	}); err != nil {
		return fmt.Errorf("exec in sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// DeletePod removes the sandbox's Pod.
func (e *Executor) DeletePod(ctx context.Context, sandboxID string) error {
	err := e.clientset.CoreV1().Pods(e.namespace).Delete(ctx, podName(sandboxID), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

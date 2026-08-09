package sandbox

import (
	"bytes"
	"context"
	"errors"
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
	clientgoexec "k8s.io/client-go/util/exec"
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

// podExecutor builds the SPDY executor for a pods/exec call against
// sandboxID's Pod running command, with stdin wired in iff withStdin is set.
func (e *Executor) podExecutor(sandboxID string, command []string, withStdin bool) (remotecommand.Executor, error) {
	req := e.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(e.namespace).
		Name(podName(sandboxID)).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: sandboxContainerName,
			Command:   command,
			Stdin:     withStdin,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.restConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("build exec stream for sandbox %s: %w", sandboxID, err)
	}
	return executor, nil
}

// Exec runs command inside the sandbox's already-running Pod and streams
// its combined stdout/stderr to out.
func (e *Executor) Exec(ctx context.Context, sandboxID, command string, out io.Writer) error {
	executor, err := e.podExecutor(sandboxID, []string{"/bin/sh", "-c", command}, false)
	if err != nil {
		return err
	}

	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: out,
		Stderr: out,
	}); err != nil {
		return fmt.Errorf("exec in sandbox %s: %w", sandboxID, err)
	}
	return nil
}

// UploadFile writes r to path inside the sandbox's Pod, creating parent
// directories as needed and overwriting any existing file. Unlike Exec,
// stderr is kept separate from the transfer and only surfaced as part of
// the returned error, since here stdin carries file bytes that must not be
// interleaved with anything else.
func (e *Executor) UploadFile(ctx context.Context, sandboxID, path string, r io.Reader) error {
	executor, err := e.podExecutor(sandboxID, []string{"sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path}, true)
	if err != nil {
		return err
	}

	var stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  r,
		Stdout: io.Discard,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("upload file to sandbox %s: %w", sandboxID, annotate(err, &stderr))
	}
	return nil
}

// DownloadFile streams path's contents from inside the sandbox's Pod to w.
// Callers are expected to have confirmed the file exists (see StatFile)
// before calling this, since once w starts receiving bytes there is no way
// to turn a failure partway through into anything but a truncated stream.
func (e *Executor) DownloadFile(ctx context.Context, sandboxID, path string, w io.Writer) error {
	executor, err := e.podExecutor(sandboxID, []string{"cat", path}, false)
	if err != nil {
		return err
	}

	var stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: w,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("download file from sandbox %s: %w", sandboxID, annotate(err, &stderr))
	}
	return nil
}

// StatFile reports whether path exists as a regular file inside the
// sandbox's Pod. It runs synchronously, with no streaming, so callers can
// resolve existence before committing to an HTTP response status — the same
// reason Service.Exec resolves lock/existence errors before its handler
// commits headers to an SSE stream.
func (e *Executor) StatFile(ctx context.Context, sandboxID, path string) (bool, error) {
	executor, err := e.podExecutor(sandboxID, []string{"test", "-f", path}, false)
	if err != nil {
		return false, err
	}

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err == nil {
		return true, nil
	}
	var exitErr clientgoexec.CodeExitError
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("stat file in sandbox %s: %w", sandboxID, err)
}

// annotate appends any captured stderr output to err's message, so failures
// from the shell snippets above (missing permissions, bad paths, ...) are
// diagnosable instead of surfacing as a bare non-zero exit code.
func annotate(err error, stderr *bytes.Buffer) error {
	if stderr.Len() == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, bytes.TrimSpace(stderr.Bytes()))
}

// DeletePod removes the sandbox's Pod.
func (e *Executor) DeletePod(ctx context.Context, sandboxID string) error {
	err := e.clientset.CoreV1().Pods(e.namespace).Delete(ctx, podName(sandboxID), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

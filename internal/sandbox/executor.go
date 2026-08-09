package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

const (
	execLabelKey = "agent-sandbox.dev/sandbox-id"

	// jobTTLSeconds is a backstop: the backend explicitly deletes the Job
	// once it has finished reading its logs, but if that never happens
	// (e.g. the process crashes mid-exec) the TTL-after-finished controller
	// still reclaims it, so a sandbox never gets permanently wedged.
	jobTTLSeconds int32 = 10

	// jobActiveDeadlineSeconds bounds a single exec so a hung command
	// can't hold the lock (the Job's existence) forever.
	jobActiveDeadlineSeconds int64 = 600

	podReadyWaitTimeout = 60 * time.Second
)

// Executor runs a single command to completion in an ephemeral Job. The
// Job's name is deterministic per sandbox, so a second concurrent exec
// attempt fails at Create with AlreadyExists — that conflict, straight from
// the Kubernetes API server, is the distributed lock. No separate lock
// object or CR status field is needed.
type Executor struct {
	clientset kubernetes.Interface
	namespace string
}

func NewExecutor(clientset kubernetes.Interface, namespace string) *Executor {
	return &Executor{clientset: clientset, namespace: namespace}
}

func jobName(sandboxID string) string {
	return fmt.Sprintf("sandbox-%s-exec", sandboxID)
}

// CreateJob starts the exec Job for sandboxID. It returns ErrExecuting if
// one is already outstanding (running, or finished but not yet cleaned up).
func (e *Executor) CreateJob(ctx context.Context, sandboxID, image, command string) error {
	backoffLimit := int32(0)
	ttl := jobTTLSeconds
	deadline := jobActiveDeadlineSeconds

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName(sandboxID),
			Namespace: e.namespace,
			Labels:    map[string]string{execLabelKey: sandboxID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{execLabelKey: sandboxID},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "exec",
							Image:   image,
							Command: []string{"/bin/sh", "-c", command},
						},
					},
				},
			},
		},
	}

	_, err := e.clientset.BatchV1().Jobs(e.namespace).Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return ErrExecuting
	}
	return err
}

// StreamLogs waits for the exec Job's pod to start, then follows its
// combined stdout/stderr into out until the container terminates.
func (e *Executor) StreamLogs(ctx context.Context, sandboxID string, out io.Writer) error {
	podName, err := e.waitForPod(ctx, sandboxID)
	if err != nil {
		return err
	}

	req := e.clientset.CoreV1().Pods(e.namespace).GetLogs(podName, &corev1.PodLogOptions{Follow: true})
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("open log stream for pod %s: %w", podName, err)
	}
	defer stream.Close()

	if _, err := io.Copy(out, stream); err != nil {
		return fmt.Errorf("stream logs for pod %s: %w", podName, err)
	}
	return nil
}

// waitForPod blocks until the Job's pod exists and has either started
// running or already reached a terminal phase, returning its name.
func (e *Executor) waitForPod(ctx context.Context, sandboxID string) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, podReadyWaitTimeout)
	defer cancel()

	watcher, err := e.clientset.CoreV1().Pods(e.namespace).Watch(waitCtx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName(sandboxID)),
	})
	if err != nil {
		return "", fmt.Errorf("watch pods for sandbox %s: %w", sandboxID, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("timed out waiting for exec pod to start: %w", waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return "", fmt.Errorf("pod watch closed before exec pod started")
			}
			if event.Type == watch.Deleted {
				continue
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}
			switch pod.Status.Phase {
			case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
				return pod.Name, nil
			}
		}
	}
}

// IsExecuting reports whether an exec Job is currently outstanding for the
// given sandbox (running, or finished but not yet cleaned up).
func (e *Executor) IsExecuting(ctx context.Context, sandboxID string) (bool, error) {
	_, err := e.clientset.BatchV1().Jobs(e.namespace).Get(ctx, jobName(sandboxID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteJob removes the exec Job, cascading to its pod. Best-effort: see
// jobTTLSeconds for the backstop if this never runs or fails.
func (e *Executor) DeleteJob(ctx context.Context, sandboxID string) error {
	propagation := metav1.DeletePropagationBackground
	err := e.clientset.BatchV1().Jobs(e.namespace).Delete(ctx, jobName(sandboxID), metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

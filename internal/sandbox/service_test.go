package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

type fakeExecutor struct {
	createErr  error
	execErr    error
	execOutput string
	deleteErr  error

	createCalls int
	execCalls   int
	deleteCalls int
}

func (f *fakeExecutor) CreatePod(ctx context.Context, sandboxID, image string) error {
	f.createCalls++
	return f.createErr
}

func (f *fakeExecutor) Exec(ctx context.Context, sandboxID, command string, out io.Writer) error {
	f.execCalls++
	if f.execOutput != "" {
		_, _ = out.Write([]byte(f.execOutput))
	}
	return f.execErr
}

func (f *fakeExecutor) DeletePod(ctx context.Context, sandboxID string) error {
	f.deleteCalls++
	return f.deleteErr
}

func newTestService(exec *fakeExecutor) (*Service, *Store) {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxGVR: "SandboxList",
	})
	store := NewStore(client, "default")
	return NewService(store, exec), store
}

const validID = "d290f1ee-6c54-4b01-90e6-d701748f0851"

func TestServiceCreateValidatesID(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	if err := svc.Create(context.Background(), "not-a-uuid", "alpine"); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestServiceCreateRejectsUppercaseUUID(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	upper := "D290F1EE-6C54-4B01-90E6-D701748F0851"
	if err := svc.Create(context.Background(), upper, "alpine"); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for non-canonical uuid, got %v", err)
	}
}

func TestServiceCreateValidatesImage(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	if err := svc.Create(context.Background(), validID, "  "); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation for blank image, got %v", err)
	}
}

func TestServiceCreateAndGet(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine:latest"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sb, err := svc.Get(ctx, validID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sb.ID != validID || sb.Image != "alpine:latest" {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}
}

func TestServiceDeleteNotFound(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	if err := svc.Delete(context.Background(), validID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestServiceDeleteRemovesPodThenRecord(t *testing.T) {
	exec := &fakeExecutor{}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, validID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if exec.deleteCalls != 1 {
		t.Fatalf("expected DeletePod to be called once, got %d", exec.deleteCalls)
	}
	if _, err := svc.Get(ctx, validID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected sandbox record to be gone, got %v", err)
	}
}

func TestServiceCreateRollsBackRecordWhenPodFailsToStart(t *testing.T) {
	exec := &fakeExecutor{createErr: errors.New("boom")}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err == nil {
		t.Fatalf("expected Create to fail when CreatePod fails")
	}
	if _, err := svc.Get(ctx, validID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected sandbox record to be rolled back, got %v", err)
	}
}

func TestServiceExecRejectsEmptyCommand(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	_, err := svc.Exec(context.Background(), validID, "   ")
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestServiceExecNotFound(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	_, err := svc.Exec(context.Background(), validID, "echo hi")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestServiceExecRejectsConcurrentExec(t *testing.T) {
	exec := &fakeExecutor{}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	start1, err := svc.Exec(ctx, validID, "sleep 1")
	if err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if _, err := svc.Exec(ctx, validID, "echo hi"); !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting for concurrent exec, got %v", err)
	}

	var buf bytes.Buffer
	if err := start1(&buf); err != nil {
		t.Fatalf("start1: %v", err)
	}

	// The lock is released once the first exec's stream finishes, so a
	// second exec should now be allowed.
	start2, err := svc.Exec(ctx, validID, "echo hi")
	if err != nil {
		t.Fatalf("Exec after release: %v", err)
	}
	if err := start2(&buf); err != nil {
		t.Fatalf("start2: %v", err)
	}
}

func TestServiceDeleteRejectsWhileExecuting(t *testing.T) {
	exec := &fakeExecutor{}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	start, err := svc.Exec(ctx, validID, "sleep 1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := svc.Delete(ctx, validID); !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
	}

	var buf bytes.Buffer
	if err := start(&buf); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := svc.Delete(ctx, validID); err != nil {
		t.Fatalf("Delete after exec finished: %v", err)
	}
}

func TestServiceExecStreamsOutput(t *testing.T) {
	exec := &fakeExecutor{execOutput: "hello world"}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	start, err := svc.Exec(ctx, validID, "echo hello world")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	var buf bytes.Buffer
	if err := start(&buf); err != nil {
		t.Fatalf("start: %v", err)
	}
	if buf.String() != "hello world" {
		t.Fatalf("unexpected output: %q", buf.String())
	}
}

func TestServiceExecSurfacesStreamError(t *testing.T) {
	exec := &fakeExecutor{execErr: errors.New("boom")}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	start, err := svc.Exec(ctx, validID, "echo hi")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	var buf bytes.Buffer
	if err := start(&buf); err == nil {
		t.Fatalf("expected start to surface the stream error")
	}
}

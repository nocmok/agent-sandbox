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
	createErr    error
	streamErr    error
	streamOutput string
	executing    bool
	executingErr error

	createCalls int
	deleteCalls int
}

func (f *fakeExecutor) CreateJob(ctx context.Context, sandboxID, image, command string) error {
	f.createCalls++
	return f.createErr
}

func (f *fakeExecutor) StreamLogs(ctx context.Context, sandboxID string, out io.Writer) error {
	if f.streamOutput != "" {
		_, _ = out.Write([]byte(f.streamOutput))
	}
	return f.streamErr
}

func (f *fakeExecutor) IsExecuting(ctx context.Context, sandboxID string) (bool, error) {
	return f.executing, f.executingErr
}

func (f *fakeExecutor) DeleteJob(ctx context.Context, sandboxID string) error {
	f.deleteCalls++
	return nil
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

func TestServiceDeleteRejectsWhileExecuting(t *testing.T) {
	exec := &fakeExecutor{executing: true}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, validID); !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
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

func TestServiceExecAlreadyExecuting(t *testing.T) {
	exec := &fakeExecutor{createErr: ErrExecuting}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := svc.Exec(ctx, validID, "echo hi")
	if !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
	}
	if exec.createCalls != 1 {
		t.Fatalf("expected exactly one CreateJob call, got %d", exec.createCalls)
	}
}

func TestServiceExecStreamsOutputAndCleansUp(t *testing.T) {
	exec := &fakeExecutor{streamOutput: "hello world"}
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
	if exec.deleteCalls != 1 {
		t.Fatalf("expected DeleteJob to be called once after streaming, got %d", exec.deleteCalls)
	}
}

func TestServiceExecCleansUpEvenOnStreamError(t *testing.T) {
	exec := &fakeExecutor{streamErr: errors.New("boom")}
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
	if exec.deleteCalls != 1 {
		t.Fatalf("expected DeleteJob to be called even after a stream error, got %d", exec.deleteCalls)
	}
}

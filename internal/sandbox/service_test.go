package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
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

	uploadErr      error
	uploaded       []byte
	statExists     bool
	statErr        error
	downloadErr    error
	downloadOutput string

	createCalls   int
	execCalls     int
	deleteCalls   int
	uploadCalls   int
	downloadCalls int
	statCalls     int
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

func (f *fakeExecutor) UploadFile(ctx context.Context, sandboxID, path string, r io.Reader) error {
	f.uploadCalls++
	if f.uploadErr != nil {
		return f.uploadErr
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.uploaded = data
	return nil
}

func (f *fakeExecutor) DownloadFile(ctx context.Context, sandboxID, path string, w io.Writer) error {
	f.downloadCalls++
	if f.downloadOutput != "" {
		_, _ = w.Write([]byte(f.downloadOutput))
	}
	return f.downloadErr
}

func (f *fakeExecutor) StatFile(ctx context.Context, sandboxID, path string) (bool, error) {
	f.statCalls++
	return f.statExists, f.statErr
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

func TestServiceUploadFileRejectsEmptyPath(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	if err := svc.UploadFile(context.Background(), validID, "  ", bytes.NewReader(nil)); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestServiceUploadFileNotFound(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	if err := svc.UploadFile(context.Background(), validID, "a.txt", bytes.NewReader(nil)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestServiceUploadFileWritesContent(t *testing.T) {
	exec := &fakeExecutor{}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.UploadFile(ctx, validID, "a.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if string(exec.uploaded) != "hello" {
		t.Fatalf("unexpected uploaded content: %q", exec.uploaded)
	}
}

func TestServiceUploadFileRejectsWhileExecuting(t *testing.T) {
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
	if err := svc.UploadFile(ctx, validID, "a.txt", strings.NewReader("hello")); !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
	}

	var buf bytes.Buffer
	if err := start(&buf); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestServiceDownloadFileRejectsEmptyPath(t *testing.T) {
	svc, _ := newTestService(&fakeExecutor{})
	if _, err := svc.DownloadFile(context.Background(), validID, "  "); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestServiceDownloadFileNotFound(t *testing.T) {
	exec := &fakeExecutor{statExists: false}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.DownloadFile(ctx, validID, "missing.txt"); !errors.Is(err, ErrFileNotFound) {
		t.Fatalf("expected ErrFileNotFound, got %v", err)
	}
	if exec.downloadCalls != 0 {
		t.Fatalf("expected DownloadFile not to be called when stat says missing, got %d calls", exec.downloadCalls)
	}
}

func TestServiceDownloadFileStreamsContent(t *testing.T) {
	exec := &fakeExecutor{statExists: true, downloadOutput: "hello world"}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	start, err := svc.DownloadFile(ctx, validID, "a.txt")
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	var buf bytes.Buffer
	if err := start(&buf); err != nil {
		t.Fatalf("start: %v", err)
	}
	if buf.String() != "hello world" {
		t.Fatalf("unexpected content: %q", buf.String())
	}
}

func TestServiceDownloadFileRejectsWhileExecuting(t *testing.T) {
	exec := &fakeExecutor{statExists: true}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	start, err := svc.Exec(ctx, validID, "sleep 1")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if _, err := svc.DownloadFile(ctx, validID, "a.txt"); !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
	}

	var buf bytes.Buffer
	if err := start(&buf); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func TestServiceDownloadFileReleasesLockAfterStatFails(t *testing.T) {
	exec := &fakeExecutor{statErr: errors.New("boom")}
	svc, _ := newTestService(exec)
	ctx := context.Background()
	if err := svc.Create(ctx, validID, "alpine"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := svc.DownloadFile(ctx, validID, "a.txt"); err == nil {
		t.Fatalf("expected DownloadFile to surface the stat error")
	}

	// The lock must have been released even though DownloadFile failed
	// before returning a start function, or every later Exec/Delete would
	// wrongly see ErrExecuting.
	if _, err := svc.Exec(ctx, validID, "echo hi"); err != nil {
		t.Fatalf("expected Exec to succeed after failed DownloadFile, got %v", err)
	}
}

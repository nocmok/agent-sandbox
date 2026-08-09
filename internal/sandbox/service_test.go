package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func newTestService() (*Service, *fakeRepository, *fakeDocker, *fakeStorage) {
	repo := newFakeRepository()
	docker := newFakeDocker()
	storage := newFakeStorage()
	svc := NewService(repo, docker, storage)
	return svc, repo, docker, storage
}

func noopOnStart() {}

func TestCreateSandbox_HappyPath(t *testing.T) {
	svc, _, _, storage := newTestService()
	sb, err := svc.CreateSandbox(context.Background(), "alpine", "/workspace", uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !storage.allocated[sb.ID] {
		t.Fatal("expected storage directory to be allocated")
	}
}

func TestCreateSandbox_IdempotencyKeyReturnsExisting(t *testing.T) {
	svc, _, _, _ := newTestService()
	key := uuid.New()
	sb1, err := svc.CreateSandbox(context.Background(), "alpine", "/workspace", key)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	sb2, err := svc.CreateSandbox(context.Background(), "alpine", "/workspace", key)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sb1.ID != sb2.ID {
		t.Fatalf("expected same sandbox id for repeated idempotency key, got %s and %s", sb1.ID, sb2.ID)
	}
}

func TestCreateSandbox_Validation(t *testing.T) {
	svc, _, _, _ := newTestService()
	cases := []struct {
		name      string
		image     string
		workspace string
	}{
		{"empty image", "", "/workspace"},
		{"relative workspace path", "alpine", "workspace"},
		{"empty workspace path", "alpine", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateSandbox(context.Background(), tc.image, tc.workspace, uuid.New())
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func TestListSandboxes_Paginates(t *testing.T) {
	svc, _, _, _ := newTestService()
	for i := 0; i < 3; i++ {
		mustCreate(t, svc)
	}

	page0, err := svc.ListSandboxes(context.Background(), 0, 2)
	if err != nil {
		t.Fatalf("list page 0: %v", err)
	}
	if len(page0) != 2 {
		t.Fatalf("expected 2 results on page 0, got %d", len(page0))
	}

	page1, err := svc.ListSandboxes(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(page1) != 1 {
		t.Fatalf("expected 1 result on page 1, got %d", len(page1))
	}
}

func TestListSandboxes_Validation(t *testing.T) {
	svc, _, _, _ := newTestService()
	cases := []struct {
		name string
		page int
		size int
	}{
		{"negative page", -1, 10},
		{"zero size", 0, 0},
		{"negative size", 0, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ListSandboxes(context.Background(), tc.page, tc.size)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("expected ErrValidation, got %v", err)
			}
		})
	}
}

func mustCreate(t *testing.T, svc *Service) Sandbox {
	t.Helper()
	sb, err := svc.CreateSandbox(context.Background(), "alpine", "/workspace", uuid.New())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return sb
}

func TestExec_HappyPathCallsDockerInOrderAndCleansUp(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)
	docker.runExitCode = 0
	docker.runOutput = "hello\n"

	var buf bytes.Buffer
	started := false
	code, err := svc.Exec(context.Background(), sb.ID, "echo hello", func() { started = true }, &buf)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !started {
		t.Fatal("expected onStart to be called")
	}
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}
	if buf.String() != "hello\n" {
		t.Fatalf("expected captured output, got %q", buf.String())
	}

	want := []string{"ContainerStatus", "ImageExists", "PullImage", "CreateVolume", "RunContainer", "RemoveVolume"}
	if len(docker.calls) != len(want) {
		t.Fatalf("expected calls %v, got %v", want, docker.calls)
	}
	for i, name := range want {
		if docker.calls[i] != name {
			t.Fatalf("expected call %d to be %s, got %s (calls=%v)", i, name, docker.calls[i], docker.calls)
		}
	}
}

func TestExec_SkipsPullWhenImageAlreadyPresent(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)
	docker.imageExists = true

	var buf bytes.Buffer
	_, err := svc.Exec(context.Background(), sb.ID, "echo hi", noopOnStart, &buf)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	for _, c := range docker.calls {
		if c == "PullImage" {
			t.Fatalf("expected PullImage to be skipped when image already exists, got %v", docker.calls)
		}
	}
	if !slices.Contains(docker.calls, "ImageExists") {
		t.Fatalf("expected ImageExists to be called, got %v", docker.calls)
	}
}

func TestExec_NotFound(t *testing.T) {
	svc, _, docker, _ := newTestService()
	var buf bytes.Buffer
	_, err := svc.Exec(context.Background(), uuid.New(), "echo hi", noopOnStart, &buf)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	if docker.callCount() != 0 {
		t.Fatalf("expected no docker calls, got %v", docker.calls)
	}
}

func TestExec_AlreadyExecutingReturnsErrExecuting(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)
	docker.containerStatus = ContainerStatusRunning

	var buf bytes.Buffer
	onStartCalled := false
	_, err := svc.Exec(context.Background(), sb.ID, "echo hi", func() { onStartCalled = true }, &buf)
	if !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
	}
	if onStartCalled {
		t.Fatal("expected onStart not to be called when already executing")
	}
}

func TestExec_LeftoverExitedContainerIsCleanedUpThenExecRuns(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)
	docker.containerStatus = ContainerStatusExited

	var buf bytes.Buffer
	code, err := svc.Exec(context.Background(), sb.ID, "echo hi", noopOnStart, &buf)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d", code)
	}

	found := map[string]bool{}
	for _, c := range docker.calls {
		found[c] = true
	}
	if !found["RemoveContainer"] {
		t.Fatalf("expected leftover container to be removed, got %v", docker.calls)
	}
}

func TestExec_EmptyCommandIsValidationError(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)

	var buf bytes.Buffer
	_, err := svc.Exec(context.Background(), sb.ID, "   ", noopOnStart, &buf)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if docker.callCount() != 0 {
		t.Fatalf("expected no docker calls, got %v", docker.calls)
	}
}

func TestExec_RunFailureStillRemovesVolume(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)
	docker.runErr = errors.New("boom")

	var buf bytes.Buffer
	_, err := svc.Exec(context.Background(), sb.ID, "echo hi", noopOnStart, &buf)
	if err == nil {
		t.Fatal("expected error")
	}
	found := map[string]bool{}
	for _, c := range docker.calls {
		found[c] = true
	}
	if !found["RemoveVolume"] {
		t.Fatalf("expected volume cleanup after run failure, got %v", docker.calls)
	}
}

func TestDeleteSandbox_HappyPath(t *testing.T) {
	svc, _, _, storage := newTestService()
	sb := mustCreate(t, svc)

	if err := svc.DeleteSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !storage.removed[sb.ID] {
		t.Fatal("expected storage directory to be removed")
	}
	_, err := svc.GetSandbox(context.Background(), sb.ID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestDeleteSandbox_ExecutingFailsAndDoesNotDelete(t *testing.T) {
	svc, _, docker, storage := newTestService()
	sb := mustCreate(t, svc)
	docker.containerStatus = ContainerStatusRunning

	err := svc.DeleteSandbox(context.Background(), sb.ID)
	if !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
	}
	if storage.removed[sb.ID] {
		t.Fatal("expected storage directory NOT to be removed when delete is rejected")
	}
	if _, err := svc.GetSandbox(context.Background(), sb.ID); err != nil {
		t.Fatalf("expected sandbox to still exist, got %v", err)
	}
}

func TestDeleteSandbox_NotFound(t *testing.T) {
	svc, _, _, _ := newTestService()
	err := svc.DeleteSandbox(context.Background(), uuid.New())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUploadThenDownloadFile(t *testing.T) {
	svc, _, _, _ := newTestService()
	sb := mustCreate(t, svc)

	if err := svc.UploadFile(context.Background(), sb.ID, "a.txt", bytes.NewReader([]byte("hi"))); err != nil {
		t.Fatalf("upload: %v", err)
	}

	var got []byte
	err := svc.DownloadFile(context.Background(), sb.ID, "a.txt", func(r io.Reader) error {
		var err error
		got, err = io.ReadAll(r)
		return err
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("expected 'hi', got %q", got)
	}
}

func TestUploadFile_RejectedWhileExecuting(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)
	docker.containerStatus = ContainerStatusRunning

	err := svc.UploadFile(context.Background(), sb.ID, "a.txt", bytes.NewReader([]byte("hi")))
	if !errors.Is(err, ErrExecuting) {
		t.Fatalf("expected ErrExecuting, got %v", err)
	}
}

func TestExec_ConcurrentCallsOnSameSandboxOneWins(t *testing.T) {
	svc, _, docker, _ := newTestService()
	sb := mustCreate(t, svc)

	docker.blockOn = "PullImage"
	docker.unblock = make(chan struct{})
	docker.entered = make(chan struct{})

	var wg sync.WaitGroup
	firstErrCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		var buf bytes.Buffer
		_, err := svc.Exec(context.Background(), sb.ID, "echo hi", noopOnStart, &buf)
		firstErrCh <- err
	}()

	<-docker.entered // first goroutine now holds the lock, blocked inside PullImage

	var buf bytes.Buffer
	_, secondErr := svc.Exec(context.Background(), sb.ID, "echo hi", noopOnStart, &buf)
	if !errors.Is(secondErr, ErrLocked) {
		t.Fatalf("expected ErrLocked while first exec is in flight, got %v", secondErr)
	}

	close(docker.unblock)
	wg.Wait()
	if firstErr := <-firstErrCh; firstErr != nil {
		t.Fatalf("expected first exec to succeed, got %v", firstErr)
	}
}

func TestExec_DifferentSandboxesDoNotContend(t *testing.T) {
	svc, _, _, _ := newTestService()
	sb1 := mustCreate(t, svc)
	sb2 := mustCreate(t, svc)

	var buf1, buf2 bytes.Buffer
	if _, err := svc.Exec(context.Background(), sb1.ID, "echo hi", noopOnStart, &buf1); err != nil {
		t.Fatalf("exec sb1: %v", err)
	}
	if _, err := svc.Exec(context.Background(), sb2.ID, "echo hi", noopOnStart, &buf2); err != nil {
		t.Fatalf("exec sb2: %v", err)
	}
}

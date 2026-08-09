package sandbox

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func newTestStore() *Store {
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		sandboxGVR: "SandboxList",
	})
	return NewStore(client, "default")
}

func TestStoreCreateAndGet(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	if err := s.Create(ctx, "sbx-1", "alpine:latest"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, "sbx-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "sbx-1" || got.Spec.Image != "alpine:latest" {
		t.Fatalf("unexpected sandbox: %+v", got)
	}
}

func TestStoreCreateDuplicate(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()

	if err := s.Create(ctx, "sbx-1", "alpine:latest"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	err := s.Create(ctx, "sbx-1", "alpine:latest")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestStoreGetNotFound(t *testing.T) {
	s := newTestStore()
	_, err := s.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStoreDeleteNotFound(t *testing.T) {
	s := newTestStore()
	err := s.Delete(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestStoreDeleteRemovesSandbox(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()
	if err := s.Create(ctx, "sbx-1", "alpine:latest"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete(ctx, "sbx-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "sbx-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestStoreListPagination(t *testing.T) {
	s := newTestStore()
	ctx := context.Background()
	ids := []string{"c", "a", "b", "d", "e"}
	for _, id := range ids {
		if err := s.Create(ctx, id, "alpine:latest"); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}

	page0, err := s.List(ctx, 0, 2)
	if err != nil {
		t.Fatalf("List page0: %v", err)
	}
	if got := []string{page0[0].Name, page0[1].Name}; got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected sorted [a b], got %v", got)
	}

	page2, err := s.List(ctx, 2, 2)
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2) != 1 || page2[0].Name != "e" {
		t.Fatalf("expected last page [e], got %v", page2)
	}

	pageOOB, err := s.List(ctx, 5, 2)
	if err != nil {
		t.Fatalf("List pageOOB: %v", err)
	}
	if len(pageOOB) != 0 {
		t.Fatalf("expected empty out-of-bounds page, got %v", pageOOB)
	}
}

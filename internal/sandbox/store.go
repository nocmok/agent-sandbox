package sandbox

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

// Store persists Sandbox metadata as instances of the Sandbox custom
// resource. It is the sole source of truth for sandbox existence.
type Store struct {
	client    dynamic.Interface
	namespace string
}

func NewStore(client dynamic.Interface, namespace string) *Store {
	return &Store{client: client, namespace: namespace}
}

func (s *Store) resource() dynamic.ResourceInterface {
	return s.client.Resource(sandboxGVR).Namespace(s.namespace)
}

func (s *Store) Create(ctx context.Context, id, image string) error {
	res := &SandboxResource{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sandboxGVR.GroupVersion().String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      id,
			Namespace: s.namespace,
		},
		Spec: SandboxSpec{Image: image},
	}
	u, err := toUnstructured(res)
	if err != nil {
		return err
	}
	_, err = s.resource().Create(ctx, u, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return ErrAlreadyExists
	}
	return err
}

func (s *Store) Get(ctx context.Context, id string) (*SandboxResource, error) {
	u, err := s.resource().Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fromUnstructured(u)
}

// List returns sandboxes ordered by name, sliced into the requested page.
// Kubernetes' list API doesn't offer offset-based pagination, so for this
// POC's scale we list everything and paginate in-process.
func (s *Store) List(ctx context.Context, page, size int) ([]SandboxResource, error) {
	list, err := s.resource().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	items := make([]SandboxResource, 0, len(list.Items))
	for i := range list.Items {
		res, err := fromUnstructured(&list.Items[i])
		if err != nil {
			return nil, err
		}
		items = append(items, *res)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	start := page * size
	if start >= len(items) {
		return []SandboxResource{}, nil
	}
	end := start + size
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], nil
}

func (s *Store) Delete(ctx context.Context, id string) error {
	err := s.resource().Delete(ctx, id, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return ErrNotFound
	}
	return err
}

func toUnstructured(res *SandboxResource) (*unstructured.Unstructured, error) {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(res)
	if err != nil {
		return nil, fmt.Errorf("convert sandbox to unstructured: %w", err)
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

func fromUnstructured(u *unstructured.Unstructured) (*SandboxResource, error) {
	var res SandboxResource
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &res); err != nil {
		return nil, fmt.Errorf("convert unstructured to sandbox: %w", err)
	}
	return &res, nil
}

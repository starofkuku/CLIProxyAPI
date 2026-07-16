package auth

import (
	"context"
	"testing"
)

type recycleLoadStore struct {
	items []*Auth
}

func (s *recycleLoadStore) List(context.Context) ([]*Auth, error) { return s.items, nil }
func (s *recycleLoadStore) Save(context.Context, *Auth) (string, error) {
	return "", nil
}
func (s *recycleLoadStore) Delete(context.Context, string) error { return nil }

func TestManagerLoadSkipsRecycleBinRecords(t *testing.T) {
	store := &recycleLoadStore{items: []*Auth{
		{
			ID:       "active.json",
			Provider: "codex",
			Status:   StatusActive,
			Metadata: map[string]any{"type": "codex"},
		},
		{
			ID:       ".recycle-bin/deleted.json",
			Provider: "unknown",
			Status:   StatusActive,
			Metadata: map[string]any{
				"_recycle_bin": map[string]any{"version": 1},
				"content":      map[string]any{"type": "codex"},
			},
		},
	}}
	manager := NewManager(store, nil, nil)
	if errLoad := manager.Load(context.Background()); errLoad != nil {
		t.Fatal(errLoad)
	}
	items := manager.List()
	if len(items) != 1 || items[0].ID != "active.json" {
		t.Fatalf("loaded auths = %+v", items)
	}
}

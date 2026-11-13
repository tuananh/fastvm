package store

import (
	"context"
	"testing"
	"time"

	"fastvm/internal/types"
)

func openTestStore(t *testing.T) (Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/state.db"
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestSandboxCRUDAndPersistence(t *testing.T) {
	ctx := context.Background()
	s, path := openTestStore(t)

	sb := &types.Sandbox{
		ID: "sb-1", Name: "web", BaseDisk: "sha256:abc", VCPUs: 2, MemoryMB: 512,
		State: types.StateRunning, CreatedAt: time.Unix(1700000000, 0),
	}
	if err := s.PutSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetSandbox(ctx, "sb-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Name != "web" || got.State != types.StateRunning {
		t.Fatalf("unexpected record: %+v", got)
	}

	list, err := s.ListSandboxes(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}

	// Persist across close/reopen.
	_ = s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, ok, err := s2.GetSandbox(ctx, "sb-1"); err != nil || !ok {
		t.Fatalf("after reopen: ok=%v err=%v", ok, err)
	}
	if err := s2.DeleteSandbox(ctx, "sb-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s2.GetSandbox(ctx, "sb-1"); ok {
		t.Fatal("expected sandbox to be deleted")
	}
}

func TestGetMissingAndValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	if _, ok, err := s.GetSandbox(ctx, "nope"); err != nil || ok {
		t.Fatalf("missing get: ok=%v err=%v", ok, err)
	}
	if err := s.PutSandbox(ctx, nil); err == nil {
		t.Fatal("expected error for nil sandbox")
	}
	if err := s.PutSandbox(ctx, &types.Sandbox{}); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestEventOrderingAndLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	base := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		err := s.AppendEvent(ctx, types.Event{
			Time: base.Add(time.Duration(i) * time.Second), SandboxID: "sb-1",
			Type: "state", Message: "x",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ListEvents(ctx, 10)
	if err != nil || len(all) != 5 {
		t.Fatalf("events: %v len=%d", err, len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Time.Before(all[i-1].Time) {
			t.Fatalf("events out of order: %v then %v", all[i-1].Time, all[i].Time)
		}
	}
	newest, err := s.ListEvents(ctx, 2)
	if err != nil || len(newest) != 2 {
		t.Fatalf("limited events: %v len=%d", err, len(newest))
	}
	if !newest[1].Time.Equal(base.Add(4 * time.Second)) {
		t.Fatalf("newest event time = %v", newest[1].Time)
	}
}

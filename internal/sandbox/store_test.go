package sandbox

import (
	"encoding/json"
	"errors"
	"testing"
)

func mustStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemoryStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func attrs(s string) json.RawMessage { return json.RawMessage(s) }

func TestSequence_CreateReadListReadYourWrite(t *testing.T) {
	s := mustStore(t)
	const sbx = "sbx_test1"

	created, err := s.Insert(sbx, "transaction", "tx_001", attrs(`{"amount":5000,"status":"pending"}`), nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 0 {
		t.Fatalf("new resource version = %d, want 0", created.Version)
	}

	got, err := s.GetOne(sbx, "transaction", "tx_001")
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Attributes) != `{"amount":5000,"status":"pending"}` {
		t.Fatalf("read-back mismatch: %s", got.Attributes)
	}

	for _, k := range []string{"tx_002", "tx_003", "tx_004"} {
		if _, err := s.Insert(sbx, "transaction", k, attrs(`{"amount":1}`), nil, 1001); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := s.List(sbx, "transaction", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Items) != 2 || page1.Items[0].ResourceKey != "tx_001" || page1.NextCursorKey == nil {
		t.Fatalf("page1 wrong: %+v", page1)
	}
	page2, err := s.List(sbx, "transaction", 2, page1.NextCursorKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Items) != 2 || page2.Items[0].ResourceKey != "tx_003" {
		t.Fatalf("keyset pagination broken: %+v", page2)
	}

	v1 := int64(0)
	updated, err := s.Update(sbx, "transaction", "tx_001", attrs(`{"amount":5000,"status":"success"}`), &v1, nil, false, 2000)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 1 {
		t.Fatalf("version after update = %d, want 1", updated.Version)
	}
	if _, err := s.Update(sbx, "transaction", "tx_001", attrs(`{}`), &v1, nil, false, 2001); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version must conflict, got %v", err)
	}
	if _, err := s.Update(sbx, "transaction", "tx_missing", attrs(`{}`), nil, nil, false, 2002); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing must be not-found, got %v", err)
	}
}

func TestPutIsIdempotentSeeding(t *testing.T) {
	s := mustStore(t)
	for i := 0; i < 3; i++ {
		if _, err := s.Put("sbx_a", "customer", "cus_1", attrs(`{"name":"seeded"}`), nil, int64(1000+i)); err != nil {
			t.Fatal(err)
		}
	}
	totals, err := s.Totals("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if totals.Count != 1 {
		t.Fatalf("re-seeding must not duplicate: count=%d", totals.Count)
	}
}

func TestSandboxIsolation(t *testing.T) {
	s := mustStore(t)
	s.Insert("sbx_a", "tx", "k1", attrs(`{"a":1}`), nil, 1)
	if _, err := s.GetOne("sbx_b", "tx", "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("one sandbox must never read another's state")
	}
}

func TestAllocateSeqMonotonicNeverReused(t *testing.T) {
	s := mustStore(t)

	for want := int64(1); want < 6; want++ {
		got, err := s.AllocateSeq("sbx_seq")
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("seq = %d, want %d", got, want)
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := mustStore(t)
	const sbx = "sbx_snap"
	st := "settled"
	s.Insert(sbx, "transaction", "tx_1", attrs(`{"amount":100}`), &st, 5000)
	s.Insert(sbx, "customer", "cus_1", attrs(`{"tier":"gold"}`), nil, 5001)

	snap, err := s.Serialize(sbx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 || snap[0].Type != "customer" {
		t.Fatalf("snapshot wrong: %+v", snap)
	}
	if err := s.Clear(sbx); err != nil {
		t.Fatal(err)
	}
	if totals, _ := s.Totals(sbx); totals.Count != 0 {
		t.Fatal("clear must empty the sandbox")
	}
	restored, err := s.Load(sbx, snap)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Count != 2 {
		t.Fatalf("load restored %d, want 2", restored.Count)
	}
	again, _ := s.Serialize(sbx)
	a, _ := json.Marshal(snap)
	b, _ := json.Marshal(again)
	if string(a) != string(b) {
		t.Fatalf("round-trip not identical:\n%s\n%s", a, b)
	}
}

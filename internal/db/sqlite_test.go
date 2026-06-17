package db

import (
	"testing"
)

func openMemDB(t *testing.T) *DB {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestOpen(t *testing.T) {
	store := openMemDB(t)
	if store == nil {
		t.Fatal("expected non-nil DB")
	}
}

func TestOpen_InvalidPath(t *testing.T) {
	// A directory path cannot be opened as a SQLite file.
	_, err := Open("/")
	if err == nil {
		t.Fatal("expected error opening '/' as db, got nil")
	}
}

func TestUpsert_Insert(t *testing.T) {
	store := openMemDB(t)

	r := FileRecord{Path: "/a/b.txt", Name: "b.txt", Size: 100, MD5: "abc123"}
	if err := store.Upsert(r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	total, _, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 1 {
		t.Errorf("want 1 file, got %d", total)
	}
}

func TestLookup_NotFound(t *testing.T) {
	store := openMemDB(t)
	r, err := store.Lookup("/no/such/file.txt")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if r != nil {
		t.Errorf("want nil, got %+v", r)
	}
}

func TestLookup_Found(t *testing.T) {
	store := openMemDB(t)

	orig := FileRecord{Path: "/a/b.txt", Name: "b.txt", Size: 42, MD5: "deadbeef", Mtime: 1700000000}
	if err := store.Upsert(orig); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := store.Lookup("/a/b.txt")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got == nil {
		t.Fatal("expected record, got nil")
	}
	if got.Size != orig.Size || got.Mtime != orig.Mtime || got.MD5 != orig.MD5 {
		t.Errorf("got %+v, want size=%d mtime=%d md5=%s", got, orig.Size, orig.Mtime, orig.MD5)
	}
}

func TestUpsert_Update(t *testing.T) {
	store := openMemDB(t)

	r := FileRecord{Path: "/a/b.txt", Name: "b.txt", Size: 100, MD5: "aaa"}
	if err := store.Upsert(r); err != nil {
		t.Fatalf("first Upsert: %v", err)
	}

	// Update same path with new MD5.
	r.MD5 = "bbb"
	r.Size = 200
	if err := store.Upsert(r); err != nil {
		t.Fatalf("second Upsert: %v", err)
	}

	total, _, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 1 {
		t.Errorf("upsert should keep 1 row, got %d", total)
	}

	// Confirm the updated values are stored.
	groups, err := store.Duplicates()
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("expected no duplicates after upsert, got %d groups", len(groups))
	}
}

func TestDuplicates_None(t *testing.T) {
	store := openMemDB(t)

	for _, r := range []FileRecord{
		{Path: "/a.txt", Name: "a.txt", Size: 1, MD5: "aaa"},
		{Path: "/b.txt", Name: "b.txt", Size: 2, MD5: "bbb"},
	} {
		if err := store.Upsert(r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	groups, err := store.Duplicates()
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("want 0 groups, got %d", len(groups))
	}
}

func TestDuplicates_OneGroup(t *testing.T) {
	store := openMemDB(t)

	for _, r := range []FileRecord{
		{Path: "/a.txt", Name: "a.txt", Size: 10, MD5: "same"},
		{Path: "/b.txt", Name: "b.txt", Size: 10, MD5: "same"},
		{Path: "/c.txt", Name: "c.txt", Size: 20, MD5: "unique"},
	} {
		if err := store.Upsert(r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	groups, err := store.Duplicates()
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	if groups[0].MD5 != "same" {
		t.Errorf("wrong MD5: %s", groups[0].MD5)
	}
	if len(groups[0].Files) != 2 {
		t.Errorf("want 2 files in group, got %d", len(groups[0].Files))
	}
}

func TestDuplicates_MultipleGroups(t *testing.T) {
	store := openMemDB(t)

	records := []FileRecord{
		{Path: "/a1.txt", Name: "a1.txt", Size: 5, MD5: "hash1"},
		{Path: "/a2.txt", Name: "a2.txt", Size: 5, MD5: "hash1"},
		{Path: "/b1.txt", Name: "b1.txt", Size: 8, MD5: "hash2"},
		{Path: "/b2.txt", Name: "b2.txt", Size: 8, MD5: "hash2"},
		{Path: "/b3.txt", Name: "b3.txt", Size: 8, MD5: "hash2"},
	}
	for _, r := range records {
		if err := store.Upsert(r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	groups, err := store.Duplicates()
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}

	counts := map[string]int{}
	for _, g := range groups {
		counts[g.MD5] = len(g.Files)
	}
	if counts["hash1"] != 2 {
		t.Errorf("hash1 group: want 2 files, got %d", counts["hash1"])
	}
	if counts["hash2"] != 3 {
		t.Errorf("hash2 group: want 3 files, got %d", counts["hash2"])
	}
}

func TestDeleteRecord(t *testing.T) {
	store := openMemDB(t)

	r := FileRecord{Path: "/x.txt", Name: "x.txt", Size: 1, MD5: "abc"}
	if err := store.Upsert(r); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := store.DeleteRecord("/x.txt"); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}

	total, _, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 0 {
		t.Errorf("want 0 after delete, got %d", total)
	}
}

func TestDeleteRecord_NonExistent(t *testing.T) {
	store := openMemDB(t)
	// Deleting a missing path should not return an error.
	if err := store.DeleteRecord("/no/such/file.txt"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStats_Empty(t *testing.T) {
	store := openMemDB(t)
	total, dups, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 0 || dups != 0 {
		t.Errorf("want 0,0; got %d,%d", total, dups)
	}
}

func TestStats_WithDuplicates(t *testing.T) {
	store := openMemDB(t)

	records := []FileRecord{
		{Path: "/1.txt", Name: "1.txt", Size: 1, MD5: "dup"},
		{Path: "/2.txt", Name: "2.txt", Size: 1, MD5: "dup"},
		{Path: "/3.txt", Name: "3.txt", Size: 2, MD5: "unique"},
	}
	for _, r := range records {
		if err := store.Upsert(r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}

	total, dups, err := store.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if total != 3 {
		t.Errorf("want total=3, got %d", total)
	}
	if dups != 2 {
		t.Errorf("want dups=2, got %d", dups)
	}
}

func TestAllFiles_Empty(t *testing.T) {
	store := openMemDB(t)
	records, err := store.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("want 0, got %d", len(records))
	}
}

func TestAllFiles_Multiple(t *testing.T) {
	store := openMemDB(t)
	for _, r := range []FileRecord{
		{Path: "/c.txt", Name: "c.txt", Size: 1, MD5: "aaa"},
		{Path: "/a.txt", Name: "a.txt", Size: 2, MD5: "bbb"},
		{Path: "/b.txt", Name: "b.txt", Size: 3, MD5: "ccc"},
	} {
		if err := store.Upsert(r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	records, err := store.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("want 3, got %d", len(records))
	}
	// Must be sorted by path.
	if records[0].Path != "/a.txt" || records[1].Path != "/b.txt" || records[2].Path != "/c.txt" {
		t.Errorf("unexpected order: %v", []string{records[0].Path, records[1].Path, records[2].Path})
	}
}

func TestFindByMD5_NotFound(t *testing.T) {
	store := openMemDB(t)
	records, err := store.FindByMD5("nosuchhash")
	if err != nil {
		t.Fatalf("FindByMD5: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("want 0, got %d", len(records))
	}
}

func TestFindByMD5_Found(t *testing.T) {
	store := openMemDB(t)
	md5 := "deadbeef"
	for _, r := range []FileRecord{
		{Path: "/x.txt", Name: "x.txt", Size: 5, MD5: md5},
		{Path: "/y.txt", Name: "y.txt", Size: 5, MD5: md5},
		{Path: "/z.txt", Name: "z.txt", Size: 5, MD5: "other"},
	} {
		if err := store.Upsert(r); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	records, err := store.FindByMD5(md5)
	if err != nil {
		t.Fatalf("FindByMD5: %v", err)
	}
	if len(records) != 2 {
		t.Errorf("want 2, got %d", len(records))
	}
	for _, r := range records {
		if r.MD5 != md5 {
			t.Errorf("unexpected MD5 %s", r.MD5)
		}
	}
}

// TestLookup_ClosedDB exercises the non-ErrNoRows error path in Lookup.
func TestLookup_ClosedDB(t *testing.T) {
	store := openMemDB(t)
	store.Close()
	_, err := store.Lookup("/any/path")
	if err == nil {
		t.Error("expected error from Lookup on closed connection")
	}
}

// TestDuplicates_ClosedDB exercises the query error path in Duplicates.
func TestDuplicates_ClosedDB(t *testing.T) {
	store := openMemDB(t)
	store.Close()
	_, err := store.Duplicates()
	if err == nil {
		t.Error("expected error from Duplicates on closed connection")
	}
}

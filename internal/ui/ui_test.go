package ui

import (
	"bufio"
	"strings"
	"testing"

	"onley/internal/db"
)

func br(s string) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(s))
}

// --- formatSize (unexported, same package) ---

func TestFormatSize_Bytes(t *testing.T) {
	got := formatSize(512)
	if got != "512 B" {
		t.Errorf("want '512 B', got %q", got)
	}
}

func TestFormatSize_KB(t *testing.T) {
	got := formatSize(1024)
	if got != "1.0 KB" {
		t.Errorf("want '1.0 KB', got %q", got)
	}
}

func TestFormatSize_MB(t *testing.T) {
	got := formatSize(2 * 1024 * 1024)
	if got != "2.0 MB" {
		t.Errorf("want '2.0 MB', got %q", got)
	}
}

func TestFormatSize_GB(t *testing.T) {
	got := formatSize(3 * 1024 * 1024 * 1024)
	if got != "3.0 GB" {
		t.Errorf("want '3.0 GB', got %q", got)
	}
}

// --- ShowDuplicates ---

func TestShowDuplicates_Empty(t *testing.T) {
	ShowDuplicates(nil)
	ShowDuplicates([]db.DuplicateGroup{})
}

func TestShowDuplicates_WithGroups(t *testing.T) {
	groups := []db.DuplicateGroup{
		{
			MD5:  "abc123",
			Size: 1024,
			Files: []db.FileRecord{
				{Path: "/a/foo.txt"},
				{Path: "/b/foo.txt"},
			},
		},
	}
	ShowDuplicates(groups)
}

// --- CleanInteractive ---

func makeGroups() []db.DuplicateGroup {
	return []db.DuplicateGroup{
		{
			MD5:  "hash1",
			Size: 100,
			Files: []db.FileRecord{
				{Path: "/dir/a.txt"},
				{Path: "/dir/b.txt"},
				{Path: "/dir/c.txt"},
			},
		},
		{
			MD5:  "hash2",
			Size: 200,
			Files: []db.FileRecord{
				{Path: "/dir/x.txt"},
				{Path: "/dir/y.txt"},
			},
		},
	}
}

func TestCleanInteractive_Empty(t *testing.T) {
	if result := CleanInteractive(nil, br("")); result != nil {
		t.Errorf("want nil, got %v", result)
	}
	if result := CleanInteractive([]db.DuplicateGroup{}, br("")); result != nil {
		t.Errorf("want nil, got %v", result)
	}
}

func TestCleanInteractive_SkipAll(t *testing.T) {
	result := CleanInteractive(makeGroups(), br("\n\n"))
	if len(result) != 0 {
		t.Errorf("want no deletions on skip, got %v", result)
	}
}

func TestCleanInteractive_KeepFirst(t *testing.T) {
	// Group 1: keep [1] → delete b.txt and c.txt; Group 2: skip
	result := CleanInteractive(makeGroups(), br("1\n\n"))
	want := []string{"/dir/b.txt", "/dir/c.txt"}
	if len(result) != len(want) {
		t.Fatalf("want %v, got %v", want, result)
	}
	for i, p := range want {
		if result[i] != p {
			t.Errorf("[%d] want %s, got %s", i, p, result[i])
		}
	}
}

func TestCleanInteractive_KeepMultiple(t *testing.T) {
	// Group 1: keep [1,3] → delete b.txt; Group 2: keep [2] → delete x.txt
	result := CleanInteractive(makeGroups(), br("1,3\n2\n"))
	want := []string{"/dir/b.txt", "/dir/x.txt"}
	if len(result) != len(want) {
		t.Fatalf("want %v, got %v", want, result)
	}
	for i, p := range want {
		if result[i] != p {
			t.Errorf("[%d] want %s, got %s", i, p, result[i])
		}
	}
}

func TestCleanInteractive_InvalidNumber(t *testing.T) {
	// "99" out of range → skip group 1; group 2: keep [1] → delete y.txt
	result := CleanInteractive(makeGroups(), br("99\n1\n"))
	want := []string{"/dir/y.txt"}
	if len(result) != len(want) {
		t.Fatalf("want %v, got %v", want, result)
	}
	if result[0] != want[0] {
		t.Errorf("want %s, got %s", want[0], result[0])
	}
}

func TestCleanInteractive_InvalidNonNumeric(t *testing.T) {
	result := CleanInteractive(makeGroups(), br("abc\n\n"))
	if len(result) != 0 {
		t.Errorf("want no deletions on invalid input, got %v", result)
	}
}

// --- ConfirmDelete ---

func TestConfirmDelete_EmptyList(t *testing.T) {
	if ConfirmDelete(nil, br("y\n")) {
		t.Error("empty list should return false regardless of input")
	}
}

func TestConfirmDelete_Yes(t *testing.T) {
	if !ConfirmDelete([]string{"/tmp/file.txt"}, br("y\n")) {
		t.Error("expected true for 'y' input")
	}
}

func TestConfirmDelete_YesUppercase(t *testing.T) {
	if !ConfirmDelete([]string{"/tmp/file.txt"}, br("Y\n")) {
		t.Error("expected true for 'Y' input")
	}
}

func TestConfirmDelete_No(t *testing.T) {
	if ConfirmDelete([]string{"/tmp/file.txt"}, br("n\n")) {
		t.Error("expected false for 'n' input")
	}
}

func TestConfirmDelete_Enter(t *testing.T) {
	if ConfirmDelete([]string{"/tmp/file.txt"}, br("\n")) {
		t.Error("expected false for empty (Enter) input")
	}
}

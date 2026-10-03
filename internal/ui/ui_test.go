package ui

import (
	"bufio"
	"bytes"
	"errors"
	"io"
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

// --- ShowDuplicatesW ---

func TestShowDuplicatesW_Empty(t *testing.T) {
	var out bytes.Buffer
	ShowDuplicatesW(nil, &out)
	ShowDuplicatesW([]db.DuplicateGroup{}, &out)
	if got := out.String(); !strings.Contains(got, "No duplicate files found.") {
		t.Errorf("want the empty-case message, got %q", got)
	}
}

func TestShowDuplicatesW_WithGroups(t *testing.T) {
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
	var out bytes.Buffer
	ShowDuplicatesW(groups, &out)
	got := out.String()
	for _, want := range []string{"1 duplicate group", "abc123", "/a/foo.txt", "/b/foo.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q: %s", want, got)
		}
	}
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
	result, err := CleanInteractive(nil, io.Discard, br(""))
	if err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
	if result != nil {
		t.Errorf("want nil, got %v", result)
	}
	result, err = CleanInteractive([]db.DuplicateGroup{}, io.Discard, br(""))
	if err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
	if result != nil {
		t.Errorf("want nil, got %v", result)
	}
}

func TestCleanInteractive_SkipAll(t *testing.T) {
	result, err := CleanInteractive(makeGroups(), io.Discard, br("\n\n"))
	if err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("want no deletions on skip, got %v", result)
	}
}

func TestCleanInteractive_KeepFirst(t *testing.T) {
	// Group 1: keep [1] → delete b.txt and c.txt; Group 2: skip
	result, err := CleanInteractive(makeGroups(), io.Discard, br("1\n\n"))
	if err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
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
	result, err := CleanInteractive(makeGroups(), io.Discard, br("1,3\n2\n"))
	if err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
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
	result, err := CleanInteractive(makeGroups(), io.Discard, br("99\n1\n"))
	if err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
	want := []string{"/dir/y.txt"}
	if len(result) != len(want) {
		t.Fatalf("want %v, got %v", want, result)
	}
	if result[0] != want[0] {
		t.Errorf("want %s, got %s", want[0], result[0])
	}
}

func TestCleanInteractive_InvalidNonNumeric(t *testing.T) {
	result, err := CleanInteractive(makeGroups(), io.Discard, br("abc\n\n"))
	if err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("want no deletions on invalid input, got %v", result)
	}
}

// --- ConfirmDelete ---

func TestConfirmDelete_EmptyList(t *testing.T) {
	if ok, _ := ConfirmDelete(nil, io.Discard, br("y\n")); ok {
		t.Error("empty list should return false regardless of input")
	}
}

func TestConfirmDelete_Yes(t *testing.T) {
	if ok, _ := ConfirmDelete([]string{"/tmp/file.txt"}, io.Discard, br("y\n")); !ok {
		t.Error("expected true for 'y' input")
	}
}

func TestConfirmDelete_YesUppercase(t *testing.T) {
	if ok, _ := ConfirmDelete([]string{"/tmp/file.txt"}, io.Discard, br("Y\n")); !ok {
		t.Error("expected true for 'Y' input")
	}
}

func TestConfirmDelete_No(t *testing.T) {
	if ok, _ := ConfirmDelete([]string{"/tmp/file.txt"}, io.Discard, br("n\n")); ok {
		t.Error("expected false for 'n' input")
	}
}

func TestConfirmDelete_Enter(t *testing.T) {
	if ok, _ := ConfirmDelete([]string{"/tmp/file.txt"}, io.Discard, br("\n")); ok {
		t.Error("expected false for empty (Enter) input")
	}
}

// --- output routing and exhausted input ---

// TestCleanInteractive_WritesToGivenWriter is the reason the writer is a
// parameter: the prompts used to go to os.Stdout directly, so they were lost
// whenever the caller redirected its own output.
func TestCleanInteractive_WritesToGivenWriter(t *testing.T) {
	var out bytes.Buffer
	if _, err := CleanInteractive(makeGroups(), &out, br("1\n\n")); err != nil {
		t.Fatalf("CleanInteractive: %v", err)
	}
	got := out.String()
	for _, want := range []string{"For each group", "Keep number(s)", "── Group 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q: %s", want, got)
		}
	}
}

func TestConfirmDelete_WritesToGivenWriter(t *testing.T) {
	var out bytes.Buffer
	if _, err := ConfirmDelete([]string{"/tmp/a"}, &out, br("y\n")); err != nil {
		t.Fatalf("ConfirmDelete: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "/tmp/a") {
		t.Errorf("the deletion list is missing from the output: %s", got)
	}
	if !strings.Contains(got, "Confirm deletion?") {
		t.Errorf("the confirmation prompt is missing from the output: %s", got)
	}
}

// TestConfirmDelete_ExhaustedInput covers a run with nothing on stdin, which is
// what a cron job looks like. Reporting that as "declined" would leave the
// caller believing it had made a decision.
func TestConfirmDelete_ExhaustedInput(t *testing.T) {
	var out bytes.Buffer
	ok, err := ConfirmDelete([]string{"/tmp/a"}, &out, br(""))
	if !errors.Is(err, ErrNoInput) {
		t.Errorf("err = %v, want ErrNoInput", err)
	}
	if ok {
		t.Error("an exhausted input must not confirm a deletion")
	}
}

// TestConfirmDelete_FinalLineWithoutNewlineStillCounts keeps `printf 1 |` working:
// a stream that delivers an answer and then closes has answered.
func TestConfirmDelete_FinalLineWithoutNewlineStillCounts(t *testing.T) {
	ok, err := ConfirmDelete([]string{"/tmp/a"}, io.Discard, br("y"))
	if err != nil {
		t.Errorf("err = %v, want nil: a last line without a newline is an answer", err)
	}
	if !ok {
		t.Error("want the trailing 'y' to be honoured")
	}
}

func TestCleanInteractive_ExhaustedInputMidRun(t *testing.T) {
	// One group answered, the stream ends before the second is offered.
	_, err := CleanInteractive(makeGroups(), io.Discard, br("1\n"))
	if !errors.Is(err, ErrNoInput) {
		t.Errorf("err = %v, want ErrNoInput", err)
	}
}

// TestCleanInteractive_AllGroupsAnsweredThenEOF keeps the normal end of a piped
// session from looking like a failure: every group was offered and answered.
func TestCleanInteractive_AllGroupsAnsweredThenEOF(t *testing.T) {
	if _, err := CleanInteractive(makeGroups(), io.Discard, br("1\n2\n")); err != nil {
		t.Errorf("err = %v, want nil once every group is answered", err)
	}
}

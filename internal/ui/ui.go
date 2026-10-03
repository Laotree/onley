package ui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"onley/internal/db"
)

// ErrNoInput reports that the input ended before the question was answered.
// It is deliberately distinct from answering no: an empty stream cannot decline,
// it simply has nothing to say, and a command that deletes files must not treat
// the two the same.
var ErrNoInput = errors.New("input ended before the question was answered")

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// ShowDuplicatesW writes all duplicate groups to w.
func ShowDuplicatesW(groups []db.DuplicateGroup, w io.Writer) {
	if len(groups) == 0 {
		fmt.Fprintln(w, "No duplicate files found.")
		return
	}
	fmt.Fprintf(w, "Found %d duplicate group(s):\n\n", len(groups))
	for i, g := range groups {
		fmt.Fprintf(w, "── Group %d  MD5: %s  size: %s ──\n", i+1, g.MD5, formatSize(g.Size))
		for j, f := range g.Files {
			fmt.Fprintf(w, "  [%d] %s\n", j+1, f.Path)
		}
		fmt.Fprintln(w)
	}
}

// readAnswer reads one line of input. The bool is false when the stream ended
// without an answer, which is ErrNoInput rather than a declined question.
//
// A final line without a trailing newline still counts as an answer: a here-doc
// or a pipe that writes "1" and closes has answered, it has not gone silent.
func readAnswer(r *bufio.Reader) (string, bool) {
	line, err := r.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// CleanInteractive lets the user pick which duplicates to delete for each group.
// It returns the paths the user chose to delete (not yet deleted from disk).
// r must be a shared bufio.Reader so that ConfirmDelete can read from the same
// stream.
//
// It returns ErrNoInput when the input ends before every group has been offered,
// so the caller can tell an incomplete run from a deliberate one.
func CleanInteractive(groups []db.DuplicateGroup, w io.Writer, r *bufio.Reader) ([]string, error) {
	if len(groups) == 0 {
		return nil, nil
	}

	var toDelete []string

	fmt.Fprintln(w, "For each group, enter the number(s) to KEEP (others will be deleted).")
	fmt.Fprintln(w, "Press Enter to skip a group.")
	fmt.Fprintln(w)

	for i, g := range groups {
		fmt.Fprintf(w, "── Group %d  MD5: %s  size: %s ──\n", i+1, g.MD5, formatSize(g.Size))
		for j, f := range g.Files {
			fmt.Fprintf(w, "  [%d] %s\n", j+1, f.Path)
		}
		fmt.Fprint(w, "Keep number(s) (e.g. 1 or 1,2; Enter to skip): ")

		line, ok := readAnswer(r)
		if !ok {
			return toDelete, ErrNoInput
		}
		if line == "" {
			fmt.Fprintln(w, "Skipped.")
			fmt.Fprintln(w)
			continue
		}

		keepSet := map[int]bool{}
		for _, part := range strings.Split(line, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(g.Files) {
				fmt.Fprintf(w, "  Invalid number %q, skipping group.\n", part)
				keepSet = nil
				break
			}
			keepSet[n] = true
		}
		if keepSet == nil {
			fmt.Fprintln(w)
			continue
		}

		for j, f := range g.Files {
			if !keepSet[j+1] {
				toDelete = append(toDelete, f.Path)
			}
		}
		fmt.Fprintln(w)
	}

	return toDelete, nil
}

// ConfirmDelete shows the files to be deleted and asks for confirmation.
// r must be the same shared bufio.Reader used by CleanInteractive.
//
// It returns ErrNoInput when the input ends before the question is answered, so
// that a run with nothing to read on stdin fails instead of reporting that it
// declined to delete.
func ConfirmDelete(paths []string, w io.Writer, r *bufio.Reader) (bool, error) {
	if len(paths) == 0 {
		return false, nil
	}
	fmt.Fprintf(w, "The following %d file(s) will be permanently deleted:\n", len(paths))
	for _, p := range paths {
		fmt.Fprintf(w, "  - %s\n", p)
	}
	fmt.Fprint(w, "Confirm deletion? (y/N): ")

	line, ok := readAnswer(r)
	if !ok {
		return false, ErrNoInput
	}
	return strings.EqualFold(line, "y"), nil
}

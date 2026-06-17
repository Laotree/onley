package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"onley/internal/db"
)

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

// ShowDuplicates prints all duplicate groups to os.Stdout.
func ShowDuplicates(groups []db.DuplicateGroup) {
	ShowDuplicatesW(groups, os.Stdout)
}

// CleanInteractive lets the user pick which duplicates to delete for each group.
// It returns the paths the user chose to delete (not yet deleted from disk).
// r must be a shared bufio.Reader so that ConfirmDelete can read from the same stream.
func CleanInteractive(groups []db.DuplicateGroup, r *bufio.Reader) []string {
	if len(groups) == 0 {
		return nil
	}

	var toDelete []string

	fmt.Println("For each group, enter the number(s) to KEEP (others will be deleted).")
	fmt.Println("Press Enter to skip a group.")
	fmt.Println()

	for i, g := range groups {
		fmt.Printf("── Group %d  MD5: %s  size: %s ──\n", i+1, g.MD5, formatSize(g.Size))
		for j, f := range g.Files {
			fmt.Printf("  [%d] %s\n", j+1, f.Path)
		}
		fmt.Print("Keep number(s) (e.g. 1 or 1,2; Enter to skip): ")

		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			fmt.Println("Skipped.")
			fmt.Println()
			continue
		}

		keepSet := map[int]bool{}
		for _, part := range strings.Split(line, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || n < 1 || n > len(g.Files) {
				fmt.Printf("  Invalid number %q, skipping group.\n", part)
				keepSet = nil
				break
			}
			keepSet[n] = true
		}
		if keepSet == nil {
			fmt.Println()
			continue
		}

		for j, f := range g.Files {
			if !keepSet[j+1] {
				toDelete = append(toDelete, f.Path)
			}
		}
		fmt.Println()
	}

	return toDelete
}

// ConfirmDelete shows the files to be deleted and asks for confirmation.
// r must be the same shared bufio.Reader used by CleanInteractive.
func ConfirmDelete(paths []string, r *bufio.Reader) bool {
	if len(paths) == 0 {
		return false
	}
	fmt.Printf("The following %d file(s) will be permanently deleted:\n", len(paths))
	for _, p := range paths {
		fmt.Printf("  - %s\n", p)
	}
	fmt.Print("Confirm deletion? (y/N): ")

	line, _ := r.ReadString('\n')
	return strings.TrimSpace(strings.ToLower(line)) == "y"
}

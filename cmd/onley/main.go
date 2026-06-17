package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"onley/internal/db"
	"onley/internal/replica"
	"onley/internal/scanner"
	"onley/internal/ui"
)

const defaultDBFile = "onley.db"

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("onley", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", defaultDBFile, "SQLite database path")
	workers := fs.Int("workers", max(1, runtime.NumCPU()-1), "number of concurrent workers")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: onley [options] <subcommand> [args]\n\n")
		fmt.Fprintf(stderr, "Subcommands:\n")
		fmt.Fprintf(stderr, "  scan <dir>           index files in a directory\n")
		fmt.Fprintf(stderr, "  dupes                list all duplicate files\n")
		fmt.Fprintf(stderr, "  clean                interactively remove duplicates\n")
		fmt.Fprintf(stderr, "  clean-all            keep first file per group, delete the rest (with confirmation)\n")
		fmt.Fprintf(stderr, "  stats                show index statistics\n")
		fmt.Fprintf(stderr, "  serve                start master HTTP server\n")
		fmt.Fprintf(stderr, "  replica check        compare with master and apply plan\n\n")
		fmt.Fprintf(stderr, "Options:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 1
	}

	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return 1
	}

	switch rest[0] {
	case "scan":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "error: scan requires a directory argument")
			return 1
		}
		return cmdScan(*dbPath, rest[1], *workers, stdout, stderr)
	case "dupes":
		return cmdDupes(*dbPath, stdout, stderr)
	case "clean":
		return cmdClean(*dbPath, stdin, stdout, stderr)
	case "clean-all":
		return cmdCleanAll(*dbPath, stdin, stdout, stderr)
	case "stats":
		return cmdStats(*dbPath, stdout, stderr)
	case "serve":
		return cmdServe(*dbPath, rest[1:], stdout, stderr)
	case "replica":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "error: replica requires a subcommand, e.g.: replica check")
			return 1
		}
		switch rest[1] {
		case "check":
			return cmdReplicaCheck(*dbPath, rest[2:], stdin, stdout, stderr)
		default:
			fmt.Fprintf(stderr, "unknown replica subcommand: %s\n", rest[1])
			return 1
		}
	default:
		fmt.Fprintf(stderr, "unknown subcommand: %s\n", rest[0])
		fs.Usage()
		return 1
	}
}

func openDB(path string, stderr io.Writer) *db.DB {
	store, err := db.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "failed to open database: %v\n", err)
		return nil
	}
	return store
}

func cmdScan(dbPath, dir string, workers int, stdout, stderr io.Writer) int {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "failed to resolve path: %v\n", err)
		return 1
	}
	if _, err := os.Stat(absDir); err != nil {
		fmt.Fprintf(stderr, "directory not found: %v\n", err)
		return 1
	}

	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	fmt.Fprintf(stdout, "Scanning: %s\n", absDir)

	// workerFiles[i] = path currently being hashed by worker i; "" = idle.
	workerFiles := make([]string, workers)
	var errCount, skippedCount, current int
	start := time.Now()
	drawn := false // whether we have already drawn the worker block

	redraw := func() {
		if drawn {
			// Move cursor up (workers + 1) lines to overwrite the whole block.
			fmt.Fprintf(stdout, "\033[%dA", workers+1)
		}
		for i, f := range workerFiles {
			if f == "" {
				fmt.Fprintf(stdout, "\033[2K\r  worker %2d: —\n", i)
			} else {
				fmt.Fprintf(stdout, "\033[2K\r  worker %2d: %s\n", i, truncate(f, 60))
			}
		}
		elapsed := time.Since(start).Seconds()
		var speed float64
		if elapsed > 0 {
			speed = float64(current) / elapsed
		}
		fmt.Fprintf(stdout, "\033[2K\r  processed %-6d | %.1f/s\n", current, speed)
		drawn = true
	}

	// Draw the initial (all-idle) block before the first event arrives.
	redraw()

	for p := range scanner.Scan(absDir, store, workers) {
		if p.Err != nil {
			fmt.Fprintf(stderr, "  warning: %v\n", p.Err)
			errCount++
			continue
		}
		if !p.Done {
			// Worker started hashing this file.
			workerFiles[p.WorkerID] = p.Path
		} else {
			// Worker finished — mark idle and update totals.
			workerFiles[p.WorkerID] = ""
			current = p.Current
			if p.Skipped {
				skippedCount++
			}
		}
		redraw()
	}

	total, dups, err := store.Stats()
	if err == nil {
		fmt.Fprintf(stdout, "Done: %d file(s) indexed (%d unchanged skipped), %d with duplicates.\n", total, skippedCount, dups)
	}
	if errCount > 0 {
		fmt.Fprintf(stdout, "  (%d file(s) skipped due to errors)\n", errCount)
	}
	return 0
}

func cmdDupes(dbPath string, stdout, stderr io.Writer) int {
	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	groups, err := store.Duplicates()
	if err != nil {
		fmt.Fprintf(stderr, "query failed: %v\n", err)
		return 1
	}
	ui.ShowDuplicatesW(groups, stdout)
	return 0
}

func cmdClean(dbPath string, stdin io.Reader, stdout, stderr io.Writer) int {
	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	groups, err := store.Duplicates()
	if err != nil {
		fmt.Fprintf(stderr, "query failed: %v\n", err)
		return 1
	}
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "No duplicate files found.")
		return 0
	}

	ui.ShowDuplicatesW(groups, stdout)
	br := bufio.NewReader(stdin)
	toDelete := ui.CleanInteractive(groups, br)
	if len(toDelete) == 0 {
		fmt.Fprintln(stdout, "No files selected, exiting.")
		return 0
	}

	if !ui.ConfirmDelete(toDelete, br) {
		fmt.Fprintln(stdout, "Cancelled.")
		return 0
	}

	var deleted, failed int
	for _, path := range toDelete {
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(stderr, "  delete failed %s: %v\n", path, err)
			failed++
			continue
		}
		if err := store.DeleteRecord(path); err != nil {
			fmt.Fprintf(stderr, "  index cleanup failed %s: %v\n", path, err)
		}
		deleted++
	}
	fmt.Fprintf(stdout, "Done: %d deleted, %d failed.\n", deleted, failed)
	return 0
}

func cmdCleanAll(dbPath string, stdin io.Reader, stdout, stderr io.Writer) int {
	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	groups, err := store.Duplicates()
	if err != nil {
		fmt.Fprintf(stderr, "query failed: %v\n", err)
		return 1
	}
	if len(groups) == 0 {
		fmt.Fprintln(stdout, "No duplicate files found.")
		return 0
	}

	// Collect files to delete: all except the first in each group.
	var toDelete []string
	for _, g := range groups {
		for _, f := range g.Files[1:] {
			toDelete = append(toDelete, f.Path)
		}
	}

	// Preview what will happen.
	fmt.Fprintf(stdout, "%d duplicate group(s): keeping the first file in each, deleting %d other(s):\n\n", len(groups), len(toDelete))
	for _, g := range groups {
		fmt.Fprintf(stdout, "  keep:   %s\n", g.Files[0].Path)
		for _, f := range g.Files[1:] {
			fmt.Fprintf(stdout, "  delete: %s\n", f.Path)
		}
		fmt.Fprintln(stdout)
	}

	if !ui.ConfirmDelete(toDelete, bufio.NewReader(stdin)) {
		fmt.Fprintln(stdout, "Cancelled.")
		return 0
	}

	var deleted, failed int
	for _, path := range toDelete {
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(stderr, "  delete failed %s: %v\n", path, err)
			failed++
			continue
		}
		if err := store.DeleteRecord(path); err != nil {
			fmt.Fprintf(stderr, "  index cleanup failed %s: %v\n", path, err)
		}
		deleted++
	}
	fmt.Fprintf(stdout, "Done: %d deleted, %d failed.\n", deleted, failed)
	return 0
}

func cmdStats(dbPath string, stdout, stderr io.Writer) int {
	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	total, dups, err := store.Stats()
	if err != nil {
		fmt.Fprintf(stderr, "stats query failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Total indexed: %d\n", total)
	fmt.Fprintf(stdout, "Duplicates:    %d\n", dups)
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n+3:]
}

func cmdServe(dbPath string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 8080, "HTTP listen port")
	storeDir := fs.String("store", "onley-store", "directory for storing migrated files")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if err := os.MkdirAll(*storeDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "failed to create store directory: %v\n", err)
		return 1
	}

	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	srv := replica.NewServer(store, *storeDir)
	addr := fmt.Sprintf(":%d", *port)
	fmt.Fprintf(stdout, "onley master listening on %s, store: %s\n", addr, *storeDir)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		fmt.Fprintf(stderr, "server error: %v\n", err)
		return 1
	}
	return 0
}

func cmdReplicaCheck(dbPath string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replica check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	masterURL := fs.String("master", "", "master address (e.g. http://master-host:8080)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *masterURL == "" {
		fmt.Fprintln(stderr, "error: -master flag is required")
		return 1
	}

	client := replica.NewClient(*masterURL)
	if err := client.Ping(); err != nil {
		fmt.Fprintf(stderr, "cannot reach master %s: %v\n", *masterURL, err)
		return 1
	}

	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	files, err := store.AllFiles()
	if err != nil {
		fmt.Fprintf(stderr, "failed to read local index: %v\n", err)
		return 1
	}
	if len(files) == 0 {
		fmt.Fprintln(stdout, "Local index is empty; run scan first.")
		return 0
	}

	fmt.Fprintf(stdout, "Comparing %d file(s) with master...\n", len(files))

	var toDelete, toMigrate []replica.PlanEntry
	var queryFailed int

	for i, f := range files {
		found, err := client.Check(f.MD5)
		if err != nil {
			fmt.Fprintf(stderr, "  query failed %s: %v\n", f.Path, err)
			queryFailed++
			continue
		}
		entry := replica.PlanEntry{Path: f.Path, MD5: f.MD5, Size: f.Size}
		if found {
			entry.Action = replica.ActionDeleteLocal
			toDelete = append(toDelete, entry)
		} else {
			entry.Action = replica.ActionMigrate
			toMigrate = append(toMigrate, entry)
		}
		fmt.Fprintf(stdout, "\r  progress: %d/%d", i+1, len(files))
	}
	fmt.Fprintln(stdout)

	if queryFailed > 0 {
		fmt.Fprintf(stdout, "  (%d file(s) skipped due to query errors)\n", queryFailed)
	}

	if len(toDelete) == 0 && len(toMigrate) == 0 {
		fmt.Fprintln(stdout, "Nothing to do.")
		return 0
	}

	if len(toDelete) > 0 {
		fmt.Fprintf(stdout, "\nDelete locally (already on master, %d file(s)):\n", len(toDelete))
		for _, e := range toDelete {
			fmt.Fprintf(stdout, "  %s\n", e.Path)
		}
	}
	if len(toMigrate) > 0 {
		fmt.Fprintf(stdout, "\nMigrate to master (not on master, %d file(s)):\n", len(toMigrate))
		for _, e := range toMigrate {
			fmt.Fprintf(stdout, "  %s\n", e.Path)
		}
	}

	fmt.Fprintf(stdout, "\nProceed with the above? [y/N] ")
	br := bufio.NewReader(stdin)
	line, _ := br.ReadString('\n')
	if strings.TrimSpace(strings.ToLower(line)) != "y" {
		fmt.Fprintln(stdout, "Cancelled.")
		return 0
	}

	var deleted, migrated, failed int

	for _, e := range toDelete {
		if err := os.Remove(e.Path); err != nil {
			fmt.Fprintf(stderr, "  delete failed %s: %v\n", e.Path, err)
			failed++
			continue
		}
		store.DeleteRecord(e.Path)
		deleted++
	}

	for _, e := range toMigrate {
		if err := client.Ingest(e.Path, e.MD5); err != nil {
			fmt.Fprintf(stderr, "  migrate failed %s: %v\n", e.Path, err)
			failed++
			continue
		}
		if err := os.Remove(e.Path); err != nil {
			fmt.Fprintf(stderr, "  delete local after migrate failed %s: %v\n", e.Path, err)
		}
		store.DeleteRecord(e.Path)
		migrated++
	}

	fmt.Fprintf(stdout, "\nDone: %d deleted, %d migrated, %d failed.\n", deleted, migrated, failed)
	return 0
}

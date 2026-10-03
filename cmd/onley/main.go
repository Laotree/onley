package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"onley/internal/config"
	"onley/internal/db"
	"onley/internal/replica"
	"onley/internal/scanner"
	"onley/internal/ui"
	"onley/internal/watcher"
)

const (
	// dbDirName and dbFileName make up the default index location under the
	// home directory.
	dbDirName  = ".onley"
	dbFileName = "local.db"
	// legacyDBFile is the working-directory-relative name the index used to
	// default to.
	legacyDBFile = "onley.db"
	// configFileName is the settings file inside dbDirName.
	configFileName = "config.json"
	// masterEnvVar overrides the configured master without touching the file,
	// which is what a cron job pointing at a different master needs.
	masterEnvVar = "ONLEY_MASTER"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("onley", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// The default stays empty so an explicit -db is distinguishable from an
	// omitted one, and so PrintDefaults does not print a stale path.
	dbPath := fs.String("db", "", "SQLite database path (default: ~/"+dbDirName+"/"+dbFileName+")")
	workers := fs.Int("workers", max(1, runtime.NumCPU()-1), "number of concurrent workers")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: onley [options] <subcommand> [args]\n\n")
		fmt.Fprintf(stderr, "Subcommands:\n")
		fmt.Fprintf(stderr, "  scan <dir>           index files in a directory\n")
		fmt.Fprintf(stderr, "  watch <dir>          keep the index up to date as files change\n")
		fmt.Fprintf(stderr, "  dupes                list all duplicate files\n")
		fmt.Fprintf(stderr, "  clean                interactively remove duplicates\n")
		fmt.Fprintf(stderr, "  clean-all            keep first file per group, delete the rest (with confirmation)\n")
		fmt.Fprintf(stderr, "  stats                show index statistics\n")
		fmt.Fprintf(stderr, "  serve                start master HTTP server\n")
		fmt.Fprintf(stderr, "  replica check        compare with master and apply plan\n")
		fmt.Fprintf(stderr, "  config <sub>         store or display settings (config set master <url>)\n\n")
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

	// Resolved after the subcommand is known, so printing usage or rejecting a
	// bad flag does not create a home directory as a side effect.
	if *dbPath == "" {
		resolved, err := defaultDBPath(stderr)
		if err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		*dbPath = resolved
	}

	switch rest[0] {
	case "scan":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "error: scan requires a directory argument")
			return 1
		}
		if err := rejectExtraDirs(rest[2:]); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return cmdScan(*dbPath, rest[1], *workers, stdout, stderr)
	case "watch":
		if len(rest) < 2 {
			fmt.Fprintln(stderr, "error: watch requires a directory argument")
			return 1
		}
		return cmdWatch(*dbPath, rest[1], rest[2:], stdout, stderr)
	case "dupes":
		if err := rejectExtraDirs(rest[1:]); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return cmdDupes(*dbPath, stdout, stderr)
	case "clean":
		return cmdClean(*dbPath, stdin, stdout, stderr)
	case "clean-all":
		return cmdCleanAll(*dbPath, stdin, stdout, stderr)
	case "stats":
		return cmdStats(*dbPath, stdout, stderr)
	case "config":
		if len(rest) < 2 {
			fmt.Fprintf(stderr, "error: config requires a subcommand, e.g.: config set master <url>\n")
			return 1
		}
		return cmdConfig(rest[1], rest[2:], stdout, stderr)
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

// rejectExtraArgs refuses arguments a command cannot act on.
func rejectExtraArgs(extra []string) error {
	if len(extra) == 0 {
		return nil
	}
	quoted := make([]string, len(extra))
	for i, a := range extra {
		quoted[i] = strconv.Quote(a)
	}
	if len(extra) == 1 {
		return fmt.Errorf("unexpected argument %s", quoted[0])
	}
	return fmt.Errorf("unexpected arguments %s", strings.Join(quoted, ", "))
}

// rejectExtraDirs refuses directory arguments a command cannot act on.
//
// scan and dupes took the first extra argument and dropped the rest without a
// word, so `onley scan ~/Downloads ~/Documents` reported success having indexed
// one directory. Naming the ignored arguments is the whole fix: silently doing
// part of what was asked is worse than refusing.
func rejectExtraDirs(extra []string) error {
	if len(extra) == 0 {
		return nil
	}
	quoted := make([]string, len(extra))
	for i, a := range extra {
		quoted[i] = strconv.Quote(a)
	}
	if len(extra) == 1 {
		return fmt.Errorf("unexpected argument %s; this command takes a single directory", quoted[0])
	}
	return fmt.Errorf("unexpected arguments %s; this command takes a single directory",
		strings.Join(quoted, ", "))
}

// isTerminal reports whether w is a terminal someone is watching.
//
// A writer that is not an *os.File, which is every test and anything wrapping a
// buffer, counts as not interactive. That is the safe direction: skipping the
// animation costs a progress display, emitting cursor-movement into a buffer or
// a log corrupts it.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// reportNoInput turns an exhausted input stream into a failure.
//
// It is separated from answering no on purpose. An empty stream has not declined
// anything, and a command that deletes files must not report a decision nobody
// made. A cron job with nothing on stdin used to get "Cancelled." and exit 0,
// which reads as a successful run that deliberately left the files alone.
func reportNoInput(err error, stderr io.Writer) int {
	if errors.Is(err, ui.ErrNoInput) {
		fmt.Fprintf(stderr, "error: %v; nothing was deleted\n", err)
		fmt.Fprintln(stderr, "hint: run this from a terminal, or feed the answer on stdin, e.g. echo y |")
		return 1
	}
	return -1 // not this error; the caller handles it as it sees fit
}

func openDB(path string, stderr io.Writer) *db.DB {
	store, err := db.Open(path)
	if err != nil {
		fmt.Fprintf(stderr, "failed to open database: %v\n", err)
		return nil
	}
	return store
}

// defaultDBPath returns the index location used when -db is not given, and
// creates its directory.
//
// The index used to default to "onley.db" in the working directory, which meant
// every directory had its own index: a scan in one place left `onley stats` in
// another reporting an empty tree. The home directory gives one index per user
// instead.
//
// Failing to locate the home directory is reported rather than worked around.
// Falling back to a working-directory path would reintroduce exactly the
// behaviour this replaces.
func defaultDBPath(stderr io.Writer) (string, error) {
	dir, err := onleyDir()
	if err != nil {
		return "", fmt.Errorf("%w, pass -db to choose an index", err)
	}
	target := filepath.Join(dir, dbFileName)
	adoptLegacyDB(target, stderr)
	return target, nil
}

// onleyDir returns ~/.onley, creating it if it does not exist. The index and
// the settings share it, so both follow the user instead of the working
// directory.
//
// It is 0700 because the index lists the paths and digests of the user's files.
// The directory is what keeps that private; the modes on the files inside it do
// not have to be restrictive as well.
func onleyDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the home directory: %w", err)
	}
	dir := filepath.Join(home, dbDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cannot create %s: %w", dir, err)
	}
	return dir, nil
}

// defaultConfigPath returns ~/.onley/config.json, creating the directory if
// needed. It is resolved lazily by the commands that read settings, so that a
// command with nothing to configure does not touch the home directory.
func defaultConfigPath() (string, error) {
	dir, err := onleyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, configFileName), nil
}

// adoptLegacyDB copies a working-directory onley.db to the new default location
// the first time onley runs without -db. It runs once, because the default
// index existing is what marks the migration as done.
//
// The old file is left in place: a user who wants it can keep passing
// -db ./onley.db, and a copy is recoverable if the new location turns out to be
// wrong.
func adoptLegacyDB(target string, stderr io.Writer) {
	if _, err := os.Stat(target); err == nil {
		return
	}
	if _, err := os.Stat(legacyDBFile); err != nil {
		return
	}
	if err := copyFile(legacyDBFile, target); err != nil {
		fmt.Fprintf(stderr, "warning: could not copy %s to %s: %v\n", legacyDBFile, target, err)
		return
	}
	fmt.Fprintf(stderr, "Copied %s to %s. Pass -db %s to keep using the old index.\n",
		legacyDBFile, target, legacyDBFile)
}

// copyFile copies src to dst without loading either into memory, because an
// index can outgrow a buffer. It refuses to overwrite, so a concurrent onley
// that created dst first keeps its index.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil // another process got there first
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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

	// The worker block redraws itself with cursor-movement escapes. Those are
	// only readable on a terminal; redirected to a file or a pipe they turn the
	// log into a wall of escape codes, so the block is skipped and only the
	// final summary is written.
	interactive := isTerminal(stdout)

	// workerFiles[i] = path currently being hashed by worker i; "" = idle.
	workerFiles := make([]string, workers)
	var errCount, skippedCount, current int
	start := time.Now()
	drawn := false // whether we have already drawn the worker block

	redraw := func() {
		if !interactive {
			return
		}
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

func cmdWatch(dbPath, dir string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	debounce := fs.Duration("debounce", watcher.DefaultDebounce, "how long a file must stop changing before it is indexed")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(stderr, "failed to resolve path: %v\n", err)
		return 1
	}
	info, err := os.Stat(absDir)
	if err != nil {
		fmt.Fprintf(stderr, "directory not found: %v\n", err)
		return 1
	}
	if !info.IsDir() {
		fmt.Fprintf(stderr, "not a directory: %s\n", absDir)
		return 1
	}

	w, err := watcher.New(absDir, *debounce)
	if err != nil {
		fmt.Fprintf(stderr, "failed to watch %s: %v\n", absDir, err)
		return 1
	}
	defer w.Close()

	store := openDB(dbPath, stderr)
	if store == nil {
		return 1
	}
	defer store.Close()

	// The watcher stops on the first interrupt so the deferred Close and store
	// Close run and the index is left consistent.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go w.Run(ctx)

	fmt.Fprintf(stdout, "Watching %s (debounce %s). Press Ctrl-C to stop.\n", absDir, *debounce)

	var indexed, removed, failed int
	for ev := range w.Events() {
		if ev.Err != nil {
			fmt.Fprintf(stderr, "  warning: %v\n", ev.Err)
			failed++
			continue
		}
		if ev.Remove {
			// Only the index record goes. The file is already gone, or the
			// rename moved it, and deleting anything here could destroy a file
			// the user still has.
			if err := store.DeleteRecord(ev.Path); err != nil {
				fmt.Fprintf(stderr, "  index cleanup failed %s: %v\n", ev.Path, err)
				failed++
				continue
			}
			removed++
			fmt.Fprintf(stdout, "  removed  %s\n", ev.Path)
			continue
		}
		if err := scanner.IndexFile(ev.Path, store); err != nil {
			fmt.Fprintf(stderr, "  index failed %s: %v\n", ev.Path, err)
			failed++
			continue
		}
		indexed++
		fmt.Fprintf(stdout, "  indexed  %s\n", ev.Path)
	}

	// Report what the run did, so a long watch that is stopped from another
	// terminal still leaves a record of its work.
	total, dups, err := store.Stats()
	if err != nil {
		fmt.Fprintf(stderr, "  stats failed on shutdown: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "\nStopped: %d indexed, %d removed, %d failed. Index holds %d file(s), %d with duplicates.\n",
		indexed, removed, failed, total, dups)
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
	toDelete, err := ui.CleanInteractive(groups, stdout, br)
	if code := reportNoInput(err, stderr); code >= 0 {
		return code
	}
	if len(toDelete) == 0 {
		fmt.Fprintln(stdout, "No files selected, exiting.")
		return 0
	}

	confirmed, err := ui.ConfirmDelete(toDelete, stdout, br)
	if code := reportNoInput(err, stderr); code >= 0 {
		return code
	}
	if !confirmed {
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

	confirmed, err := ui.ConfirmDelete(toDelete, stdout, bufio.NewReader(stdin))
	if code := reportNoInput(err, stderr); code >= 0 {
		return code
	}
	if !confirmed {
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

// cmdConfig reads and writes the per-user settings.
func cmdConfig(action string, args []string, stdout, stderr io.Writer) int {
	path, err := defaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}

	switch action {
	case "show":
		return configShow(path, stdout, stderr)
	case "set":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "error: config set requires a key and a value, e.g.: config set master http://host:8080")
			return 1
		}
		if err := rejectExtraArgs(args[2:]); err != nil {
			fmt.Fprintf(stderr, "error: %v\n", err)
			return 1
		}
		return configSet(path, args[0], args[1], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown config subcommand: %s\n", action)
		return 1
	}
}

// configShow prints the settings that are in the file.
func configShow(path string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", path)
	if cfg.Master == "" {
		fmt.Fprintln(stdout, "  master: (not set)")
	} else {
		fmt.Fprintf(stdout, "  master: %s\n", cfg.Master)
	}
	if v := os.Getenv(masterEnvVar); v != "" {
		fmt.Fprintf(stdout, "  %s overrides master: %s\n", masterEnvVar, v)
	}
	return 0
}

// configSet writes one setting. Rejecting an unknown key here keeps a typo from
// being stored in a file that parses cleanly and is then silently ignored.
func configSet(path, key, value string, stdout, stderr io.Writer) int {
	if key != "master" {
		fmt.Fprintf(stderr, "error: unknown setting %q; only \"master\" can be set\n", key)
		return 1
	}
	if err := validateMasterURL(value); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	cfg.Master = value
	if err := cfg.Save(path); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "master = %s\n", value)
	fmt.Fprintf(stdout, "Written to %s\n", path)
	return 0
}

// validateMasterURL catches the mistakes that are worth catching before a URL is
// stored: a value with no scheme cannot be reached, and one with no host names
// nothing. The check is local, so setting the value stays fast and offline.
func validateMasterURL(raw string) error {
	if raw == "" {
		return errors.New("master URL must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("master URL %q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("master URL %q needs an http:// or https:// scheme", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("master URL %q has no host", raw)
	}
	return nil
}

// resolveMaster picks the master URL from the flag, the environment and the
// settings file, in that order, and says which one it used.
//
// The flag keeps its empty default, so an omitted flag and an explicitly empty
// one are the same thing: neither is a value, and both fall through. Printing
// the source matters because a stale URL in the file is otherwise invisible, and
// the failure would look like the master being unreachable.
func resolveMaster(flagValue string, cfg config.Config) (url, source string) {
	if flagValue != "" {
		return flagValue, "flag"
	}
	if v := os.Getenv(masterEnvVar); v != "" {
		return v, masterEnvVar
	}
	if cfg.Master != "" {
		return cfg.Master, "config"
	}
	return "", ""
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

	cfgPath, err := defaultConfigPath()
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	master, source := resolveMaster(*masterURL, cfg)
	if master == "" {
		fmt.Fprintln(stderr, "error: no master is configured")
		fmt.Fprintf(stderr, "hint: pass -master <url>, set %s, or run: onley config set master <url>\n", masterEnvVar)
		return 1
	}
	fmt.Fprintf(stdout, "Master: %s (from %s)\n", master, source)

	client := replica.NewClient(master)
	if err := client.Ping(); err != nil {
		fmt.Fprintf(stderr, "cannot reach master %s: %v\n", master, err)
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
	line, readErr := br.ReadString('\n')
	// The plan above ends in deletions, so an input that never arrives has to be
	// a failure rather than a quiet "no".
	if readErr != nil && strings.TrimSpace(line) == "" {
		if code := reportNoInput(ui.ErrNoInput, stderr); code >= 0 {
			return code
		}
	}
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

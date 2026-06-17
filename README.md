# onley

A local file deduplication tool. Indexes files by MD5, finds duplicates, and lets you clean them up interactively — on a single machine or across multiple machines.

## Features

- MD5-based duplicate detection across a directory tree
- SQLite index stored locally
- Resume support: unchanged files (same size + mtime) are skipped on re-scan
- Concurrent scanning via a configurable goroutine worker pool
- Interactive cleanup (`clean`) or automatic cleanup (`clean-all`)
- Multi-machine mode: a replica compares its local index against a master and either deletes local copies or migrates unique files to the master over HTTP

## Install

```sh
go install onley/cmd/onley@latest
```

Or build from source:

```sh
git clone <repo>
cd onley
make build      # produces ./onley
```

## Quick start

```sh
# Index a directory
onley scan ~/Downloads

# See what's duplicated
onley dupes

# Interactively pick which files to keep
onley clean

# Or automatically keep the first file per group and delete the rest
onley clean-all

# Show index statistics
onley stats
```

## Commands

| Command | Description |
|---|---|
| `scan <dir>` | Walk `dir` and index every file by MD5 |
| `dupes` | List all duplicate groups |
| `clean` | Interactive: choose which files to keep per group |
| `clean-all` | Non-interactive: keep the first file per group, delete the rest |
| `stats` | Print total files and duplicate count |
| `serve` | Start master HTTP server (for replica mode) |
| `replica check` | Compare local index against a master, apply plan |

## Global flags

| Flag | Default | Description |
|---|---|---|
| `-db <path>` | `onley.db` | SQLite database file |
| `-workers <n>` | `max(1, CPUs-1)` | Concurrent hash workers |

## Scan

```sh
onley scan /Volumes/data
onley -workers 4 scan /Volumes/data
```

Progress is shown per worker. A second scan over the same directory skips files whose size and modification time are unchanged, so interrupted scans resume cheaply.

## Clean

`clean` shows each duplicate group and asks which file(s) to keep. Enter one number, a comma-separated list, or press Enter to skip the group.

```
── Group 1  MD5: d8e8fca2dc0f896fd7cb4cb0031ba249  size: 4.0 KB ──
  [1] /Users/alice/docs/report.pdf
  [2] /Users/alice/backup/report.pdf
Keep number(s) (e.g. 1 or 1,2; Enter to skip): 1
```

`clean-all` is non-interactive: it keeps the alphabetically first path in each group and deletes the rest after a single confirmation.

## Replica mode

Replica mode lets multiple machines consolidate unique files onto one master.

**On the master machine**, start the HTTP server:

```sh
onley -db /data/master.db serve -port 8080 -store /data/files
```

| Flag | Default | Description |
|---|---|---|
| `-port <n>` | `8080` | Listen port |
| `-store <dir>` | `onley-store` | Directory for incoming files |

**On each replica**, scan local files then run a check:

```sh
onley -db local.db scan /my/files
onley -db local.db replica check -master http://master-host:8080
```

`replica check` queries the master for every file in the local index and builds a plan:

- **Delete locally** — master already has this MD5; the local copy is redundant.
- **Migrate to master** — master does not have this file; it will be uploaded and then removed locally.

The plan is shown before any changes are made. Confirm with `y` to execute, or press Enter / type `n` to cancel.

```
Comparing 1 234 file(s) with master...

Delete locally (already on master, 892 file(s)):
  /my/files/photo_001.jpg
  ...

Migrate to master (not on master, 342 file(s)):
  /my/files/project_final_v3.zip
  ...

Proceed with the above? [y/N]
```

Migrated files are uploaded via HTTP multipart and stored on the master in a content-addressed layout (`<store>/<md5[0:2]>/<md5[2:]>/<filename>`). Successful uploads are removed from the replica and from its local index.

## Master HTTP API

The master exposes a small JSON API used internally by `replica check`. It can also be called directly.

| Method | Path | Description |
|---|---|---|
| `GET` | `/v1/health` | Returns `{"ok":true}` |
| `GET` | `/v1/check?md5=<hash>` | Returns `{"found":bool,"paths":[...]}` |
| `POST` | `/v1/ingest` | Multipart upload: fields `file`, `md5` |

## Development

```sh
make test       # run all tests
make coverage   # test with coverage report → coverage.html
make lint       # golangci-lint
make clean      # remove binary and coverage files
```

Requirements: Go 1.22+

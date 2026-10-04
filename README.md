# onley

<p align="center">
  <img src="logo.png" alt="onley logo" width="200">
</p>

A local file deduplication tool. Indexes files by MD5, finds duplicates, and lets you clean them up interactively — on a single machine or across multiple machines.

## Features

- MD5-based duplicate detection across a directory tree
- SQLite index stored locally
- Resume support: unchanged files (same size + mtime) are skipped on re-scan
- Concurrent scanning via a configurable goroutine worker pool
- Continuous indexing (`watch`): the index follows files as they are created, changed and deleted
- Interactive cleanup (`clean`) or automatic cleanup (`clean-all`)
- Multi-machine mode: a replica compares its local index against a master and either deletes local copies or migrates unique files to the master over HTTP

## Install

Download a release archive for your platform:

```sh
# macOS (Apple silicon)
tar xzf onley_<version>_darwin_arm64.tar.gz

# Linux (x86-64)
tar xzf onley_<version>_linux_amd64.tar.gz
```

Archives are published for darwin and linux on amd64 and arm64, plus windows on
amd64, each with a `checksums.txt` alongside. See
[releases](https://github.com/Laotree/onley/releases).

`go install` does not work and is not offered: the module path is `onley`, which
is not a resolvable repository location. Fixing that means changing the import
path of every package, so it has not been done.

To build from source:

```sh
git clone https://github.com/Laotree/onley
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

# Or keep the index up to date while files change
onley watch ~/Downloads
```

## Commands

| Command | Description |
|---|---|
| `scan <dir>` | Walk `dir` and index every file by MD5 |
| `watch <dir>` | Keep the index current as files under `dir` change |
| `dupes` | List all duplicate groups |
| `clean` | Interactive: choose which files to keep per group |
| `clean-all` | Keep the first file per group and delete the rest, after one confirmation |
| `stats` | Print total files and duplicate count |
| `serve` | Start master HTTP server (for replica mode) |
| `replica check` | Compare local index against a master, apply plan |
| `config set` / `config show` | Store and display per-user settings |

## Global flags

| Flag | Default | Description |
|---|---|---|
| `-db <path>` | `~/.onley/local.db` | SQLite database file |
| `-workers <n>` | `max(1, CPUs-1)` | Concurrent hash workers |
| `-y`, `--yes` | off | Assume yes for confirmations |

Options go before the subcommand: `onley -y clean-all`, not `onley clean-all -y`.

`-y` is permission to carry out a plan, not permission to destroy. In replica
mode it authorises deleting a local copy only when this run has already uploaded
that content to the master. Files the master already had are left alone with a
warning, and the run exits non-zero because there is work waiting for a person.
See [Replica mode](#replica-mode).

The default index lives in the home directory so that every directory sees the
same one: `onley scan ~/Downloads` followed by `onley stats` from somewhere else
reports what was scanned. The directory is created if missing.

If an `onley.db` sits in the working directory and the default index does not
exist yet, it is copied there once and onley says so. The old file is left where
it is; pass `-db ./onley.db` to keep using it.

An explicit `-db` path is used exactly as given, and a relative one is relative
to the working directory. onley does not create missing parent directories for
it, so a typo in the path is reported rather than quietly making one.

## Scan

```sh
onley scan /Volumes/data
onley -workers 4 scan /Volumes/data
```

Progress is shown per worker. A second scan over the same directory skips files whose size and modification time are unchanged, so interrupted scans resume cheaply.

## Watch

`watch` keeps the index current without being run again:

```sh
onley watch ~/Downloads
onley watch -debounce 2s /Volumes/data
```

| Flag | Default | Description |
|---|---|---|
| `-debounce <dur>` | `500ms` | How long a file must stop changing before it is indexed |
| `-master <url>` | none | Push new files to this master; without it, only the index is maintained |
| `-interval <dur>` | `30s` | How often to push queued files |

It covers the whole directory tree, the same way `scan` does, and it indexes the files that are already there on startup. New subdirectories are picked up as they appear.

`watch` only maintains the index. When a file is deleted or renamed away, its index entry is dropped, but nothing is deleted from disk — cleaning up is still `clean` and `clean-all`.

A file is indexed once it stops changing for `-debounce`, and its size and modification time are checked again when the window closes, so a file that grows while the watcher waits is not hashed mid-write. Raise `-debounce` if your files are written in bursts with long pauses: a writer that pauses for longer than the window looks like a writer that finished.

Other commands can run against the same index while `watch` is running. Stop it with Ctrl-C.

### Pushing to a master

Given a `-master`, `watch` also uploads what it indexes:

```sh
onley watch -master http://master-host:8080 ~/Downloads
```

**It uploads and never deletes.** Every new file takes the migrate path in
`replica check`, which uploads and then removes the local copy — so a syncing
watcher would empty the directory it was told to watch. The local copies stay
here, and `replica check` remains the place where a deletion is proposed and
confirmed.

Be clear about what that costs: the master holds a copy **and** the local copy
stays, so disk use goes up rather than down. This is a push, not the
consolidation that gives `replica check` its purpose. If you want the space back,
run `replica check` deliberately.

How it behaves:

- The queue is whatever has been indexed since the last round. On startup that is
  the whole existing tree, because the startup sweep reports it — so the first
  round is the reconcile that catches what a previous run missed.
- Files sharing content are one question and one upload, not one each.
- A file the master already holds is not uploaded again, and not deleted.
- A round that fails doubles the wait before the next, up to sixteen times the
  interval, and a successful round puts it back. A master that is down for a week
  is not probed every 30 seconds.
- A master that cannot be reached does not stop the indexing.

Uploads run on their own goroutine. They have to: the watcher's event channel is
bounded and drops events when it fills, so an upload running in the indexing loop
would cost index entries silently rather than merely slow down.

## Clean

`clean` shows each duplicate group and asks which file(s) to keep. Enter one number, a comma-separated list, or press Enter to skip the group.

```
── Group 1  MD5: d8e8fca2dc0f896fd7cb4cb0031ba249  size: 4.0 KB ──
  [1] /Users/alice/docs/report.pdf
  [2] /Users/alice/backup/report.pdf
Keep number(s) (e.g. 1 or 1,2; Enter to skip): 1
```

`clean-all` keeps the alphabetically first path in each group and deletes the rest after a single confirmation.

`-y` skips that confirmation, which is what makes `clean-all` runnable from cron with nothing on stdin:

```sh
onley -y clean-all
```

`-y` does not stand in for the per-group question in `clean`. Deciding which copy to keep is what that command is for, and answering it on your behalf would pick a different deletion target than the one you wanted. So `onley -y clean` still reads stdin for the choices and only skips the final yes/no.

Without `-y`, an exhausted stdin fails rather than quietly keeping every file: an empty stream has not declined anything. Answering `n` is a decision, and succeeds.

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

**On each replica**, point onley at the master once, then scan and check:

```sh
onley config set master http://master-host:8080
onley scan /my/files
onley replica check
```

`replica check` needs to know which master to talk to, and that is fixed per
machine, so it is remembered in `~/.onley/config.json` next to the index. It is
still overridable per run:

| Source | Example | Use for |
|---|---|---|
| `-master` | `onley replica check -master http://other:8080` | a one-off run against a different master |
| `ONLEY_MASTER` | `ONLEY_MASTER=http://other:8080 onley replica check` | a cron job pointing elsewhere, without editing the file |
| `~/.onley/config.json` | `onley config set master http://host:8080` | the normal case |

The first one that is set wins, so `-master ""` is the same as leaving it out.
`replica check` prints the master it resolved and which of the three it came
from, because a stale URL in the file is otherwise invisible and the failure
looks like the master being down.

`config set` refuses a URL with no scheme or no host, and refuses a key it does
not know, so a typo is caught at the moment it is written instead of at the next
replica run. A configuration file that cannot be parsed stops the command with
an error naming the file: it is not treated as empty, because in replica mode
the consequences of aiming at the wrong master are deletions.

`replica check` queries the master for every file in the local index and builds a plan:

- **Delete locally** — master already has this MD5; the local copy is redundant.
- **Migrate to master** — master does not have this file; it will be uploaded and then removed locally.

The plan is shown before any changes are made. Confirm with `y` to execute, or press Enter / type `n` to cancel.

`-y` runs the plan unattended, with one exception. Uploads happen and the local
copies that this run put on the master are removed, because the content is in two
places before anything is deleted locally. The **delete locally** entries are
skipped: the master already had that content, so nothing would be left anywhere
if the deletion turned out to be wrong. They stay listed in the plan, onley warns
on stderr, and the run exits non-zero.

```sh
$ onley -y replica check
Delete locally (already on master, 892 file(s)):
  /my/files/photo_001.jpg
  ...

warning: 892 file(s) are already on the master and were left in place.
         -y does not delete a local copy the master already has, because nothing
         would be left anywhere if that turned out to be wrong.
         The list above is what is waiting for a decision: delete them yourself,
         or drop -y and confirm the plan interactively.

Done: 0 deleted, 342 migrated, 0 failed.
$ echo $?
1
```

To let a scheduled run delete them, review the list first. Removing `-y` and
answering `y` is the interactive path and still deletes everything in the plan.

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

`onley` sends this request with chunked transfer encoding: the file is streamed
rather than measured and buffered first, so `Content-Length` is absent. A direct
client should not depend on `Content-Length` being present.

## Development

```sh
make test       # run all tests
make coverage   # test with coverage report → coverage.html
make lint       # golangci-lint
make clean      # remove binary and coverage files
```

Requirements: Go 1.26.4 or newer (the version the module declares)

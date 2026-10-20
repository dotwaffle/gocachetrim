# gocachetrim

gocachetrim deletes old entries from the Go build cache (`go env GOCACHE`) by age and by total size.
It runs once, or as a daemon that trims the cache at an interval.

The go command trims its cache at most once a day, and only deletes entries that it has not used for five days.
Many parallel builds, for example from coding agents, can write more than 10GiB of data an hour into the cache.
`go clean -cache` deletes all entries, including the entries that running builds use.
This tool keeps the cache near a fixed size, and uses the mtimes of entries to keep the entries that builds used recently.

## Install

gocachetrim requires Go 1.25 or later.
It runs on Linux, macOS, FreeBSD, NetBSD, OpenBSD, DragonFly BSD and illumos.
On other systems, such as Windows, it builds but stops with an error, because it uses `flock` and `st_blocks`.

```sh
go install github.com/dotwaffle/gocachetrim@latest
```

## Usage

Trim the cache with the default limits:

```sh
gocachetrim
```

Trim the cache with a 20GiB size limit:

```sh
gocachetrim -max-size 20G
```

Show what a trim with a 1-day age limit would delete, but do not delete it:

```sh
gocachetrim -max-age 1d -dry-run
```

Trim every hour until the process gets SIGINT or SIGTERM:

```sh
gocachetrim -daemon
```

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `-cache` | `go env GOCACHE` | The cache directory. |
| `-max-age` | `5d` | Delete entries not used for this duration. `0` disables the age limit. The default is the limit that the go command uses in its own trim. |
| `-max-size` | `10G` | Delete the oldest entries until the cache is not larger than this size. `0` disables the size limit. |
| `-min-age` | `2h` | Never delete an entry with an mtime within this duration to meet `-max-size`. |
| `-size-metric` | `allocated` | Measure sizes as `allocated` disk space or `logical` file size. |
| `-workers` | `8` | The number of concurrent scan and delete workers. |
| `-daemon` | off | Trim now, then again at each `-interval`, until SIGINT or SIGTERM. |
| `-interval` | `1h` | The time between trims in daemon mode. |
| `-dry-run` | off | Report what a trim would delete, but do not delete it. |
| `-v` | off | Log debug messages. |

A duration uses the syntax of `time.ParseDuration`, or a number of days with a `d` suffix, for example `1.5d`.
A size is a number of bytes with an optional `K`, `M`, `G` or `T` suffix for powers of 1024.
An optional `B` or `iB` can follow the suffix, for example `10G` or `512MiB`.

At least one of `-max-age` and `-max-size` must be more than 0.
If `-max-age` is more than 0, it must be at least `-min-age`.
`-workers` must be at least 1.
With `-daemon`, `-interval` must be more than 0.

Without `-cache`, gocachetrim runs `go env GOCACHE`.
It stops with an error if `GOCACHE` is `off` or `GOCACHEPROG` is set, because then the go command does not use a cache directory.

## How a trim works

The cache has 256 subdirectories, `00` to `ff`.
Each subdirectory holds action entries (`<hash>-a`, 175 bytes each) and output entries (`<hash>-d`).
An output entry can also be a directory that holds one cached executable.
When the go command uses an entry, it sets the mtime of the entry to the current time.
It does this only if the mtime is at least one hour old.
So the mtime can be up to one hour before the last use.

A trim does these steps:

1. Lock the cache directory with a non-blocking `flock`.
   If a different trim holds the lock, log a message and stop.
2. Read the names and the file status of all entries in the 256 subdirectories.
   Ignore every other file in the cache, for example `fuzz/`, `README` and `testexpire.txt`.
3. Sort all entries by mtime, oldest first, then by subdirectory and name.
4. If `-max-age` is more than 0, select each entry with an mtime before the current time minus `-max-age`.
5. If `-max-size` is more than 0 and the other entries are larger than `-max-size`, select the oldest of them until the total is not larger than `-max-size`.
   Do not select an entry with an mtime after the current time minus `-min-age`.
6. Delete the selected entries, oldest first in each subdirectory.
   Before each delete, read the mtime again.
   If the mtime is newer than in step 2, do not delete the entry.
7. If the trim is not a dry run and was not stopped, write the current time to `trim.txt`.
   Do this only if `-max-age` is more than 0 and not more than five days.
   The go command then skips its own trim for 24 hours.
   A larger `-max-age`, or a `-max-age` of 0, does not delete all the entries that the trim of the go command deletes.
   Then the go command must still do its own trim.
8. Log a summary of the trim.

If `-max-size` is more than 0 and the cache is still larger than `-max-size` after the trim, the trim logs a message with the difference.
The cause can be `-min-age`, entries that a build used during the trim, or errors.
Step 6 keeps the entries that builds use during the trim, so a difference is normal while builds run.
If the trim had no errors and the difference is not more than the size of these entries, the message is at the info level.
Otherwise, the message is a warning.

Step 3 sorts action entries and output entries together, and does not keep pairs together.
An action entry for a deleted output entry causes a cache miss, and the go command builds the output again.

### Trim summary

Step 8 logs a `trim done` message with these fields:

| Field | Meaning |
| --- | --- |
| `dry_run` | The trim did not delete entries. The counts show what it would delete. |
| `stopped` | A signal stopped the trim in step 6. The counts include only the completed deletes. |
| `scanned`, `scanned_size` | The entries that step 2 found, and their total size. |
| `expired`, `expired_size` | The entries that step 6 deleted because of `-max-age`. |
| `evicted`, `evicted_size` | The entries that step 6 deleted because of `-max-size`. |
| `used_since_scan`, `used_since_scan_size` | The selected entries that step 6 kept, because a build used them after step 2. |
| `already_gone` | The selected entries that a different process deleted after step 2. |
| `final_size` | The size of the cache after the trim, from steps 2 and 6. It does not include entries that builds wrote during the trim. |
| `errors` | The number of errors for subdirectories, entries and `trim.txt`. |
| `took` | The duration of the trim. |

With `-v`, a `scan done` message before step 6 gives the number of entries that step 4 and step 5 selected (`age_victims` and `size_victims`).

### Size metric

The `allocated` metric is the disk space of a file (`st_blocks * 512`).
Compression can make this value less than the `logical` metric (`st_size`), and block overhead can make it more.
For example, one cache on ZFS had 177GiB of logical data in 72GiB of disk space.
On ZFS with RAID-Z, each action entry uses about 7KiB of disk space, so 230,000 action entries use about 1.6GiB.

### Safety

gocachetrim does not delete files in a directory that does not have the `README` that the go command writes in each cache.
All file access goes through `os.Root`, so a symbolic link in the cache cannot cause a delete outside of the cache.
The lock on the cache directory does not block the go command, because the go command never locks the directory itself.

The protection for running builds is best effort.
The mtime check in step 6 and the delete are two operations, and the go command can use an entry between them.
The trim of the go command has the same gap.
The one-hour mtime resolution also hides a use within the last hour from the check.
A build that runs for longer than `-min-age` can also lose an entry that it read at its start.
In these cases, the build can fail because a file is missing, and the next build creates the entry again.

The default `-min-age` of two hours is the one-hour mtime resolution plus one hour.
Set `-min-age` to more than the duration of your longest build plus one hour.
A value less than one hour can delete entries that a running build uses, so gocachetrim logs a warning at start.

## Run as a systemd user service

The file `contrib/systemd/gocachetrim.service` runs the daemon with the default limits.

1. Install the binary with `go install`.
   The unit expects it at `~/go/bin/gocachetrim`.
2. Copy the unit file to `~/.config/systemd/user/`.
3. Run `systemctl --user daemon-reload`.
4. Run `systemctl --user enable --now gocachetrim.service`.

To change the limits, run `systemctl --user edit gocachetrim.service` and override `ExecStart`.
The unit sets `-cache ~/.cache/go-build`, because the `PATH` of a user service often does not include the go command.
If `go env GOCACHE` shows a different directory, override `ExecStart` and set `-cache` to that directory.
Otherwise, the daemon does not trim your cache.
If the directory does not exist, the daemon logs a warning at each interval.
If the directory is not a Go build cache, the daemon stops, and the unit stays in the failed state.
Read the log with `journalctl --user -u gocachetrim`.

## Exit status

| Status | Meaning |
| --- | --- |
| 0 | The trim completed, a different trim held the lock, or `-h` showed the usage. A cache that is still over `-max-size` after the trim is not an error. |
| 1 | The trim could not start, the trim had errors, or a signal stopped a one-shot trim. |
| 2 | A flag or an argument is not valid. |

In daemon mode, if the first trim cannot start, the daemon exits with status 1.
For example, the first trim cannot start if the directory is not a Go build cache.
A cache directory that does not exist is not a failure, because the go command makes it at its first build.
The daemon then logs a warning and trims again at the next interval.
After the first trim, a failed trim does not stop the daemon.
The daemon logs the error and trims again at the next interval.
The daemon exits with status 0 after SIGINT or SIGTERM.

## Performance

These measurements are from a cache of 306,000 entries on ZFS, with the file metadata in memory.
The memory measurement is from a test cache of the same number of empty entries.

- A scan of all entries takes about 1.2 seconds.
- Each worker deletes the entries of whole subdirectories, because an unlink takes an exclusive lock on its parent directory.
  A benchmark deleted a copy of 9,158 entries from 16 subdirectories.
  One worker deleted about 20,000 entries a second, and four workers deleted about 76,000 a second.
  Eight and 32 workers were not much faster than four.
  With 32 workers that shared subdirectories, the rate decreased to 37,000 a second.
- A trim that deletes 233,000 of the 306,000 entries has a peak live heap of about 50MiB and a peak RSS of about 100MiB.
  The systemd unit sets `GOMEMLIMIT=128MiB`.
  This is a soft limit for the Go runtime.
  It makes the garbage collector run more often, but it does not limit the RSS.

Published `fs_mark` results show that XFS also gets faster with parallel unlinks, up to about eight threads, and that ext4 gets a smaller increase.
To tune a different filesystem, change `-workers`.

## License

gocachetrim uses the BSD 3-Clause license.
The full text is in `LICENSE`.

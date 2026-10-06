# gocachetrim

gocachetrim deletes old entries from the Go build cache (`go env GOCACHE`) by age and by total size.
It runs once, or as a daemon that trims the cache at an interval.

The go command trims its cache at most once a day, and only deletes entries that it has not used for five days.
Many parallel builds, for example from coding agents, can write more than 10GiB of data an hour into the cache.
`go clean -cache` deletes all entries, also the entries that running builds use.
This tool keeps the cache at a fixed size, and does not delete entries that a build used recently.

## Install

gocachetrim requires Go 1.25 or later.

```sh
go install github.com/dotwaffle/gocachetrim@latest
```

## Usage

Trim the cache with the default limits:

```sh
gocachetrim
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
| `-max-age` | `3d` | Delete entries not used for this duration. `0` disables the age limit. |
| `-max-size` | `10G` | Delete the oldest entries until the cache is not larger than this size. `0` disables the size limit. |
| `-min-age` | `2h` | Never delete entries used within this duration to meet `-max-size`. |
| `-size-metric` | `allocated` | Measure sizes as `allocated` disk space or `logical` file size. |
| `-workers` | `8` | The number of concurrent scan and delete workers. |
| `-daemon` | off | Trim now, then again at each `-interval`, until SIGINT or SIGTERM. |
| `-interval` | `1h` | The time between trims in daemon mode. |
| `-dry-run` | off | Report what a trim would delete, but do not delete it. |
| `-v` | off | Log debug messages. |

A duration uses the syntax of `time.ParseDuration`, or a number of days with a `d` suffix, for example `1.5d`.
A size is a number of bytes with an optional `K`, `M`, `G` or `T` suffix for powers of 1024, for example `10G` or `512MiB`.

Without `-cache`, gocachetrim runs `go env GOCACHE`.
It stops with an error if `GOCACHE` is `off` or `GOCACHEPROG` is set, because then the go command does not use a cache directory.

## How a trim works

The cache has 256 subdirectories, `00` to `ff`.
Each subdirectory holds action entries (`<hash>-a`, 175 bytes each) and output entries (`<hash>-d`).
An output entry can also be a directory that holds one cached executable.
When the go command uses an entry, it sets the mtime of the entry to the current time.
It does this only if the mtime is more than one hour old, so the mtime can be up to one hour before the last use.

A trim does these steps:

1. Lock the cache directory with a non-blocking `flock`.
   If a different trim holds the lock, log a message and stop.
2. Read the names and the file status of all entries in the 256 subdirectories.
   Ignore all other files, for example `fuzz/`, `README` and `testexpire.txt`.
3. Select each entry with an mtime before the current time minus `-max-age`.
4. If the other entries are larger than `-max-size`, sort them by mtime and select the oldest entries until the total is not larger than `-max-size`.
   Do not select an entry with an mtime after the current time minus `-min-age`.
5. Delete the selected entries.
   Before each delete, read the mtime again.
   If the go command used the entry after step 2, do not delete it.
6. If `-max-age` is not more than five days and the trim is not a dry run, write the current time to `trim.txt`.
   The go command then skips its own trim for 24 hours.
7. Log a summary of the trim.

If the cache is still larger than `-max-size` after the trim, the trim logs a warning with the difference.
The cause can be `-min-age`, entries that a build used during the trim, or errors.
The default `-min-age` of two hours is the one-hour mtime resolution plus one hour.
A value less than one hour can delete entries that a running build uses, and gocachetrim then logs a warning at start.

Step 4 sorts action entries and output entries together, by mtime, and does not keep pairs together.
An action entry for a deleted output entry causes a cache miss, and the go command builds the output again.

### Size metric

The `allocated` metric is the disk space of a file (`st_blocks * 512`).
On a filesystem with compression, this value is less than the `logical` metric (`st_size`).
For example, one cache on ZFS had 177GiB of logical data in 72GiB of disk space.
On ZFS with RAID-Z, each action entry uses about 7KiB of disk space, so 230,000 action entries use about 1.6GiB.

### Safety

gocachetrim does not delete files in a directory that does not have the `README` that the go command writes in each cache.
All file access goes through `os.Root`, so a symbolic link in the cache cannot cause a delete outside of the cache.
The lock on the cache directory does not block the go command, because the go command never locks the directory itself.

The protection for running builds is best effort.
The mtime check in step 5 and the delete are two operations, and the go command can use an entry between them.
The trim of the go command has the same gap.
A build that runs for longer than `-min-age` can also lose an entry that it read at its start.
In both cases, the build fails because a file is missing, and the next build creates the entry again.
Set `-min-age` to more than the duration of your longest build.

## Run as a systemd user service

The file `contrib/systemd/gocachetrim.service` runs the daemon with the default limits.

1. Install the binary with `go install`.
   The unit expects it at `~/go/bin/gocachetrim`.
2. Copy the unit file to `~/.config/systemd/user/`.
3. Run `systemctl --user daemon-reload`.
4. Run `systemctl --user enable --now gocachetrim.service`.

To change the limits, run `systemctl --user edit gocachetrim.service` and override `ExecStart`.
The unit sets `-cache`, because the `PATH` of a user service often does not include the go command.
Read the log with `journalctl --user -u gocachetrim`.

## Exit status

| Status | Meaning |
| --- | --- |
| 0 | The trim completed. Status 0 also applies when a different trim held the lock, and when the cache is still over `-max-size` after the trim. |
| 1 | The trim failed, or it could not read or delete one or more entries. |
| 2 | A flag or an argument is not valid. |

In daemon mode, an error in one trim is logged, and the next trim starts at the next interval.
The daemon exits with status 0 after SIGINT or SIGTERM.

## Performance

These measurements are from a cache of 306,000 entries on ZFS, with the file metadata in memory.
The memory measurement is from a test cache of the same number of empty entries.

- A scan of all entries takes about 1.2 seconds.
- Each worker deletes the entries of whole subdirectories, because an unlink takes an exclusive lock on its parent directory.
  With 9,158 entries, one worker deleted about 20,000 entries a second, and four workers deleted about 76,000 a second.
  Eight and 32 workers were not much faster than four.
  With 32 workers that shared subdirectories, the rate decreased to 37,000 a second.
- A trim that deletes 233,000 of the 306,000 entries has a peak live heap of about 70MiB and a peak RSS of about 160MiB.
  With `GOMEMLIMIT=128MiB`, the peak RSS is about 127MiB, and the trim takes about 15% more time.

These measurements are for ZFS only.
Published `fs_mark` results show that XFS also gets faster with parallel unlinks, up to about eight threads, and that ext4 gets a smaller increase.
To tune a different filesystem, change `-workers`.

## Platforms

gocachetrim builds on Linux, macOS, FreeBSD, NetBSD, OpenBSD, DragonFly BSD and illumos.
On other systems, such as Windows, it builds but stops with an error, because it uses `flock` and `st_blocks`.

## License

gocachetrim uses the BSD 3-Clause license.
The full text is in `LICENSE`.

# Schema exporter resource limits

`schema_file` and `schema_command` each accept at most 64 MiB of DDL. File reads stop after 64 MiB plus one byte, so an oversized file cannot be loaded in full.

Each `schema_command` run has a five-minute deadline, in addition to the caller's deadline. The command runs at least twice in each CLI command to check that its output is deterministic and did not change while the command worked; see [when the export runs](schema-inputs.md#when-the-export-runs). Stdout remains a regular temporary file because some exporters truncate output when stdout is a pipe. A watcher checks the file every 20 ms and cancels the command when it exceeds 64 MiB. Stderr is retained up to 1 MiB.

The watcher is **not a disk quota**. A fast writer can write beyond 64 MiB between checks, and process scheduling or storage stalls can delay a check. On macOS and Linux, cancellation kills the exporter's process group; on Windows, the exporter starts suspended, is assigned to a job object, and then resumes. Job members are terminated on cancellation or job close. Commands that deliberately detach from their group or job can continue running. Failure to create or assign the Windows job rejects the export rather than running without that containment.

Only regular files are accepted as schema input. On macOS and Linux, files are opened in nonblocking mode to avoid hanging if a repository path changes to a FIFO between inspection and open. These checks are resource safeguards for trusted project commands, not a sandbox for code in the checkout.

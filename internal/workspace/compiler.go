package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jokull/onwardpg/internal/timing"
)

const maxCompilerOutput = 64 << 20
const maxCompilerDuration = 5 * time.Minute
const compilerOutputCheckInterval = 20 * time.Millisecond

type CompiledDDL struct {
	DDL        []byte
	Provenance string
}

// CompileDDL runs the configured schema export twice and requires byte-for-
// byte deterministic output. It is the narrow CLI boundary for schema_file
// and schema_command; it does not expose a framework integration API.
//
// The checkout fingerprint taken after the first run is also the fingerprint
// before the second run. A change between the two runs therefore fails as a
// modified input instead of passing unseen.
func CompileDDL(ctx context.Context, root, targetName string, target Target) (CompiledDDL, error) {
	first, tree, err := compileDDLOnce(ctx, root, targetName, target, "")
	if err != nil {
		return CompiledDDL{}, err
	}
	second, _, err := compileDDLOnce(ctx, root, targetName, target, tree)
	if err != nil {
		return CompiledDDL{}, err
	}
	if !first.equal(second) {
		return CompiledDDL{}, nondeterministicExport(first, second)
	}
	return first, nil
}

func (c CompiledDDL) equal(other CompiledDDL) bool {
	return c.Provenance == other.Provenance && bytes.Equal(c.DDL, other.DDL)
}

func nondeterministicExport(first, second CompiledDDL) error {
	return fmt.Errorf("DDL export is nondeterministic: two consecutive schema_command runs produced different bytes (first %s, second %s); remove timestamps, random identifiers, environment-dependent ordering, and tool chatter from stdout", ddlDigest(first.DDL), ddlDigest(second.DDL))
}

// Export is one command's view of the configured schema export. StartExport
// runs the export once, so the command can plan from it. Confirm runs it again
// immediately before the command commits its result. Equal bytes from the two
// runs prove what CompileDDL proves (the export is deterministic) and also
// that the export did not change while the command worked.
//
// The checkout fingerprint that follows the first run is taken while the
// caller loads the DDL into scratch PostgreSQL. Settle waits for it. Confirm
// settles first, so no result is committed from a run that wrote to the
// checkout, whether or not the caller settled earlier.
type Export struct {
	root       string
	targetName string
	target     Target
	first      CompiledDDL
	firstCheck *readOnlyCheck
	// attempted is set by the first Confirm, whatever its result. The
	// confirming run is the final check of a command: it is not repeated.
	attempted bool
}

// StartExport runs the configured export once and keeps its output.
func StartExport(ctx context.Context, root, targetName string, target Target) (*Export, error) {
	first, check, err := compileDDLDeferred(ctx, root, targetName, target, "")
	if err != nil {
		return nil, err
	}
	return &Export{root: root, targetName: targetName, target: target, first: first, firstCheck: check}, nil
}

// Compiled returns the output of the first run.
func (e *Export) Compiled() CompiledDDL { return e.first }

// Settle waits for the checkout fingerprint that follows the first run and
// reports a run that changed the checkout. Call it when the work that
// overlaps the fingerprint is done, before any error of that work is
// reported, so that a changed checkout is reported first. It is safe to call
// more than once.
func (e *Export) Settle() error {
	_, err := e.firstCheck.wait()
	return err
}

// Confirm runs the export again. A nil result means the bytes equal the first
// run. Otherwise the export changed after the command started: Confirm then
// requires a third run to equal the second, so that the returned export is
// deterministic, and returns it for the caller to compare with the first by
// catalog fingerprint. A second and third run that differ are an error.
//
// Confirm is the final check of a command and runs once. A caller that
// receives an error must not commit a result that rests on the export.
func (e *Export) Confirm(ctx context.Context) (*CompiledDDL, error) {
	if e.attempted {
		return nil, errors.New("the schema export of this command was already confirmed or rejected")
	}
	e.attempted = true
	if err := e.Settle(); err != nil {
		return nil, err
	}
	second, tree, err := compileDDLOnce(ctx, e.root, e.targetName, e.target, "")
	if err != nil {
		return nil, err
	}
	if e.first.equal(second) {
		return nil, nil
	}
	third, _, err := compileDDLOnce(ctx, e.root, e.targetName, e.target, tree)
	if err != nil {
		return nil, err
	}
	if !second.equal(third) {
		return nil, nondeterministicExport(second, third)
	}
	return &second, nil
}

// ConfirmUnchanged is the confirming run of a command that ends without the
// catalog comparison that precedes a write, for example with a report of an
// unsupported schema or of a failed verification. Such a result describes the
// first export. It is reported only when the confirming run has the same
// bytes; an export that changed while the command worked is an error, because
// the result would describe a schema that is no longer the configured one.
//
// A command that started no export needs no run. A command whose Confirm
// already ran needs none either: Confirm returned its result, or its error,
// to the code that then reported it, and a rejected export is not run again
// in the hope of another answer.
func ConfirmUnchanged(ctx context.Context, export *Export) error {
	if export == nil || export.attempted {
		return nil
	}
	changed, err := export.Confirm(ctx)
	if err != nil {
		return err
	}
	if changed != nil {
		return ErrExportChanged
	}
	return nil
}

// ErrExportChanged reports a deterministic export whose output changed after
// the command read it.
var ErrExportChanged = errors.New("the configured schema export changed while the command worked; run the command again")

var errModifiedInputs = errors.New("DDL export command modified repository inputs; schema_command must be read-only")

// treeDigest is a checkout fingerprint that is taken in the background.
type treeDigest struct {
	done  chan struct{}
	value string
	err   error
}

func startTreeDigest(ctx context.Context, root string) *treeDigest {
	digest := &treeDigest{done: make(chan struct{})}
	go func() {
		defer close(digest.done)
		defer timing.Start("input_tree_digest")()
		digest.value, digest.err = digestTree(ctx, root)
	}()
	return digest
}

func (d *treeDigest) wait() (string, error) {
	<-d.done
	return d.value, d.err
}

// readOnlyCheck compares the checkout fingerprint from before one export run
// with the fingerprint taken after it. A nil check passes: schema_file runs
// no command.
type readOnlyCheck struct {
	before string
	after  *treeDigest
}

// wait returns the fingerprint after the run, or the reason the run is not
// accepted.
func (c *readOnlyCheck) wait() (string, error) {
	if c == nil {
		return "", nil
	}
	after, err := c.after.wait()
	if err != nil {
		return "", fmt.Errorf("fingerprint DDL export tree after command: %w", err)
	}
	if c.before != after {
		return "", errModifiedInputs
	}
	return after, nil
}

// compileDDLOnce runs the export one time and waits for its checkout
// fingerprints. before is the fingerprint when the caller already holds it
// for the current state of the tree, or the empty string. The returned
// fingerprint describes the tree after the run; it is empty for schema_file,
// which runs no command.
func compileDDLOnce(ctx context.Context, root, targetName string, target Target, before string) (CompiledDDL, string, error) {
	compiled, check, err := compileDDLDeferred(ctx, root, targetName, target, before)
	if err != nil {
		return CompiledDDL{}, "", err
	}
	after, err := check.wait()
	if err != nil {
		return CompiledDDL{}, "", err
	}
	return compiled, after, nil
}

// compileDDLDeferred runs the export one time. After a successful run it
// returns the output at once, with the check of the checkout fingerprint still
// in progress; the caller must wait for that check before it commits anything
// that rests on the output. After a failed run it waits for the check itself,
// because a changed checkout is reported before a failed command.
func compileDDLDeferred(ctx context.Context, root, targetName string, target Target, before string) (CompiledDDL, *readOnlyCheck, error) {
	if root == "" || !filepath.IsAbs(root) {
		return CompiledDDL{}, nil, fmt.Errorf("compiler root must be absolute")
	}
	if targetName == "" {
		return CompiledDDL{}, nil, fmt.Errorf("compiler target is required")
	}
	if err := target.Validate(); err != nil {
		return CompiledDDL{}, nil, err
	}
	if target.SchemaFile != "" {
		name := filepath.Join(root, filepath.FromSlash(target.SchemaFile))
		data, err := readCompilerOutput(ctx, name)
		if err != nil {
			return CompiledDDL{}, nil, fmt.Errorf("read declarative schema file: %w", err)
		}
		return CompiledDDL{DDL: data, Provenance: "schema_file:" + target.SchemaFile}, nil, nil
	}

	if before == "" {
		digest, err := startTreeDigest(ctx, root).wait()
		if err != nil {
			return CompiledDDL{}, nil, fmt.Errorf("fingerprint DDL export tree before command: %w", err)
		}
		before = digest
	}
	stdout, err := os.CreateTemp("", "onwardpg-schema-export-*.sql")
	if err != nil {
		return CompiledDDL{}, nil, fmt.Errorf("create DDL export capture: %w", err)
	}
	stdoutName := stdout.Name()
	defer os.Remove(stdoutName)
	defer stdout.Close()
	stderr := &limitedBuffer{limit: 1 << 20}
	stopCommand := timing.Start("schema_command")
	commandErr := runSchemaCommand(ctx, root, target.SchemaCommand, stdout, stderr, maxCompilerDuration)
	stopCommand()
	closeErr := stdout.Close()
	check := &readOnlyCheck{before: before, after: startTreeDigest(ctx, root)}
	// The capture file is outside the checkout, so reading it does not
	// disturb the fingerprint that is in progress.
	failure := func(err error) (CompiledDDL, *readOnlyCheck, error) {
		if _, checkErr := check.wait(); checkErr != nil {
			return CompiledDDL{}, nil, checkErr
		}
		return CompiledDDL{}, nil, err
	}
	if commandErr != nil {
		message := strings.TrimSpace(stderr.String())
		context := "check the exporter command, its runtime dependencies, and disposable database connectivity"
		lower := strings.ToLower(message)
		if strings.Contains(lower, "server version") && strings.Contains(lower, "pg_dump version") || strings.Contains(lower, "major") && strings.Contains(lower, "pg_dump") {
			context = "the pg_dump client and disposable PostgreSQL server likely use different major versions"
		}
		if message == "" {
			return failure(fmt.Errorf("DDL export command %q failed (%s): %w", strings.Join(target.SchemaCommand, " "), context, commandErr))
		}
		return failure(fmt.Errorf("DDL export command %q failed (%s): %w; stderr: %s", strings.Join(target.SchemaCommand, " "), context, commandErr, message))
	}
	if closeErr != nil {
		return failure(fmt.Errorf("close DDL export capture: %w", closeErr))
	}
	ddl, err := readCompilerOutput(ctx, stdoutName)
	if err != nil {
		return failure(fmt.Errorf("read DDL export capture: %w", err))
	}
	return CompiledDDL{DDL: ddl, Provenance: "schema_command"}, check, nil
}

// Keep stdout as an *os.File: some schema exporters inspect their stdout and
// truncate output when it is a pipe. The size watcher cancels a running
// exporter after detecting excess output. It is not a strict disk quota: a
// fast writer can grow the file between checks.
func runSchemaCommand(ctx context.Context, root string, args []string, stdout *os.File, stderr *limitedBuffer, timeout time.Duration) error {
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, args[0], args[1:]...)
	command.Dir = root
	command.Env = os.Environ()
	command.Stdout, command.Stderr = stdout, stderr
	// If a child inherits the stderr pipe, do not wait forever for its EOF.
	command.WaitDelay = 5 * time.Second
	group, err := newExporterProcessGroup()
	if err != nil {
		return fmt.Errorf("prepare DDL export process group: %w", err)
	}
	defer group.close()
	group.configure(command)
	command.Cancel = func() error { return group.stop(command) }
	if err := command.Start(); err != nil {
		return err
	}
	if err := group.attach(command); err != nil {
		_ = group.stop(command)
		_ = command.Wait()
		if commandCtx.Err() != nil {
			return fmt.Errorf("DDL export command stopped: %w", commandCtx.Err())
		}
		return fmt.Errorf("attach DDL exporter to process group: %w", err)
	}
	// Closing the group also stops descendants that kept stdout open after
	// their parent exited. This happens on every return path, including success.
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(compilerOutputCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			if commandCtx.Err() != nil {
				return fmt.Errorf("DDL export command stopped: %w", commandCtx.Err())
			}
			return err
		case <-ticker.C:
			info, err := stdout.Stat()
			if err != nil {
				cancel()
				<-done
				return fmt.Errorf("inspect DDL export capture: %w", err)
			}
			if info.Size() > maxCompilerOutput {
				cancel()
				<-done
				return fmt.Errorf("DDL export output exceeds %d bytes", maxCompilerOutput)
			}
		case <-commandCtx.Done():
			<-done
			return fmt.Errorf("DDL export command stopped: %w", commandCtx.Err())
		}
	}
}

// LimitReader enforces the bound even if a file changes after Stat. The
// initial size check avoids allocating for a known oversized regular file.
func readCompilerOutput(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openRegularCompilerFile(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxCompilerOutput {
		return nil, fmt.Errorf("DDL export output exceeds %d bytes", maxCompilerOutput)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx: ctx, reader: file}, maxCompilerOutput+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCompilerOutput {
		return nil, fmt.Errorf("DDL export output exceeds %d bytes", maxCompilerOutput)
	}
	return data, nil
}

func ddlDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = b.buffer.Write(data)
	}
	if original > remaining {
		b.exceeded = true
	}
	return original, nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }

// digestTree fingerprints every repository input under root: each name, its
// mode, and its link target or its size and bytes. The walk fixes the sorted
// name list; workers then hash the entries, and the entry digests are folded
// in name order. The result therefore does not depend on the worker count.
func digestTree(ctx context.Context, root string) (string, error) {
	var entries []treeEntry
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == root {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		if ignoredCompilerTreeEntry(entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entries = append(entries, treeEntry{name: filepath.ToSlash(relative), listedRegular: entry.Type().IsRegular()})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	digests := make([][sha256.Size]byte, len(entries))
	failures := make([]error, len(entries))
	workers := min(runtime.GOMAXPROCS(0), maxDigestWorkers, len(entries))
	var next atomic.Int64
	var failed atomic.Bool
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				index := int(next.Add(1)) - 1
				if index >= len(entries) || failed.Load() {
					return
				}
				digests[index], failures[index] = digestTreeEntry(ctx, root, entries[index])
				if failures[index] != nil {
					failed.Store(true)
				}
			}
		}()
	}
	group.Wait()
	// Workers stop after a failure, so later entries can be unread. Report the
	// recorded failure whose name sorts first.
	for _, failure := range failures {
		if failure != nil {
			return "", failure
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hash := sha256.New()
	for index := range digests {
		_, _ = hash.Write(digests[index][:])
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// Each worker holds one open file, so this also bounds descriptor use.
const maxDigestWorkers = 16

type treeEntry struct {
	name string
	// listedRegular is the file type that the directory listing reported. It
	// only selects how the entry is opened; the digest uses the type and mode
	// of the object that was opened.
	listedRegular bool
}

func digestTreeEntry(ctx context.Context, root string, entry treeEntry) (digest [sha256.Size]byte, err error) {
	if err := ctx.Err(); err != nil {
		return digest, err
	}
	name := entry.name
	full := filepath.Join(root, filepath.FromSlash(name))
	hash := sha256.New()
	writeDigestFrame(hash, []byte(name))
	if entry.listedRegular {
		// Most entries are regular files. Open them without a path lookup
		// before the open. Anything unexpected takes the general path below.
		if file, info, ok := openListedRegularFile(full); ok {
			writeDigestFrame(hash, []byte(info.Mode().String()))
			if err := digestFileContent(ctx, hash, file, info.Size(), name); err != nil {
				return digest, err
			}
			hash.Sum(digest[:0])
			return digest, nil
		}
	}
	info, err := os.Lstat(full)
	if err != nil {
		return digest, err
	}
	writeDigestFrame(hash, []byte(info.Mode().String()))
	switch {
	case info.IsDir():
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return digest, err
		}
		writeDigestFrame(hash, []byte(target))
	case !info.Mode().IsRegular():
		return digest, fmt.Errorf("compiler tree contains unsupported path %s", name)
	default:
		file, err := openRegularCompilerFile(full)
		if err != nil {
			return digest, err
		}
		if err := digestFileContent(ctx, hash, file, info.Size(), name); err != nil {
			return digest, err
		}
	}
	hash.Sum(digest[:0])
	return digest, nil
}

// digestFileContent adds the size and the bytes of file to hash and closes
// file. It rejects a file whose length differs from size, so a file that
// changes during the read cannot yield a digest of neither state.
func digestFileContent(ctx context.Context, hash io.Writer, file *os.File, size int64, name string) error {
	var length [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(length[:], uint64(size))
	_, _ = hash.Write(length[:n])
	copied, copyErr := io.CopyN(hash, contextReader{ctx: ctx, reader: file}, size)
	var extra [1]byte
	extraCount, extraErr := file.Read(extra[:])
	closeErr := file.Close()
	if copyErr != nil && copyErr != io.EOF {
		return copyErr
	}
	if extraErr != nil && extraErr != io.EOF {
		return extraErr
	}
	if closeErr != nil {
		return closeErr
	}
	if copied != size || extraCount != 0 {
		return fmt.Errorf("compiler tree file changed while fingerprinting: %s", name)
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// Dependency installations and VCS internals are not repository inputs to a
// schema export. Walking either can turn a seconds-long export into minutes in
// a monorepo, and package managers legitimately maintain their own caches.
// Generated project files remain in scope: a schema command that builds or
// rewrites the checkout is still rejected.
func ignoredCompilerTreeEntry(entry os.DirEntry) bool {
	return entry.Name() == ".git" || entry.Name() == "node_modules"
}

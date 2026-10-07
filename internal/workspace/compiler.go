package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
func CompileDDL(ctx context.Context, root, targetName string, target Target) (CompiledDDL, error) {
	watch := watchExportSideEffects(ctx, root, target)
	first, err := compileDDLOnce(ctx, root, targetName, target)
	if err != nil {
		return CompiledDDL{}, err
	}
	second, err := compileDDLOnce(ctx, root, targetName, target)
	if err != nil {
		return CompiledDDL{}, err
	}
	watch.finish(ctx)
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
type Export struct {
	root       string
	targetName string
	target     Target
	first      CompiledDDL
	// watch observes the git work tree from before the first run until after
	// the last. It reports; it never changes the result of the command.
	watch *sideEffectWatch
	// attempted is set by the first Confirm, whatever its result. The
	// confirming run is the final check of a command: it is not repeated.
	attempted bool
}

// StartExport runs the configured export once and keeps its output.
func StartExport(ctx context.Context, root, targetName string, target Target) (*Export, error) {
	watch := watchExportSideEffects(ctx, root, target)
	first, err := compileDDLOnce(ctx, root, targetName, target)
	if err != nil {
		return nil, err
	}
	return &Export{root: root, targetName: targetName, target: target, first: first, watch: watch}, nil
}

// Compiled returns the output of the first run.
func (e *Export) Compiled() CompiledDDL { return e.first }

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
	// The observation ends after the last run and before the caller writes
	// anything, so the command's own files are not reported.
	defer e.watch.finish(ctx)
	second, err := compileDDLOnce(ctx, e.root, e.targetName, e.target)
	if err != nil {
		return nil, err
	}
	if e.first.equal(second) {
		return nil, nil
	}
	third, err := compileDDLOnce(ctx, e.root, e.targetName, e.target)
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

// compileDDLOnce runs the export one time. onwardpg does not check what the
// command writes to the checkout: see watchExportSideEffects.
func compileDDLOnce(ctx context.Context, root, targetName string, target Target) (CompiledDDL, error) {
	if root == "" || !filepath.IsAbs(root) {
		return CompiledDDL{}, fmt.Errorf("compiler root must be absolute")
	}
	if targetName == "" {
		return CompiledDDL{}, fmt.Errorf("compiler target is required")
	}
	if err := target.Validate(); err != nil {
		return CompiledDDL{}, err
	}
	if target.SchemaFile != "" {
		name := filepath.Join(root, filepath.FromSlash(target.SchemaFile))
		data, err := readCompilerOutput(ctx, name)
		if err != nil {
			return CompiledDDL{}, fmt.Errorf("read declarative schema file: %w", err)
		}
		return CompiledDDL{DDL: data, Provenance: "schema_file:" + target.SchemaFile}, nil
	}

	stdout, err := os.CreateTemp("", "onwardpg-schema-export-*.sql")
	if err != nil {
		return CompiledDDL{}, fmt.Errorf("create DDL export capture: %w", err)
	}
	stdoutName := stdout.Name()
	defer os.Remove(stdoutName)
	defer stdout.Close()
	stderr := &limitedBuffer{limit: 1 << 20}
	stopCommand := timing.Start("schema_command")
	commandErr := runSchemaCommand(ctx, root, target.SchemaCommand, stdout, stderr, maxCompilerDuration)
	stopCommand()
	closeErr := stdout.Close()
	if commandErr != nil {
		message := strings.TrimSpace(stderr.String())
		context := "check the exporter command, its runtime dependencies, and disposable database connectivity"
		lower := strings.ToLower(message)
		if strings.Contains(lower, "server version") && strings.Contains(lower, "pg_dump version") || strings.Contains(lower, "major") && strings.Contains(lower, "pg_dump") {
			context = "the pg_dump client and disposable PostgreSQL server likely use different major versions"
		}
		if message == "" {
			return CompiledDDL{}, fmt.Errorf("DDL export command %q failed (%s): %w", strings.Join(target.SchemaCommand, " "), context, commandErr)
		}
		return CompiledDDL{}, fmt.Errorf("DDL export command %q failed (%s): %w; stderr: %s", strings.Join(target.SchemaCommand, " "), context, commandErr, message)
	}
	if closeErr != nil {
		return CompiledDDL{}, fmt.Errorf("close DDL export capture: %w", closeErr)
	}
	ddl, err := readCompilerOutput(ctx, stdoutName)
	if err != nil {
		return CompiledDDL{}, fmt.Errorf("read DDL export capture: %w", err)
	}
	return CompiledDDL{DDL: ddl, Provenance: "schema_command"}, nil
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

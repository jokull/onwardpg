package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	first, err := compileDDLOnce(ctx, root, targetName, target)
	if err != nil {
		return CompiledDDL{}, err
	}
	second, err := compileDDLOnce(ctx, root, targetName, target)
	if err != nil {
		return CompiledDDL{}, err
	}
	if first.Provenance != second.Provenance || !bytes.Equal(first.DDL, second.DDL) {
		return CompiledDDL{}, fmt.Errorf("DDL export is nondeterministic: two consecutive schema_command runs produced different bytes (first %s, second %s); remove timestamps, random identifiers, environment-dependent ordering, and tool chatter from stdout", ddlDigest(first.DDL), ddlDigest(second.DDL))
	}
	return first, nil
}

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

	before, err := digestTree(ctx, root)
	if err != nil {
		return CompiledDDL{}, fmt.Errorf("fingerprint DDL export tree before command: %w", err)
	}
	stdout, err := os.CreateTemp("", "onwardpg-schema-export-*.sql")
	if err != nil {
		return CompiledDDL{}, fmt.Errorf("create DDL export capture: %w", err)
	}
	stdoutName := stdout.Name()
	defer os.Remove(stdoutName)
	defer stdout.Close()
	stderr := &limitedBuffer{limit: 1 << 20}
	commandErr := runSchemaCommand(ctx, root, target.SchemaCommand, stdout, stderr, maxCompilerDuration)
	closeErr := stdout.Close()
	after, digestErr := digestTree(ctx, root)
	if digestErr != nil {
		return CompiledDDL{}, fmt.Errorf("fingerprint DDL export tree after command: %w", digestErr)
	}
	if before != after {
		return CompiledDDL{}, fmt.Errorf("DDL export command modified repository inputs; schema_command must be read-only")
	}
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

func digestTree(ctx context.Context, root string) (string, error) {
	var names []string
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
		names = append(names, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		full := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if err != nil {
			return "", err
		}
		writeDigestFrame(hash, []byte(name))
		writeDigestFrame(hash, []byte(info.Mode().String()))
		if info.IsDir() {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			writeDigestFrame(hash, []byte(target))
			continue
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("compiler tree contains unsupported path %s", name)
		}
		file, err := openRegularCompilerFile(full)
		if err != nil {
			return "", err
		}
		var length [10]byte
		n := binary.PutUvarint(length[:], uint64(info.Size()))
		_, _ = hash.Write(length[:n])
		copied, copyErr := io.CopyN(hash, contextReader{ctx: ctx, reader: file}, info.Size())
		var extra [1]byte
		extraCount, extraErr := file.Read(extra[:])
		closeErr := file.Close()
		if copyErr != nil && copyErr != io.EOF {
			return "", copyErr
		}
		if extraErr != nil && extraErr != io.EOF {
			return "", extraErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if copied != info.Size() || extraCount != 0 {
			return "", fmt.Errorf("compiler tree file changed while fingerprinting: %s", name)
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
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

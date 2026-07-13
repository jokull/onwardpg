package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const maxCompilerOutput = 64 << 20

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
		return CompiledDDL{}, fmt.Errorf("DDL export is nondeterministic")
	}
	return CompiledDDL{DDL: append([]byte(nil), first.DDL...), Provenance: first.Provenance}, nil
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
		data, err := os.ReadFile(name)
		if err != nil {
			return CompiledDDL{}, fmt.Errorf("read declarative schema file: %w", err)
		}
		return CompiledDDL{DDL: append([]byte(nil), data...), Provenance: "schema_file:" + target.SchemaFile}, nil
	}

	before, err := digestTree(root)
	if err != nil {
		return CompiledDDL{}, fmt.Errorf("fingerprint DDL export tree before command: %w", err)
	}
	command := exec.CommandContext(ctx, target.SchemaCommand[0], target.SchemaCommand[1:]...)
	command.Dir = root
	command.Env = os.Environ()
	stdout := &limitedBuffer{limit: maxCompilerOutput}
	stderr := &limitedBuffer{limit: 1 << 20}
	command.Stdout, command.Stderr = stdout, stderr
	commandErr := command.Run()
	after, digestErr := digestTree(root)
	if digestErr != nil {
		return CompiledDDL{}, fmt.Errorf("fingerprint DDL export tree after command: %w", digestErr)
	}
	if before != after {
		return CompiledDDL{}, fmt.Errorf("DDL export command modified repository inputs; schema_command must be read-only")
	}
	if commandErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = commandErr.Error()
		}
		return CompiledDDL{}, fmt.Errorf("DDL export command failed: %s", message)
	}
	if stdout.exceeded {
		return CompiledDDL{}, fmt.Errorf("DDL export output exceeds %d bytes", maxCompilerOutput)
	}
	return CompiledDDL{DDL: stdout.Bytes(), Provenance: "schema_command"}, nil
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

func (b *limitedBuffer) Bytes() []byte  { return append([]byte(nil), b.buffer.Bytes()...) }
func (b *limitedBuffer) String() string { return b.buffer.String() }

func digestTree(root string) (string, error) {
	var names []string
	err := filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
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
		names = append(names, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
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
		data, err := os.ReadFile(full)
		if err != nil {
			return "", err
		}
		writeDigestFrame(hash, data)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

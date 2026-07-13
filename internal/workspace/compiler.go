package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jokull/onwardpg/adapter"
)

const maxCompilerOutput = 64 << 20

type TargetCompiler struct {
	TargetName string
	Target     Target
}

func (c TargetCompiler) Compile(ctx context.Context, request adapter.CompileRequest) (adapter.Artifact, error) {
	if request.Root == "" || !filepath.IsAbs(request.Root) {
		return adapter.Artifact{}, fmt.Errorf("compiler root must be absolute")
	}
	if request.Target != "" && request.Target != c.TargetName {
		return adapter.Artifact{}, fmt.Errorf("compiler target is %q, want %q", request.Target, c.TargetName)
	}
	if err := c.Target.Validate(); err != nil {
		return adapter.Artifact{}, err
	}
	if c.Target.SchemaFile != "" {
		name := filepath.Join(request.Root, filepath.FromSlash(c.Target.SchemaFile))
		data, err := os.ReadFile(name)
		if err != nil {
			return adapter.Artifact{}, fmt.Errorf("read declarative schema file: %w", err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return adapter.Artifact{}, fmt.Errorf("declarative schema file is empty")
		}
		return adapter.DDL(c.Target.Adapter+":"+c.Target.SchemaFile, data), nil
	}

	before, err := digestTree(request.Root)
	if err != nil {
		return adapter.Artifact{}, fmt.Errorf("fingerprint compiler tree before command: %w", err)
	}
	command := exec.CommandContext(ctx, c.Target.SchemaCommand[0], c.Target.SchemaCommand[1:]...)
	command.Dir = request.Root
	command.Env = os.Environ()
	stdout := &limitedBuffer{limit: maxCompilerOutput}
	stderr := &limitedBuffer{limit: 1 << 20}
	command.Stdout, command.Stderr = stdout, stderr
	commandErr := command.Run()
	after, digestErr := digestTree(request.Root)
	if digestErr != nil {
		return adapter.Artifact{}, fmt.Errorf("fingerprint compiler tree after command: %w", digestErr)
	}
	if before != after {
		return adapter.Artifact{}, fmt.Errorf("schema compiler modified its isolated input tree; undeclared outputs are not allowed")
	}
	if commandErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = commandErr.Error()
		}
		return adapter.Artifact{}, fmt.Errorf("schema compiler failed: %s", message)
	}
	if stdout.exceeded {
		return adapter.Artifact{}, fmt.Errorf("schema compiler output exceeds %d bytes", maxCompilerOutput)
	}
	data := stdout.Bytes()
	if len(bytes.TrimSpace(data)) == 0 {
		return adapter.Artifact{}, fmt.Errorf("schema compiler produced empty DDL")
	}
	return adapter.DDL(c.Target.Adapter+":command", data), nil
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
		_, _ = io.WriteString(hash, name+"\x00"+info.Mode().String()+"\x00")
		if info.IsDir() {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			_, _ = io.WriteString(hash, target)
			continue
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("compiler tree contains unsupported path %s", name)
		}
		file, err := os.Open(full)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

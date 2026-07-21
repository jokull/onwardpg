// Package testkit provides private end-to-end test machinery for onwardpg.
// It deliberately drives the compiled command and persisted artifacts instead
// of substituting planner internals for the public product boundary.
package testkit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var commandSequence atomic.Uint64

type Binary struct {
	Path string
}

type CommandResult struct {
	Argv        []string
	Environment []string
	ExitCode    int
	Stdout      string
	Stderr      string
	Duration    time.Duration
	redactions  []string
}

func BuildBinary(ctx context.Context, repositoryRoot, destination string) (Binary, error) {
	command := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", destination, "./cmd/onwardpg")
	command.Dir = repositoryRoot
	output, err := command.CombinedOutput()
	if err != nil {
		return Binary{}, fmt.Errorf("build onwardpg: %w\n%s", err, output)
	}
	return Binary{Path: destination}, nil
}

func (b Binary) Run(ctx context.Context, directory string, environment map[string]string, arguments ...string) CommandResult {
	started := time.Now()
	result := CommandResult{Argv: append([]string{b.Path}, arguments...), ExitCode: -1}
	command := exec.CommandContext(ctx, b.Path, arguments...)
	command.Dir = directory
	command.Env = mergedEnvironment(environment)
	for key := range environment {
		result.Environment = append(result.Environment, key)
	}
	for _, value := range environment {
		if value != "" {
			result.redactions = append(result.redactions, value)
		}
	}
	sort.Strings(result.Environment)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result.Duration = time.Since(started)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if err == nil {
		result.ExitCode = 0
	} else if exitError, ok := err.(*exec.ExitError); ok {
		result.ExitCode = exitError.ExitCode()
	}
	recordCommandResult(result)
	return result
}

func (r CommandResult) DecodeJSON(destination any) error {
	redacted := r.redacted()
	decoder := json.NewDecoder(strings.NewReader(redacted.Stdout))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode command JSON: %w (exit=%d stdout=%q stderr=%q)", err, redacted.ExitCode, redacted.Stdout, redacted.Stderr)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("decode command JSON: trailing value or invalid data in stdout %q: %v", redacted.Stdout, err)
	}
	return nil
}

func (r CommandResult) Failure() string {
	redacted := r.redacted()
	return fmt.Sprintf("argv=%q env_keys=%q exit=%d duration=%s\nstdout:\n%s\nstderr:\n%s", redacted.Argv, redacted.Environment, redacted.ExitCode, redacted.Duration, redacted.Stdout, redacted.Stderr)
}

func mergedEnvironment(overrides map[string]string) []string {
	values := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[key] = value
		}
	}
	for key, value := range overrides {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func recordCommandResult(result CommandResult) {
	directory := os.Getenv("ONWARDPG_ACCEPTANCE_ARTIFACT_DIR")
	if directory == "" {
		return
	}
	redacted := result.redacted()
	body, err := json.MarshalIndent(redacted, "", "  ")
	if err != nil || os.MkdirAll(directory, 0o700) != nil {
		return
	}
	name := fmt.Sprintf("command-%04d.json", commandSequence.Add(1))
	_ = os.WriteFile(filepath.Join(directory, name), append(body, '\n'), 0o600)
}

func (r CommandResult) redacted() CommandResult {
	redacted := r
	redacted.Argv = append([]string(nil), r.Argv...)
	redacted.redactions = nil
	for _, secret := range r.redactions {
		if secret == "" {
			continue
		}
		redacted.Stdout = strings.ReplaceAll(redacted.Stdout, secret, "[REDACTED]")
		redacted.Stderr = strings.ReplaceAll(redacted.Stderr, secret, "[REDACTED]")
		for index := range redacted.Argv {
			redacted.Argv[index] = strings.ReplaceAll(redacted.Argv[index], secret, "[REDACTED]")
		}
	}
	return redacted
}

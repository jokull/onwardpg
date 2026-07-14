// Package targetlock serializes lifecycle commands for one repository config.
// It locks the existing config file itself, so aliases, cache settings, users,
// and process crashes cannot create separate pathname locks or stale lock
// artifacts in the checkout.
package targetlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var ErrBusy = errors.New("target history update is already in progress")

type Lock struct {
	path string
	file *os.File
	info os.FileInfo
}

func Acquire(configPath, target string) (*Lock, error) {
	if configPath == "" || !filepath.IsAbs(configPath) {
		return nil, fmt.Errorf("target lock configuration path must be absolute")
	}
	if !safeName(target) {
		return nil, fmt.Errorf("target lock name %q is invalid", target)
	}
	file, err := os.Open(configPath)
	if err != nil {
		return nil, fmt.Errorf("open target history lock: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("inspect target history lock: %w", err)
	}
	locked, err := tryLockFile(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire target history lock: %w", err)
	}
	if !locked {
		_ = file.Close()
		return nil, fmt.Errorf("%w: %s (%s)", ErrBusy, configPath, target)
	}
	return &Lock{path: configPath, file: file, info: info}, nil
}

func safeName(value string) bool {
	if value == "" || strings.Trim(value, ".") == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

// ValidatePath proves that the locked config inode is still the file resolved
// by the configured path. Atomic editor replacement therefore blocks a commit
// even when the replacement has byte-identical TOML.
func (l *Lock) ValidatePath() error {
	if l == nil || l.file == nil || l.info == nil {
		return fmt.Errorf("target history lock is not held")
	}
	current, err := os.Stat(l.path)
	if err != nil {
		return fmt.Errorf("inspect locked configuration path: %w", err)
	}
	if !os.SameFile(l.info, current) {
		return fmt.Errorf("repository configuration file was replaced since the command started")
	}
	return nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	l.info = nil
	l.path = ""
	unlockErr := unlockFile(file)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("release target history lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close target history lock: %w", closeErr)
	}
	return nil
}

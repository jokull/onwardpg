package targetlock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireSerializesOneTarget(t *testing.T) {
	config := testConfig(t)
	first, err := Acquire(config, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(config, "primary"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire error = %v", err)
	}
	if _, err := Acquire(config, "analytics"); !errors.Is(err, ErrBusy) {
		t.Fatalf("repository config lock did not serialize another target: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := Acquire(config, "analytics")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRejectsUnsafeTargetName(t *testing.T) {
	if _, err := Acquire(testConfig(t), "../outside"); err == nil {
		t.Fatal("unsafe target lock name was accepted")
	}
}

func TestOldInodeReleaseCannotReleaseReplacementLock(t *testing.T) {
	config := testConfig(t)
	stale, err := Acquire(config, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stale.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := stale.ValidatePath(); err == nil {
		t.Fatal("atomically replaced configuration retained the old lock identity")
	}
	replacement, err := Acquire(config, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if err := stale.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(config, "primary"); !errors.Is(err, ErrBusy) {
		t.Fatalf("replacement lock was removed: %v", err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigPathAliasUsesSameLock(t *testing.T) {
	config := testConfig(t)
	aliasRoot := t.TempDir()
	alias := filepath.Join(aliasRoot, "config.toml")
	if err := os.Symlink(config, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	lock, err := Acquire(config, "primary")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if _, err := Acquire(alias, "primary"); !errors.Is(err, ErrBusy) {
		t.Fatalf("config alias acquired a separate lock: %v", err)
	}
}

func testConfig(t *testing.T) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), ".onwardpg.toml")
	if err := os.WriteFile(name, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

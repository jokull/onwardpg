//go:build windows

package bundle

// Windows does not expose a portable directory fsync through os.File.Sync.
// Individual bundle files are still flushed before the atomic rename.
func syncDirectory(string) error { return nil }

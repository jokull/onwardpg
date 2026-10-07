//go:build darwin || linux

package workspace

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

type exporterProcessGroup struct{ command *exec.Cmd }

func newExporterProcessGroup() (*exporterProcessGroup, error) {
	return &exporterProcessGroup{}, nil
}

func (g *exporterProcessGroup) configure(command *exec.Cmd) {
	g.command = command
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func (g *exporterProcessGroup) attach(_ *exec.Cmd) error { return nil }

func (g *exporterProcessGroup) stop(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	if command.Process.Pid == 0 {
		return command.Process.Kill()
	}
	if err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = command.Process.Kill()
		return fmt.Errorf("kill DDL exporter process group: %w", err)
	}
	return nil
}

func (g *exporterProcessGroup) close() {
	if g.command != nil && g.command.Process != nil {
		_ = syscall.Kill(-g.command.Process.Pid, syscall.SIGKILL)
	}
}

func openRegularCompilerFile(name string) (*os.File, error) {
	if err := checkRegularCompilerPath(name); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := checkRegularCompilerFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// openListedRegularFile opens a checkout file that a directory listing
// reported as regular, with no path lookup before the open. O_NOFOLLOW stops
// the open at a symbolic link that replaced the file after the listing, and
// O_NONBLOCK stops it from waiting on a FIFO. The open descriptor is then
// inspected, and only a regular file is returned. ok is false when the caller
// must inspect the path itself.
func openListedRegularFile(name string) (file *os.File, info os.FileInfo, ok bool) {
	fd, err := syscall.Open(name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, false
	}
	file = os.NewFile(uintptr(fd), name)
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, false
	}
	return file, info, true
}

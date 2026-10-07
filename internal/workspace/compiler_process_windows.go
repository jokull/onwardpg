//go:build windows

package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type exporterProcessGroup struct{ job windows.Handle }

func newExporterProcessGroup() (*exporterProcessGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return &exporterProcessGroup{job: job}, nil
}

func (g *exporterProcessGroup) configure(command *exec.Cmd) {
	// The child cannot start its own children before job assignment.
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
}

func (g *exporterProcessGroup) attach(command *exec.Cmd) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(g.job, process); err != nil {
		return fmt.Errorf("assign process to Windows job: %w", err)
	}
	if err := resumeExporterProcess(uint32(command.Process.Pid)); err != nil {
		return fmt.Errorf("resume Windows exporter: %w", err)
	}
	return nil
}

// Go's exec.Cmd exposes the process handle but not CreateProcess's primary
// thread handle. A newly suspended process has only its primary thread, so
// enumerate that thread after assignment and resume it. Every opened handle
// is closed before returning.
func resumeExporterProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		if entry.OwnerProcessID == pid {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err != nil {
				return err
			}
			previous, resumeErr := windows.ResumeThread(thread)
			closeErr := windows.CloseHandle(thread)
			if resumeErr != nil {
				return resumeErr
			}
			if closeErr != nil {
				return closeErr
			}
			if previous != 1 {
				return fmt.Errorf("unexpected primary thread suspend count %d", previous)
			}
			return nil
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			return fmt.Errorf("find suspended exporter thread: %w", err)
		}
	}
}

func (g *exporterProcessGroup) stop(command *exec.Cmd) error {
	_ = windows.TerminateJobObject(g.job, 1)
	_ = command.Process.Kill()
	return nil
}

func (g *exporterProcessGroup) close() { _ = windows.CloseHandle(g.job) }

func openRegularCompilerFile(name string) (*os.File, error) {
	if err := checkRegularCompilerPath(name); err != nil {
		return nil, err
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	if err := checkRegularCompilerFile(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// openListedRegularFile has no Windows form: os.Open cannot refuse to follow
// a link. The caller inspects every path itself.
func openListedRegularFile(string) (*os.File, os.FileInfo, bool) { return nil, nil, false }

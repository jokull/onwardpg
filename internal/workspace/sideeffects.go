package workspace

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jokull/onwardpg/internal/timing"
)

// SideEffectObserver collects the paths of a git work tree whose status
// changed while a schema export command ran. It is an observation for the
// person or agent that reads the result. No result of a command depends on
// it, and nothing that onwardpg stores contains it.
type SideEffectObserver struct {
	mutex sync.Mutex
	paths map[string]struct{}
}

type sideEffectObserverKey struct{}

// ObserveExportSideEffects returns a context under which StartExport and
// CompileDDL watch the git work tree around their export runs, and the
// observer that receives what they see. Without it they do not run git.
func ObserveExportSideEffects(ctx context.Context) (context.Context, *SideEffectObserver) {
	observer := &SideEffectObserver{}
	return context.WithValue(ctx, sideEffectObserverKey{}, observer), observer
}

// Take returns the observed paths in sorted order and forgets them. The
// paths are relative to the top level of the git work tree.
func (o *SideEffectObserver) Take() []string {
	if o == nil {
		return nil
	}
	o.mutex.Lock()
	defer o.mutex.Unlock()
	paths := make([]string, 0, len(o.paths))
	for path := range o.paths {
		paths = append(paths, path)
	}
	o.paths = nil
	sort.Strings(paths)
	return paths
}

func (o *SideEffectObserver) add(paths []string) {
	if len(paths) == 0 {
		return
	}
	o.mutex.Lock()
	defer o.mutex.Unlock()
	if o.paths == nil {
		o.paths = make(map[string]struct{}, len(paths))
	}
	for _, path := range paths {
		o.paths[path] = struct{}{}
	}
}

// sideEffectWatch is one observation: the git status from before the first
// export run of a command, to compare with the status after the last run.
type sideEffectWatch struct {
	observer *SideEffectObserver
	root     string
	before   map[string]string
}

// watchExportSideEffects reads the git status of the work tree that contains
// root, before the first run of schema_command.
//
// onwardpg does not require the export command to leave the checkout
// unchanged. The integrity of a result rests on the export output: two runs
// must give the same bytes, and the result is then proved by replay on
// PostgreSQL (docs/safety-model.md). A command that also writes files does
// not make that result wrong, but it is usually an accident, so the command
// reports it as a warning.
//
// The observation is best effort. It returns nil, and nothing is observed,
// when the caller did not ask for it, when the target has no schema_command,
// when root is not in a git work tree, when there is no git executable, and
// when git fails or does not answer in time.
func watchExportSideEffects(ctx context.Context, root string, target Target) *sideEffectWatch {
	observer, _ := ctx.Value(sideEffectObserverKey{}).(*SideEffectObserver)
	if observer == nil || len(target.SchemaCommand) == 0 {
		return nil
	}
	before, ok := gitStatus(ctx, root)
	if !ok {
		return nil
	}
	return &sideEffectWatch{observer: observer, root: root, before: before}
}

// finish reads the git status again and gives the observer every path whose
// status line differs. Call it after the last export run and before the
// command writes its own files.
func (w *sideEffectWatch) finish(ctx context.Context) {
	if w == nil {
		return
	}
	after, ok := gitStatus(ctx, w.root)
	if !ok {
		return
	}
	var changed []string
	for path, line := range w.before {
		if after[path] != line {
			changed = append(changed, path)
		}
	}
	for path := range after {
		if _, known := w.before[path]; !known {
			changed = append(changed, path)
		}
	}
	w.observer.add(changed)
}

// A status that takes longer than this is abandoned. The observation must
// not make a command slow.
const gitStatusTimeout = 10 * time.Second

// gitStatus returns one status line for each path that git reports as
// changed or untracked. Files that git ignores are not reported. The command
// takes no optional lock, so it does not refresh or write the index, and it
// does not disturb another git command in the same work tree.
func gitStatus(ctx context.Context, root string) (map[string]string, bool) {
	defer timing.Start("export_side_effect_status")()
	statusCtx, cancel := context.WithTimeout(ctx, gitStatusTimeout)
	defer cancel()
	command := exec.CommandContext(statusCtx, "git", "-C", root, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	command.WaitDelay = time.Second
	output, err := command.Output()
	if err != nil {
		return nil, false
	}
	return parseGitStatus(string(output)), true
}

// parseGitStatus reads `git status --porcelain=v2 -z` output into a map from
// path to status record. A record carries the state of the index and of HEAD
// and the kind of change in the work tree. It does not carry the content of
// the file in the work tree, so a second write to a file that was already
// modified or untracked leaves its record unchanged.
func parseGitStatus(output string) map[string]string {
	entries := make(map[string]string)
	records := strings.Split(output, "\x00")
	for index := 0; index < len(records); index++ {
		record := records[index]
		if record == "" {
			continue
		}
		var path string
		switch record[0] {
		case '1':
			path = statusField(record, 8)
		case '2':
			// A rename or copy has a second field: the original path.
			path = statusField(record, 9)
			if index+1 < len(records) {
				index++
				record += "\x00" + records[index]
			}
		case 'u':
			path = statusField(record, 10)
		case '?':
			path = strings.TrimPrefix(record, "? ")
		}
		if path != "" {
			entries[path] = record
		}
	}
	return entries
}

// statusField returns the text after the first n space-separated fields of a
// status record: the path, which can itself contain spaces.
func statusField(record string, n int) string {
	fields := strings.SplitN(record, " ", n+1)
	if len(fields) <= n {
		return ""
	}
	return fields[n]
}

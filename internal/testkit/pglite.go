package testkit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	PinnedPGliteVersion       = "0.4.5"
	PinnedPGliteSocketVersion = "0.1.5"

	pgliteLoopbackHost = "127.0.0.1"
)

var pgliteToolInstall struct {
	sync.Mutex
	ready bool
}

type synchronizedBuffer struct {
	sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.buffer.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.Lock()
	defer b.Unlock()
	return b.buffer.String()
}

// PGlite is one isolated, in-memory PGlite socket process. It exists only for
// scoped preflight tests; native PostgreSQL remains the acceptance authority.
type PGlite struct {
	URL          string
	Capabilities CapabilityReport

	command *exec.Cmd
	logs    synchronizedBuffer
	done    chan struct{}
	waitMu  sync.Mutex
	waitErr error
	close   sync.Once
}

type pgliteReady struct {
	Kind          string `json:"kind"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	PGliteVersion string `json:"pglite_version"`
	SocketVersion string `json:"socket_version"`
}

// StartPGlite installs the pinned private Node tool, binds it to an
// OS-assigned loopback port, proves pgx wire compatibility, and runs the
// capability probe before returning.
func StartPGlite(ctx context.Context) (*PGlite, error) {
	if err := preparePGliteTool(ctx); err != nil {
		return nil, err
	}

	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("locate Node.js for PGlite preflight: %w", err)
	}

	toolDir := pgliteToolDir()
	process := &PGlite{done: make(chan struct{})}
	process.command = exec.Command(node, "server.mjs")
	process.command.Dir = toolDir
	process.command.Env = append(os.Environ(), "NODE_NO_WARNINGS=1")
	stdout, err := process.command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("capture PGlite socket stdout: %w", err)
	}
	process.command.Stderr = &process.logs

	if err := process.command.Start(); err != nil {
		return nil, fmt.Errorf("start PGlite socket process: %w", err)
	}
	go func() {
		err := process.command.Wait()
		process.waitMu.Lock()
		process.waitErr = err
		process.waitMu.Unlock()
		close(process.done)
	}()

	readyLines := make(chan string)
	scanErrors := make(chan error, 1)
	go func() {
		defer close(readyLines)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			_, _ = process.logs.Write([]byte(line + "\n"))
			readyLines <- line
		}
		scanErrors <- scanner.Err()
	}()

	startupTimer := time.NewTimer(30 * time.Second)
	defer startupTimer.Stop()
	var ready pgliteReady
waitForReady:
	for {
		select {
		case <-ctx.Done():
			_ = process.Close()
			return nil, fmt.Errorf("start PGlite: %w", ctx.Err())
		case <-startupTimer.C:
			_ = process.Close()
			return nil, fmt.Errorf("PGlite did not become ready within 30s; logs:\n%s", process.Logs())
		case <-process.done:
			return nil, fmt.Errorf("PGlite exited before readiness: %v; logs:\n%s", process.processError(), process.Logs())
		case line, ok := <-readyLines:
			if !ok {
				scanErr := <-scanErrors
				_ = process.Close()
				return nil, fmt.Errorf("PGlite readiness stream closed: %v; logs:\n%s", scanErr, process.Logs())
			}
			var candidate pgliteReady
			if json.Unmarshal([]byte(line), &candidate) == nil && candidate.Kind == "ready" {
				ready = candidate
				break waitForReady
			}
		}
	}

	if ready.Host != pgliteLoopbackHost || ready.Port < 1 || ready.Port > 65535 {
		_ = process.Close()
		return nil, fmt.Errorf("PGlite reported unsafe listener %q:%d", ready.Host, ready.Port)
	}
	if ready.PGliteVersion != PinnedPGliteVersion || ready.SocketVersion != PinnedPGliteSocketVersion {
		_ = process.Close()
		return nil, fmt.Errorf(
			"PGlite tool version drift: got pglite=%q socket=%q, want pglite=%q socket=%q",
			ready.PGliteVersion, ready.SocketVersion, PinnedPGliteVersion, PinnedPGliteSocketVersion,
		)
	}

	process.URL = "postgres://postgres:postgres@" + pgliteLoopbackHost + ":" + strconv.Itoa(ready.Port) + "/postgres?sslmode=disable"
	probeContext, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	report, err := ProbePGliteCapabilities(probeContext, process.URL)
	if err != nil {
		_ = process.Close()
		return nil, fmt.Errorf("PGlite startup capability probe: %w; logs:\n%s", err, process.Logs())
	}
	process.Capabilities = report
	return process, nil
}

// PGliteConnConfig returns pgx's proven PGlite-only wire mode. Native
// PostgreSQL tests must use their normal pgx configuration instead.
func PGliteConnConfig(databaseURL string) (*pgx.ConnConfig, error) {
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PGlite URL: %w", err)
	}
	if config.Host != pgliteLoopbackHost {
		return nil, fmt.Errorf("PGlite host must be loopback %s, got %q", pgliteLoopbackHost, config.Host)
	}
	config.ConnectTimeout = 5 * time.Second
	config.StatementCacheCapacity = 0
	config.DescriptionCacheCapacity = 0
	config.DefaultQueryExecMode = pgx.QueryExecModeExec
	config.RuntimeParams["application_name"] = "onwardpg_pglite_preflight"
	return config, nil
}

func (p *PGlite) Connect(ctx context.Context) (*pgx.Conn, error) {
	config, err := PGliteConnConfig(p.URL)
	if err != nil {
		return nil, err
	}
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect pgx to PGlite: %w", err)
	}
	return connection, nil
}

func (p *PGlite) Logs() string {
	return strings.TrimSpace(p.logs.String())
}

func (p *PGlite) processError() error {
	p.waitMu.Lock()
	defer p.waitMu.Unlock()
	return p.waitErr
}

// Close gracefully stops the socket and database, then escalates to killing
// only this child process if it does not exit within the bound.
func (p *PGlite) Close() error {
	var closeErr error
	p.close.Do(func() {
		select {
		case <-p.done:
			closeErr = p.processError()
			return
		default:
		}

		if err := p.command.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			closeErr = fmt.Errorf("signal PGlite process: %w", err)
		}
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-p.done:
			if err := p.processError(); err != nil && closeErr == nil {
				closeErr = fmt.Errorf("stop PGlite process: %w", err)
			}
		case <-timer.C:
			if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && closeErr == nil {
				closeErr = fmt.Errorf("kill unresponsive PGlite process: %w", err)
			}
			<-p.done
		}
	})
	return closeErr
}

func preparePGliteTool(ctx context.Context) error {
	pgliteToolInstall.Lock()
	defer pgliteToolInstall.Unlock()
	if pgliteToolInstall.ready {
		return nil
	}

	npm, err := exec.LookPath("npm")
	if err != nil {
		return fmt.Errorf("locate npm for pinned PGlite tool: %w", err)
	}
	installContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(installContext, npm, "ci", "--ignore-scripts", "--omit=peer", "--no-audit", "--no-fund")
	command.Dir = pgliteToolDir()
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("install pinned PGlite tool: %w\n%s", err, strings.TrimSpace(string(output)))
	}

	for relative, want := range map[string]string{
		"node_modules/@electric-sql/pglite/package.json":        PinnedPGliteVersion,
		"node_modules/@electric-sql/pglite-socket/package.json": PinnedPGliteSocketVersion,
	} {
		contents, err := os.ReadFile(filepath.Join(pgliteToolDir(), relative))
		if err != nil {
			return fmt.Errorf("read installed PGlite manifest %s: %w", relative, err)
		}
		var manifest struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(contents, &manifest); err != nil {
			return fmt.Errorf("parse installed PGlite manifest %s: %w", relative, err)
		}
		if manifest.Version != want {
			return fmt.Errorf("installed PGlite dependency drift at %s: got %q, want %q", relative, manifest.Version, want)
		}
	}

	pgliteToolInstall.ready = true
	return nil
}

func pgliteToolDir() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		panic("locate internal/testkit PGlite helper")
	}
	return filepath.Join(filepath.Dir(filename), "pglite-tool")
}

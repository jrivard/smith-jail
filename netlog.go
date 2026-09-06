// Copyright 2026 Jason D. Rivard <code@jrivard.org>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

// tailPollInterval bounds how quickly a --follow tail notices new events.
// Event volume is low enough (one line per DNS query/TCP connection) that
// polling is simpler than a filesystem-notify dependency and plenty fast
// for a human watching a terminal.
const tailPollInterval = 300 * time.Millisecond

// netLogPath returns the durable, per-project event log path: every jailed
// session for this agent+directory appends to the same file, so history
// survives across runs and is findable from the directory alone — no
// session ID needed. It's created under $XDG_DATA_HOME (default
// ~/.local/share), mirroring config.go's own $XDG_CONFIG_HOME handling.
func netLogPath(agent *Agent, dir string) (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dataHome = filepath.Join(home, ".local", "share")
	}

	logDir := filepath.Join(dataHome, "smith-jail", "netlog")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", err
	}

	name := agent.Name + "-" + projectHash(dir) + ".jsonl"
	return filepath.Join(logDir, name), nil
}

// ensureFileExists creates path (and nothing else) if it doesn't already
// exist, so it's safe to bind-mount before the proxy has written to it. The
// proxy sidecar appends to this bind-mounted file as UID 5353, not the host
// user running smith-jail, so it must be world-writable — 0644 (or a
// pre-existing file left over from before this fix) leaves it unwritable to
// the container, and the proxy silently falls back to stdout-only logging,
// which is invisible to netlog/netview since they read straight from disk.
func ensureFileExists(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Chmod(0o666)
}

// tailEvents reads every event already in path, decoding each line as
// proxyproto.Event; with follow, it keeps polling for lines appended after
// it reaches the end instead of returning, until ctx is cancelled. The
// events channel is closed when tailing stops (EOF reached and !follow, an
// error occurred, or ctx was cancelled); at most one error is ever sent to
// the error channel.
func tailEvents(ctx context.Context, path string, follow bool) (<-chan proxyproto.Event, <-chan error) {
	events := make(chan proxyproto.Event)
	errCh := make(chan error, 1)

	go func() {
		defer close(events)

		f, err := os.Open(path)
		if err != nil {
			errCh <- err
			return
		}
		defer f.Close()

		reader := bufio.NewReader(f)
		var partial strings.Builder

		for {
			chunk, err := reader.ReadString('\n')
			if err != nil {
				partial.WriteString(chunk) // keep any bytes read before hitting EOF
				if !errors.Is(err, io.EOF) {
					errCh <- err
					return
				}
				if !follow {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(tailPollInterval):
				}
				continue
			}

			line := strings.TrimSpace(partial.String() + chunk)
			partial.Reset()
			if line == "" {
				continue
			}

			var e proxyproto.Event
			if err := json.Unmarshal([]byte(line), &e); err != nil {
				continue // a malformed line shouldn't abort the whole tail
			}

			select {
			case events <- e:
			case <-ctx.Done():
				return
			}
		}
	}()

	return events, errCh
}

// formatEvent renders one Event as a single colorized terminal line, using
// the same palette printOK/printWarn/printErr use elsewhere (ui.go).
func formatEvent(e proxyproto.Event) string {
	color := colorYellow
	switch e.Action {
	case "allowed":
		color = colorGreen
	case "blocked":
		color = colorRed
	}

	target := e.Host
	if e.DestIP != "" {
		ipPart := e.DestIP
		if e.DestPort != 0 {
			ipPart = fmt.Sprintf("%s:%d", e.DestIP, e.DestPort)
		}
		if target != "" {
			target = fmt.Sprintf("%s (%s)", target, ipPart)
		} else {
			target = ipPart
		}
	}
	if target == "" {
		target = "-"
	}

	line := fmt.Sprintf("%s%s%s  %s%-4s %-8s%s  %s",
		colorDim, e.Time.Local().Format("15:04:05"), colorReset,
		color, e.Kind, e.Action, colorReset,
		target)
	if e.Error != "" {
		line += fmt.Sprintf("  %s(%s)%s", colorDim, e.Error, colorReset)
	}
	return line
}

// resolveDirOnly resolves and validates a project directory without
// touching config or Docker — netlog/netview need to work even after the
// session (and the proxy container) are long gone.
func resolveDirOnly(rawDir string) string {
	dir, err := filepath.Abs(rawDir)
	if err != nil {
		die("Cannot resolve path: " + err.Error())
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		die("Directory does not exist: " + dir)
	}
	return dir
}

// parseNetLogFlags parses the flags for the netlog command. The directory
// is optional and defaults to cwd, same as setup's parseSetupFlags.
func parseNetLogFlags(args []string, cmd string) (follow, blockedOnly bool, rawDir string) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	f := fs.Bool("follow", false, "Keep tailing new events (like tail -f)")
	b := fs.Bool("blocked-only", false, "Show only blocked/failed events")
	fs.Usage = func() { fmt.Print(helpText) }
	_ = fs.Parse(args)

	rawDir = "."
	if fs.NArg() > 0 {
		rawDir = fs.Arg(0)
	}
	return *f, *b, rawDir
}

// cmdNetLog prints a project's persisted network activity history — the
// same events the proxy sidecar logged during every --network-jail session
// for this agent+directory, whether or not any session is currently
// running.
func cmdNetLog(agent *Agent, rawDir string, follow, blockedOnly bool) {
	dir := resolveDirOnly(rawDir)

	path, err := netLogPath(agent, dir)
	if err != nil {
		die("Resolving network log path: " + err.Error())
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		printInfo("No network activity recorded yet for this project — run a session with --network-jail first.")
		return
	}

	if FindRunningContainer(agent, projectHash(dir)) == "" {
		printInfo("No active session for this project — showing recorded history only.")
	}

	ctx := context.Background()
	if follow {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
		}()
	}

	events, errCh := tailEvents(ctx, path, follow)
	for e := range events {
		if blockedOnly && !e.Blocked() {
			continue
		}
		fmt.Println(formatEvent(e))
	}

	select {
	case err := <-errCh:
		if err != nil {
			die("Reading network log: " + err.Error())
		}
	default:
	}
}

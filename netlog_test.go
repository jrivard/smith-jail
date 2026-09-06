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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

func TestNetLogPathIsStableAndKeyedByAgentAndDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	dir := t.TempDir()
	a, err := netLogPath(AgentClaude, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := netLogPath(AgentClaude, dir)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("netLogPath is not stable: %q != %q", a, b)
	}

	other, err := netLogPath(AgentGemini, dir)
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Errorf("different agents produced the same log path: %q", a)
	}

	otherDir, err := netLogPath(AgentClaude, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if otherDir == a {
		t.Errorf("different projects produced the same log path: %q", a)
	}
}

func TestNetLogPathCreatesDataDir(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	path, err := netLogPath(AgentClaude, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("expected netLogPath to create its directory: %v", err)
	}
}

func TestEnsureFileExistsIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")

	if err := ensureFileExists(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file was not created: %v", err)
	}

	// Write something, then call again — must not truncate.
	if err := os.WriteFile(path, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureFileExists(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing\n" {
		t.Errorf("ensureFileExists truncated an existing file: %q", data)
	}

	// Must be world-writable even for a file that pre-dates this check (e.g.
	// left over from before this fix) — the proxy sidecar appends to it as
	// UID 5353, not the host user, and a merely host-owner-writable file
	// leaves it silently falling back to stdout-only logging.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o002 == 0 {
		t.Errorf("ensureFileExists left %s not world-writable: %v", path, info.Mode().Perm())
	}
}

func writeEvent(t *testing.T, f *os.File, e proxyproto.Event) {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestTailEventsReadsExistingLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeEvent(t, f, proxyproto.Event{Kind: "dns", Action: "allowed", Host: "api.anthropic.com"})
	writeEvent(t, f, proxyproto.Event{Kind: "tcp", Action: "blocked", DestIP: "1.2.3.4", DestPort: 443})
	f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events, errCh := tailEvents(ctx, path, false)

	var got []proxyproto.Event
	for e := range events {
		got = append(got, e)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Host != "api.anthropic.com" || got[0].Action != "allowed" {
		t.Errorf("event[0] = %+v, unexpected", got[0])
	}
	if got[1].DestIP != "1.2.3.4" || got[1].Action != "blocked" {
		t.Errorf("event[1] = %+v, unexpected", got[1])
	}
}

func TestTailEventsMissingFile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	events, errCh := tailEvents(ctx, filepath.Join(t.TempDir(), "nope.jsonl"), false)

	for range events {
		t.Fatal("expected no events for a missing file")
	}
	if err := <-errCh; err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// This is the whole point of --follow / netview: events appended by a
// concurrent writer (the proxy sidecar, in reality) must show up without
// re-running the command.
func TestTailEventsFollowsAppendedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writeEvent(t, f, proxyproto.Event{Kind: "dns", Action: "allowed", Host: "first.example.com"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, errCh := tailEvents(ctx, path, true)

	first := <-events
	if first.Host != "first.example.com" {
		t.Fatalf("first event = %+v, unexpected", first)
	}

	writeEvent(t, f, proxyproto.Event{Kind: "dns", Action: "blocked", Host: "second.example.com"})
	f.Close()

	select {
	case second := <-events:
		if second.Host != "second.example.com" {
			t.Fatalf("second event = %+v, unexpected", second)
		}
	case err := <-errCh:
		t.Fatalf("tailEvents errored while following: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for the appended event")
	}
}

func TestFormatEventContainsHostAndDest(t *testing.T) {
	e := proxyproto.Event{
		Time: time.Now(), Kind: "tcp", Action: "allowed",
		Host: "api.anthropic.com", DestIP: "160.79.104.10", DestPort: 443,
	}
	out := formatEvent(e)
	for _, want := range []string{"api.anthropic.com", "160.79.104.10:443", "allowed"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatEvent output missing %q:\n%s", want, out)
		}
	}
}

func TestEventBlockedPredicate(t *testing.T) {
	cases := []struct {
		action string
		want   bool
	}{
		{"allowed", false},
		{"blocked", true},
		{"error", true},
		{"dial-error", true},
		{"upstream-error", true},
	}
	for _, c := range cases {
		e := proxyproto.Event{Action: c.action}
		if got := e.Blocked(); got != c.want {
			t.Errorf("Event{Action: %q}.Blocked() = %v, want %v", c.action, got, c.want)
		}
	}
}

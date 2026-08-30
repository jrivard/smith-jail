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
	"os"
	"path/filepath"
	"testing"
)

func TestAnyFailed(t *testing.T) {
	cases := []struct {
		name   string
		checks []DoctorCheck
		want   bool
	}{
		{"empty", nil, false},
		{"all ok", []DoctorCheck{{Status: statusOK}, {Status: statusOK}}, false},
		{"warn only", []DoctorCheck{{Status: statusOK}, {Status: statusWarn}}, false},
		{"one failure", []DoctorCheck{{Status: statusOK}, {Status: statusFail}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AnyFailed(c.checks); got != c.want {
				t.Errorf("AnyFailed(%v) = %v, want %v", c.checks, got, c.want)
			}
		})
	}
}

func TestCheckDirWritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	c := checkDirWritable("Test dir", dir)
	if c.Status != statusOK {
		t.Fatalf("expected OK for a fresh writable dir, got %v: %s", c.Status, c.Detail)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("checkDirWritable should have created %s: %v", dir, err)
	}
	// The write probe must not be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no leftover files in %s, found %v", dir, entries)
	}
}

func TestCheckDirWritableUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)

	c := checkDirWritable("Test dir", dir)
	if c.Status != statusFail {
		t.Fatalf("expected Fail for a read-only dir, got %v: %s", c.Status, c.Detail)
	}
}

func TestCheckCredentialDirMissing(t *testing.T) {
	cfg := &Config{UID: os.Getuid(), GID: os.Getgid()}
	path := filepath.Join(t.TempDir(), "does-not-exist")

	c := checkCredentialDir("Test creds", path, cfg)
	if c.Status != statusOK {
		t.Fatalf("expected OK for a not-yet-created credential dir, got %v: %s", c.Status, c.Detail)
	}
}

func TestCheckCredentialDirOwnedByCurrentUser(t *testing.T) {
	cfg := &Config{UID: os.Getuid(), GID: os.Getgid()}
	dir := t.TempDir()

	c := checkCredentialDir("Test creds", dir, cfg)
	if c.Status != statusOK {
		t.Fatalf("expected OK when the dir is owned by the configured UID/GID, got %v: %s", c.Status, c.Detail)
	}
}

func TestCheckCredentialDirWrongOwner(t *testing.T) {
	// Simulate a mismatch without needing root to chown anything: pretend the
	// container runs as a UID that can't be this process's own.
	cfg := &Config{UID: os.Getuid() + 12345, GID: os.Getgid()}
	dir := t.TempDir()

	c := checkCredentialDir("Test creds", dir, cfg)
	if c.Status != statusWarn {
		t.Fatalf("expected Warn on UID mismatch, got %v: %s", c.Status, c.Detail)
	}
	if c.Fix == "" {
		t.Error("expected a suggested fix for mismatched ownership")
	}
}

func TestCheckDiskSpaceReportsFreeBytes(t *testing.T) {
	c := checkDiskSpace(t.TempDir())
	if c.Status == statusFail {
		t.Fatalf("checkDiskSpace should never hard-fail, got: %s", c.Detail)
	}
	if c.Detail == "" {
		t.Error("expected a non-empty detail describing free space")
	}
}

func TestRunDoctorChecksNilConfig(t *testing.T) {
	// Doctor should degrade gracefully rather than panic when config loading
	// itself failed.
	checks := RunDoctorChecks(nil)
	if len(checks) == 0 {
		t.Fatal("expected at least the Docker-only checks to run with a nil config")
	}
	for _, c := range checks {
		if c.Name == "" {
			t.Error("every check should have a name")
		}
	}
}

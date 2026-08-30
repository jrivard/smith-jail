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
	"strings"
	"testing"
)

func TestSetEnvValuesActivatesCommentedTemplateLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smith-jail.env")
	if err := os.WriteFile(path, []byte(envTemplateJail), 0600); err != nil {
		t.Fatal(err)
	}

	err := SetEnvValues(path, map[string]string{
		"JAIL_INIT_PACKAGES": "curl jq golang",
		"JAIL_MEM_LIMIT":     "16g",
		"JAIL_RUN_AS_ROOT":   "true",
	})
	if err != nil {
		t.Fatal(err)
	}

	env, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"JAIL_INIT_PACKAGES": "curl jq golang",
		"JAIL_MEM_LIMIT":     "16g",
		"JAIL_RUN_AS_ROOT":   "true",
	} {
		if got := env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// The value must land on the template's own line rather than being
	// appended, so it stays next to the comment documenting it.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "# The image rebuilds automatically when this list changes.\nJAIL_INIT_PACKAGES=") {
		t.Error("JAIL_INIT_PACKAGES was not activated in place under its documentation")
	}

	// Prose containing "=" must survive untouched.
	if !strings.Contains(text, "sudo setcap cap_net_admin+ep $(which smith-jail)") {
		t.Error("prose comment was corrupted")
	}
}

func TestSetEnvValuesReplacesActiveAssignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smith-jail.env")
	original := "# leading comment\nJAIL_MEM_LIMIT=4g\n\n# JAIL_MEM_LIMIT=99g\nJAIL_CPU_LIMIT=1.0\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SetEnvValues(path, map[string]string{"JAIL_MEM_LIMIT": "32g"}); err != nil {
		t.Fatal(err)
	}

	body, _ := os.ReadFile(path)
	text := string(body)

	if !strings.Contains(text, "JAIL_MEM_LIMIT=32g\n") {
		t.Errorf("active assignment not replaced:\n%s", text)
	}
	// The commented duplicate must be left alone; only one active line may win.
	if !strings.Contains(text, "# JAIL_MEM_LIMIT=99g") {
		t.Errorf("commented duplicate should be untouched:\n%s", text)
	}
	if strings.Count(text, "\nJAIL_MEM_LIMIT=") != 1 {
		t.Errorf("expected exactly one active assignment:\n%s", text)
	}
}

func TestSetEnvValuesAppendsUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smith-jail.env")
	if err := os.WriteFile(path, []byte("JAIL_CPU_LIMIT=2.0\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SetEnvValues(path, map[string]string{"JAIL_BRAND_NEW": "yes"}); err != nil {
		t.Fatal(err)
	}

	env, _ := parseEnvFile(path)
	if env["JAIL_BRAND_NEW"] != "yes" {
		t.Errorf("appended key not readable back: %#v", env)
	}
	if env["JAIL_CPU_LIMIT"] != "2.0" {
		t.Errorf("existing key lost: %#v", env)
	}
}

func TestSetEnvValuesCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "smith-jail.env")

	if err := SetEnvValues(path, map[string]string{"JAIL_SUDO": "true"}); err != nil {
		t.Fatal(err)
	}

	env, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if env["JAIL_SUDO"] != "true" {
		t.Errorf("value not written: %#v", env)
	}
}

// Values are round-tripped through the same parser the CLI uses, so quoting has
// to survive spaces, empties, and comment characters.
func TestQuoteEnvValueRoundTrips(t *testing.T) {
	values := []string{
		"curl gcc build-essential make",
		"debian:trixie-slim",
		"",
		"8g",
		"github.com registry.npmjs.org",
		"weird#hash",
	}

	path := filepath.Join(t.TempDir(), "smith-jail.env")
	set := map[string]string{}
	for i, v := range values {
		set["JAIL_TEST_"+string(rune('A'+i))] = v
	}
	if err := SetEnvValues(path, set); err != nil {
		t.Fatal(err)
	}

	env, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range set {
		if got := env[k]; got != want {
			t.Errorf("%s round-tripped as %q, want %q", k, got, want)
		}
	}
}

// A value containing both " and ' has no representation stripQuotes can
// round-trip (it has no escaping mechanism at all), so SetEnvValues must
// error rather than silently drop characters on write-back.
func TestSetEnvValuesErrorsOnUnquotableValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smith-jail.env")

	err := SetEnvValues(path, map[string]string{
		"JAIL_INIT_PACKAGES": `it's a "test" package`,
	})
	if err == nil {
		t.Fatal("expected an error for a value containing both \" and ', got nil")
	}

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("file should not have been created on error, stat err: %v", statErr)
	}
}

func TestSetEnvValuesPreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smith-jail.env")
	if err := os.WriteFile(path, []byte("JAIL_SUDO=false\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SetEnvValues(path, map[string]string{"JAIL_SUDO": "true"}); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0600 {
		t.Errorf("mode = %v, want 0600 — credentials files must not widen", perm)
	}
}

func TestUnsetEnvValuesRemovesActiveAssignment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project.env")
	original := "JAIL_BASE_IMAGE=golang:1.22\nJAIL_MEM_LIMIT=16g\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	if err := UnsetEnvValues(path, "JAIL_MEM_LIMIT"); err != nil {
		t.Fatal(err)
	}

	env, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := env["JAIL_MEM_LIMIT"]; ok {
		t.Errorf("JAIL_MEM_LIMIT should have been removed: %#v", env)
	}
	if env["JAIL_BASE_IMAGE"] != "golang:1.22" {
		t.Errorf("unrelated key lost: %#v", env)
	}
}

// Unrelated prose and other keys' commented documentation must survive a
// clear untouched — only the cleared key's own line goes away.
func TestUnsetEnvValuesLeavesUnrelatedContentIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "smith-jail.env")
	if err := os.WriteFile(path, []byte(envTemplateJail), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SetEnvValues(path, map[string]string{"JAIL_MEM_LIMIT": "16g"}); err != nil {
		t.Fatal(err)
	}

	if err := UnsetEnvValues(path, "JAIL_MEM_LIMIT"); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "JAIL_MEM_LIMIT=") {
		t.Errorf("JAIL_MEM_LIMIT line should be gone entirely:\n%s", text)
	}
	if !strings.Contains(text, "# JAIL_CPU_LIMIT=2.0") {
		t.Errorf("neighboring commented documentation should be left alone:\n%s", text)
	}
	if !strings.Contains(text, "sudo setcap cap_net_admin+ep $(which smith-jail)") {
		t.Error("prose comment was corrupted")
	}
}

func TestUnsetEnvValuesNoopOnMissingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project.env")
	original := "JAIL_BASE_IMAGE=golang:1.22\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	if err := UnsetEnvValues(path, "JAIL_DOES_NOT_EXIST"); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != original {
		t.Errorf("file should be unchanged, got:\n%s", body)
	}
}

func TestUnsetEnvValuesOnMissingFileIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.env")

	if err := UnsetEnvValues(path, "JAIL_MEM_LIMIT"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("UnsetEnvValues should not create a file that never existed")
	}
}

func TestSplitPackagesDeduplicates(t *testing.T) {
	got := splitPackages("  curl gcc   curl make gcc ")
	want := []string{"curl", "gcc", "make"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v (order must be preserved)", got, want)
		}
	}
}

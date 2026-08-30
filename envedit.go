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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// envKeyPattern is deliberately strict: it matches the shouty-snake-case keys
// smith-jail uses and nothing else. Prose inside the config templates routinely
// contains "=" ("cap_net_admin+ep $(which smith-jail)"), and only a strict key
// shape reliably tells a commented-out assignment apart from a sentence.
var envKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// SetEnvValues writes key/value pairs into an env file while preserving its
// comments, ordering, and documentation. For each key:
//
//   - an existing active assignment is replaced in place;
//   - otherwise a commented-out template line for that key is activated in
//     place, so the value stays next to the comment that explains it;
//   - otherwise the assignment is appended at the end.
//
// The write is atomic — the file is never left half-rewritten if the process
// dies partway through.
func SetEnvValues(path string, values map[string]string) error {
	lines, mode, err := readEnvLines(path)
	if err != nil {
		return err
	}

	pending := make(map[string]string, len(values))
	for k, v := range values {
		pending[k] = v
	}

	// Active assignments win: if the user already set a key explicitly, that
	// is the line their eye will go to.
	for i, line := range lines {
		if key, ok := envKeyOfLine(line, false); ok {
			if v, want := pending[key]; want {
				quoted, err := quoteEnvValue(v)
				if err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				lines[i] = key + "=" + quoted
				delete(pending, key)
			}
		}
	}

	// Then activate commented template lines, first occurrence only.
	for i, line := range lines {
		if key, ok := envKeyOfLine(line, true); ok {
			if v, want := pending[key]; want {
				quoted, err := quoteEnvValue(v)
				if err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				lines[i] = key + "=" + quoted
				delete(pending, key)
			}
		}
	}

	if len(pending) > 0 {
		keys := make([]string, 0, len(pending))
		for k := range pending {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) != "" {
			lines = append(lines, "")
		}
		for _, k := range keys {
			quoted, err := quoteEnvValue(pending[k])
			if err != nil {
				return fmt.Errorf("%s: %w", k, err)
			}
			lines = append(lines, k+"="+quoted)
		}
	}

	return writeEnvLines(path, lines, mode)
}

// UnsetEnvValues removes the active assignment line for each key, returning
// that key to whatever the next layer down provides. Unlike SetEnvValues,
// commented-out template lines are left untouched — there's nothing useful
// to restore them to in a project file, which carries no documentation of
// its own. A missing key or file is not an error.
func UnsetEnvValues(path string, keys ...string) error {
	lines, mode, err := readEnvLines(path)
	if err != nil {
		return err
	}
	if lines == nil {
		// readEnvLines can't distinguish "empty file" from "no file" — check
		// directly so clearing a never-set override doesn't conjure an empty
		// file into existence.
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			return nil
		}
	}

	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}

	out := lines[:0]
	for _, line := range lines {
		if key, ok := envKeyOfLine(line, false); ok && want[key] {
			continue
		}
		out = append(out, line)
	}

	return writeEnvLines(path, out, mode)
}

// readEnvLines returns the file's lines and mode. A missing file is not an
// error: it yields an empty document that SetEnvValues will populate.
func readEnvLines(path string) ([]string, os.FileMode, error) {
	mode := os.FileMode(0600)

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, mode, nil
	}
	if err != nil {
		return nil, mode, err
	}
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, mode, nil
	}
	return strings.Split(text, "\n"), mode, nil
}

// writeEnvLines replaces path atomically via a temp file in the same directory.
func writeEnvLines(path string, lines []string, mode os.FileMode) error {
	body := strings.Join(lines, "\n") + "\n"

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".smith-jail-env-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// envKeyOfLine extracts the key from an assignment. When commented is true it
// looks only at commented-out lines, otherwise only at active ones.
func envKeyOfLine(line string, commented bool) (string, bool) {
	s := strings.TrimSpace(line)

	if commented {
		if !strings.HasPrefix(s, "#") {
			return "", false
		}
		s = strings.TrimSpace(strings.TrimLeft(s, "#"))
	} else if s == "" || strings.HasPrefix(s, "#") {
		return "", false
	}

	key, _, found := strings.Cut(s, "=")
	if !found {
		return "", false
	}
	key = strings.TrimSpace(key)
	if !envKeyPattern.MatchString(key) {
		return "", false
	}
	return key, true
}

// quoteEnvValue quotes a value when it would otherwise be ambiguous. It mirrors
// what parseEnvFile's stripQuotes accepts on the way back in.
//
// stripQuotes has no escaping mechanism at all — it just strips a matching
// leading/trailing quote character. That means a value containing both " and
// ' has no representation this format can round-trip, so we error rather than
// silently drop characters.
func quoteEnvValue(v string) (string, error) {
	if v == "" {
		return `""`, nil
	}
	if !strings.ContainsAny(v, " \t\"'#") {
		return v, nil
	}
	hasDouble := strings.Contains(v, `"`)
	hasSingle := strings.Contains(v, `'`)
	if hasDouble && hasSingle {
		return "", fmt.Errorf("value contains both \" and ' and cannot be safely quoted: %q", v)
	}
	if hasDouble {
		return "'" + v + "'", nil
	}
	return `"` + v + `"`, nil
}

// splitPackages parses a space-separated package list into a deduplicated
// slice, preserving the order the user wrote them in.
func splitPackages(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Fields(s) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

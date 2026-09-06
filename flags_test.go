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
	"reflect"
	"testing"
)

func TestParseRunFlags_OrderIndependent(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantJail    bool
		wantDir     string
		wantExtra   []string
		wantAllowed []string
	}{
		{
			name:     "flag before dir",
			args:     []string{"--network-jail", "/proj"},
			wantJail: true,
			wantDir:  "/proj",
		},
		{
			name:     "flag after dir",
			args:     []string{"/proj", "--network-jail"},
			wantJail: true,
			wantDir:  "/proj",
		},
		{
			name:      "flag after dir and extra args",
			args:      []string{"/proj", "--network-jail", "--resume"},
			wantJail:  true,
			wantDir:   "/proj",
			wantExtra: []string{"--resume"},
		},
		{
			name:        "value flag after dir",
			args:        []string{"/proj", "--allow", "example.com"},
			wantDir:     "/proj",
			wantAllowed: []string{"example.com"},
		},
		{
			name:      "unknown flag after dir still passes through",
			args:      []string{"/proj", "--some-agent-flag"},
			wantDir:   "/proj",
			wantExtra: []string{"--some-agent-flag"},
		},
		{
			name:      "escape hatch after dir passes literal flag through",
			args:      []string{"/proj", "--", "--network-jail"},
			wantDir:   "/proj",
			wantExtra: []string{"--", "--network-jail"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, dir, extra := parseRunFlags(tc.args, "claude run")
			if opts.NetworkJail != tc.wantJail {
				t.Errorf("NetworkJail = %v, want %v", opts.NetworkJail, tc.wantJail)
			}
			if dir != tc.wantDir {
				t.Errorf("dir = %q, want %q", dir, tc.wantDir)
			}
			if !reflect.DeepEqual(extra, tc.wantExtra) && !(len(extra) == 0 && len(tc.wantExtra) == 0) {
				t.Errorf("extraArgs = %v, want %v", extra, tc.wantExtra)
			}
			if !reflect.DeepEqual(opts.ExtraAllowHosts, tc.wantAllowed) && !(len(opts.ExtraAllowHosts) == 0 && len(tc.wantAllowed) == 0) {
				t.Errorf("ExtraAllowHosts = %v, want %v", opts.ExtraAllowHosts, tc.wantAllowed)
			}
		})
	}
}

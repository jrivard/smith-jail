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

//go:build !linux

package main

import (
	"fmt"
	"net"
	"runtime"
)

// getOriginalDstPlatform has no non-Linux implementation — SO_ORIGINAL_DST
// is a Linux netfilter-specific socket option — so this always fails. This
// binary is only ever built into a Linux container image; this stub exists
// solely so `go build`/`go vet` succeed when smith-jail itself is
// cross-compiled for another platform (see relay.go's doc comment).
func getOriginalDstPlatform(conn *net.TCPConn) (*net.TCPAddr, error) {
	return nil, fmt.Errorf("SO_ORIGINAL_DST is not supported on %s", runtime.GOOS)
}

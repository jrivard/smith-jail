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

import "fmt"

// Version is smith-jail's release version. Overridden at build time via
// -ldflags "-X main.Version=..." by the release workflow; defaults to this
// value for plain `go build`.
var Version = "0.1.1"

const copyrightLine = "Copyright 2026 Jason D. Rivard <code@jrivard.org>"

// cmdVersion prints the version and copyright/license banner.
func cmdVersion() {
	fmt.Printf("smith-jail %s\n", Version)
	fmt.Println(copyrightLine)
	fmt.Println("Licensed under the Apache License, Version 2.0")
}

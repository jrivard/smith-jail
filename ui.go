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
)

const (
	colorReset  = "\033[0m"
	colorRed    = "\033[0;31m"
	colorGreen  = "\033[0;32m"
	colorYellow = "\033[1;33m"
	colorCyan   = "\033[0;36m"
	colorBlue   = "\033[0;34m"
	colorBold   = "\033[1m"
	colorDim    = "\033[2m"
)

func printInfo(msg string) {
	fmt.Printf("%s▸%s %s\n", colorCyan, colorReset, msg)
}

func printOK(msg string) {
	fmt.Printf("%s✔%s %s\n", colorGreen, colorReset, msg)
}

func printWarn(msg string) {
	fmt.Printf("%s⚠%s  %s\n", colorYellow, colorReset, msg)
}

func printErr(msg string) {
	fmt.Fprintf(os.Stderr, "%s✖ ERROR:%s %s\n", colorRed, colorReset, msg)
}

func die(msg string) {
	printErr(msg)
	os.Exit(1)
}

func printHeader(title, color string) {
	fmt.Printf("%s%s=== %s ===%s\n", colorBold, color, title, colorReset)
}

// clearScreen wipes the terminal and homes the cursor, so whatever printed
// before a hand-off to an interactive session (a shell, an agent) doesn't
// scroll away the explanation of what's about to happen.
func clearScreen() {
	fmt.Print("\033[H\033[2J")
}

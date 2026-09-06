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
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

// Event is this package's local name for the shared event shape — see
// proxyproto.Event. It's a type alias (not a new type) so relay.go/dns.go's
// Event{...} literals need no changes, while netlog/netview on the host
// side decode the exact same JSON shape this package writes.
type Event = proxyproto.Event

// Logger writes newline-delimited JSON Events, safe for concurrent use by
// the relay's per-connection goroutines and the DNS server's handler.
type Logger struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func NewLogger(w io.Writer) *Logger {
	return &Logger{enc: json.NewEncoder(w)}
}

func (l *Logger) Event(e Event) {
	e.Time = time.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(e) // best-effort: a logging failure must never break the relay
}

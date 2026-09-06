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
	"errors"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// getOriginalDst recovers the destination a connection was actually dialed
// to, before the netsetup helper's nft REDIRECT rule rewrote it to this
// process's own listening port. This is the mechanism the whole relay
// depends on: the kernel — not SNI-sniffing, not a fake-IP DNS table — is
// the source of truth for "what did the container's process really try to
// reach", for any TCP traffic on any port, regardless of how the
// destination was discovered (DNS, a hardcoded IP, a cached address).
//
// It only succeeds for connections that actually passed through a
// REDIRECT/DNAT rule and have a conntrack entry to recover — a direct,
// non-redirected connection to this port has nothing to recover and
// getsockopt fails, which callers should treat as "not a connection this
// relay should be handling" rather than panic-worthy.
func getOriginalDst(conn *net.TCPConn) (*net.TCPAddr, error) {
	sc, err := conn.SyscallConn()
	if err != nil {
		return nil, err
	}

	var raw unix.RawSockaddrInet4
	size := uint32(unsafe.Sizeof(raw))
	var sockErr error

	ctrlErr := sc.Control(func(fd uintptr) {
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(unix.SOL_IP),
			uintptr(unix.SO_ORIGINAL_DST),
			uintptr(unsafe.Pointer(&raw)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			sockErr = errno
		}
	})
	if ctrlErr != nil {
		return nil, ctrlErr
	}
	if sockErr != nil {
		return nil, sockErr
	}

	return parseOriginalDst(raw), nil
}

// parseOriginalDst converts the raw sockaddr_in the kernel filled in into a
// *net.TCPAddr. Split out from getOriginalDst so the one genuinely fiddly
// part — the kernel writes Port in network byte order, but Go's struct
// field reads it back as a native-endian uint16 on this (little-endian)
// architecture, so it comes out byte-swapped — is covered by a unit test
// without needing an actual redirected connection to exercise it.
func parseOriginalDst(raw unix.RawSockaddrInet4) *net.TCPAddr {
	port := (raw.Port >> 8) | (raw.Port << 8) // ntohs
	ip := net.IPv4(raw.Addr[0], raw.Addr[1], raw.Addr[2], raw.Addr[3])
	return &net.TCPAddr{IP: ip, Port: int(port)}
}

// Relay accepts redirected TCP connections, recovers their true destination,
// checks it against Policy, and — if allowed — dials out and splices the two
// sockets together. TLS/plaintext HTTP payloads are never inspected or
// terminated; only the destination the kernel reports is examined.
type Relay struct {
	policy      *Policy
	dialTimeout time.Duration
	logger      *Logger
}

func NewRelay(policy *Policy, logger *Logger) *Relay {
	return &Relay{policy: policy, dialTimeout: 10 * time.Second, logger: logger}
}

// Serve accepts connections from ln until it's closed.
func (r *Relay) Serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		tc, ok := c.(*net.TCPConn)
		if !ok {
			c.Close()
			continue
		}
		go r.handle(tc)
	}
}

func (r *Relay) handle(conn *net.TCPConn) {
	defer conn.Close()

	dst, err := getOriginalDst(conn)
	if err != nil {
		r.logger.Event(Event{Kind: "tcp", Action: "error", Error: err.Error()})
		return
	}

	host, allowed := r.policy.AllowsIP(dst.IP)
	if !allowed {
		r.logger.Event(Event{Kind: "tcp", Action: "blocked", DestIP: dst.IP.String(), DestPort: dst.Port})
		return
	}

	upstream, err := net.DialTimeout("tcp", dst.String(), r.dialTimeout)
	if err != nil {
		r.logger.Event(Event{Kind: "tcp", Action: "dial-error", Host: host, DestIP: dst.IP.String(), DestPort: dst.Port, Error: err.Error()})
		return
	}
	defer upstream.Close()

	r.logger.Event(Event{Kind: "tcp", Action: "allowed", Host: host, DestIP: dst.IP.String(), DestPort: dst.Port})
	splice(conn, upstream)
}

// splice copies bytes in both directions until either side is done, without
// inspecting or altering them — the relay never terminates TLS.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
}

// closeWrite half-closes so the other direction can still drain in-flight
// data instead of getting cut off the instant one side finishes.
func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

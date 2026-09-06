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
	"bytes"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// parseOriginalDst is the one part of SO_ORIGINAL_DST handling that doesn't
// need an actual redirected kernel connection to verify: the port the
// kernel writes into RawSockaddrInet4.Port is network-byte-order, but Go
// reads the struct field back as native-endian, so on this (little-endian)
// architecture it comes out byte-swapped unless corrected.
func TestParseOriginalDstFixesPortByteOrder(t *testing.T) {
	raw := unix.RawSockaddrInet4{
		Port: 0xBB01, // the kernel writes port 443 (0x01BB) as network-order
		// bytes {0x01, 0xBB}; Go's native-endian (little-endian)
		// read of those same two bytes reports 0xBB01
		Addr: [4]byte{93, 184, 216, 34},
	}

	addr := parseOriginalDst(raw)

	if addr.Port != 443 {
		t.Errorf("Port = %d, want 443", addr.Port)
	}
	if !addr.IP.Equal(net.IPv4(93, 184, 216, 34)) {
		t.Errorf("IP = %v, want 93.184.216.34", addr.IP)
	}
}

func TestParseOriginalDstPort80(t *testing.T) {
	raw := unix.RawSockaddrInet4{
		Port: 0x5000, // network-order bytes for port 80 (0x00, 0x50), read
		// back native-endian as 0x5000
		Addr: [4]byte{1, 2, 3, 4},
	}

	addr := parseOriginalDst(raw)
	if addr.Port != 80 {
		t.Errorf("Port = %d, want 80", addr.Port)
	}
}

// getOriginalDst must fail gracefully — not panic — on a connection that
// was never actually redirected via nft/conntrack, since that's exactly
// what happens if this relay is ever reached directly instead of through
// the netsetup helper's REDIRECT rule.
func TestGetOriginalDstFailsGracefullyWithoutRedirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := getOriginalDst(c.(*net.TCPConn)); err == nil {
			t.Error("expected an error recovering SO_ORIGINAL_DST on a non-redirected connection")
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-done
}

func TestSpliceCopiesBothDirections(t *testing.T) {
	aServer, aClient := net.Pipe()
	bServer, bClient := net.Pipe()

	go splice(aServer, bServer)

	go func() {
		aClient.Write([]byte("hello"))
		aClient.Close()
	}()

	buf := make([]byte, 5)
	if _, err := bClient.Read(buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, []byte("hello")) {
		t.Errorf("got %q, want %q", buf, "hello")
	}
}

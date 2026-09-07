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
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// getOriginalDstPlatform is the real SO_ORIGINAL_DST recovery — see
// getOriginalDst's doc comment in relay.go for the mechanism and why it can
// fail on an unredirected connection.
func getOriginalDstPlatform(conn *net.TCPConn) (*net.TCPAddr, error) {
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

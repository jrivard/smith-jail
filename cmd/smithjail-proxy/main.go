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

// smithjail-proxy is the per-session sidecar smith-jail's network jail
// starts. It owns the session's network namespace: the netsetup helper
// installs nft rules (inside that namespace, not the host's) that redirect
// the namespace's outbound :80/:443/:53 traffic here, and this process
// decides — by consulting the shared Policy — whether to relay each
// connection/query or refuse it. See network.go (host side) and this
// package's policy.go/relay.go/dns.go for the mechanism.
package main

import (
	"io"
	"log"
	"net"
	"os"

	"github.com/miekg/dns"

	"github.com/jrivard/smith-jail/internal/proxyproto"
)

func main() {
	policyPath := envOr(proxyproto.PolicyFileEnv, proxyproto.PolicyFilePath)
	upstreamDNS := envOr(proxyproto.UpstreamDNSEnv, proxyproto.UpstreamDNSDefault)

	policy, err := LoadPolicy(policyPath)
	if err != nil {
		log.Fatalf("smithjail-proxy: loading policy %s: %v", policyPath, err)
	}

	logger := NewLogger(eventLogWriter())
	relay := NewRelay(policy, logger)

	httpLn, err := net.Listen("tcp", "127.0.0.1:"+proxyproto.HTTPPort)
	if err != nil {
		log.Fatalf("smithjail-proxy: listen :%s: %v", proxyproto.HTTPPort, err)
	}
	tlsLn, err := net.Listen("tcp", "127.0.0.1:"+proxyproto.TLSPort)
	if err != nil {
		log.Fatalf("smithjail-proxy: listen :%s: %v", proxyproto.TLSPort, err)
	}

	forwarder := NewDNSForwarder(policy, upstreamDNS, logger)
	mux := dns.NewServeMux()
	mux.HandleFunc(".", forwarder.ServeDNS)

	// Bind synchronously here (rather than inside ListenAndServe, which
	// would race with the readyMarker print below) so readiness is
	// guaranteed, not polled-for.
	udpConn, err := net.ListenPacket("udp", "127.0.0.1:"+proxyproto.DNSPort)
	if err != nil {
		log.Fatalf("smithjail-proxy: listen udp :%s: %v", proxyproto.DNSPort, err)
	}
	dnsTCPLn, err := net.Listen("tcp", "127.0.0.1:"+proxyproto.DNSPort)
	if err != nil {
		log.Fatalf("smithjail-proxy: listen tcp :%s: %v", proxyproto.DNSPort, err)
	}
	udpSrv := &dns.Server{PacketConn: udpConn, Handler: mux}
	tcpSrv := &dns.Server{Listener: dnsTCPLn, Handler: mux}

	go relay.Serve(httpLn)
	go relay.Serve(tlsLn)
	go func() {
		if err := udpSrv.ActivateAndServe(); err != nil {
			log.Fatalf("smithjail-proxy: dns udp server: %v", err)
		}
	}()
	go func() {
		if err := tcpSrv.ActivateAndServe(); err != nil {
			log.Fatalf("smithjail-proxy: dns tcp server: %v", err)
		}
	}()

	log.Println(proxyproto.ReadyMarker)
	select {} // container lifecycle is managed by the host; nothing to exit for
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// eventLogWriter tees the activity log to a bind-mounted host file when the
// host asked for one (SMITHJAIL_EVENT_LOG), in addition to stdout — stdout
// alone disappears with the container at session end, which is exactly what
// makes the mounted file worth having. A file that can't be opened is
// logged and skipped rather than fatal: losing the persisted history is
// never worth taking the relay down over.
func eventLogWriter() io.Writer {
	path := os.Getenv(proxyproto.EventLogEnv)
	if path == "" {
		return os.Stdout
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("smithjail-proxy: could not open event log %s, continuing with stdout only: %v", path, err)
		return os.Stdout
	}
	return io.MultiWriter(os.Stdout, f)
}

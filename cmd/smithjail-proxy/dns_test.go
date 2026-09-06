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
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeResponseWriter captures the message ServeDNS writes back, without
// needing a real UDP/TCP socket.
type fakeResponseWriter struct {
	dns.ResponseWriter
	written *dns.Msg
}

func (f *fakeResponseWriter) WriteMsg(m *dns.Msg) error {
	f.written = m
	return nil
}

func newQuery(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	return m
}

func TestServeDNSBlocksDisallowedHost(t *testing.T) {
	policy, err := NewPolicy([]string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := NewDNSForwarder(policy, "unused:53", NewLogger(discard{}))
	f.exchange = func(req *dns.Msg, upstream string) (*dns.Msg, time.Duration, error) {
		t.Fatal("exchange must not be called for a disallowed host")
		return nil, 0, nil
	}

	w := &fakeResponseWriter{}
	f.ServeDNS(w, newQuery("evil.example.com"))

	if w.written == nil {
		t.Fatal("no response written")
	}
	if w.written.Rcode != dns.RcodeNameError {
		t.Errorf("Rcode = %v, want NXDOMAIN", w.written.Rcode)
	}
}

func TestServeDNSForwardsAllowedHostAndLearnsIP(t *testing.T) {
	policy, err := NewPolicy([]string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := NewDNSForwarder(policy, "unused:53", NewLogger(discard{}))

	resolvedIP := net.ParseIP("160.79.104.10")
	f.exchange = func(req *dns.Msg, upstream string) (*dns.Msg, time.Duration, error) {
		resp := new(dns.Msg)
		resp.SetReply(req)
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET},
			A:   resolvedIP,
		})
		return resp, 0, nil
	}

	w := &fakeResponseWriter{}
	f.ServeDNS(w, newQuery("api.anthropic.com"))

	if w.written == nil {
		t.Fatal("no response written")
	}
	if w.written.Rcode != dns.RcodeSuccess {
		t.Errorf("Rcode = %v, want success", w.written.Rcode)
	}

	if _, ok := policy.AllowsIP(resolvedIP); !ok {
		t.Error("the resolved IP should have been learned into the policy")
	}
}

func TestServeDNSUpstreamErrorFailsClosed(t *testing.T) {
	policy, err := NewPolicy([]string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := NewDNSForwarder(policy, "unused:53", NewLogger(discard{}))
	f.exchange = func(req *dns.Msg, upstream string) (*dns.Msg, time.Duration, error) {
		return nil, 0, errors.New("network unreachable")
	}

	w := &fakeResponseWriter{}
	f.ServeDNS(w, newQuery("api.anthropic.com"))

	// dns.HandleFailed replies SERVFAIL; the key property is that an
	// upstream failure never falls back to silently allowing the query.
	if w.written != nil && w.written.Rcode == dns.RcodeSuccess {
		t.Error("an upstream error must never be reported as a successful answer")
	}
}

// discard is an io.Writer that throws everything away, so tests can build a
// Logger without caring where its output goes.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

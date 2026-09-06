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
	"time"

	"github.com/miekg/dns"
)

// DNSForwarder answers every query the container issues: allowed names are
// forwarded to a real upstream resolver and answered truthfully (their
// resolved IPs are learned into Policy for the relay's independent check);
// anything not on the allow-list gets a clean NXDOMAIN rather than a
// silently hanging or reset connection. This replaces smith-jail's old
// approach of resolving the allow-list once, statically, at jail startup.
type DNSForwarder struct {
	policy   *Policy
	upstream string
	logger   *Logger

	// exchange is overridable in tests so ServeDNS's policy logic can be
	// exercised without a real upstream resolver on the network.
	exchange func(req *dns.Msg, upstream string) (*dns.Msg, time.Duration, error)
}

func NewDNSForwarder(policy *Policy, upstream string, logger *Logger) *DNSForwarder {
	client := &dns.Client{Timeout: 5 * time.Second}
	return &DNSForwarder{
		policy:   policy,
		upstream: upstream,
		logger:   logger,
		exchange: client.Exchange,
	}
}

func (f *DNSForwarder) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) != 1 {
		writeFailure(w, req)
		return
	}
	name := req.Question[0].Name

	if !f.policy.AllowsHost(name) {
		f.logger.Event(Event{Kind: "dns", Action: "blocked", Host: name})
		m := new(dns.Msg)
		m.SetRcode(req, dns.RcodeNameError)
		_ = w.WriteMsg(m)
		return
	}

	resp, _, err := f.exchange(req, f.upstream)
	if err != nil || resp == nil {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		}
		f.logger.Event(Event{Kind: "dns", Action: "upstream-error", Host: name, Error: errMsg})
		writeFailure(w, req)
		return
	}

	for _, rr := range resp.Answer {
		var ip net.IP
		switch a := rr.(type) {
		case *dns.A:
			ip = a.A
		case *dns.AAAA:
			ip = a.AAAA
		default:
			continue
		}
		f.policy.Learn(name, ip)
	}

	f.logger.Event(Event{Kind: "dns", Action: "allowed", Host: name})
	_ = w.WriteMsg(resp)
}

// writeFailure replies SERVFAIL. Used instead of the (deprecated)
// dns.HandleFailed so a malformed query or upstream failure never falls
// back to silently answering — it always produces an explicit failure the
// caller sees as a resolution error, not a hang or a false allow.
func writeFailure(w dns.ResponseWriter, req *dns.Msg) {
	m := new(dns.Msg)
	m.SetRcode(req, dns.RcodeServerFailure)
	_ = w.WriteMsg(m)
}

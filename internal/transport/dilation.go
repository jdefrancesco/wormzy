package transport

import (
	"errors"
	"net"
	"strings"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	metaPrefix = "META:"
	chunkSize  = 1 << 16
)

func buildCandidates(self rendezvous.SelfInfo, loopback bool, relayAddr string) []rendezvous.Candidate {
	var out []rendezvous.Candidate
	seen := make(map[string]bool)
	add := func(typ, proto, addr string, prio int) {
		if addr == "" {
			return
		}
		key := proto + "|" + addr
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, rendezvous.Candidate{
			Type:     typ,
			Proto:    proto,
			Addr:     addr,
			Priority: prio,
		})
	}

	if loopback && self.Local != "" {
		add("loopback", "udp", self.Local, 120)
		return out
	}

	add("reflexive", "udp", self.Public, 100)
	add("local", "udp", self.Local, 60)
	add("relay", "udp", relayAddr, 40)
	return out
}

func selectPeerCandidate(self, peer rendezvous.SelfInfo, loopback bool) (rendezvous.Candidate, *rendezvous.Candidate, error) {
	if loopback && peer.Local != "" {
		return rendezvous.Candidate{
			Type:     "loopback",
			Proto:    "udp",
			Addr:     peer.Local,
			Priority: 120,
		}, nil, nil
	}

	preferLocal := loopback || samePublicIP(self.Public, peer.Public)

	var (
		best      *rendezvous.Candidate
		bestLocal *rendezvous.Candidate
		relayCand *rendezvous.Candidate
	)
	for _, cand := range peer.Candidates {
		if cand.Proto != "udp" {
			continue
		}
		cand := cand
		if strings.Contains(strings.ToLower(cand.Type), "relay") {
			if relayCand == nil {
				relayCand = &cand
			}
			// Never pick relay as best unless no other options.
			continue
		}
		if cand.Type == "local" && preferLocal {
			if bestLocal == nil || cand.Priority > bestLocal.Priority {
				bestLocal = &cand
			}
		}
		if best == nil || cand.Priority > best.Priority {
			best = &cand
		}
	}
	if preferLocal && bestLocal != nil {
		return *bestLocal, relayCand, nil
	}
	if best != nil {
		return *best, relayCand, nil
	}
	if relayCand != nil {
		return *relayCand, relayCand, nil
	}
	if peer.Public != "" && !preferLocal {
		return rendezvous.Candidate{
			Type:     "legacy-public",
			Proto:    "udp",
			Addr:     peer.Public,
			Priority: 10,
		}, relayCand, nil
	}
	if peer.Local != "" {
		return rendezvous.Candidate{
			Type:     "legacy-local",
			Proto:    "udp",
			Addr:     peer.Local,
			Priority: 5,
		}, relayCand, nil
	}
	return rendezvous.Candidate{}, relayCand, errors.New("peer did not advertise any UDP candidates")
}

func samePublicIP(a, b string) bool {
	ha := hostPart(a)
	hb := hostPart(b)
	return ha != "" && ha == hb
}

func hostPart(addr string) string {
	if addr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

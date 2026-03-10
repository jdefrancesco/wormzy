package transport

import (
	"errors"

	"github.com/jdefrancesco/internal/rendezvous"
)

const (
	metaPrefix = "META:"
	chunkSize  = 1 << 16
)

func buildCandidates(self rendezvous.SelfInfo, loopback bool) []rendezvous.Candidate {
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
	return out
}

func selectPeerCandidate(peer rendezvous.SelfInfo, loopback bool) (rendezvous.Candidate, error) {
	if loopback && peer.Local != "" {
		return rendezvous.Candidate{
			Type:     "loopback",
			Proto:    "udp",
			Addr:     peer.Local,
			Priority: 120,
		}, nil
	}

	var (
		best rendezvous.Candidate
		ok   bool
	)
	for _, cand := range peer.Candidates {
		if cand.Proto != "udp" {
			continue
		}
		if !ok || cand.Priority > best.Priority {
			best = cand
			ok = true
		}
	}
	if ok {
		return best, nil
	}
	if peer.Public != "" {
		return rendezvous.Candidate{
			Type:     "legacy-public",
			Proto:    "udp",
			Addr:     peer.Public,
			Priority: 10,
		}, nil
	}
	if peer.Local != "" {
		return rendezvous.Candidate{
			Type:     "legacy-local",
			Proto:    "udp",
			Addr:     peer.Local,
			Priority: 5,
		}, nil
	}
	return rendezvous.Candidate{}, errors.New("peer did not advertise any UDP candidates")
}

package transport

import (
	"errors"
	"net"
	"sort"
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

func selectPeerCandidates(self, peer rendezvous.SelfInfo, loopback bool) ([]rendezvous.Candidate, *rendezvous.Candidate, error) {
	if loopback && peer.Local != "" {
		return []rendezvous.Candidate{{
			Type:     "loopback",
			Proto:    "udp",
			Addr:     peer.Local,
			Priority: 120,
		}}, nil, nil
	}

	preferLocal := loopback || samePublicIP(self.Public, peer.Public)

	var (
		relayCand *rendezvous.Candidate
		direct    []rendezvous.Candidate
	)
	seen := make(map[string]bool)
	addDirect := func(cand rendezvous.Candidate) {
		if cand.Proto != "udp" || cand.Addr == "" {
			return
		}
		key := cand.Proto + "|" + cand.Addr
		if seen[key] {
			return
		}
		seen[key] = true
		direct = append(direct, cand)
	}
	for _, cand := range peer.Candidates {
		if cand.Proto != "udp" {
			continue
		}
		cand := cand
		if strings.Contains(strings.ToLower(cand.Type), "relay") {
			if relayCand == nil {
				relayCand = &cand
			}
			continue
		}
		addDirect(cand)
	}
	if !preferLocal && peer.Public != "" {
		addDirect(rendezvous.Candidate{
			Type:     "legacy-public",
			Proto:    "udp",
			Addr:     peer.Public,
			Priority: 10,
		})
	}
	if peer.Local != "" {
		addDirect(rendezvous.Candidate{
			Type:     "legacy-local",
			Proto:    "udp",
			Addr:     peer.Local,
			Priority: 5,
		})
	}

	sort.SliceStable(direct, func(i, j int) bool {
		li := candidateRaceWeight(direct[i], preferLocal)
		lj := candidateRaceWeight(direct[j], preferLocal)
		if li == lj {
			return direct[i].Priority > direct[j].Priority
		}
		return li > lj
	})

	if len(direct) > 0 {
		return direct, relayCand, nil
	}
	if relayCand != nil {
		return nil, relayCand, nil
	}
	return nil, relayCand, errors.New("peer did not advertise any UDP candidates")
}

func candidateRaceWeight(cand rendezvous.Candidate, preferLocal bool) int {
	score := cand.Priority
	switch strings.ToLower(cand.Type) {
	case "local":
		if preferLocal {
			score += 1000
		}
	case "reflexive":
		if !preferLocal {
			score += 900
		}
	}
	return score
}

func classifyCandidateByRemote(remote net.Addr, candidates []rendezvous.Candidate) *rendezvous.Candidate {
	if remote == nil {
		return nil
	}
	remoteHost, remotePort, err := net.SplitHostPort(remote.String())
	if err != nil {
		return nil
	}
	for i := range candidates {
		host, port, err := net.SplitHostPort(candidates[i].Addr)
		if err != nil {
			continue
		}
		if host == remoteHost && port == remotePort {
			cand := candidates[i]
			return &cand
		}
	}
	for i := range candidates {
		host, _, err := net.SplitHostPort(candidates[i].Addr)
		if err != nil {
			continue
		}
		if host == remoteHost {
			cand := candidates[i]
			return &cand
		}
	}
	return nil
}

func pickFallbackDirectCandidate(candidates []rendezvous.Candidate) rendezvous.Candidate {
	if len(candidates) == 0 {
		return rendezvous.Candidate{
			Type:     "direct-unknown",
			Proto:    "udp",
			Priority: 0,
		}
	}
	return candidates[0]
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

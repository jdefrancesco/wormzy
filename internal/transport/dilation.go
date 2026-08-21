package transport

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	metaPrefix              = "META:"
	chunkSize               = 1 << 16
	maxPeerCandidateCount   = 16
	maxCandidateFieldLength = 256
	maxCandidatePriority    = 10000
	minCandidatePriority    = -10000
)

func buildCandidates(self rendezvous.SelfInfo, loopback bool, upnpAddr, relayAddr string) []rendezvous.Candidate {
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

	add("upnp", "udp", upnpAddr, 110)
	add("reflexive", "udp", self.Public, 100)
	add("local", "udp", self.Local, 60)
	add("relay", "udp", relayAddr, 40)
	return out
}

func selectPeerCandidates(self, peer rendezvous.SelfInfo, loopback bool) ([]rendezvous.Candidate, *rendezvous.Candidate, error) {
	if err := validatePeerCandidateMetadata(peer); err != nil {
		return nil, nil, err
	}
	if loopback && peer.Local != "" {
		local, ok := normalizedPeerCandidate(rendezvous.Candidate{Type: "loopback", Proto: "udp", Addr: peer.Local}, true, true, nil)
		if !ok {
			return nil, nil, errors.New("peer loopback candidate is invalid")
		}
		return []rendezvous.Candidate{{
			Type:     local.Type,
			Proto:    local.Proto,
			Addr:     local.Addr,
			Priority: local.Priority,
		}}, nil, nil
	}

	// Outside explicit loopback mode, Pion ICE validates same-LAN host
	// candidates with connectivity checks. The legacy race must not dial a
	// peer-supplied private address merely because the peer claims our public IP.
	preferLocal := loopback
	allowedRelays := make(map[string]struct{})
	for _, candidate := range self.Candidates {
		if strings.EqualFold(candidate.Type, "relay") && strings.EqualFold(candidate.Proto, "udp") {
			allowedRelays[candidate.Addr] = struct{}{}
		}
	}

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
		cand, ok := normalizedPeerCandidate(cand, preferLocal, loopback, allowedRelays)
		if !ok {
			continue
		}
		if cand.Type == "relay" {
			if relayCand == nil {
				relayCand = &cand
			}
			continue
		}
		addDirect(cand)
	}
	if !preferLocal && peer.Public != "" {
		candidate, ok := normalizedPeerCandidate(rendezvous.Candidate{
			Type:     "legacy-public",
			Proto:    "udp",
			Addr:     peer.Public,
			Priority: 10,
		}, preferLocal, loopback, allowedRelays)
		if ok {
			addDirect(candidate)
		}
	}
	if preferLocal && peer.Local != "" {
		candidate, ok := normalizedPeerCandidate(rendezvous.Candidate{
			Type:     "legacy-local",
			Proto:    "udp",
			Addr:     peer.Local,
			Priority: 5,
		}, preferLocal, loopback, allowedRelays)
		if ok {
			addDirect(candidate)
		}
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

// validatePeerCandidateMetadata bounds and sanitizes metadata before it reaches logs or dial scheduling.
func validatePeerCandidateMetadata(peer rendezvous.SelfInfo) error {
	if len(peer.Candidates) > maxPeerCandidateCount {
		return fmt.Errorf("peer candidate count exceeds limit of %d", maxPeerCandidateCount)
	}
	for name, value := range map[string]string{"public": peer.Public, "local": peer.Local} {
		if value != "" && !safeCandidateText(value) {
			return fmt.Errorf("peer %s endpoint contains invalid text", name)
		}
	}
	for _, candidate := range peer.Candidates {
		if !safeCandidateText(candidate.Type) || !safeCandidateText(candidate.Proto) || !safeCandidateText(candidate.Addr) {
			return errors.New("peer candidate contains invalid text")
		}
		if candidate.Priority < minCandidatePriority || candidate.Priority > maxCandidatePriority {
			return errors.New("peer candidate priority is out of range")
		}
	}
	return nil
}

// normalizedPeerCandidate validates a candidate target and assigns a local priority.
func normalizedPeerCandidate(
	candidate rendezvous.Candidate,
	preferLocal bool,
	loopback bool,
	allowedRelays map[string]struct{},
) (rendezvous.Candidate, bool) {
	typ := strings.ToLower(strings.TrimSpace(candidate.Type))
	proto := strings.ToLower(strings.TrimSpace(candidate.Proto))
	if proto != "udp" || candidate.Addr == "" {
		return rendezvous.Candidate{}, false
	}
	if typ == "relay" {
		if _, ok := allowedRelays[candidate.Addr]; !ok {
			return rendezvous.Candidate{}, false
		}
		return rendezvous.Candidate{Type: "relay", Proto: "udp", Addr: candidate.Addr, Priority: 40}, true
	}
	host, portText, err := net.SplitHostPort(candidate.Addr)
	if err != nil {
		return rendezvous.Candidate{}, false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return rendezvous.Candidate{}, false
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return rendezvous.Candidate{}, false
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	switch typ {
	case "reflexive":
		if !isUsableExternalIPv4(ip) {
			return rendezvous.Candidate{}, false
		}
		return rendezvous.Candidate{Type: typ, Proto: "udp", Addr: addr, Priority: 100}, true
	case "upnp":
		if !isUsableExternalIPv4(ip) {
			return rendezvous.Candidate{}, false
		}
		return rendezvous.Candidate{Type: typ, Proto: "udp", Addr: addr, Priority: 110}, true
	case "local", "legacy-local":
		if !preferLocal || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLoopback() {
			return rendezvous.Candidate{}, false
		}
		priority := 60
		if typ == "legacy-local" {
			priority = 5
		}
		return rendezvous.Candidate{Type: typ, Proto: "udp", Addr: addr, Priority: priority}, true
	case "loopback":
		if !loopback || !ip.IsLoopback() {
			return rendezvous.Candidate{}, false
		}
		return rendezvous.Candidate{Type: typ, Proto: "udp", Addr: addr, Priority: 120}, true
	case "legacy-public":
		if !isUsableExternalIPv4(ip) {
			return rendezvous.Candidate{}, false
		}
		return rendezvous.Candidate{Type: typ, Proto: "udp", Addr: addr, Priority: 10}, true
	default:
		return rendezvous.Candidate{}, false
	}
}

// safeCandidateText rejects oversized, invalid, or terminal-control-bearing metadata.
func safeCandidateText(value string) bool {
	if value == "" || len(value) > maxCandidateFieldLength || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func candidateRaceWeight(cand rendezvous.Candidate, preferLocal bool) int {
	score := cand.Priority
	switch strings.ToLower(cand.Type) {
	case "local":
		if preferLocal {
			score += 1000
		}
	case "upnp":
		if !preferLocal {
			score += 950
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

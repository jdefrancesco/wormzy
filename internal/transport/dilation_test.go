package transport

import (
	"testing"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

func TestSelectPeerCandidatePrefersLocalWhenSamePublic(t *testing.T) {
	self := rendezvous.SelfInfo{Public: "71.1.1.1:5000"}
	peer := rendezvous.SelfInfo{
		Public: "71.1.1.1:6000",
		Local:  "192.168.10.25:7000",
		Candidates: []rendezvous.Candidate{
			{Type: "reflexive", Proto: "udp", Addr: "71.1.1.1:6000", Priority: 100},
			{Type: "local", Proto: "udp", Addr: "192.168.10.25:7000", Priority: 60},
		},
	}
	cands, relay, err := selectPeerCandidates(self, peer, false)
	if err != nil {
		t.Fatalf("selectPeerCandidates err: %v", err)
	}
	if relay != nil {
		t.Fatalf("unexpected relay candidate: %+v", relay)
	}
	if len(cands) == 0 || cands[0].Addr != peer.Local {
		t.Fatalf("expected local candidate first (%s), got %+v", peer.Local, cands)
	}
}

func TestSelectPeerCandidateReflexiveByDefault(t *testing.T) {
	self := rendezvous.SelfInfo{Public: "71.1.1.1:5000"}
	peer := rendezvous.SelfInfo{
		Public: "99.1.1.1:6000",
		Local:  "192.168.10.25:7000",
		Candidates: []rendezvous.Candidate{
			{Type: "reflexive", Proto: "udp", Addr: "99.1.1.1:6000", Priority: 100},
			{Type: "local", Proto: "udp", Addr: "192.168.10.25:7000", Priority: 60},
		},
	}
	cands, relay, err := selectPeerCandidates(self, peer, false)
	if err != nil {
		t.Fatalf("selectPeerCandidates err: %v", err)
	}
	if relay != nil {
		t.Fatalf("unexpected relay candidate: %+v", relay)
	}
	if len(cands) == 0 || cands[0].Type != "reflexive" {
		t.Fatalf("expected reflexive candidate first, got %+v", cands)
	}
}

func TestSelectPeerCandidatePrefersUPnPOverReflexiveOffLAN(t *testing.T) {
	self := rendezvous.SelfInfo{Public: "71.1.1.1:5000"}
	peer := rendezvous.SelfInfo{
		Public: "99.1.1.1:6000",
		Local:  "192.168.10.25:7000",
		Candidates: []rendezvous.Candidate{
			{Type: "reflexive", Proto: "udp", Addr: "99.1.1.1:6000", Priority: 100},
			{Type: "upnp", Proto: "udp", Addr: "99.1.1.1:6100", Priority: 110},
			{Type: "local", Proto: "udp", Addr: "192.168.10.25:7000", Priority: 60},
		},
	}
	cands, relay, err := selectPeerCandidates(self, peer, false)
	if err != nil {
		t.Fatalf("selectPeerCandidates err: %v", err)
	}
	if relay != nil {
		t.Fatalf("unexpected relay candidate: %+v", relay)
	}
	if len(cands) == 0 || cands[0].Type != "upnp" {
		t.Fatalf("expected upnp candidate first, got %+v", cands)
	}
}

func TestSelectPeerCandidateKeepsLocalFirstWhenUPnPSamePublic(t *testing.T) {
	self := rendezvous.SelfInfo{Public: "71.1.1.1:5000"}
	peer := rendezvous.SelfInfo{
		Public: "71.1.1.1:6000",
		Local:  "192.168.10.25:7000",
		Candidates: []rendezvous.Candidate{
			{Type: "upnp", Proto: "udp", Addr: "71.1.1.1:6100", Priority: 110},
			{Type: "local", Proto: "udp", Addr: "192.168.10.25:7000", Priority: 60},
		},
	}
	cands, relay, err := selectPeerCandidates(self, peer, false)
	if err != nil {
		t.Fatalf("selectPeerCandidates err: %v", err)
	}
	if relay != nil {
		t.Fatalf("unexpected relay candidate: %+v", relay)
	}
	if len(cands) == 0 || cands[0].Type != "local" {
		t.Fatalf("expected local candidate first, got %+v", cands)
	}
}

func TestSelectPeerCandidateLoopback(t *testing.T) {
	self := rendezvous.SelfInfo{}
	peer := rendezvous.SelfInfo{
		Local: "127.0.0.1:7000",
		Candidates: []rendezvous.Candidate{
			{Type: "local", Proto: "udp", Addr: "127.0.0.1:7000", Priority: 60},
		},
	}
	cands, relay, err := selectPeerCandidates(self, peer, true)
	if err != nil {
		t.Fatalf("selectPeerCandidates err: %v", err)
	}
	if relay != nil {
		t.Fatalf("unexpected relay candidate: %+v", relay)
	}
	if len(cands) == 0 || cands[0].Addr != peer.Local {
		t.Fatalf("expected loopback candidate first (%s), got %+v", peer.Local, cands)
	}
}

func TestSelectPeerCandidatePicksRelayAsLastResort(t *testing.T) {
	self := rendezvous.SelfInfo{}
	peer := rendezvous.SelfInfo{
		Candidates: []rendezvous.Candidate{
			{Type: "relay", Proto: "udp", Addr: "relay.example.com:3478", Priority: 40},
		},
	}
	cands, relay, err := selectPeerCandidates(self, peer, false)
	if err != nil {
		t.Fatalf("selectPeerCandidates err: %v", err)
	}
	if len(cands) != 0 || relay == nil || relay.Type != "relay" {
		t.Fatalf("expected relay-only fallback, got direct %+v relay %+v", cands, relay)
	}
}

func TestBuildCandidatesIncludesUPnPBeforeReflexive(t *testing.T) {
	self := rendezvous.SelfInfo{
		Public: "203.0.113.10:5000",
		Local:  "192.168.1.10:5000",
	}
	cands := buildCandidates(self, false, "203.0.113.10:6100", "relay.example.com:3478")
	if len(cands) < 2 {
		t.Fatalf("expected multiple candidates, got %+v", cands)
	}
	if cands[0].Type != "upnp" {
		t.Fatalf("expected upnp first, got %+v", cands)
	}
	if cands[1].Type != "reflexive" {
		t.Fatalf("expected reflexive second, got %+v", cands)
	}
}

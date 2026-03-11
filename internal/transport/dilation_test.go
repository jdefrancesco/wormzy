package transport

import (
	"testing"

	"github.com/jdefrancesco/internal/rendezvous"
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
	cand, err := selectPeerCandidate(self, peer, false)
	if err != nil {
		t.Fatalf("selectPeerCandidate err: %v", err)
	}
	if cand.Addr != peer.Local {
		t.Fatalf("expected local candidate %s, got %s", peer.Local, cand.Addr)
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
	cand, err := selectPeerCandidate(self, peer, false)
	if err != nil {
		t.Fatalf("selectPeerCandidate err: %v", err)
	}
	if cand.Type != "reflexive" {
		t.Fatalf("expected reflexive candidate, got %+v", cand)
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
	cand, err := selectPeerCandidate(self, peer, true)
	if err != nil {
		t.Fatalf("selectPeerCandidate err: %v", err)
	}
	if cand.Addr != peer.Local {
		t.Fatalf("expected loopback candidate %s, got %s", peer.Local, cand.Addr)
	}
}

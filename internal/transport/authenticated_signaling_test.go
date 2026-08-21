package transport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

// TestAuthenticatePeerSnapshot_Bilateral verifies both peers authenticate the exact published metadata.
func TestAuthenticatePeerSnapshot_Bilateral(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	toReceiver := make(chan mailboxMessage, 2)
	toSender := make(chan mailboxMessage, 2)
	senderMailbox := &iceTestMailbox{outbound: toReceiver, inbound: toSender}
	receiverMailbox := &iceTestMailbox{outbound: toSender, inbound: toReceiver}
	sender := authenticatedSignalingTestInfo("198.51.100.10:4000")
	receiver := authenticatedSignalingTestInfo("203.0.113.20:5000")
	psk := []byte("authenticated signaling test key")

	errorsCh := make(chan error, 2)
	go func() {
		errorsCh <- authenticatePeerSnapshot(ctx, senderMailbox, "code-1", "send", psk, sender, receiver)
	}()
	go func() {
		errorsCh <- authenticatePeerSnapshot(ctx, receiverMailbox, "code-1", "recv", psk, receiver, sender)
	}()
	for range 2 {
		if err := <-errorsCh; err != nil {
			t.Fatalf("authenticatePeerSnapshot: %v", err)
		}
	}
}

// TestSelfSnapshotAuthMessage_RejectsMetadataTampering verifies mailbox rewrites cannot steer candidates.
func TestSelfSnapshotAuthMessage_RejectsMetadataTampering(t *testing.T) {
	psk := []byte("authenticated signaling test key")
	info := authenticatedSignalingTestInfo("198.51.100.10:4000")
	message, err := newSelfSnapshotAuthMessage("code-1", "send", psk, info)
	if err != nil {
		t.Fatalf("newSelfSnapshotAuthMessage: %v", err)
	}
	if err := verifySelfSnapshotAuthMessage(message, "code-1", "send", psk, info); err != nil {
		t.Fatalf("verifySelfSnapshotAuthMessage: %v", err)
	}

	tampered := info
	tampered.Candidates = append([]rendezvous.Candidate(nil), info.Candidates...)
	tampered.Candidates[0].Addr = "192.168.1.99:22"
	if err := verifySelfSnapshotAuthMessage(message, "code-1", "send", psk, tampered); err == nil {
		t.Fatal("accepted a tampered peer metadata snapshot")
	}
}

// TestDecodeSelfSnapshotAuthMessage_RejectsMalformedInput verifies strict bounded decoding.
func TestDecodeSelfSnapshotAuthMessage_RejectsMalformedInput(t *testing.T) {
	tests := map[string]json.RawMessage{
		"unknown field": []byte(`{"role":"send","digest":"x","mac":"y","extra":true}`),
		"trailing JSON": []byte(`{"role":"send","digest":"x","mac":"y"}{}`),
		"oversized":     []byte(strings.Repeat("x", maxSelfSnapshotAuthMessageSize+1)),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeSelfSnapshotAuthMessage(raw); err == nil {
				t.Fatal("accepted malformed authenticated snapshot")
			}
		})
	}
}

// authenticatedSignalingTestInfo returns a bounded metadata snapshot for signaling tests.
func authenticatedSignalingTestInfo(public string) rendezvous.SelfInfo {
	return rendezvous.SelfInfo{
		Public: public,
		Local:  "192.168.1.20:4000",
		Candidates: []rendezvous.Candidate{{
			Type: "reflexive", Proto: "udp", Addr: public, Priority: 100,
		}},
		Features: []string{
			featureICEv1,
			featureProgressiveUPnPV1,
			featureTransferCompletionV1,
			featureAuthenticatedSignalingV1,
		},
	}
}

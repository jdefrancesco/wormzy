package transport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	featureAuthenticatedSignalingV1 = "authenticated-signaling-v1"
	selfSnapshotAuthMessageType     = "self-snapshot-auth-v1"
	selfSnapshotMACLabel            = "wormzy-self-snapshot-auth-v1"
	maxSelfSnapshotAuthMessageSize  = 4096
	maxSelfFeatureCount             = 16
	maxSelfFeatureLength            = 64
)

type selfSnapshotAuthMessage struct {
	Role   string `json:"role"`
	Digest string `json:"digest"`
	MAC    string `json:"mac"`
}

type canonicalSelfInfo struct {
	Public     string                 `json:"public"`
	Local      string                 `json:"local"`
	Candidates []rendezvous.Candidate `json:"candidates"`
	Features   []string               `json:"features"`
}

// authenticatePeerSnapshot proves that post-PAKE peer metadata matches what its owner published.
func authenticatePeerSnapshot(
	ctx context.Context,
	mbox mailbox,
	code string,
	role string,
	psk []byte,
	self rendezvous.SelfInfo,
	peer rendezvous.SelfInfo,
) error {
	if !peerSupportsFeature(peer, featureAuthenticatedSignalingV1) {
		return fmt.Errorf("peer lacks authenticated signaling (%s); update Wormzy on both devices", featureAuthenticatedSignalingV1)
	}
	localMessage, err := newSelfSnapshotAuthMessage(code, role, psk, self)
	if err != nil {
		return err
	}
	if err := mbox.Send(ctx, selfSnapshotAuthMessageType, localMessage); err != nil {
		return fmt.Errorf("send authenticated peer snapshot: %w", err)
	}
	wire, err := receiveMailboxType(ctx, mbox, selfSnapshotAuthMessageType)
	if err != nil {
		return fmt.Errorf("receive authenticated peer snapshot: %w", err)
	}
	remoteMessage, err := decodeSelfSnapshotAuthMessage(wire.Body)
	if err != nil {
		return err
	}
	if err := verifySelfSnapshotAuthMessage(remoteMessage, code, oppositeRole(role), psk, peer); err != nil {
		return err
	}
	return nil
}

// newSelfSnapshotAuthMessage creates a PAKE-keyed authenticator for one complete metadata snapshot.
func newSelfSnapshotAuthMessage(
	code string,
	role string,
	psk []byte,
	info rendezvous.SelfInfo,
) (selfSnapshotAuthMessage, error) {
	if role != "send" && role != "recv" {
		return selfSnapshotAuthMessage{}, fmt.Errorf("invalid snapshot role %q", role)
	}
	if code == "" || len(psk) == 0 {
		return selfSnapshotAuthMessage{}, errors.New("missing snapshot authentication key material")
	}
	digest, err := selfSnapshotDigest(info)
	if err != nil {
		return selfSnapshotAuthMessage{}, err
	}
	message := selfSnapshotAuthMessage{Role: role, Digest: digest}
	message.MAC = selfSnapshotMAC(code, message, psk)
	return message, nil
}

// verifySelfSnapshotAuthMessage verifies the peer role, snapshot digest, and PAKE-keyed MAC.
func verifySelfSnapshotAuthMessage(
	message selfSnapshotAuthMessage,
	code string,
	expectedRole string,
	psk []byte,
	info rendezvous.SelfInfo,
) error {
	if message.Role != expectedRole {
		return fmt.Errorf("authenticated snapshot role %q; want %q", message.Role, expectedRole)
	}
	wantDigest, err := selfSnapshotDigest(info)
	if err != nil {
		return err
	}
	gotDigest, err := hex.DecodeString(message.Digest)
	if err != nil || len(gotDigest) != sha256.Size {
		return errors.New("invalid authenticated snapshot digest")
	}
	wantDigestBytes, _ := hex.DecodeString(wantDigest)
	if !hmac.Equal(gotDigest, wantDigestBytes) {
		return errors.New("peer metadata did not match its authenticated snapshot")
	}
	wantMAC, _ := hex.DecodeString(selfSnapshotMAC(code, selfSnapshotAuthMessage{
		Role: message.Role, Digest: message.Digest,
	}, psk))
	gotMAC, err := hex.DecodeString(message.MAC)
	if err != nil || len(gotMAC) != sha256.Size || !hmac.Equal(gotMAC, wantMAC) {
		return errors.New("peer metadata snapshot authentication failed")
	}
	return nil
}

// decodeSelfSnapshotAuthMessage strictly decodes a bounded snapshot authenticator.
func decodeSelfSnapshotAuthMessage(raw json.RawMessage) (selfSnapshotAuthMessage, error) {
	if len(raw) == 0 || len(raw) > maxSelfSnapshotAuthMessageSize {
		return selfSnapshotAuthMessage{}, errors.New("invalid authenticated snapshot message size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var message selfSnapshotAuthMessage
	if err := decoder.Decode(&message); err != nil {
		return selfSnapshotAuthMessage{}, fmt.Errorf("decode authenticated snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return selfSnapshotAuthMessage{}, errors.New("authenticated snapshot contains trailing data")
	}
	return message, nil
}

// selfSnapshotDigest returns a stable digest of bounded peer metadata.
func selfSnapshotDigest(info rendezvous.SelfInfo) (string, error) {
	if err := validatePeerCandidateMetadata(info); err != nil {
		return "", err
	}
	if len(info.Features) > maxSelfFeatureCount {
		return "", fmt.Errorf("peer feature count exceeds limit of %d", maxSelfFeatureCount)
	}
	features := append([]string(nil), info.Features...)
	for _, feature := range features {
		if len(feature) == 0 || len(feature) > maxSelfFeatureLength || !safeCandidateText(feature) {
			return "", errors.New("peer feature contains invalid text")
		}
	}
	sort.Strings(features)
	candidates := append([]rendezvous.Candidate(nil), info.Candidates...)
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Type != right.Type {
			return left.Type < right.Type
		}
		if left.Proto != right.Proto {
			return left.Proto < right.Proto
		}
		if left.Addr != right.Addr {
			return left.Addr < right.Addr
		}
		return left.Priority < right.Priority
	})
	raw, err := json.Marshal(canonicalSelfInfo{
		Public: info.Public, Local: info.Local, Candidates: candidates, Features: features,
	})
	if err != nil {
		return "", fmt.Errorf("encode peer metadata snapshot: %w", err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

// selfSnapshotMAC computes the domain-separated snapshot authenticator.
func selfSnapshotMAC(code string, message selfSnapshotAuthMessage, psk []byte) string {
	mac := hmac.New(sha256.New, psk)
	for _, field := range []string{selfSnapshotMACLabel, code, message.Role, message.Digest} {
		_, _ = mac.Write([]byte(strconv.Itoa(len(field))))
		_, _ = mac.Write([]byte{':'})
		_, _ = mac.Write([]byte(field))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

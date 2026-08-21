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
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	featureProgressiveUPnPV1         = "progressive-upnp-v1"
	legacyCandidatesReadyMessageType = "legacy-candidates-ready-v1"
	upnpFallbackDelay                = 1500 * time.Millisecond
	progressiveUPnPSyncMargin        = 3 * time.Second
	maxProgressiveCandidateCount     = 16
	maxLegacyReadyMessageSize        = 4096
	legacyReadyMACLabel              = "wormzy-legacy-candidates-ready-v1"
	upnpCleanupAttempts              = 2
	upnpCleanupRetryDelay            = 100 * time.Millisecond
)

// progressiveUPnPSyncTimeout covers the delayed mapping budget plus mailbox signaling skew.
func progressiveUPnPSyncTimeout(cfg Config) time.Duration {
	return upnpFallbackDelay + upnpTimeout(cfg.HandshakeTimeout) + progressiveUPnPSyncMargin
}

type upnpFallbackStatus string

const (
	upnpFallbackMapped   upnpFallbackStatus = "mapped"
	upnpFallbackFailed   upnpFallbackStatus = "failed"
	upnpFallbackDisabled upnpFallbackStatus = "disabled"
)

type upnpFallbackResult struct {
	mapping *upnpMapping
	status  upnpFallbackStatus
	err     error
}

type delayedUPnPAttempt struct {
	cancel  context.CancelFunc
	result  <-chan upnpFallbackResult
	once    sync.Once
	outcome upnpFallbackResult
}

type legacyCandidatesReadyMessage struct {
	Role   string             `json:"role"`
	UPnP   upnpFallbackStatus `json:"upnp"`
	Digest string             `json:"digest"`
	MAC    string             `json:"mac"`
}

// claimPairingCode creates or validates the local PAKE secret, publishes it to
// the user, and gives the mailbox only its opaque routing identifier.
func claimPairingCode(ctx context.Context, cfg Config, rep Reporter, mb mailbox) (string, error) {
	code, err := normalizeConfiguredPairingCode(cfg.Mode, cfg.Code)
	if err != nil {
		return "", fmt.Errorf("invalid pairing code: %w", err)
	}
	sessionID, err := deriveMailboxSessionID(code)
	if err != nil {
		return "", fmt.Errorf("derive mailbox session ID: %w", err)
	}
	if rep != nil {
		rep.Stage(StageRendezvous, StageStateRunning, "code "+code)
		rep.Logf("rendezvous claiming opaque session %s", mailboxSessionAlias(sessionID))
	}
	claimedID, err := mb.Claim(ctx, sessionID)
	if err != nil {
		return "", friendlyRendezvousErr(err)
	}
	if claimedID != sessionID {
		return "", errors.New("rendezvous returned an unexpected session identifier")
	}
	if rep != nil {
		rep.Logf("rendezvous claimed opaque session %s", mailboxSessionAlias(sessionID))
	}
	return code, nil
}

// attemptUPnPAfter waits for a fallback trigger before invoking the mapper.
func attemptUPnPAfter(
	ctx context.Context,
	trigger <-chan time.Time,
	mapper func(context.Context) (*upnpMapping, error),
) upnpFallbackResult {
	select {
	case <-ctx.Done():
		return upnpFallbackResult{status: upnpFallbackFailed, err: ctx.Err()}
	case <-trigger:
	}
	if mapper == nil {
		return upnpFallbackResult{status: upnpFallbackFailed, err: errors.New("UPnP mapper is not configured")}
	}
	mapping, err := mapper(ctx)
	if err != nil {
		return upnpFallbackResult{status: upnpFallbackFailed, err: err}
	}
	if mapping == nil {
		return upnpFallbackResult{status: upnpFallbackFailed, err: errors.New("UPnP mapper returned no mapping")}
	}
	return upnpFallbackResult{mapping: mapping, status: upnpFallbackMapped}
}

// startDelayedUPnPFallback prepares a cancellable mapping attempt for the legacy UDP socket.
func startDelayedUPnPFallback(
	ctx context.Context,
	cfg Config,
	conn *net.UDPConn,
	publicAddr string,
	rep Reporter,
) *delayedUPnPAttempt {
	childCtx, cancel := context.WithCancel(ctx)
	results := make(chan upnpFallbackResult, 1)
	attempt := &delayedUPnPAttempt{cancel: cancel, result: results}
	if cfg.DisableUPnP || cfg.Loopback {
		results <- upnpFallbackResult{status: upnpFallbackDisabled}
		return attempt
	}

	if rep != nil {
		rep.Logf("upnp/fallback armed delay=%s", upnpFallbackDelay)
	}
	go func() {
		timer := time.NewTimer(upnpFallbackDelay)
		defer timer.Stop()
		results <- attemptUPnPAfter(childCtx, timer.C, func(mapCtx context.Context) (*upnpMapping, error) {
			if rep != nil {
				rep.Logf("upnp/fallback starting after unresolved direct ICE")
			}
			return setupUPnPMapping(mapCtx, cfg, conn, publicAddr, rep)
		})
	}()
	return attempt
}

// wait joins the delayed mapping worker and returns its single outcome.
func (a *delayedUPnPAttempt) wait() upnpFallbackResult {
	if a == nil {
		return upnpFallbackResult{status: upnpFallbackDisabled}
	}
	a.once.Do(func() {
		a.outcome = <-a.result
	})
	return a.outcome
}

// stop cancels and joins the delayed mapping worker.
func (a *delayedUPnPAttempt) stop() upnpFallbackResult {
	if a == nil {
		return upnpFallbackResult{status: upnpFallbackDisabled}
	}
	a.cancel()
	return a.wait()
}

// stopAndCleanupUPnPFallback cancels a mapping worker and removes any mapping it completed.
func stopAndCleanupUPnPFallback(attempt *delayedUPnPAttempt, rep Reporter, reason string) upnpFallbackResult {
	if attempt == nil {
		return upnpFallbackResult{status: upnpFallbackDisabled}
	}
	outcome := attempt.stop()
	cleanupUPnPMapping(outcome.mapping, rep)
	if rep != nil {
		rep.Logf("upnp/fallback canceled reason=%s", reason)
	}
	return outcome
}

// cleanupUPnPMapping removes a completed fallback mapping with a bounded context.
func cleanupUPnPMapping(mapping *upnpMapping, rep Reporter) {
	if mapping == nil {
		return
	}
	var lastErr error
	for attempt := 1; attempt <= upnpCleanupAttempts; attempt++ {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultUPnPCleanupTimeout)
		lastErr = mapping.Close(cleanupCtx)
		cancel()
		if lastErr == nil {
			if rep != nil {
				rep.Logf("upnp/cleanup external=%s", mapping.externalAddr)
			}
			return
		}
		if attempt < upnpCleanupAttempts {
			time.Sleep(upnpCleanupRetryDelay)
		}
	}
	if rep != nil {
		rep.Logf("upnp/cleanup failed after %d attempts: %v", upnpCleanupAttempts, lastErr)
	}
}

// synchronizeUPnPFallback stores the local result, authenticates readiness, and refreshes peer candidates.
func synchronizeUPnPFallback(
	ctx context.Context,
	mb mailbox,
	self rendezvous.SelfInfo,
	initialPeer rendezvous.SelfInfo,
	outcome upnpFallbackResult,
	code string,
	role string,
	psk []byte,
	rep Reporter,
) (rendezvous.SelfInfo, error) {
	updatedSelf := cloneSelfInfo(self)
	if outcome.status == upnpFallbackMapped && outcome.mapping != nil {
		updatedSelf.Candidates = addUPnPCandidate(updatedSelf.Candidates, outcome.mapping.externalAddr)
	}
	if err := mb.StoreSelf(ctx, updatedSelf); err != nil {
		return initialPeer, fmt.Errorf("store progressive UPnP candidates: %w", err)
	}

	ready, err := newLegacyCandidatesReadyMessage(code, role, outcome.status, updatedSelf.Candidates, psk)
	if err != nil {
		return initialPeer, err
	}
	if err := mb.Send(ctx, legacyCandidatesReadyMessageType, ready); err != nil {
		return initialPeer, fmt.Errorf("send progressive UPnP readiness: %w", err)
	}

	msg, err := receiveMailboxType(ctx, mb, legacyCandidatesReadyMessageType)
	if err != nil {
		return initialPeer, fmt.Errorf("receive progressive UPnP readiness: %w", err)
	}
	remoteReady, err := decodeLegacyCandidatesReadyMessage(msg.Body)
	if err != nil {
		return initialPeer, err
	}
	if err := verifyLegacyCandidatesReadyMessage(remoteReady, code, oppositeRole(role), psk); err != nil {
		return initialPeer, err
	}

	refreshed, err := mb.WaitPeer(ctx)
	if err != nil {
		return initialPeer, fmt.Errorf("refresh progressive UPnP candidates: %w", err)
	}
	if len(refreshed.Candidates) > maxProgressiveCandidateCount {
		return initialPeer, fmt.Errorf("peer candidate refresh exceeds limit of %d", maxProgressiveCandidateCount)
	}
	digest, err := candidateSetDigest(refreshed.Candidates)
	if err != nil {
		return initialPeer, err
	}
	if !hmac.Equal([]byte(digest), []byte(remoteReady.Digest)) {
		return initialPeer, errors.New("peer candidate refresh did not match its authenticated digest")
	}
	if rep != nil {
		rep.Logf("upnp/fallback peer status=%s", remoteReady.UPnP)
	}
	return mergeUPnPFallbackPeer(initialPeer, *refreshed, remoteReady.UPnP), nil
}

// newLegacyCandidatesReadyMessage authenticates a candidate snapshot with the PAKE key.
func newLegacyCandidatesReadyMessage(
	code string,
	role string,
	status upnpFallbackStatus,
	candidates []rendezvous.Candidate,
	psk []byte,
) (legacyCandidatesReadyMessage, error) {
	if role != "send" && role != "recv" {
		return legacyCandidatesReadyMessage{}, fmt.Errorf("invalid readiness role %q", role)
	}
	if !validUPnPFallbackStatus(status) {
		return legacyCandidatesReadyMessage{}, fmt.Errorf("invalid UPnP fallback status %q", status)
	}
	if len(psk) == 0 {
		return legacyCandidatesReadyMessage{}, errors.New("missing PAKE key for candidate authentication")
	}
	digest, err := candidateSetDigest(candidates)
	if err != nil {
		return legacyCandidatesReadyMessage{}, err
	}
	message := legacyCandidatesReadyMessage{Role: role, UPnP: status, Digest: digest}
	message.MAC = legacyReadyMAC(code, message, psk)
	return message, nil
}

// decodeLegacyCandidatesReadyMessage decodes a bounded readiness payload with no unknown fields.
func decodeLegacyCandidatesReadyMessage(raw json.RawMessage) (legacyCandidatesReadyMessage, error) {
	if len(raw) == 0 || len(raw) > maxLegacyReadyMessageSize {
		return legacyCandidatesReadyMessage{}, errors.New("invalid progressive UPnP readiness message size")
	}
	var message legacyCandidatesReadyMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return legacyCandidatesReadyMessage{}, fmt.Errorf("decode progressive UPnP readiness: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return legacyCandidatesReadyMessage{}, errors.New("progressive UPnP readiness contains trailing data")
	}
	return message, nil
}

// verifyLegacyCandidatesReadyMessage validates readiness metadata and its PAKE-keyed MAC.
func verifyLegacyCandidatesReadyMessage(
	message legacyCandidatesReadyMessage,
	code string,
	expectedRole string,
	psk []byte,
) error {
	if len(psk) == 0 {
		return errors.New("missing PAKE key for candidate authentication")
	}
	if message.Role != expectedRole {
		return fmt.Errorf("progressive UPnP readiness role %q; want %q", message.Role, expectedRole)
	}
	if !validUPnPFallbackStatus(message.UPnP) {
		return fmt.Errorf("invalid peer UPnP fallback status %q", message.UPnP)
	}
	if len(message.Digest) != sha256.Size*2 {
		return errors.New("invalid peer candidate digest")
	}
	if _, err := hex.DecodeString(message.Digest); err != nil {
		return errors.New("invalid peer candidate digest")
	}
	wantMAC, err := hex.DecodeString(legacyReadyMAC(code, legacyCandidatesReadyMessage{
		Role:   message.Role,
		UPnP:   message.UPnP,
		Digest: message.Digest,
	}, psk))
	if err != nil {
		return err
	}
	gotMAC, err := hex.DecodeString(message.MAC)
	if err != nil || len(gotMAC) != sha256.Size {
		return errors.New("invalid progressive UPnP readiness authentication")
	}
	if !hmac.Equal(gotMAC, wantMAC) {
		return errors.New("progressive UPnP readiness authentication failed")
	}
	return nil
}

// legacyReadyMAC computes the domain-separated readiness authenticator.
func legacyReadyMAC(code string, message legacyCandidatesReadyMessage, psk []byte) string {
	mac := hmac.New(sha256.New, psk)
	for _, field := range []string{
		legacyReadyMACLabel,
		code,
		message.Role,
		string(message.UPnP),
		message.Digest,
	} {
		_, _ = mac.Write([]byte(strconv.Itoa(len(field))))
		_, _ = mac.Write([]byte{':'})
		_, _ = mac.Write([]byte(field))
	}
	return hex.EncodeToString(mac.Sum(nil))
}

// candidateSetDigest returns a stable digest for a complete candidate snapshot.
func candidateSetDigest(candidates []rendezvous.Candidate) (string, error) {
	if len(candidates) > maxProgressiveCandidateCount {
		return "", fmt.Errorf("candidate snapshot exceeds limit of %d", maxProgressiveCandidateCount)
	}
	canonical := append([]rendezvous.Candidate(nil), candidates...)
	sort.Slice(canonical, func(i, j int) bool {
		left := canonical[i]
		right := canonical[j]
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
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode candidate snapshot: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// mergeUPnPFallbackPeer accepts at most one validated mapped candidate from a refreshed snapshot.
func mergeUPnPFallbackPeer(
	initial rendezvous.SelfInfo,
	refreshed rendezvous.SelfInfo,
	status upnpFallbackStatus,
) rendezvous.SelfInfo {
	if status != upnpFallbackMapped || len(refreshed.Candidates) > maxProgressiveCandidateCount {
		return initial
	}
	for _, candidate := range refreshed.Candidates {
		validated, ok := validateProgressiveUPnPCandidate(initial, candidate)
		if !ok {
			continue
		}
		initial.Candidates = addUPnPCandidate(initial.Candidates, validated.Addr)
		break
	}
	return initial
}

// validateProgressiveUPnPCandidate restricts refreshed dial targets to the peer's public IPv4 address.
func validateProgressiveUPnPCandidate(
	initial rendezvous.SelfInfo,
	candidate rendezvous.Candidate,
) (rendezvous.Candidate, bool) {
	if !strings.EqualFold(candidate.Type, "upnp") || !strings.EqualFold(candidate.Proto, "udp") {
		return rendezvous.Candidate{}, false
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
	if ip == nil || !isUsableExternalIPv4(ip) {
		return rendezvous.Candidate{}, false
	}
	publicIP := parseAddrIPv4(initial.Public)
	if publicIP != nil && isUsableExternalIPv4(publicIP) && !ip.Equal(publicIP) {
		return rendezvous.Candidate{}, false
	}
	return rendezvous.Candidate{Type: "upnp", Proto: "udp", Addr: net.JoinHostPort(ip.String(), strconv.Itoa(port)), Priority: 110}, true
}

// addUPnPCandidate appends or promotes a mapped endpoint without duplicating an address.
func addUPnPCandidate(candidates []rendezvous.Candidate, addr string) []rendezvous.Candidate {
	out := append([]rendezvous.Candidate(nil), candidates...)
	for i := range out {
		if strings.EqualFold(out[i].Proto, "udp") && out[i].Addr == addr {
			out[i] = rendezvous.Candidate{Type: "upnp", Proto: "udp", Addr: addr, Priority: 110}
			return out
		}
	}
	return append(out, rendezvous.Candidate{Type: "upnp", Proto: "udp", Addr: addr, Priority: 110})
}

// cloneSelfInfo copies slices so progressive updates do not mutate the initial snapshot.
func cloneSelfInfo(info rendezvous.SelfInfo) rendezvous.SelfInfo {
	info.Candidates = append([]rendezvous.Candidate(nil), info.Candidates...)
	info.Features = append([]string(nil), info.Features...)
	return info
}

// validUPnPFallbackStatus reports whether status is part of the versioned readiness protocol.
func validUPnPFallbackStatus(status upnpFallbackStatus) bool {
	switch status {
	case upnpFallbackMapped, upnpFallbackFailed, upnpFallbackDisabled:
		return true
	default:
		return false
	}
}

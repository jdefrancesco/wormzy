package transport

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	mailboxCapabilitySize        = 32
	mailboxCapabilityEncodedSize = 43
)

var (
	errMailboxAuthentication = errors.New("mailbox authentication failed")
	errMailboxUnavailable    = errors.New("mailbox unavailable")
	missingMailboxVerifier   = mailboxCapabilityVerifierBytes(make([]byte, mailboxCapabilitySize))
)

// generateMailboxCapability creates a random bearer capability and its stored verifier.
func generateMailboxCapability(random io.Reader) (string, string, error) {
	if random == nil {
		random = rand.Reader
	}
	secret := make([]byte, mailboxCapabilitySize)
	if _, err := io.ReadFull(random, secret); err != nil {
		return "", "", fmt.Errorf("generate mailbox capability: %w", err)
	}
	defer clear(secret)
	raw := base64.RawURLEncoding.EncodeToString(secret)
	verifier := mailboxCapabilityVerifierBytes(secret)
	return raw, verifier, nil
}

// mailboxCapabilityVerifier derives the verifier for a canonical raw capability.
func mailboxCapabilityVerifier(raw string) (string, error) {
	secret, err := decodeMailboxCapability(raw)
	if err != nil {
		return "", errMailboxAuthentication
	}
	defer clear(secret)
	return mailboxCapabilityVerifierBytes(secret), nil
}

// mailboxCapabilityVerifierBytes hashes capability bytes for safe persistence.
func mailboxCapabilityVerifierBytes(secret []byte) string {
	digest := sha256.Sum256(secret)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// validateMailboxCapabilityVerifier accepts only canonical SHA-256 verifier encodings.
func validateMailboxCapabilityVerifier(verifier string) error {
	if len(verifier) != mailboxCapabilityEncodedSize {
		return errMailboxUnavailable
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(verifier)
	if err != nil || len(decoded) != sha256.Size || base64.RawURLEncoding.EncodeToString(decoded) != verifier {
		return errMailboxUnavailable
	}
	return nil
}

// mailboxCapabilityVerifierEqual compares canonical verifiers without data-dependent equality timing.
func mailboxCapabilityVerifierEqual(left, right string) bool {
	if len(left) != mailboxCapabilityEncodedSize || len(right) != mailboxCapabilityEncodedSize {
		return false
	}
	var leftFixed, rightFixed [sha256.Size]byte
	leftRaw, leftErr := base64.RawURLEncoding.Strict().DecodeString(left)
	rightRaw, rightErr := base64.RawURLEncoding.Strict().DecodeString(right)
	copy(leftFixed[:], leftRaw)
	copy(rightFixed[:], rightRaw)
	valid := 1
	if leftErr != nil || rightErr != nil ||
		len(leftRaw) != sha256.Size || len(rightRaw) != sha256.Size ||
		base64.RawURLEncoding.EncodeToString(leftRaw) != left ||
		base64.RawURLEncoding.EncodeToString(rightRaw) != right {
		valid = 0
	}
	return valid&subtle.ConstantTimeCompare(leftFixed[:], rightFixed[:]) == 1
}

// compareMissingMailboxCapability performs the same verifier comparison for absent identities.
func compareMissingMailboxCapability(supplied string) {
	mailboxCapabilityVerifierEqual(missingMailboxVerifier, supplied)
}

// mailboxAuthorizationVerifier validates a Bearer capability and derives its verifier.
func mailboxAuthorizationVerifier(header string) (string, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", errMailboxAuthentication
	}
	raw := strings.TrimPrefix(header, prefix)
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") {
		return "", errMailboxAuthentication
	}
	return mailboxCapabilityVerifier(raw)
}

// decodeMailboxCapability decodes a canonical 32-byte raw base64url capability.
func decodeMailboxCapability(raw string) ([]byte, error) {
	if len(raw) != mailboxCapabilityEncodedSize {
		return nil, errMailboxAuthentication
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(raw)
	if err != nil || len(decoded) != mailboxCapabilitySize || base64.RawURLEncoding.EncodeToString(decoded) != raw {
		return nil, errMailboxAuthentication
	}
	return decoded, nil
}

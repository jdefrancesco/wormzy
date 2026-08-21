package transport

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	mailboxSessionIDPrefix = "mbx2-"
	mailboxSessionIDLabel  = "wormzy-mailbox-session-id-v2"
	mailboxSessionIDBytes  = sha256.Size
	mailboxSessionIDLength = len(mailboxSessionIDPrefix) + 43
)

// deriveMailboxSessionID turns the high-entropy pairing secret into an opaque
// routing identifier so the mailbox never learns the CPace password.
func deriveMailboxSessionID(code string) (string, error) {
	normalized, err := rendezvous.NormalizeCode(code)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	for _, field := range []string{mailboxSessionIDLabel, normalized} {
		_, _ = hash.Write([]byte(strconv.Itoa(len(field))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(field))
	}
	return mailboxSessionIDPrefix + base64.RawURLEncoding.EncodeToString(hash.Sum(nil)), nil
}

// validMailboxSessionID reports whether an identifier has the exact opaque
// mailbox routing format accepted by current clients.
func validMailboxSessionID(sessionID string) bool {
	if len(sessionID) != mailboxSessionIDLength || !strings.HasPrefix(sessionID, mailboxSessionIDPrefix) {
		return false
	}
	encoded := strings.TrimPrefix(sessionID, mailboxSessionIDPrefix)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	return err == nil && len(raw) == mailboxSessionIDBytes && base64.RawURLEncoding.EncodeToString(raw) == encoded
}

// mailboxSessionAlias returns a short non-secret label suitable for operator
// diagnostics without exposing the full bearer-like routing identifier.
func mailboxSessionAlias(sessionID string) string {
	if !validMailboxSessionID(sessionID) {
		return "unknown"
	}
	encoded := strings.TrimPrefix(sessionID, mailboxSessionIDPrefix)
	if len(encoded) < 8 {
		return "unknown"
	}
	return "m-" + encoded[:8]
}

// normalizeConfiguredPairingCode validates a supplied code or creates a fresh
// sender secret when no code was requested.
func normalizeConfiguredPairingCode(mode, configured string) (string, error) {
	if strings.TrimSpace(configured) == "" {
		if mode != "send" {
			return "", errors.New("receiver requires a pairing code")
		}
		return rendezvous.GenerateCode()
	}
	return rendezvous.NormalizeCode(configured)
}

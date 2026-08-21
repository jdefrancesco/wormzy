package transport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/quic-go/quic-go"
	"golang.org/x/crypto/hkdf"
)

const (
	featureTransferCompletionV1        = "transfer-completion-v1"
	transferCompletionReceiverVerified = "receiver-verified"
	transferCompletionSenderConfirmed  = "sender-confirmed"
	transferCompletionReceiverFinished = "receiver-finished"
	transferCompletionSenderFinished   = "sender-finished"
	transferCompletionKeyLabel         = "wormzy-transfer-completion-key-v1"
	transferCompletionMACLabel         = "wormzy-transfer-completion-message-v1"
	transferCompletionMaxMessageSize   = 2048
	transferCompletionDigestSize       = 32
	defaultTransferCompletionTimeout   = 10 * time.Second
	transferCompletionLingerTimeout    = 2 * time.Second
	transferIncompleteCode             = quic.ApplicationErrorCode(0x575a02)
	transferIncompleteReason           = "wormzy transfer completion failed"
)

type transferCompletionMessage struct {
	Role string `json:"role"`
	MAC  string `json:"mac"`
}

type transferCompletionStream interface {
	io.Reader
	io.Writer
	SetDeadline(time.Time) error
}

type transferCompletionLingerStream interface {
	io.Reader
	SetReadDeadline(time.Time) error
}

// finishTransferSession performs the authenticated receipt protocol when both peers support it.
func finishTransferSession(
	ctx context.Context,
	cfg Config,
	peer rendezvous.SelfInfo,
	conn *quic.Conn,
	code string,
	fileKey []byte,
	digest []byte,
	size int64,
	rep Reporter,
) error {
	if err := requirePeerTransferCompletion(peer); err != nil {
		return err
	}
	if conn == nil {
		return errors.New("transfer completion requires a QUIC connection")
	}
	finishCtx, cancel := context.WithTimeout(ctx, transferCompletionTimeout(cfg.IdleTimeout))
	defer cancel()
	if err := exchangeAuthenticatedTransferCompletion(finishCtx, conn, cfg.Mode, code, fileKey, digest, size); err != nil {
		_ = conn.CloseWithError(transferIncompleteCode, transferIncompleteReason)
		return fmt.Errorf("authenticated transfer completion: %w", err)
	}
	if rep != nil {
		rep.Logf("transfer/ack verified size=%d digest=%s", size, hex.EncodeToString(digest))
	}
	return nil
}

// requirePeerTransferCompletion fails closed when the peer cannot prove file receipt.
func requirePeerTransferCompletion(peer rendezvous.SelfInfo) error {
	if peerSupportsFeature(peer, featureTransferCompletionV1) {
		return nil
	}
	return fmt.Errorf("peer lacks authenticated transfer completion (%s); update Wormzy on both devices", featureTransferCompletionV1)
}

// transferCompletionTimeout bounds the post-transfer acknowledgement wait.
func transferCompletionTimeout(idleTimeout time.Duration) time.Duration {
	if idleTimeout <= 0 || idleTimeout > defaultTransferCompletionTimeout {
		return defaultTransferCompletionTimeout
	}
	return idleTimeout
}

// exchangeAuthenticatedTransferCompletion exchanges file-key-authenticated receipt messages over QUIC.
func exchangeAuthenticatedTransferCompletion(
	ctx context.Context,
	conn *quic.Conn,
	mode string,
	code string,
	fileKey []byte,
	digest []byte,
	size int64,
) error {
	var (
		stream *quic.Stream
		err    error
	)
	if mode == "recv" {
		stream, err = conn.OpenStreamSync(ctx)
	} else if mode == "send" {
		stream, err = conn.AcceptStream(ctx)
	} else {
		return fmt.Errorf("invalid transfer completion mode %q", mode)
	}
	if err != nil {
		return err
	}
	defer stream.Close()

	if err := runTransferCompletionProtocol(ctx, stream, mode, code, fileKey, digest, size); err != nil {
		return err
	}
	// Both roles have sufficient authenticated application-level evidence after
	// four messages. Half-close the stream, then let both peers linger for the
	// opposite FIN so the final frames can be acknowledged before either UDP
	// transport is released. A teardown error does not revoke that evidence.
	_ = stream.Close()
	lingerAfterAuthenticatedTransferCompletion(ctx, stream)
	return nil
}

// lingerAfterAuthenticatedTransferCompletion best-effort waits for the peer's clean FIN.
func lingerAfterAuthenticatedTransferCompletion(ctx context.Context, stream transferCompletionLingerStream) {
	deadline := time.Now().Add(transferCompletionLingerTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = stream.SetReadDeadline(deadline)
	_ = waitForTransferCompletionEOF(stream)
}

// waitForTransferCompletionEOF reports whether a completion stream ended with a clean peer FIN.
func waitForTransferCompletionEOF(r io.Reader) error {
	var trailing [1]byte
	n, err := r.Read(trailing[:])
	if n == 0 && errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("transfer completion stream contains trailing data")
	}
	return fmt.Errorf("wait for transfer completion stream close: %w", err)
}

// runTransferCompletionProtocol authenticates receipt, confirmation, and final delivery on a control stream.
func runTransferCompletionProtocol(
	ctx context.Context,
	stream transferCompletionStream,
	mode string,
	code string,
	fileKey []byte,
	digest []byte,
	size int64,
) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := stream.SetDeadline(deadline); err != nil {
			return err
		}
		defer stream.SetDeadline(time.Time{})
	}

	send := func(role string) error {
		message, err := newTransferCompletionMessage(code, role, fileKey, digest, size)
		if err != nil {
			return err
		}
		return writeTransferCompletionMessage(stream, message)
	}
	receive := func(role string) error {
		message, err := readTransferCompletionMessage(stream)
		if err != nil {
			return err
		}
		return verifyTransferCompletionMessage(message, code, role, fileKey, digest, size)
	}

	switch mode {
	case "send":
		if err := receive(transferCompletionReceiverVerified); err != nil {
			return err
		}
		if err := send(transferCompletionSenderConfirmed); err != nil {
			return err
		}
		if err := receive(transferCompletionReceiverFinished); err != nil {
			return err
		}
		return send(transferCompletionSenderFinished)
	case "recv":
		if err := send(transferCompletionReceiverVerified); err != nil {
			return err
		}
		if err := receive(transferCompletionSenderConfirmed); err != nil {
			return err
		}
		if err := send(transferCompletionReceiverFinished); err != nil {
			return err
		}
		return receive(transferCompletionSenderFinished)
	default:
		return fmt.Errorf("invalid transfer completion mode %q", mode)
	}
}

// newTransferCompletionMessage creates a file-key-authenticated completion message.
func newTransferCompletionMessage(
	code string,
	role string,
	fileKey []byte,
	digest []byte,
	size int64,
) (transferCompletionMessage, error) {
	if code == "" {
		return transferCompletionMessage{}, errors.New("transfer completion code is empty")
	}
	if !validTransferCompletionRole(role) {
		return transferCompletionMessage{}, fmt.Errorf("invalid transfer completion role %q", role)
	}
	if len(fileKey) != 32 {
		return transferCompletionMessage{}, errors.New("invalid transfer completion file key")
	}
	if len(digest) != transferCompletionDigestSize {
		return transferCompletionMessage{}, errors.New("invalid transfer completion digest")
	}
	if size < 0 {
		return transferCompletionMessage{}, errors.New("invalid transfer completion size")
	}
	message := transferCompletionMessage{Role: role}
	mac, err := transferCompletionMAC(code, role, fileKey, digest, size)
	if err != nil {
		return transferCompletionMessage{}, err
	}
	message.MAC = mac
	return message, nil
}

// verifyTransferCompletionMessage verifies role, file identity, and the file-key-derived MAC.
func verifyTransferCompletionMessage(
	message transferCompletionMessage,
	code string,
	expectedRole string,
	fileKey []byte,
	expectedDigest []byte,
	expectedSize int64,
) error {
	if message.Role != expectedRole || !validTransferCompletionRole(message.Role) {
		return fmt.Errorf("unexpected transfer completion role %q", message.Role)
	}
	wantMAC, err := transferCompletionMAC(code, message.Role, fileKey, expectedDigest, expectedSize)
	if err != nil {
		return err
	}
	want, err := hex.DecodeString(wantMAC)
	if err != nil {
		return err
	}
	got, err := hex.DecodeString(message.MAC)
	if err != nil || len(got) != sha256.Size || !hmac.Equal(got, want) {
		return errors.New("transfer completion authentication failed")
	}
	return nil
}

// transferCompletionMAC computes a domain-separated MAC using a key derived from the file key.
func transferCompletionMAC(code, role string, fileKey, digest []byte, size int64) (string, error) {
	if code == "" {
		return "", errors.New("transfer completion code is empty")
	}
	if !validTransferCompletionRole(role) {
		return "", fmt.Errorf("invalid transfer completion role %q", role)
	}
	if len(fileKey) != 32 {
		return "", errors.New("invalid transfer completion file key")
	}
	if len(digest) != transferCompletionDigestSize {
		return "", errors.New("invalid transfer completion digest")
	}
	if size < 0 {
		return "", errors.New("invalid transfer completion size")
	}
	key := make([]byte, sha256.Size)
	if _, err := io.ReadFull(hkdf.New(sha256.New, fileKey, nil, []byte(transferCompletionKeyLabel)), key); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	for _, field := range []string{
		transferCompletionMACLabel,
		code,
		role,
		strconv.FormatInt(size, 10),
		hex.EncodeToString(digest),
	} {
		_, _ = mac.Write([]byte(strconv.Itoa(len(field))))
		_, _ = mac.Write([]byte{':'})
		_, _ = mac.Write([]byte(field))
	}
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// writeTransferCompletionMessage writes a bounded length-prefixed completion message.
func writeTransferCompletionMessage(w io.Writer, message transferCompletionMessage) error {
	raw, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > transferCompletionMaxMessageSize {
		return errors.New("invalid transfer completion message size")
	}
	var header [2]byte
	// The size is bounded to 2048 above, so this narrowing conversion is safe.
	binary.BigEndian.PutUint16(header[:], uint16(len(raw))) // #nosec G115
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, raw)
}

// writeAll writes the complete buffer while accepting legal short writes.
func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("invalid write count %d", n)
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

// readTransferCompletionMessage reads and strictly decodes a bounded completion message.
func readTransferCompletionMessage(r io.Reader) (transferCompletionMessage, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return transferCompletionMessage{}, err
	}
	size := int(binary.BigEndian.Uint16(header[:]))
	if size == 0 || size > transferCompletionMaxMessageSize {
		return transferCompletionMessage{}, errors.New("invalid transfer completion message size")
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(r, raw); err != nil {
		return transferCompletionMessage{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var message transferCompletionMessage
	if err := decoder.Decode(&message); err != nil {
		return transferCompletionMessage{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return transferCompletionMessage{}, errors.New("transfer completion message contains trailing data")
	}
	return message, nil
}

// validTransferCompletionRole reports whether a role belongs to the completion protocol.
func validTransferCompletionRole(role string) bool {
	switch role {
	case transferCompletionReceiverVerified, transferCompletionSenderConfirmed,
		transferCompletionReceiverFinished, transferCompletionSenderFinished:
		return true
	default:
		return false
	}
}

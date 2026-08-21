package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
)

const (
	maxDeferredMailboxMessages  = 16
	maxDeferredMailboxBodySize  = 64 << 10
	maxMailboxMessageTypeLength = 64
)

type mailbox interface {
	Claim(ctx context.Context, requested string) (string, error)
	StoreSelf(ctx context.Context, info rendezvous.SelfInfo) error
	WaitPeer(ctx context.Context) (*rendezvous.SelfInfo, error)
	Send(ctx context.Context, typ string, body any) error
	Receive(ctx context.Context) (mailboxMessage, error)
	ReportStats(ctx context.Context, stats transferStats) error
	Close() error
}

type mailboxMessage struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

type protocolMailbox struct {
	mailbox
	receiveMu sync.Mutex
	deferred  []mailboxMessage
}

// newProtocolMailbox adds bounded, phase-aware receiving to a mailbox.
func newProtocolMailbox(base mailbox) mailbox {
	if base == nil {
		return nil
	}
	return &protocolMailbox{mailbox: base}
}

// Receive returns the oldest deferred message before reading the underlying mailbox.
func (m *protocolMailbox) Receive(ctx context.Context) (mailboxMessage, error) {
	m.receiveMu.Lock()
	defer m.receiveMu.Unlock()
	if len(m.deferred) > 0 {
		message := m.deferred[0]
		m.deferred[0] = mailboxMessage{}
		m.deferred = m.deferred[1:]
		if err := validateMailboxMessage(message); err != nil {
			return mailboxMessage{}, err
		}
		return message, nil
	}
	message, err := m.mailbox.Receive(ctx)
	if err != nil {
		return mailboxMessage{}, err
	}
	if err := validateMailboxMessage(message); err != nil {
		return mailboxMessage{}, err
	}
	return message, nil
}

// receiveType returns the next message for the requested protocol phase while preserving others.
func (m *protocolMailbox) receiveType(ctx context.Context, accepted map[string]struct{}) (mailboxMessage, error) {
	m.receiveMu.Lock()
	defer m.receiveMu.Unlock()

	for i, message := range m.deferred {
		if _, ok := accepted[message.Type]; !ok {
			continue
		}
		copy(m.deferred[i:], m.deferred[i+1:])
		m.deferred[len(m.deferred)-1] = mailboxMessage{}
		m.deferred = m.deferred[:len(m.deferred)-1]
		return message, nil
	}

	for {
		message, err := m.mailbox.Receive(ctx)
		if err != nil {
			return mailboxMessage{}, err
		}
		if err := validateMailboxMessage(message); err != nil {
			return mailboxMessage{}, err
		}
		if _, ok := accepted[message.Type]; ok {
			return message, nil
		}
		if err := m.deferMessage(message); err != nil {
			return mailboxMessage{}, err
		}
	}
}

// deferMessage preserves a future or stale phase message within strict memory bounds.
func (m *protocolMailbox) deferMessage(message mailboxMessage) error {
	if err := validateMailboxMessage(message); err != nil {
		return err
	}
	if len(m.deferred) >= maxDeferredMailboxMessages {
		return fmt.Errorf("mailbox deferred message limit of %d exceeded", maxDeferredMailboxMessages)
	}
	m.deferred = append(m.deferred, message)
	return nil
}

// validateMailboxMessage enforces envelope limits before any protocol decoder sees a message.
func validateMailboxMessage(message mailboxMessage) error {
	if len(message.Type) == 0 || len(message.Type) > maxMailboxMessageTypeLength {
		return errors.New("mailbox message type is invalid")
	}
	for _, char := range message.Type {
		if char < 0x21 || char > 0x7e {
			return errors.New("mailbox message type is invalid")
		}
	}
	if len(message.Body) > maxDeferredMailboxBodySize {
		return fmt.Errorf("mailbox message body exceeds %d bytes", maxDeferredMailboxBodySize)
	}
	return nil
}

// receiveMailboxType receives one of the requested message types without losing other protocol phases.
func receiveMailboxType(ctx context.Context, mbox mailbox, acceptedTypes ...string) (mailboxMessage, error) {
	if len(acceptedTypes) == 0 {
		return mailboxMessage{}, errors.New("no accepted mailbox message types")
	}
	accepted := make(map[string]struct{}, len(acceptedTypes))
	for _, messageType := range acceptedTypes {
		if messageType == "" {
			return mailboxMessage{}, errors.New("accepted mailbox message type is empty")
		}
		accepted[messageType] = struct{}{}
	}
	if routed, ok := mbox.(*protocolMailbox); ok {
		return routed.receiveType(ctx, accepted)
	}
	for {
		message, err := mbox.Receive(ctx)
		if err != nil {
			return mailboxMessage{}, err
		}
		if err := validateMailboxMessage(message); err != nil {
			return mailboxMessage{}, err
		}
		if _, ok := accepted[message.Type]; ok {
			return message, nil
		}
	}
}

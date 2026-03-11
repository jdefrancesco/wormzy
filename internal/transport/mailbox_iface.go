package transport

import (
	"context"
	"encoding/json"

	"github.com/jdefrancesco/internal/rendezvous"
)

type mailbox interface {
	Claim(ctx context.Context, requested string) (string, error)
	StoreSelf(ctx context.Context, info rendezvous.SelfInfo) error
	WaitPeer(ctx context.Context) (*rendezvous.SelfInfo, error)
	Send(ctx context.Context, typ string, body any) error
	Receive(ctx context.Context) (mailboxMessage, error)
	Close() error
}

type mailboxMessage struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

package stun

import (
	"net"
	"time"
)

type Stun struct {
	// Conn is the UDP socket used for both sending requests and receiving replies.
	Conn *net.UDPConn
	// Servers holds candidate STUN server addresses (host:port).
	Servers []string
	// RTO is the retransmission timeout applied per request.
	RTO time.Duration
	// MaxRetrans controls the number of retransmissions after the initial send.
	MaxRetrans uint
}

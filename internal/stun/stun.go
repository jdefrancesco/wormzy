package stun

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"time"

	stun "github.com/pion/stun"
)

// List of public STUN servers
var StunServers = []string{
	"stun.l.google.com:19302",
	"stun.l.google.com:5359",
	"stun1.l.google.com:19302",
	"stun2.l.google.com:19302",
	"stun3.l.google.com:19302",
	"stun.sipgate.net:3478",
	"stun.ekiga.net:3478",
	"stun.cloudflare.com:3478",
	"stun4.l.google.com:19302",
}

type Stun struct {
	// Conn is the UDP socket used for both sending requests and receiving replies.
	Conn *net.UDPConn
	// IpV4Addr is the public IPv4 address discovered via STUN.
	IpV4Addr *net.UDPAddr
	// Servers holds candidate STUN server addresses (host:port).
	Servers []string
	// RTO is the retransmission timeout applied per request.
	RTO time.Duration
	// MaxRetrans controls the number of retransmissions after the initial send.
	MaxRetrans uint
}

func NewStun() *Stun {
	s := &Stun{
		Servers:    StunServers,
		RTO:        4 * time.Second,
		MaxRetrans: 5,
	}

	if err := s.discoverIPv4(); err != nil {
		// discovery is best-effort; report but continue
		slog.Warn("STUN discovery error", "err", err)
	}
	return s
}

func (s *Stun) discoverIPv4() error {
	servers := s.Servers
	if len(servers) == 0 {
		servers = StunServers
	}

	type res struct {
		addr *net.UDPAddr
		err  error
	}

	ch := make(chan res, len(servers))

	// Launch one probe per server using its own ephemeral socket to avoid
	// concurrent reads on a shared Conn.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, srv := range servers {
		server := srv
		go func() {
			addr, err := probeServer(ctx, server, s.RTO, s.MaxRetrans)
			ch <- res{addr: addr, err: err}
		}()
	}

	var firstErr error
	for i := 0; i < len(servers); i++ {
		r := <-ch
		if r.err != nil {
			slog.Debug("STUN probe error", "err", r.err)
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		if r.addr != nil {
			s.IpV4Addr = r.addr
			// stop other probes
			cancel()
			return nil
		}
	}

	if firstErr != nil {
		return firstErr
	}
	return fmt.Errorf("no STUN response")
}

// probeServer performs a STUN Binding Request to the given server using a
// temporary UDP socket and returns the discovered public UDP address.
func probeServer(ctx context.Context, server string, rto time.Duration, maxRetrans uint) (*net.UDPAddr, error) {
	srv, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}

	// bind to an ephemeral local UDP port
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	conn, err := net.ListenUDP("udp4", laddr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if _, werr := conn.WriteToUDP(req.Raw, srv); werr != nil {
		return nil, werr
	}

	dead := time.Now().Add(time.Duration(maxRetrans+1) * rto)
	buf := make([]byte, 1500)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(rto))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// check if caller cancelled
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				default:
				}
				if time.Now().After(dead) {
					return nil, fmt.Errorf("no STUN from %s", server)
				}
				if _, werr := conn.WriteToUDP(req.Raw, srv); werr != nil {
					return nil, werr
				}
				continue
			}
			return nil, err
		}
		if !from.IP.Equal(srv.IP) || from.Port != srv.Port {
			continue
		}
		var m stun.Message
		m.Raw = buf[:n]
		if err := m.Decode(); err != nil || m.Type != stun.BindingSuccess {
			continue
		}
		var xor stun.XORMappedAddress
		if err := xor.GetFrom(&m); err == nil {
			return &net.UDPAddr{IP: xor.IP, Port: xor.Port}, nil
		}
		var ma stun.MappedAddress
		if err := ma.GetFrom(&m); err == nil {
			return &net.UDPAddr{IP: ma.IP, Port: ma.Port}, nil
		}
	}
}

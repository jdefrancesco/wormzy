package stun

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	stun "github.com/pion/stun"
)

func TestNewStun_Defaults(t *testing.T) {
	s := NewStun()
	if s == nil {
		t.Fatal("NewStun returned nil")
	}
	if s.RTO != 4*time.Second {
		t.Fatalf("unexpected RTO: got %v want %v", s.RTO, 4*time.Second)
	}
	if len(s.Servers) == 0 {
		t.Fatal("expected default STUN servers to be set")
	}
}

func TestProbeServer_BadAddress(t *testing.T) {
	ctx := context.Background()

	// Malformed address (missing host/port) should return an error quickly
	if _, err := probeServer(ctx, ":", 1*time.Millisecond, 0); err == nil {
		t.Fatalf("expected error for malformed server address, got nil")
	}
}

func TestProbeServer_Success(t *testing.T) {

	// Start a fake STUN UDP server
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer l.Close()

	mappedIP := net.ParseIP("1.2.3.4")
	mappedPort := 54321

	go func() {
		buf := make([]byte, 1500)
		nRead, raddr, rerr := l.ReadFromUDP(buf)
		if rerr != nil {
			return
		}

		// Respond with a BindingSuccess containing XOR-MAPPED-ADDRESS
		_ = nRead
		res := stun.MustBuild(stun.TransactionID, stun.BindingSuccess,
			&stun.XORMappedAddress{IP: mappedIP, Port: mappedPort}, stun.Fingerprint)
		_, _ = l.WriteToUDP(res.Raw, raddr)
	}()

	ctx := context.Background()
	la := l.LocalAddr().(*net.UDPAddr)
	serverAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(la.Port))
	got, err := probeServer(ctx, serverAddr, 2*time.Second, 0)
	if err != nil {
		t.Fatalf("probeServer failed: %v", err)
	}
	if !got.IP.Equal(mappedIP) || got.Port != mappedPort {
		t.Fatalf("unexpected mapped addr: got %v want %v:%d", got, mappedIP, mappedPort)
	}
}

func TestDiscoverOnConn(t *testing.T) {
	// fake STUN server
	server, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer server.Close()

	respIP := net.ParseIP("5.6.7.8")
	respPort := 4242

	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := server.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_ = n
			res := stun.MustBuild(stun.TransactionID, stun.BindingSuccess,
				&stun.XORMappedAddress{IP: respIP, Port: respPort}, stun.Fingerprint)
			_, _ = server.WriteToUDP(res.Raw, addr)
		}
	}()

	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("client listen failed: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	srvAddr := server.LocalAddr().(*net.UDPAddr)
	host := srvAddr.IP.String()
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	serverAddr := net.JoinHostPort(host, strconv.Itoa(srvAddr.Port))

	got, err := DiscoverOnConn(ctx, client, []string{serverAddr}, 200*time.Millisecond, 1)
	if err != nil {
		t.Fatalf("DiscoverOnConn failed: %v", err)
	}
	if got == nil || !got.IP.Equal(respIP) || got.Port != respPort {
		t.Fatalf("unexpected mapped addr: got %v want %v:%d", got, respIP, respPort)
	}
}

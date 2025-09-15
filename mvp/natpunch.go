package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pion/stun"
	quic "github.com/quic-go/quic-go"
)

const (
	alpn = "p2p-wormy-1"
)

type SelfInfo struct {
	Public string `json:"public"`
	Local  string `json:"local"`
}

func main() {
	var (
		stunServer = flag.String("stun", "stun.cloudflare.com:3478", "STUN server host:port")
		timeout    = flag.Duration("timeout", 60*time.Second, "hole punch timeout")
	)
	flag.Parse()

	// 1) Bind single UDP socket for STUN, punching, and QUIC.
	// udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	// We want to bind UDP on IPv4 ONLY
	laddr4, _ := net.ResolveUDPAddr("udp4", "0.0.0.0:0")
	udpConn, err := net.ListenUDP("udp4", laddr4)
	check(err)
	defer udpConn.Close()
	fmt.Printf("[*] Local UDP bind: %s\n", udpConn.LocalAddr().String())

	// 2) STUN to learn our public mapping.
	pub, err := stunDiscoverAny(udpConn, []string{
		*stunServer, // User specified first
		"stun.l.google.com:19302",
		"stun1.l.google.com:19302",
		"stun2.l.google.com:19302",
		"stun.cloudflare.com:3478",
		"stun.sipgate.net:3478",
	}, 4*time.Second)

	check(err)
	self := SelfInfo{Public: pub.String(), Local: udpConn.LocalAddr().String()}
	fmt.Printf("[*] STUN public: %s\n", self.Public)

	// 3) Exchange JSON blobs manually (rendezvous).
	selfJSON, _ := json.MarshalIndent(self, "", "  ")
	fmt.Println()
	fmt.Println("=== SEND THIS TO YOUR PEER ===")
	fmt.Println(string(selfJSON))
	fmt.Println("=== END ===\n")
	fmt.Println("Paste peer JSON, then Ctrl-D (EOF):")

	peer, err := readPeerInfoFromStdin()
	check(err)
	fmt.Printf("[*] Peer public: %s\n", peer.Public)

	peerUDP, err := net.ResolveUDPAddr("udp", peer.Public)
	check(err)

	// 4) Start symmetric UDP punching while we bring up QUIC.
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var wg sync.WaitGroup
	stopPunch := make(chan struct{})

	wg.Add(1)
	go func() { // fire small probes to open/refresh NAT mapping
		defer wg.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		msg := []byte("punch")
		for {
			select {
			case <-t.C:
				_, _ = udpConn.WriteToUDP(msg, peerUDP)
			case <-stopPunch:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// 5) Decide roles deterministically to avoid both sides dialing.
	role := roleFromPub(self.Public, peer.Public) // "server" or "client"
	fmt.Printf("[*] Role chosen: %s\n", role)

	// TLS configs (self-signed for server, InsecureSkipVerify for client)
	serverTLS, err := selfSignedTLS()
	check(err)
	serverTLS.NextProtos = []string{alpn}
	clientTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}

	// 6) Start QUIC over the SAME UDP socket.
	var (
		qconn quic.Connection
	)

	if role == "server" {
		// Listen on packet conn (no extra UDP sockets).
		listener, err := quic.Listen(udpConn, serverTLS, &quic.Config{
			KeepAlivePeriod: 15 * time.Second,
			EnableDatagrams: false,
		})
		check(err)
		fmt.Println("[*] QUIC: listening... (waiting for peer)")

		// Accept uses any remote that reaches us on this socket.
		qconn, err = listener.Accept(contextWithDeadline(40 * time.Second))
		check(err)
		fmt.Printf("[*] QUIC: accepted from %s\n", qconn.RemoteAddr())
	} else {
		// Client dials the peer public UDP address.
		qconn, err = quic.Dial(contextWithDeadline(40*time.Second), udpConn, peerUDP, clientTLS, &quic.Config{
			KeepAlivePeriod: 15 * time.Second,
			EnableDatagrams: false,
		})
		check(err)
		fmt.Printf("[*] QUIC: dialed %s\n", peerUDP)
	}

	// We have a QUIC connection—stop extra punching.
	close(stopPunch)
	cancel()
	wg.Wait()

	receiveFilesForever(qconn)

	// 7) Chat + file send.
	// Use bidi stream 0 for chat.
	var chat quic.Stream
	if role == "server" {
		var err error
		chat, err = qconn.AcceptStream(context.Background())
		check(err)
	} else {
		var err error
		chat, err = qconn.OpenStreamSync(context.Background())
		check(err)
	}

	fmt.Println("[*] Chat ready. Type to send. Use `/send <path>` to send a file. Ctrl-C to exit.")

	// Reader goroutine for incoming chat
	go func() {
		br := bufio.NewReader(chat)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					fmt.Println("\n[!] Peer closed chat stream.")
					return
				}
				fmt.Println("\n[!] Chat read error:", err)
				return
			}
			fmt.Printf("[peer] %s", line)
		}
	}()

	// Main input loop: chat lines or /send
	sc := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")
	for sc.Scan() {
		txt := sc.Text()
		if strings.HasPrefix(txt, "/send ") {
			path := strings.TrimSpace(strings.TrimPrefix(txt, "/send "))
			if path == "" {
				fmt.Println("[!] Usage: /send <path>")
				fmt.Print("> ")
				continue
			}
			if err := sendFile(qconn, path); err != nil {
				fmt.Println("[!] send error:", err)
			} else {
				fmt.Printf("[*] sent: %s\n", path)
			}
		} else {
			_, err := io.WriteString(chat, txt+"\n")
			if err != nil {
				fmt.Println("[!] chat write error:", err)
				break
			}
		}
		fmt.Print("> ")
	}
}

func roleFromPub(a, b string) string {
	// Simple, deterministic: lexicographically smaller string is "server".
	if a <= b {
		return "server"
	}
	return "client"
}

/*** QUIC file transfer (unidirectional stream)
Header format:
  [u16 nameLen][u64 fileSize][name bytes...][file bytes...]
Little-endian.
***/

func sendFile(conn quic.Connection, path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return fmt.Errorf("path is a directory")
	}

	name := filepath.Base(path)
	if len(name) > 65535 {
		return fmt.Errorf("filename too long")
	}
	size := fi.Size()

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Open a unidirectional stream
	us, err := conn.OpenUniStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer us.Close()

	// Write header
	hdr := make([]byte, 2+8)
	binary.LittleEndian.PutUint16(hdr[0:2], uint16(len(name)))
	binary.LittleEndian.PutUint64(hdr[2:10], uint64(size))
	if _, err := us.Write(hdr); err != nil {
		return err
	}
	if _, err := us.Write([]byte(name)); err != nil {
		return err
	}

	// Stream file
	_, err = io.Copy(us, f)
	return err
}

func receiveFilesForever(conn quic.Connection) {
	// Accept uni streams forever; save files into current dir
	go func() {
		for {
			us, err := conn.AcceptUniStream(context.Background())
			if err != nil {
				fmt.Println("[!] accept uni error:", err)
				return
			}
			go handleIncomingFile(us)
		}
	}()
}

func handleIncomingFile(us quic.ReceiveStream) {
	defer us.CancelRead(0)

	// Read header
	hdr := make([]byte, 10)
	if _, err := io.ReadFull(us, hdr); err != nil {
		fmt.Println("[!] recv hdr error:", err)
		return
	}
	nameLen := binary.LittleEndian.Uint16(hdr[0:2])
	size := binary.LittleEndian.Uint64(hdr[2:10])

	name := make([]byte, nameLen)
	if _, err := io.ReadFull(us, name); err != nil {
		fmt.Println("[!] recv name error:", err)
		return
	}
	fn := sanitizeFilename(string(name))
	fmt.Printf("[*] receiving file: %s (%d bytes)\n", fn, size)

	out, err := os.Create(fn)
	if err != nil {
		fmt.Println("[!] create file error:", err)
		return
	}
	defer out.Close()

	written, err := io.CopyN(out, us, int64(size))
	if err != nil {
		fmt.Println("[!] recv data error:", err)
		return
	}
	fmt.Printf("[*] saved %s (%d bytes)\n", fn, written)
}

func sanitizeFilename(s string) string {
	// keep it simple; strip path separators
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

/*** STUN helpers ***/

func stunDiscover(conn *net.UDPConn, server string, rto time.Duration) (*net.UDPAddr, error) {
	srv, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, err
	}

	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if _, err := conn.WriteToUDP(req.Raw, srv); err != nil {
		return nil, err
	}

	dead := time.Now().Add(3 * rto)
	buf := make([]byte, 1500)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(rto))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Now().After(dead) {
					return nil, fmt.Errorf("no STUN response from %s", server)
				}
				_, _ = conn.WriteToUDP(req.Raw, srv)
				continue
			}
			return nil, err
		}
		var m stun.Message
		m.Raw = buf[:n]
		if err := m.Decode(); err != nil {
			continue
		}
		if m.Type != stun.BindingSuccess {
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

func readPeerInfoFromStdin() (SelfInfo, error) {
	sb := &strings.Builder{}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		sb.WriteString(sc.Text())
	}
	if err := sc.Err(); err != nil {
		return SelfInfo{}, err
	}
	var peer SelfInfo
	if err := json.Unmarshal([]byte(sb.String()), &peer); err != nil {
		return SelfInfo{}, fmt.Errorf("parse peer JSON: %w", err)
	}
	if peer.Public == "" {
		return SelfInfo{}, fmt.Errorf("peer JSON missing 'public'")
	}
	return peer, nil
}

/*** TLS helpers ***/

func selfSignedTLS() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "wormy-quic"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

/*** misc ***/

func contextWithDeadline(d time.Duration) context.Context {
	ctx, _ := context.WithTimeout(context.Background(), d)
	return ctx
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// Try a list of STUN servers concurrently; return the first success.
func stunDiscoverAny(conn *net.UDPConn, servers []string, rto time.Duration) (*net.UDPAddr, error) {
	type result struct {
		addr *net.UDPAddr
		err  error
	}
	ch := make(chan result, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(server string) {
			defer wg.Done()
			addr, err := stunDiscoverIPv4(conn, server, rto)
			ch <- result{addr, err}
		}(s)
	}
	// return first success
	for i := 0; i < len(servers); i++ {
		res := <-ch
		if res.err == nil && res.addr != nil {
			// drain goroutines
			go func() {
				for i := 0; i < len(servers)-1; i++ {
					<-ch
				}
			}()
			return res.addr, nil
		}
	}
	wg.Wait()
	return nil, fmt.Errorf("no STUN response from any server")
}

// IPv4-only STUN Binding over the provided UDPConn.
func stunDiscoverIPv4(conn *net.UDPConn, server string, rto time.Duration) (*net.UDPAddr, error) {
	srv, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return nil, err
	}
	req := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)

	// Send bytes
	if _, err := conn.WriteToUDP(req.Raw, srv); err != nil {
		return nil, err
	}

	dead := time.Now().Add(3 * rto)
	buf := make([]byte, 1500)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(rto))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Now().After(dead) {
					return nil, fmt.Errorf("no STUN response from %s", server)
				}
				// Retransmit (STUN recommends exponential backoff; we keep it simple)
				_, _ = conn.WriteToUDP(req.Raw, srv)
				continue
			}
			return nil, err
		}
		// Optional: ensure the reply came from the server we queried
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

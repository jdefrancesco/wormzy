// mvp2_clean.go — Wormzy minimal P2P MVP with CPace + Noise + QUIC, with dev-loopback
package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
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

	cpace "filippo.io/cpace"
	"github.com/pion/stun"
	quic "github.com/quic-go/quic-go"

	noise "github.com/flynn/noise"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const alpn = "p2p-wormzy-1"

// abstracted connection (to avoid version-specific quic.Connection differences)
type qConn interface {
	OpenStreamSync(context.Context) (*quic.Stream, error)
	AcceptStream(context.Context) (*quic.Stream, error)
	OpenUniStreamSync(context.Context) (*quic.SendStream, error)
	AcceptUniStream(context.Context) (*quic.ReceiveStream, error)
}

// --- wire + info ---

type SelfInfo struct {
	Public string `json:"public"`
	Local  string `json:"local"`
}

type wire struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

func main() {
	var (
		mode     = flag.String("mode", "", "send or recv")
		filePath = flag.String("file", "", "file to send (send mode)")
		codeFlag = flag.String("code", "", "pairing code (optional on send; prompt on recv)")
		relay    = flag.String("relay", "127.0.0.1:9999", "rendezvous host:port")
		relayPin = flag.String("relay-pin", "", "base64(SHA256(SPKI)) pin for TLS relay (optional)")
		stunServ = flag.String("stun", "stun.cloudflare.com:3478", "STUN server")
		timeout  = flag.Duration("timeout", 60*time.Second, "punch timeout")
		loopback = flag.Bool("dev-loopback", false, "use peer.Local instead of peer.Public; also skip STUN")
	)
	flag.Parse()

	if *mode != "send" && *mode != "recv" {
		fatal("use -mode send|recv")
	}
	if *mode == "send" && *filePath == "" {
		fatal("send mode requires -file")
	}
	// Prompt for code on receiver if not provided
	if *mode == "recv" && *codeFlag == "" {
		fmt.Print("Enter code: ")
		in := bufio.NewReader(os.Stdin)
		line, _ := in.ReadString('\n')
		*codeFlag = strings.TrimSpace(line)
		if *codeFlag == "" {
			fatal("no code provided")
		}
	}

	// 1) Bind single UDP socket
	laddr4, _ := net.ResolveUDPAddr("udp4", "0.0.0.0:0")
	udpConn, err := net.ListenUDP("udp4", laddr4)
	check(err)
	defer udpConn.Close()
	fmt.Printf("[*] UDP bind: %s\n", udpConn.LocalAddr())

	// 2) Determine self info
	var self SelfInfo
	self.Local = udpConn.LocalAddr().String()
	if *loopback {
		self.Public = self.Local // local-only testing
		fmt.Printf("[*] DEV loopback: using local tuple %s as public\n", self.Public)
	} else {
		pub, err := stunDiscoverAny(udpConn, []string{
			*stunServ,
			"stun.l.google.com:19302",
			"stun1.l.google.com:19302",
			"stun2.l.google.com:19302",
		}, 4*time.Second)
		check(err)
		self.Public = pub.String()
	}
	fmt.Printf("[*] Public: %s\n", self.Public)

	// 3) Rendezvous + PAKE over relay
	peer, assigned, psk := rendezvousAndPAKE(*relay, *relayPin, *mode, *codeFlag, self)
	fmt.Printf("[*] Pairing code: %s\n", assigned)
	fmt.Printf("[*] Peer %s\n", peer.Public)

	// Address to dial/punch
	peerAddrStr := peer.Public
	if *loopback {
		peerAddrStr = peer.Local
	}
	peerUDP, err := net.ResolveUDPAddr("udp4", peerAddrStr)
	check(err)

	// 4) Start punching while QUIC connects
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	var wg sync.WaitGroup
	stopPunch := make(chan struct{})
	wg.Add(1)
	go func() {
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

	// 5) QUIC over same socket
	t := &quic.Transport{Conn: udpConn}
	serverTLS, _ := selfSignedTLS()
	serverTLS.NextProtos = []string{alpn}
	clientTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}}
	qconf := &quic.Config{KeepAlivePeriod: 15 * time.Second, EnableDatagrams: false}

	var qconn qConn
	if *mode == "send" {
		ln, err := t.Listen(serverTLS, qconf)
		check(err)
		fmt.Println("[*] QUIC listening...")
		conn, err := ln.Accept(contextWithDeadline(40 * time.Second))
		check(err)
		qconn = conn
		fmt.Printf("[*] QUIC accepted from %s\n", conn.RemoteAddr())
	} else {
		conn, err := t.Dial(contextWithDeadline(40*time.Second), peerUDP, clientTLS, qconf)
		check(err)
		qconn = conn
		fmt.Printf("[*] QUIC dialed %s\n", peerUDP)
	}

	close(stopPunch)
	cancel()
	wg.Wait()

	// 6) Noise (XX) on control stream → HKDF(psk, transcript) file key
	fileKey, err := runNoiseOverQUIC(qconn, *mode == "recv", psk)
	check(err)

	// 7) Send or receive
	if *mode == "send" {
		check(sendFileEncrypted(qconn, *filePath, fileKey))
		fmt.Println("[*] transfer complete")
	} else {
		receiveFilesEncryptedForever(qconn, fileKey)
		fmt.Println("[*] ready (receiving in background); Ctrl-C to exit")
		select {}
	}
}

// --- rendezvous / PAKE ---

func tlsPinnedConfig(pinB64 string) *tls.Config {
	var pin []byte
	if pinB64 != "" {
		if dec, err := base64.StdEncoding.DecodeString(pinB64); err == nil && len(dec) == 32 {
			pin = dec
		}
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"wormzy-rendezvous-1"},
		InsecureSkipVerify: pin == nil,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if pin == nil {
				return nil
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
			if !hmac.Equal(sum[:], pin) {
				return fmt.Errorf("relay SPKI pin mismatch")
			}
			return nil
		},
	}
}

func rendezvousAndPAKE(relayAddr, relayPin, mode, code string, me SelfInfo) (peer SelfInfo, outCode string, psk []byte) {
	var conn net.Conn
	var err error
	if relayPin != "" {
		conn, err = tls.Dial("tcp", relayAddr, tlsPinnedConfig(relayPin))
	} else {
		conn, err = tls.Dial("tcp", relayAddr, tlsPinnedConfig(""))
		if err != nil {
			conn, err = net.Dial("tcp", relayAddr)
		}
	}
	check(err)
	defer conn.Close()
	r := bufio.NewReader(conn)

	// hello
	_ = writeMsg(conn, "hello", map[string]string{"role": mode, "code": code})

	// code
	msg := mustReadMsg(r)
	if msg.Type != "code" {
		fatal("rendezvous: expected code")
	}
	var cb map[string]string
	_ = json.Unmarshal(msg.Body, &cb)
	outCode = cb["code"]
	fmt.Printf("[*] Pairing code: %s\n", outCode)

	// send self
	if err := writeMsg(conn, "self", me); err != nil {
		fatal(err.Error())
	}

	// CPace
	psk, err = runPAKEOverRelay(r, conn, mode, outCode, "send", "recv")
	check(err)

	// peer
	msg = mustReadMsg(r)
	if msg.Type != "peer" {
		fatal("rendezvous: expected peer")
	}
	_ = json.Unmarshal(msg.Body, &peer)
	return
}

func writeMsg(w io.Writer, typ string, body any) error {
	b, _ := json.Marshal(body)
	env := wire{Type: typ, Body: b}
	out, _ := json.Marshal(env)
	_, err := fmt.Fprintf(w, "%s\n", out)
	return err
}

func mustReadMsg(r *bufio.Reader) wire {
	line, err := r.ReadString('\n')
	check(err)
	var env wire
	check(json.Unmarshal([]byte(strings.TrimSpace(line)), &env))
	if env.Type == "err" {
		var e map[string]string
		_ = json.Unmarshal(env.Body, &e)
		fatal("relay error: " + e["error"])
	}
	return env
}

func runPAKEOverRelay(r *bufio.Reader, w io.Writer, role, code, idA, idB string) ([]byte, error) {
	ci := cpace.NewContextInfo(idA, idB, []byte("wormzy-pake-v1"))
	if role == "send" {
		msgA, st, err := cpace.Start(code, ci)
		if err != nil {
			return nil, err
		}
		if err := writeMsg(w, "pake1", msgA); err != nil {
			return nil, err
		}
		m := mustReadMsg(r)
		if m.Type != "pake1" {
			return nil, fmt.Errorf("want pake1, got %s", m.Type)
		}
		var msgB []byte
		_ = json.Unmarshal(m.Body, &msgB)
		keyA, err := st.Finish(msgB)
		if err != nil {
			return nil, err
		}
		_ = writeMsg(w, "pake2", []byte{})
		return keyA, nil
	}

	// recv role
	m := mustReadMsg(r)
	if m.Type != "pake1" {
		return nil, fmt.Errorf("want pake1, got %s", m.Type)
	}
	var msgA []byte
	_ = json.Unmarshal(m.Body, &msgA)
	msgB, keyB, err := cpace.Exchange(code, ci, msgA)
	if err != nil {
		return nil, err
	}
	if err := writeMsg(w, "pake1", msgB); err != nil {
		return nil, err
	}
	_ = mustReadMsg(r) // expect "pake2"
	return keyB, nil
}

// --- Noise(XX) with HKDF(psk, transcript) -> file key ---


func runNoiseOverQUIC(conn qConn, initiator bool, psk []byte) ([]byte, error) {
	var s *quic.Stream
	var err error
	ctx := context.Background()
	if initiator {
		s, err = conn.OpenStreamSync(ctx)
	} else {
		s, err = conn.AcceptStream(ctx)
	}
	if err != nil { return nil, err }
	defer s.Close()

	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashBLAKE2s)
	hs, err := noise.NewHandshakeState(noise.Config{
		Pattern:     noise.HandshakeNN, // no static keys required
		Initiator:   initiator,
		CipherSuite: suite,
		Prologue:    []byte("wormzy-noise-v1"),
		Random:      rand.Reader,
	})
	if err != nil { return nil, err }

	writeFrame := func(b []byte) error {
		if len(b) > 65535 { return fmt.Errorf("noise frame too large") }
		var hdr [2]byte
		binary.BigEndian.PutUint16(hdr[:], uint16(len(b)))
		if _, err := s.Write(hdr[:]); err != nil { return err }
		_, err := s.Write(b); return err
	}
	readFrame := func() ([]byte, error) {
		var ln uint16
		if err := binary.Read(s, binary.BigEndian, &ln); err != nil { return nil, err }
		buf := make([]byte, ln)
		_, err := io.ReadFull(s, buf)
		return buf, err
	}

	var transcript []byte
	appendT := func(b []byte) { transcript = append(transcript, b...) }

	if initiator {
		// -> e
		msg1, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil { return nil, err }
		appendT(msg1)
		if err := writeFrame(msg1); err != nil { return nil, err }

		// <- e, ee
		in2, err := readFrame(); if err != nil { return nil, err }
		appendT(in2)
		_, _, _, err = hs.ReadMessage(nil, in2); if err != nil { return nil, err }
	} else {
		// <- e
		in1, err := readFrame(); if err != nil { return nil, err }
		appendT(in1)
		_, _, _, err = hs.ReadMessage(nil, in1); if err != nil { return nil, err }

		// -> e, ee
		msg2, _, _, err := hs.WriteMessage(nil, nil)
		if err != nil { return nil, err }
		appendT(msg2)
		if err := writeFrame(msg2); err != nil { return nil, err }
	}

	// Derive single symmetric file key using PAKE psk mixed with transcript hash
	th := sha256.Sum256(transcript)
	fileKey := make([]byte, chacha20poly1305.KeySize)
	kdf := hkdf.New(sha256.New, psk, th[:], []byte("wormzy-filekey-v1"))
	if _, err := io.ReadFull(kdf, fileKey); err != nil { return nil, err }
	return fileKey, nil
}


// --- encrypted uni-stream transfer (XChaCha20-Poly1305) ---

type cipherAEAD interface {
	Seal(dst, nonce, plaintext, ad []byte) []byte
	Open(dst, nonce, ciphertext, ad []byte) ([]byte, error)
	NonceSize() int
}

type aeadWriter struct {
	w         io.Writer
	aead      cipherAEAD
	baseNonce [24]byte
	ctr       uint64
}
type aeadReader struct {
	r         io.Reader
	aead      cipherAEAD
	baseNonce [24]byte
	ctr       uint64
}

func makeNonce(base [24]byte, ctr uint64) []byte {
	b := base
	for i := 0; i < 8; i++ {
		b[23-i] ^= byte(ctr >> (8 * i))
	}
	return b[:]
}
func (w *aeadWriter) WriteChunk(p []byte) error {
	n := makeNonce(w.baseNonce, w.ctr)
	ct := w.aead.Seal(nil, n, p, nil)
	if err := binary.Write(w.w, binary.BigEndian, uint32(len(ct))); err != nil {
		return err
	}
	_, err := w.w.Write(ct)
	w.ctr++
	return err
}
func (r *aeadReader) ReadChunk() ([]byte, error) {
	var ln uint32
	if err := binary.Read(r.r, binary.BigEndian, &ln); err != nil {
		return nil, err
	}
	ct := make([]byte, ln)
	if _, err := io.ReadFull(r.r, ct); err != nil {
		return nil, err
	}
	n := makeNonce(r.baseNonce, r.ctr)
	pt, err := r.aead.Open(nil, n, ct, nil)
	if err != nil {
		return nil, err
	}
	r.ctr++
	return pt, nil
}

func sendFileEncrypted(conn qConn, path string, key []byte) error {
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

	us, err := conn.OpenUniStreamSync(context.Background())
	if err != nil {
		return err
	}
	defer us.Close()

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	var base [24]byte
	if _, err := rand.Read(base[:]); err != nil {
		return err
	}
	// send base nonce
	if _, err := us.Write(base[:]); err != nil {
		return err
	}
	aw := &aeadWriter{w: us, aead: aead, baseNonce: base}

	// header
	hdr := make([]byte, 10+len(name))
	binary.LittleEndian.PutUint16(hdr[0:2], uint16(len(name)))
	binary.LittleEndian.PutUint64(hdr[2:10], uint64(size))
	copy(hdr[10:], []byte(name))
	if err := aw.WriteChunk(hdr); err != nil {
		return err
	}

	buf := make([]byte, 1<<16)
	for {
		n, er := f.Read(buf)
		if n > 0 {
			if err := aw.WriteChunk(buf[:n]); err != nil {
				return err
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			return er
		}
	}
	return nil
}

func receiveFilesEncryptedForever(conn qConn, key []byte) {
	go func() {
		for {
			us, err := conn.AcceptUniStream(context.Background())
			if err != nil {
				fmt.Println("[!] accept uni:", err)
				return
			}
			go handleIncomingEncryptedFile(us, key)
		}
	}()
}

func handleIncomingEncryptedFile(us *quic.ReceiveStream, key []byte) {
	defer us.CancelRead(0)

	var base [24]byte
	if _, err := io.ReadFull(us, base[:]); err != nil {
		fmt.Println("[!] read base nonce:", err)
		return
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		fmt.Println("[!] aead:", err)
		return
	}
	ar := &aeadReader{r: us, aead: aead, baseNonce: base}

	hdr, err := ar.ReadChunk()
	if err != nil || len(hdr) < 10 {
		fmt.Println("[!] header:", err)
		return
	}
	nameLen := binary.LittleEndian.Uint16(hdr[0:2])
	size := binary.LittleEndian.Uint64(hdr[2:10])
	if int(10+nameLen) != len(hdr) {
		fmt.Println("[!] header name length mismatch")
		return
	}
	name := sanitizeFilename(string(hdr[10 : 10+nameLen]))
	fmt.Printf("[*] receiving: %s (%d bytes)\n", name, size)

	out, err := os.Create(name)
	if err != nil {
		fmt.Println("[!] create:", err)
		return
	}
	defer out.Close()

	var got int64
	for got < int64(size) {
		chunk, err := ar.ReadChunk()
		if err != nil {
			fmt.Println("[!] read chunk:", err)
			return
		}
		if _, err := out.Write(chunk); err != nil {
			fmt.Println("[!] write:", err)
			return
		}
		got += int64(len(chunk))
	}
	fmt.Printf("[*] saved %s\n", name)
}

// --- STUN helpers ---

func stunDiscoverAny(conn *net.UDPConn, servers []string, rto time.Duration) (*net.UDPAddr, error) {
	type res struct {
		addr *net.UDPAddr
		err  error
	}
	ch := make(chan res, len(servers))
	for _, s := range servers {
		server := s
		go func() {
			addr, err := stunDiscoverIPv4(conn, server, rto)
			ch <- res{addr, err}
		}()
	}
	var firstErr error
	for range servers {
		r := <-ch
		if r.err == nil && r.addr != nil {
			return r.addr, nil
		}
		if firstErr == nil {
			firstErr = r.err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no STUN response")
	}
	return nil, firstErr
}

func stunDiscoverIPv4(conn *net.UDPConn, server string, rto time.Duration) (*net.UDPAddr, error) {
	srv, err := net.ResolveUDPAddr("udp4", server)
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
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if time.Now().After(dead) {
					return nil, fmt.Errorf("no STUN from %s", server)
				}
				_, _ = conn.WriteToUDP(req.Raw, srv)
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

// --- utils ---

func sanitizeFilename(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

func selfSignedTLS() (*tls.Config, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "wormzy-quic"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{{
		Certificate: [][]byte{der},
		PrivateKey:  key,
	}}}, nil
}

func contextWithDeadline(d time.Duration) context.Context {
	ctx, _ := context.WithTimeout(context.Background(), d)
	return ctx
}

func check(err error) {
	if err != nil {
		fatal(err.Error())
	}
}

func fatal(s string) {
	fmt.Fprintln(os.Stderr, "error:", s)
	os.Exit(1)
}

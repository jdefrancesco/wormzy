package rendezvous

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	roleSend = "send"
	roleRecv = "recv"

	msgHello = "hello"
	msgSelf  = "self"
	msgCode  = "code"
	msgPeer  = "peer"
	msgErr   = "err"
	msgPAKE1 = "pake1"
	msgPAKE2 = "pake2"
)

// Server implements the rendezvous relay responsible for pairing senders and
// receivers, forwarding PAKE payloads, and ultimately swapping peer metadata.
type Server struct {
	Addr          string
	TLSCertFile   string
	TLSKeyFile    string
	Logger        *slog.Logger
	CodeGenerator func() string

	mu      sync.Mutex
	buckets map[string]*waiting
}

type waiting struct {
	mu       sync.RWMutex
	sendConn net.Conn
	sendInfo *SelfInfo
	recvConn net.Conn
	recvInfo *SelfInfo
}

// Hello is the initial message a client must send to the rendezvous server.
type Hello struct {
	Role string `json:"role"`
	Code string `json:"code"`
}

// SelfInfo carries the addresses a peer is willing to advertise.
type Candidate struct {
	Type     string `json:"type"`
	Proto    string `json:"proto"`
	Addr     string `json:"addr"`
	Priority int    `json:"priority"`
}

type SelfInfo struct {
	Public     string      `json:"public"`
	Local      string      `json:"local"`
	Candidates []Candidate `json:"candidates,omitempty"`
	Features   []string    `json:"features,omitempty"`
}

type message struct {
	Type string          `json:"type"`
	Body json.RawMessage `json:"body,omitempty"`
}

// ListenAndServe starts the rendezvous server and blocks until ctx is done or
// a fatal listener error occurs.
func (s *Server) ListenAndServe(ctx context.Context) error {
	addr := s.Addr
	if addr == "" {
		addr = ":9999"
	}

	base, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ln := base
	if s.TLSCertFile != "" || s.TLSKeyFile != "" {
		if s.TLSCertFile == "" || s.TLSKeyFile == "" {
			_ = base.Close()
			return errors.New("both tlscert and tlskey are required for TLS")
		}
		cert, err := tls.LoadX509KeyPair(s.TLSCertFile, s.TLSKeyFile)
		if err != nil {
			_ = base.Close()
			return err
		}
		ln = tls.NewListener(base, &tls.Config{
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"wormzy-rendezvous-1"},
			Certificates: []tls.Certificate{cert},
		})
		s.log().Info("rendezvous listening (TLS)", "addr", addr)
	} else {
		s.log().Info("rendezvous listening (plaintext)", "addr", addr)
	}

	errCh := make(chan error, 1)
	go func() {
		<-ctx.Done()
		errCh <- ln.Close()
	}()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case closeErr := <-errCh:
				wg.Wait()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return closeErr
			default:
				// If the context is done we should stop, otherwise keep accepting.
				if ctx.Err() != nil {
					wg.Wait()
					return ctx.Err()
				}
				s.log().Warn("accept error", "err", err)
				continue
			}
		}

		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			s.handleConn(ctx, c)
		}(conn)
	}
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	if ctx.Err() != nil {
		return
	}

	reader := bufio.NewReader(conn)
	msg, err := readMsg(reader)
	if err != nil {
		return
	}

	if msg.Type != msgHello {
		writeErr(conn, "expected hello")
		return
	}

	var hello Hello
	if err := json.Unmarshal(msg.Body, &hello); err != nil {
		writeErr(conn, "invalid hello")
		return
	}

	switch hello.Role {
	case roleSend:
		s.handlePeer(ctx, conn, reader, hello.Code, true)
	case roleRecv:
		s.handlePeer(ctx, conn, reader, hello.Code, false)
	default:
		writeErr(conn, "invalid role")
	}
}

func (s *Server) handlePeer(ctx context.Context, conn net.Conn, reader *bufio.Reader, code string, isSender bool) {
	if isSender && code == "" {
		code = s.generateCode()
	}

	if !isSender && code == "" {
		writeErr(conn, "missing code")
		return
	}

	w := s.bucket(code)
	var assignErr error
	if isSender {
		assignErr = w.setSender(conn)
	} else {
		assignErr = w.setReceiver(conn)
	}
	if assignErr != nil {
		writeErr(conn, assignErr.Error())
		return
	}

	if err := writeMsg(conn, msgCode, map[string]string{"code": code}); err != nil {
		return
	}

	info, err := expectSelf(reader)
	if err != nil {
		writeErr(conn, "expected self info")
		return
	}
	if isSender {
		w.setSenderInfo(info)
	} else {
		w.setReceiverInfo(info)
	}

	s.relayPAKE(code, w, conn, reader)
}

func (s *Server) relayPAKE(code string, bucket *waiting, conn net.Conn, reader *bufio.Reader) {
	for {
		msg, err := readMsg(reader)
		if err != nil {
			return
		}
		switch msg.Type {
		case msgPAKE1, msgPAKE2:
			other := bucket.otherConn(conn)
			for other == nil {
				if bucket.isClosed() {
					return
				}
				time.Sleep(25 * time.Millisecond)
				other = bucket.otherConn(conn)
			}
			if err := writeRaw(other, msg.Type, msg.Body); err != nil {
				s.log().Debug("forward PAKE payload", "err", err)
				return
			}
			if msg.Type == msgPAKE2 {
				s.tryPair(code, bucket)
				return
			}
		default:
			writeErr(conn, fmt.Sprintf("unexpected message %s", msg.Type))
			return
		}
	}
}

func (s *Server) tryPair(code string, bucket *waiting) {
	sendConn, sendInfo, recvConn, recvInfo, ok := bucket.snapshot()
	if !ok {
		return
	}

	if err := writeMsg(sendConn, msgPeer, recvInfo); err != nil {
		s.log().Debug("sending peer info to sender failed", "err", err)
	}
	if err := writeMsg(recvConn, msgPeer, sendInfo); err != nil {
		s.log().Debug("sending peer info to receiver failed", "err", err)
	}

	s.mu.Lock()
	delete(s.buckets, code)
	s.mu.Unlock()

	_ = sendConn.Close()
	_ = recvConn.Close()
	bucket.clear()
}

func (s *Server) bucket(code string) *waiting {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets == nil {
		s.buckets = make(map[string]*waiting)
	}
	w, ok := s.buckets[code]
	if !ok {
		w = &waiting{}
		s.buckets[code] = w
	}
	return w
}

func (s *Server) generateCode() string {
	if s.CodeGenerator != nil {
		return s.CodeGenerator()
	}
	return defaultCode()
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func defaultCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1<<24))
	if err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()%0xffffff)
	}
	val := n.Uint64()
	return fmt.Sprintf("%04x-%02x", val&0xffff, (val>>16)&0xff)
}

// GenerateCode returns a new human-friendly pairing code.
func GenerateCode() string {
	return defaultCode()
}

func expectSelf(r *bufio.Reader) (*SelfInfo, error) {
	msg, err := readMsg(r)
	if err != nil {
		return nil, err
	}
	if msg.Type != msgSelf {
		return nil, fmt.Errorf("expected self info, got %s", msg.Type)
	}
	var info SelfInfo
	if err := json.Unmarshal(msg.Body, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func readMsg(r *bufio.Reader) (message, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return message{}, err
	}
	line = strings.TrimSpace(line)
	var msg message
	return msg, json.Unmarshal([]byte(line), &msg)
}

func writeMsg(w io.Writer, typ string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return writeRaw(w, typ, payload)
}

func writeRaw(w io.Writer, typ string, raw json.RawMessage) error {
	env := message{Type: typ, Body: raw}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

func writeErr(w io.Writer, msg string) {
	_ = writeMsg(w, msgErr, map[string]string{"error": msg})
}

func (w *waiting) setSender(conn net.Conn) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sendConn != nil {
		return errors.New("code already used by sender")
	}
	w.sendConn = conn
	return nil
}

func (w *waiting) setReceiver(conn net.Conn) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.recvConn != nil {
		return errors.New("receiver already connected")
	}
	w.recvConn = conn
	return nil
}

func (w *waiting) setSenderInfo(info *SelfInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sendInfo = info
}

func (w *waiting) setReceiverInfo(info *SelfInfo) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recvInfo = info
}

func (w *waiting) otherConn(conn net.Conn) net.Conn {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if conn == w.sendConn {
		return w.recvConn
	}
	if conn == w.recvConn {
		return w.sendConn
	}
	return nil
}

func (w *waiting) isClosed() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.sendConn == nil && w.recvConn == nil
}

func (w *waiting) clear() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sendConn = nil
	w.recvConn = nil
	w.sendInfo = nil
	w.recvInfo = nil
}

func (w *waiting) snapshot() (net.Conn, *SelfInfo, net.Conn, *SelfInfo, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.sendConn == nil || w.recvConn == nil || w.sendInfo == nil || w.recvInfo == nil {
		return nil, nil, nil, nil, false
	}

	return w.sendConn, w.sendInfo, w.recvConn, w.recvInfo, true
}

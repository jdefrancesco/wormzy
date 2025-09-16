// relay.go - minimal rendezvous that forwards PAKE messages and swaps peer info
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

type hello struct {
	Role string `json:"role"` // "send" or "recv"
	Code string `json:"code"` // sender may leave empty; server assigns
}
type selfInfo struct {
	Public string `json:"public"`
	Local  string `json:"local"`
}
type wire struct {
	Type string          `json:"type"` // "hello" | "code" | "self" | "peer" | "pake1" | "pake2" | "err"
	Body json.RawMessage `json:"body,omitempty"`
}

type waiting struct {
	sendConn net.Conn
	sendInfo *selfInfo
	recvConn net.Conn
	recvInfo *selfInfo
}

var (
	mu      sync.Mutex
	pending = map[string]*waiting{}
)

func main() {
	addr := flag.String("addr", ":9999", "listen address")
	tlscert := flag.String("tlscert", "", "TLS cert (PEM)")
	tlskey := flag.String("tlskey", "", "TLS key (PEM)")
	flag.Parse()

	base, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	ln := base
	if *tlscert != "" && *tlskey != "" {
		cert, err := tls.LoadX509KeyPair(*tlscert, *tlskey)
		if err != nil {
			log.Fatal(err)
		}
		ln = tls.NewListener(base, &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{cert},
			NextProtos:   []string{"wormzy-rendezvous-1"},
		})
		log.Printf("rendezvous (TLS) listening on %s\n", *addr)
	} else {
		log.Printf("rendezvous (PLAINTEXT) listening on %s\n", *addr)
	}

	for {
		c, err := ln.Accept()
		if err != nil {
			log.Println("accept:", err)
			continue
		}
		_ = c.SetDeadline(time.Time{})
		go handleConn(c)
	}
}

func handleConn(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)

	// expect hello
	msg, err := readMsg(r)
	if err != nil {
		return
	}
	if msg.Type != "hello" {
		writeErr(c, "expected hello")
		return
	}
	var h hello
	_ = json.Unmarshal(msg.Body, &h)

	switch h.Role {
	case "send":
		code := h.Code
		if code == "" {
			code = genCode()
		}
		w := bucket(code)
		if w.sendConn != nil {
			writeErr(c, "code already used by sender")
			return
		}
		w.sendConn = c
		writeMsg(c, "code", map[string]string{"code": code})
		me, err := expectSelf(r)
		if err != nil {
			return
		}
		w.sendInfo = me
		if ok := relayPAKEUntilPaired(code, w, c, r); !ok {
			return
		}
	case "recv":
		if h.Code == "" {
			writeErr(c, "missing code")
			return
		}
		w := bucket(h.Code)
		if w.recvConn != nil {
			writeErr(c, "receiver already connected")
			return
		}
		w.recvConn = c
		writeMsg(c, "code", map[string]string{"code": h.Code})
		me, err := expectSelf(r)
		if err != nil {
			return
		}
		w.recvInfo = me
		if ok := relayPAKEUntilPaired(h.Code, w, c, r); !ok {
			return
		}
	default:
		writeErr(c, "invalid role")
	}
}

func relayPAKEUntilPaired(code string, w *waiting, meConn net.Conn, meReader *bufio.Reader) bool {
	other := func() net.Conn {
		if meConn == w.sendConn {
			return w.recvConn
		}
		return w.sendConn
	}
	for {
		if other() == nil {
			time.Sleep(50 * time.Millisecond)
		}
		msg, err := readMsg(meReader)
		if err != nil {
			return false
		}
		switch msg.Type {
		case "pake1", "pake2":
			oc := other()
			if oc == nil {
				for oc == nil {
					time.Sleep(25 * time.Millisecond)
					oc = other()
				}
			}
			writeMsg(oc, msg.Type, json.RawMessage(msg.Body))
			if msg.Type == "pake2" {
				tryPair(code, w)
				return true
			}
		default:
			return false
		}
	}
}

func expectSelf(r *bufio.Reader) (*selfInfo, error) {
	msg, err := readMsg(r)
	if err != nil {
		return nil, err
	}
	if msg.Type != "self" {
		return nil, io.ErrUnexpectedEOF
	}
	var s selfInfo
	if err := json.Unmarshal(msg.Body, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func bucket(code string) *waiting {
	mu.Lock()
	defer mu.Unlock()
	w, ok := pending[code]
	if !ok {
		w = &waiting{}
		pending[code] = w
	}
	return w
}

func tryPair(code string, w *waiting) {
	mu.Lock()
	defer mu.Unlock()
	if w.sendConn == nil || w.recvConn == nil || w.sendInfo == nil || w.recvInfo == nil {
		return
	}
	writeMsg(w.sendConn, "peer", w.recvInfo)
	writeMsg(w.recvConn, "peer", w.sendInfo)
	_ = w.sendConn.Close()
	_ = w.recvConn.Close()
	delete(pending, code)
}

func genCode() string {
	return fmt.Sprintf("%04x-%02x", time.Now().UnixNano()&0xffff, (time.Now().UnixNano()>>16)&0xff)
}

func writeMsg(w io.Writer, typ string, body any) {
	b, _ := json.Marshal(body)
	env := wire{Type: typ, Body: b}
	out, _ := json.Marshal(env)
	fmt.Fprintf(w, "%s\n", out)
}

func writeErr(w io.Writer, s string) { writeMsg(w, "err", map[string]string{"error": s}) }

func readMsg(r *bufio.Reader) (wire, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return wire{}, err
	}
	line = strings.TrimSpace(line)
	var env wire
	return env, json.Unmarshal([]byte(line), &env)
}

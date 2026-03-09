package rendezvous

import (
	"net"
	"regexp"
	"testing"
)

func TestDefaultCodeFormat(t *testing.T) {
	rx := regexp.MustCompile(`^[0-9a-f]{4}-[0-9a-f]{2}$`)
	for i := 0; i < 10; i++ {
		code := defaultCode()
		if !rx.MatchString(code) {
			t.Fatalf("code %q does not match expected pattern", code)
		}
	}
}

func TestWaitingLifecycle(t *testing.T) {
	w := &waiting{}

	sender, recv := net.Pipe()
	defer sender.Close()
	defer recv.Close()

	if err := w.setSender(sender); err != nil {
		t.Fatalf("unexpected error setting sender: %v", err)
	}
	if err := w.setSender(sender); err == nil {
		t.Fatalf("expected error re-setting sender")
	}

	if err := w.setReceiver(recv); err != nil {
		t.Fatalf("unexpected error setting receiver: %v", err)
	}
	if err := w.setReceiver(recv); err == nil {
		t.Fatalf("expected error re-setting receiver")
	}

	info := &SelfInfo{Public: "1.2.3.4:1234", Local: "10.0.0.1:5678"}
	w.setSenderInfo(info)
	w.setReceiverInfo(info)

	sendConn, sendInfo, recvConn, recvInfo, ok := w.snapshot()
	if !ok || sendConn != sender || recvConn != recv {
		t.Fatalf("unexpected snapshot: %v %v", ok, w)
	}
	if sendInfo.Public != info.Public || recvInfo.Local != info.Local {
		t.Fatalf("snapshot missed self info")
	}

	w.clear()
	if !w.isClosed() {
		t.Fatalf("waiting should be closed after clear")
	}
}

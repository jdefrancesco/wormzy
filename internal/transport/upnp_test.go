package transport

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type fakeUPnPClient struct {
	externalIP string
	addErr     error
	failPorts  map[uint16]error
	added      []uint16
	deleted    []uint16
}

func (f *fakeUPnPClient) AddPortMappingCtx(
	ctx context.Context,
	NewRemoteHost string,
	NewExternalPort uint16,
	NewProtocol string,
	NewInternalPort uint16,
	NewInternalClient string,
	NewEnabled bool,
	NewPortMappingDescription string,
	NewLeaseDuration uint32,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if NewRemoteHost != "" {
		return errors.New("unexpected remote host")
	}
	if NewProtocol != upnpProtocolUDP {
		return errors.New("unexpected protocol")
	}
	if NewInternalPort == 0 || NewInternalClient == "" || !NewEnabled {
		return errors.New("invalid mapping request")
	}
	if NewPortMappingDescription != upnpDescription {
		return errors.New("unexpected description")
	}
	if NewLeaseDuration == 0 {
		return errors.New("missing lease")
	}
	if f.addErr != nil {
		return f.addErr
	}
	if err := f.failPorts[NewExternalPort]; err != nil {
		return err
	}
	f.added = append(f.added, NewExternalPort)
	return nil
}

func (f *fakeUPnPClient) DeletePortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if NewRemoteHost != "" {
		return errors.New("unexpected remote host")
	}
	if NewProtocol != upnpProtocolUDP {
		return errors.New("unexpected protocol")
	}
	f.deleted = append(f.deleted, NewExternalPort)
	return nil
}

func (f *fakeUPnPClient) GetExternalIPAddressCtx(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return f.externalIP, nil
}

func TestMapUPnPPortCreatesAndCleansMapping(t *testing.T) {
	client := &fakeUPnPClient{externalIP: "8.8.8.8"}
	mapping, err := mapUPnPPort(
		context.Background(),
		net.IPv4(192, 168, 1, 20),
		42424,
		"8.8.8.8:60000",
		time.Hour,
		func(context.Context) ([]upnpPortMapper, error) {
			return []upnpPortMapper{client}, nil
		},
	)
	if err != nil {
		t.Fatalf("mapUPnPPort err: %v", err)
	}
	if mapping.externalAddr != "8.8.8.8:42424" {
		t.Fatalf("unexpected external addr: %s", mapping.externalAddr)
	}
	if len(client.added) != 1 || client.added[0] != 42424 {
		t.Fatalf("unexpected added ports: %+v", client.added)
	}
	if err := mapping.Close(context.Background()); err != nil {
		t.Fatalf("cleanup err: %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 42424 {
		t.Fatalf("unexpected deleted ports: %+v", client.deleted)
	}
}

func TestMapUPnPPortTriesAlternateExternalPort(t *testing.T) {
	client := &fakeUPnPClient{
		externalIP: "8.8.4.4",
		failPorts:  map[uint16]error{42424: errors.New("port conflict")},
	}
	mapping, err := mapUPnPPort(
		context.Background(),
		net.IPv4(192, 168, 1, 20),
		42424,
		"8.8.4.4:60000",
		time.Hour,
		func(context.Context) ([]upnpPortMapper, error) {
			return []upnpPortMapper{client}, nil
		},
	)
	if err != nil {
		t.Fatalf("mapUPnPPort err: %v", err)
	}
	if mapping.externalAddr == "8.8.4.4:42424" {
		t.Fatalf("expected alternate external port, got %s", mapping.externalAddr)
	}
	if len(client.added) != 1 || client.added[0] == 42424 {
		t.Fatalf("unexpected added ports: %+v", client.added)
	}
}

func TestMapUPnPPortRejectsExternalIPMismatch(t *testing.T) {
	client := &fakeUPnPClient{externalIP: "8.8.8.8"}
	_, err := mapUPnPPort(
		context.Background(),
		net.IPv4(192, 168, 1, 20),
		42424,
		"1.1.1.1:60000",
		time.Hour,
		func(context.Context) ([]upnpPortMapper, error) {
			return []upnpPortMapper{client}, nil
		},
	)
	if err == nil {
		t.Fatalf("expected mismatch error")
	}
	if len(client.added) != 0 {
		t.Fatalf("should not add mapping on mismatch: %+v", client.added)
	}
}

func TestUsableExternalIPv4RejectsPrivateAndCGNAT(t *testing.T) {
	if usableExternalIPv4("192.168.1.1") != nil {
		t.Fatalf("private address should be rejected")
	}
	if usableExternalIPv4("100.64.1.1") != nil {
		t.Fatalf("CGNAT address should be rejected")
	}
	if usableExternalIPv4("8.8.8.8") == nil {
		t.Fatalf("public address should be accepted")
	}
}

func TestUPnPTimeoutAllowsDiscoveryAndSOAPCalls(t *testing.T) {
	tests := []struct {
		name      string
		handshake time.Duration
		want      time.Duration
	}{
		{name: "default", handshake: 0, want: 8 * time.Second},
		{name: "normal handshake", handshake: 90 * time.Second, want: 8 * time.Second},
		{name: "medium handshake", handshake: 60 * time.Second, want: 6 * time.Second},
		{name: "short handshake retains minimum discovery budget", handshake: 20 * time.Second, want: 4 * time.Second},
		{name: "long handshake stays bounded", handshake: 5 * time.Minute, want: 8 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upnpTimeout(tt.handshake); got != tt.want {
				t.Fatalf("upnpTimeout(%s) = %s; want %s", tt.handshake, got, tt.want)
			}
		})
	}
}

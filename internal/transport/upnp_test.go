package transport

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

type fakeUPnPClient struct {
	externalIP         string
	addErr             error
	addFunc            func(context.Context, uint16) error
	failPorts          map[uint16]error
	added              []uint16
	addAttempts        []uint16
	internalClients    []string
	deleted            []uint16
	deleteErrs         []error
	deleteCalls        int
	specific           map[uint16]fakeUPnPMappingEntry
	specificErr        error
	specificLookup     []uint16
	lastDescription    string
	useLastDescription bool
}

type fakeUPnPMappingEntry struct {
	internalPort uint16
	internalIP   string
	enabled      bool
	description  string
	lease        uint32
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
	f.addAttempts = append(f.addAttempts, NewExternalPort)
	f.internalClients = append(f.internalClients, NewInternalClient)
	f.lastDescription = NewPortMappingDescription
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
	if !strings.HasPrefix(NewPortMappingDescription, upnpDescriptionPrefix) {
		return errors.New("unexpected description")
	}
	if NewLeaseDuration == 0 {
		return errors.New("missing lease")
	}
	if f.addFunc != nil {
		return f.addFunc(ctx, NewExternalPort)
	}
	if f.addErr != nil {
		return f.addErr
	}
	if err := f.failPorts[NewExternalPort]; err != nil {
		return err
	}
	if f.specific == nil {
		f.specific = make(map[uint16]fakeUPnPMappingEntry)
	}
	f.specific[NewExternalPort] = fakeUPnPMappingEntry{
		internalPort: NewInternalPort,
		internalIP:   NewInternalClient,
		enabled:      NewEnabled,
		description:  NewPortMappingDescription,
		lease:        NewLeaseDuration,
	}
	f.added = append(f.added, NewExternalPort)
	return nil
}

// fakeUPnPDiscoverer pairs test clients with the interface that discovered them.
func fakeUPnPDiscoverer(localIP net.IP, clients ...upnpPortMapper) func(context.Context) ([]discoveredUPnPPortMapper, error) {
	return func(context.Context) ([]discoveredUPnPPortMapper, error) {
		mappers := make([]discoveredUPnPPortMapper, 0, len(clients))
		for _, client := range clients {
			mappers = append(mappers, discoveredUPnPPortMapper{
				client:  client,
				localIP: append(net.IP(nil), localIP...),
			})
		}
		return mappers, nil
	}
}

func (f *fakeUPnPClient) DeletePortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string) error {
	f.deleteCalls++
	if err := ctx.Err(); err != nil {
		return err
	}
	if NewRemoteHost != "" {
		return errors.New("unexpected remote host")
	}
	if NewProtocol != upnpProtocolUDP {
		return errors.New("unexpected protocol")
	}
	if len(f.deleteErrs) > 0 {
		err := f.deleteErrs[0]
		f.deleteErrs = f.deleteErrs[1:]
		if err != nil {
			return err
		}
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

// GetSpecificPortMappingEntryCtx returns the configured router entry for reconciliation tests.
func (f *fakeUPnPClient) GetSpecificPortMappingEntryCtx(
	ctx context.Context,
	NewRemoteHost string,
	NewExternalPort uint16,
	NewProtocol string,
) (uint16, string, bool, string, uint32, error) {
	f.specificLookup = append(f.specificLookup, NewExternalPort)
	if err := ctx.Err(); err != nil {
		return 0, "", false, "", 0, err
	}
	if NewRemoteHost != "" {
		return 0, "", false, "", 0, errors.New("unexpected remote host")
	}
	if NewProtocol != upnpProtocolUDP {
		return 0, "", false, "", 0, errors.New("unexpected protocol")
	}
	if f.specificErr != nil {
		return 0, "", false, "", 0, f.specificErr
	}
	entry, ok := f.specific[NewExternalPort]
	if !ok {
		return 0, "", false, "", 0, errors.New("mapping not found")
	}
	if f.useLastDescription {
		entry.description = f.lastDescription
	}
	return entry.internalPort, entry.internalIP, entry.enabled, entry.description, entry.lease, nil
}

func TestMapUPnPPortCreatesAndCleansMapping(t *testing.T) {
	client := &fakeUPnPClient{externalIP: "8.8.8.8"}
	mapping, err := mapUPnPPort(
		context.Background(),
		net.IPv4(192, 168, 1, 20),
		42424,
		"8.8.8.8:60000",
		time.Hour,
		fakeUPnPDiscoverer(net.IPv4(192, 168, 1, 20), client),
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
	if err := mapping.Close(context.Background()); err != nil {
		t.Fatalf("repeated cleanup err: %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 42424 {
		t.Fatalf("unexpected deleted ports: %+v", client.deleted)
	}
}

// TestMapUPnPPort_UsesDiscoveredInterfaceForWildcardSocket verifies multihomed
// hosts map the shared UDP port to the interface that reached the selected IGD.
func TestMapUPnPPort_UsesDiscoveredInterfaceForWildcardSocket(t *testing.T) {
	client := &fakeUPnPClient{externalIP: "8.8.8.8"}
	discoveredIP := net.IPv4(10, 20, 30, 40)

	mapping, err := mapUPnPPort(
		context.Background(),
		nil,
		42424,
		"8.8.8.8:60000",
		time.Hour,
		fakeUPnPDiscoverer(discoveredIP, client),
	)
	if err != nil {
		t.Fatalf("mapUPnPPort err: %v", err)
	}
	if mapping == nil || !mapping.internalIP.Equal(discoveredIP) {
		t.Fatalf("mapping internal IP = %v; want discovered interface %s", mapping, discoveredIP)
	}
	if len(client.internalClients) != 1 || client.internalClients[0] != discoveredIP.String() {
		t.Fatalf("mapped internal clients = %v; want %s", client.internalClients, discoveredIP)
	}
}

// TestUPnPInternalEndpoint_WildcardDoesNotGuessInterface verifies mapper
// selection, rather than interface enumeration order, supplies InternalClient.
func TestUPnPInternalEndpoint_WildcardDoesNotGuessInterface(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	internalIP, internalPort, err := upnpInternalEndpoint(conn)
	if err != nil {
		t.Fatalf("upnpInternalEndpoint: %v", err)
	}
	if internalIP != nil {
		t.Fatalf("internal IP = %s; want nil for wildcard socket", internalIP)
	}
	if internalPort == 0 {
		t.Fatal("internal port = 0; want bound UDP port")
	}
}

// TestMapUPnPPort_SkipsDifferentExplicitInterface verifies an explicitly bound
// socket is never mapped through an IGD discovered from another interface.
func TestMapUPnPPort_SkipsDifferentExplicitInterface(t *testing.T) {
	client := &fakeUPnPClient{externalIP: "8.8.8.8"}
	_, err := mapUPnPPort(
		context.Background(),
		net.IPv4(192, 168, 1, 20),
		42424,
		"8.8.8.8:60000",
		time.Hour,
		fakeUPnPDiscoverer(net.IPv4(10, 20, 30, 40), client),
	)
	if err == nil {
		t.Fatal("expected mapper on a different interface to be rejected")
	}
	if len(client.addAttempts) != 0 {
		t.Fatalf("mapping attempts = %v; want none", client.addAttempts)
	}
}

// TestCleanupUPnPMapping_RetriesTransientFailure verifies a failed deletion is not cached permanently.
func TestCleanupUPnPMapping_RetriesTransientFailure(t *testing.T) {
	const description = "wormzy-test-cleanup"
	client := &fakeUPnPClient{
		deleteErrs: []error{errors.New("temporary router timeout")},
		specific: map[uint16]fakeUPnPMappingEntry{
			42424: {
				internalPort: 42424,
				internalIP:   "192.168.1.20",
				enabled:      true,
				description:  description,
			},
		},
	}
	mapping := &upnpMapping{
		client:       client,
		externalAddr: "8.8.8.8:42424",
		externalPort: 42424,
		internalIP:   net.IPv4(192, 168, 1, 20),
		internalPort: 42424,
		description:  description,
	}

	cleanupUPnPMapping(mapping, nil)

	if client.deleteCalls != 2 {
		t.Fatalf("delete calls = %d; want 2", client.deleteCalls)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 42424 {
		t.Fatalf("successful deletions = %#v; want port 42424", client.deleted)
	}
}

// TestUPnPMappingClose_RefusesReplacedMapping verifies cleanup cannot delete a later owner's entry.
func TestUPnPMappingClose_RefusesReplacedMapping(t *testing.T) {
	const description = "wormzy-current-owner"
	client := &fakeUPnPClient{
		specific: map[uint16]fakeUPnPMappingEntry{
			42424: {
				internalPort: 42424,
				internalIP:   "192.168.1.20",
				enabled:      true,
				description:  "wormzy-different-owner",
			},
		},
	}
	mapping := &upnpMapping{
		client:       client,
		externalAddr: "8.8.8.8:42424",
		externalPort: 42424,
		internalIP:   net.IPv4(192, 168, 1, 20),
		internalPort: 42424,
		description:  description,
	}

	if err := mapping.Close(context.Background()); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if client.deleteCalls != 0 {
		t.Fatalf("delete calls = %d; want 0 for replaced mapping", client.deleteCalls)
	}
}

// TestReconcileUPnPMapping_RejectsOlderWormzyNonce verifies ambiguous adds cannot claim an older entry.
func TestReconcileUPnPMapping_RejectsOlderWormzyNonce(t *testing.T) {
	client := &fakeUPnPClient{
		specific: map[uint16]fakeUPnPMappingEntry{
			42424: {
				internalPort: 42424,
				internalIP:   "192.168.1.20",
				enabled:      true,
				description:  "wormzy-older-nonce",
			},
		},
	}

	owned, err := reconcileUPnPMapping(
		context.Background(),
		client,
		42424,
		net.IPv4(192, 168, 1, 20),
		42424,
		"wormzy-newer-nonce",
	)
	if err != nil {
		t.Fatalf("reconcileUPnPMapping returned error: %v", err)
	}
	if owned {
		t.Fatal("older Wormzy mapping must not be claimed")
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
		fakeUPnPDiscoverer(net.IPv4(192, 168, 1, 20), client),
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

// TestMapUPnPPortRecoversAppliedMappingAfterCanceledAdd verifies ambiguous mutations remain cleanup-visible.
func TestMapUPnPPortRecoversAppliedMappingAfterCanceledAdd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeUPnPClient{
		externalIP:         "8.8.8.8",
		useLastDescription: true,
		specific: map[uint16]fakeUPnPMappingEntry{
			42424: {
				internalPort: 42424,
				internalIP:   "192.168.1.20",
				enabled:      true,
				description:  "replaced at lookup",
				lease:        3600,
			},
		},
	}
	client.addFunc = func(callCtx context.Context, _ uint16) error {
		cancel()
		return callCtx.Err()
	}

	mapping, err := mapUPnPPort(
		ctx,
		net.IPv4(192, 168, 1, 20),
		42424,
		"8.8.8.8:60000",
		time.Hour,
		fakeUPnPDiscoverer(net.IPv4(192, 168, 1, 20), client),
	)
	if err != nil {
		t.Fatalf("mapUPnPPort err: %v", err)
	}
	if mapping == nil || mapping.externalPort != 42424 {
		t.Fatalf("mapping = %#v; want recovered port 42424", mapping)
	}
	if len(client.addAttempts) != 1 || len(client.specificLookup) != 1 {
		t.Fatalf("add attempts = %v, lookups = %v; want one of each", client.addAttempts, client.specificLookup)
	}
	if err := mapping.Close(context.Background()); err != nil {
		t.Fatalf("cleanup recovered mapping: %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != 42424 {
		t.Fatalf("deleted ports = %v; want recovered port 42424", client.deleted)
	}
}

// TestMapUPnPPortRejectsUnrelatedMappingAfterCanceledAdd verifies reconciliation never claims another mapping.
func TestMapUPnPPortRejectsUnrelatedMappingAfterCanceledAdd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeUPnPClient{
		externalIP:         "8.8.8.8",
		useLastDescription: true,
		specific: map[uint16]fakeUPnPMappingEntry{
			42424: {
				internalPort: 42424,
				internalIP:   "192.168.1.99",
				enabled:      true,
				description:  "replaced at lookup",
				lease:        3600,
			},
		},
	}
	client.addFunc = func(callCtx context.Context, _ uint16) error {
		cancel()
		return callCtx.Err()
	}

	mapping, err := mapUPnPPort(
		ctx,
		net.IPv4(192, 168, 1, 20),
		42424,
		"8.8.8.8:60000",
		time.Hour,
		fakeUPnPDiscoverer(net.IPv4(192, 168, 1, 20), client),
	)
	if err == nil {
		t.Fatal("expected canceled AddPortMapping error")
	}
	if mapping != nil {
		t.Fatalf("mapping = %#v; want nil for unrelated entry", mapping)
	}
	if len(client.addAttempts) != 1 {
		t.Fatalf("add attempts = %v; want no alternate ports after cancellation", client.addAttempts)
	}
	if len(client.specificLookup) != 1 || client.specificLookup[0] != 42424 {
		t.Fatalf("specific lookups = %v; want only port 42424", client.specificLookup)
	}
	if len(client.deleted) != 0 {
		t.Fatalf("deleted ports = %v; unrelated mapping must not be deleted", client.deleted)
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
		fakeUPnPDiscoverer(net.IPv4(192, 168, 1, 20), client),
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
	for _, address := range []string{"192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1"} {
		if usableExternalIPv4(address) != nil {
			t.Fatalf("special-use address %s should be rejected", address)
		}
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

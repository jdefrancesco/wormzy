package transport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	defaultUPnPLease            = 2 * time.Hour
	defaultUPnPTimeout          = 8 * time.Second
	minimumUPnPTimeout          = 4 * time.Second
	defaultUPnPCleanupTimeout   = 3 * time.Second
	defaultUPnPReconcileTimeout = 3 * time.Second
	upnpDescriptionPrefix       = "wormzy-"
	upnpProtocolUDP             = "UDP"
)

type upnpPortMapper interface {
	AddPortMappingCtx(
		ctx context.Context,
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
		NewInternalPort uint16,
		NewInternalClient string,
		NewEnabled bool,
		NewPortMappingDescription string,
		NewLeaseDuration uint32,
	) error
	DeletePortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	GetSpecificPortMappingEntryCtx(
		ctx context.Context,
		NewRemoteHost string,
		NewExternalPort uint16,
		NewProtocol string,
	) (
		NewInternalPort uint16,
		NewInternalClient string,
		NewEnabled bool,
		NewPortMappingDescription string,
		NewLeaseDuration uint32,
		err error,
	)
}

// discoveredUPnPPortMapper binds an IGD client to the local interface used to discover it.
type discoveredUPnPPortMapper struct {
	client  upnpPortMapper
	localIP net.IP
}

type upnpMapping struct {
	client       upnpPortMapper
	externalAddr string
	externalPort uint16
	internalIP   net.IP
	internalPort uint16
	description  string
	closeMu      sync.Mutex
	closed       bool
}

// Close removes the temporary router mapping, allowing retry after a failed deletion.
func (m *upnpMapping) Close(ctx context.Context) error {
	if m == nil || m.client == nil || m.externalPort == 0 {
		return nil
	}
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	if m.closed {
		return nil
	}
	owned, err := upnpMappingMatches(
		ctx,
		m.client,
		m.externalPort,
		m.internalIP,
		m.internalPort,
		m.description,
	)
	if err != nil {
		return err
	}
	if !owned {
		m.closed = true
		return nil
	}
	if err := m.client.DeletePortMappingCtx(ctx, "", m.externalPort, upnpProtocolUDP); err != nil {
		return err
	}
	m.closed = true
	return nil
}

// setupUPnPMapping creates a temporary mapping when UPnP is enabled for the transfer.
func setupUPnPMapping(ctx context.Context, cfg Config, conn *net.UDPConn, publicAddr string, rep Reporter) (*upnpMapping, error) {
	if cfg.DisableUPnP || cfg.Loopback {
		return nil, nil
	}
	if conn == nil {
		return nil, nil
	}
	timeout := upnpTimeout(cfg.HandshakeTimeout)
	upnpCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	mapping, err := createUPnPMapping(upnpCtx, conn, publicAddr, defaultUPnPLease, discoverUPnPPortMappers)
	if err != nil {
		if rep != nil {
			rep.Logf("upnp/map failed: %v", err)
		}
		return nil, err
	}
	if rep != nil {
		rep.Logf("upnp/map external=%s lease=%s", mapping.externalAddr, defaultUPnPLease)
	}
	return mapping, nil
}

// upnpTimeout bounds discovery and mapping work relative to the handshake timeout.
func upnpTimeout(handshakeTimeout time.Duration) time.Duration {
	if handshakeTimeout <= 0 {
		return defaultUPnPTimeout
	}
	return boundedDuration(handshakeTimeout/10, minimumUPnPTimeout, defaultUPnPTimeout)
}

// createUPnPMapping maps conn's port through an IGD reached from a compatible local interface.
func createUPnPMapping(
	ctx context.Context,
	conn *net.UDPConn,
	publicAddr string,
	lease time.Duration,
	discover func(context.Context) ([]discoveredUPnPPortMapper, error),
) (*upnpMapping, error) {
	internalIP, internalPort, err := upnpInternalEndpoint(conn)
	if err != nil {
		return nil, err
	}
	return mapUPnPPort(ctx, internalIP, internalPort, publicAddr, lease, discover)
}

// upnpInternalEndpoint returns the socket's optional bound IPv4 address and required port.
func upnpInternalEndpoint(conn *net.UDPConn) (net.IP, uint16, error) {
	if conn == nil {
		return nil, 0, errors.New("nil UDP conn")
	}
	endpoint, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || endpoint == nil {
		return nil, 0, errors.New("UDP conn has an invalid local address")
	}
	if endpoint.Port <= 0 || endpoint.Port > 65535 {
		return nil, 0, fmt.Errorf("invalid local UDP port %d", endpoint.Port)
	}
	if endpoint.IP == nil || endpoint.IP.IsUnspecified() {
		return nil, uint16(endpoint.Port), nil
	}
	ip := endpoint.IP.To4()
	if ip == nil {
		return nil, 0, fmt.Errorf("UPnP requires an IPv4 local address, got %q", endpoint.IP)
	}
	if !isPrivateUPnPIPv4(ip) {
		return nil, 0, fmt.Errorf("UPnP requires a LAN IPv4 address, got %s", ip)
	}
	return append(net.IP(nil), ip...), uint16(endpoint.Port), nil
}

// mapUPnPPort selects an IGD and maps the UDP port to that IGD's discovery interface.
func mapUPnPPort(
	ctx context.Context,
	boundIP net.IP,
	internalPort uint16,
	publicAddr string,
	lease time.Duration,
	discover func(context.Context) ([]discoveredUPnPPortMapper, error),
) (*upnpMapping, error) {
	if boundIP != nil && !boundIP.IsUnspecified() {
		boundIP = boundIP.To4()
		if !isPrivateUPnPIPv4(boundIP) {
			return nil, errors.New("UPnP requires a private IPv4 bound address")
		}
	} else {
		boundIP = nil
	}
	if internalPort == 0 {
		return nil, errors.New("UPnP requires a non-zero internal port")
	}
	if discover == nil {
		discover = discoverUPnPPortMappers
	}
	description, err := newUPnPMappingDescription()
	if err != nil {
		return nil, err
	}

	mappers, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(mappers) == 0 {
		return nil, errors.New("no UPnP IGD services found")
	}

	leaseSeconds := upnpLeaseSeconds(lease)
	var firstErr error
	for _, mapper := range mappers {
		client := mapper.client
		if client == nil {
			continue
		}
		internalIP := mapper.localIP.To4()
		if !isPrivateUPnPIPv4(internalIP) {
			firstErr = rememberFirstErr(firstErr, errors.New("UPnP mapper has no valid discovery interface"))
			continue
		}
		if boundIP != nil && !internalIP.Equal(boundIP) {
			firstErr = rememberFirstErr(
				firstErr,
				fmt.Errorf("UPnP mapper interface %s does not match UDP socket interface %s", internalIP, boundIP),
			)
			continue
		}
		externalIPText, err := client.GetExternalIPAddressCtx(ctx)
		if err != nil {
			firstErr = rememberFirstErr(firstErr, err)
			continue
		}
		externalIP := usableExternalIPv4(externalIPText)
		if externalIP == nil {
			firstErr = rememberFirstErr(firstErr, fmt.Errorf("UPnP returned unusable external IP %q", externalIPText))
			continue
		}
		publicHint := publicIPv4Hint(publicAddr, internalIP)
		if publicHint != nil && !externalIP.Equal(publicHint) {
			firstErr = rememberFirstErr(
				firstErr,
				fmt.Errorf("UPnP external IP %s does not match STUN public IP %s", externalIP, publicHint),
			)
			continue
		}
		for _, externalPort := range upnpExternalPortCandidates(internalPort) {
			err = client.AddPortMappingCtx(
				ctx,
				"",
				externalPort,
				upnpProtocolUDP,
				internalPort,
				internalIP.String(),
				true,
				description,
				leaseSeconds,
			)
			if err != nil {
				firstErr = rememberFirstErr(firstErr, err)
				owned, reconcileErr := reconcileUPnPMapping(
					ctx,
					client,
					externalPort,
					internalIP,
					internalPort,
					description,
				)
				if reconcileErr == nil && owned {
					return &upnpMapping{
						client:       client,
						externalAddr: net.JoinHostPort(externalIP.String(), strconv.Itoa(int(externalPort))),
						externalPort: externalPort,
						internalIP:   append(net.IP(nil), internalIP...),
						internalPort: internalPort,
						description:  description,
					}, nil
				}
				if ctx.Err() != nil {
					return nil, err
				}
				continue
			}
			return &upnpMapping{
				client:       client,
				externalAddr: net.JoinHostPort(externalIP.String(), strconv.Itoa(int(externalPort))),
				externalPort: externalPort,
				internalIP:   append(net.IP(nil), internalIP...),
				internalPort: internalPort,
				description:  description,
			}, nil
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, errors.New("no usable UPnP IGD services found")
}

// reconcileUPnPMapping checks whether a failed AddPortMapping call still applied Wormzy's exact mapping.
func reconcileUPnPMapping(
	parent context.Context,
	client upnpPortMapper,
	externalPort uint16,
	internalIP net.IP,
	internalPort uint16,
	description string,
) (bool, error) {
	base := parent
	if parent == nil || parent.Err() != nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, defaultUPnPReconcileTimeout)
	defer cancel()

	return upnpMappingMatches(ctx, client, externalPort, internalIP, internalPort, description)
}

// upnpMappingMatches verifies that a router entry is the exact mapping created by this process.
func upnpMappingMatches(
	ctx context.Context,
	client upnpPortMapper,
	externalPort uint16,
	internalIP net.IP,
	internalPort uint16,
	description string,
) (bool, error) {
	if client == nil || externalPort == 0 || internalIP.To4() == nil || internalPort == 0 || description == "" {
		return false, errors.New("incomplete UPnP mapping ownership data")
	}
	mappedPort, mappedClient, enabled, mappedDescription, _, err := client.GetSpecificPortMappingEntryCtx(
		ctx,
		"",
		externalPort,
		upnpProtocolUDP,
	)
	if err != nil {
		return false, err
	}
	mappedIP := net.ParseIP(mappedClient).To4()
	return mappedPort == internalPort &&
		mappedIP != nil && mappedIP.Equal(internalIP) &&
		enabled && mappedDescription == description, nil
}

// newUPnPMappingDescription creates an unguessable token that binds cleanup to one mapping.
func newUPnPMappingDescription() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate UPnP mapping nonce: %w", err)
	}
	return upnpDescriptionPrefix + base64.RawURLEncoding.EncodeToString(nonce[:]), nil
}

func publicIPv4Hint(addr string, internalIP net.IP) net.IP {
	ip := parseAddrIPv4(addr)
	if ip == nil || ip.Equal(internalIP) || !isUsableExternalIPv4(ip) {
		return nil
	}
	return ip
}

func usableExternalIPv4(raw string) net.IP {
	ip := net.ParseIP(raw).To4()
	if ip == nil || !isUsableExternalIPv4(ip) {
		return nil
	}
	return ip
}

func parseAddrIPv4(addr string) net.IP {
	host := hostPart(addr)
	if host == "" {
		return nil
	}
	return net.ParseIP(host).To4()
}

// isUsableExternalIPv4 reports whether ip is an ordinary public IPv4 destination.
func isUsableExternalIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	// Reject IPv4 ranges that are not ordinary Internet-routable destinations.
	switch {
	case ip[0] == 0:
		return false
	case ip[0] == 100 && ip[1]&0xc0 == 64: // RFC 6598 shared address space.
		return false
	case ip[0] == 192 && ip[1] == 0 && (ip[2] == 0 || ip[2] == 2):
		return false
	case ip[0] == 192 && ip[1] == 88 && ip[2] == 99:
		return false
	case ip[0] == 198 && (ip[1] == 18 || ip[1] == 19):
		return false
	case ip[0] == 198 && ip[1] == 51 && ip[2] == 100:
		return false
	case ip[0] == 203 && ip[1] == 0 && ip[2] == 113:
		return false
	case ip[0] >= 224:
		return false
	default:
		return true
	}
}

func upnpLeaseSeconds(lease time.Duration) uint32 {
	if lease <= 0 {
		lease = defaultUPnPLease
	}
	seconds := uint64(lease / time.Second)
	if seconds == 0 {
		seconds = 1
	}
	if seconds > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(seconds)
}

func upnpExternalPortCandidates(internalPort uint16) []uint16 {
	out := []uint16{internalPort}
	seen := map[uint16]bool{internalPort: true}
	for i := 1; i <= 8; i++ {
		port := uint16(49152 + ((int(internalPort) + i*9973) % 16384))
		if seen[port] {
			continue
		}
		seen[port] = true
		out = append(out, port)
	}
	return out
}

func rememberFirstErr(first, next error) error {
	if first != nil {
		return first
	}
	return next
}

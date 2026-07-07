package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	igd1 "github.com/huin/goupnp/dcps/internetgateway1"
	igd2 "github.com/huin/goupnp/dcps/internetgateway2"
)

const (
	defaultUPnPLease   = 2 * time.Hour
	defaultUPnPTimeout = 3 * time.Second
	upnpDescription    = "wormzy p2p"
	upnpProtocolUDP    = "UDP"
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
}

type upnpMapping struct {
	client       upnpPortMapper
	externalAddr string
	externalPort uint16
}

func (m *upnpMapping) Close(ctx context.Context) error {
	if m == nil || m.client == nil || m.externalPort == 0 {
		return nil
	}
	return m.client.DeletePortMappingCtx(ctx, "", m.externalPort, upnpProtocolUDP)
}

func setupUPnPMapping(ctx context.Context, cfg Config, conn *net.UDPConn, publicAddr string, rep Reporter) (*upnpMapping, error) {
	if cfg.DisableUPnP || cfg.Loopback {
		return nil, nil
	}
	if conn == nil {
		return nil, nil
	}
	timeout := defaultUPnPTimeout
	if cfg.HandshakeTimeout > 0 {
		timeout = boundedDuration(cfg.HandshakeTimeout/20, 1500*time.Millisecond, defaultUPnPTimeout)
	}
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

func createUPnPMapping(
	ctx context.Context,
	conn *net.UDPConn,
	publicAddr string,
	lease time.Duration,
	discover func(context.Context) ([]upnpPortMapper, error),
) (*upnpMapping, error) {
	internalIP, internalPort, err := upnpInternalEndpoint(conn)
	if err != nil {
		return nil, err
	}
	return mapUPnPPort(ctx, internalIP, internalPort, publicAddr, lease, discover)
}

func upnpInternalEndpoint(conn *net.UDPConn) (net.IP, uint16, error) {
	if conn == nil {
		return nil, 0, errors.New("nil UDP conn")
	}
	endpoint := localEndpoint(conn)
	host, portStr, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, 0, err
	}
	if port <= 0 || port > 65535 {
		return nil, 0, fmt.Errorf("invalid local UDP port %d", port)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, 0, fmt.Errorf("UPnP requires an IPv4 local address, got %q", host)
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return nil, 0, fmt.Errorf("UPnP requires a LAN IPv4 address, got %s", ip)
	}
	return ip, uint16(port), nil
}

func mapUPnPPort(
	ctx context.Context,
	internalIP net.IP,
	internalPort uint16,
	publicAddr string,
	lease time.Duration,
	discover func(context.Context) ([]upnpPortMapper, error),
) (*upnpMapping, error) {
	if internalIP = internalIP.To4(); internalIP == nil {
		return nil, errors.New("UPnP requires an IPv4 internal client")
	}
	if internalPort == 0 {
		return nil, errors.New("UPnP requires a non-zero internal port")
	}
	if discover == nil {
		discover = discoverUPnPPortMappers
	}

	clients, err := discover(ctx)
	if err != nil {
		return nil, err
	}
	if len(clients) == 0 {
		return nil, errors.New("no UPnP IGD services found")
	}

	publicHint := publicIPv4Hint(publicAddr, internalIP)
	leaseSeconds := upnpLeaseSeconds(lease)
	var firstErr error
	for _, client := range clients {
		if client == nil {
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
				upnpDescription,
				leaseSeconds,
			)
			if err != nil {
				firstErr = rememberFirstErr(firstErr, err)
				continue
			}
			return &upnpMapping{
				client:       client,
				externalAddr: net.JoinHostPort(externalIP.String(), strconv.Itoa(int(externalPort))),
				externalPort: externalPort,
			}, nil
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, errors.New("no usable UPnP IGD services found")
}

func discoverUPnPPortMappers(ctx context.Context) ([]upnpPortMapper, error) {
	type result struct {
		clients []upnpPortMapper
		errs    []error
		err     error
		name    string
	}

	discoveries := []struct {
		name string
		fn   func(context.Context) ([]upnpPortMapper, []error, error)
	}{
		{
			name: "igd2/wanip2",
			fn: func(ctx context.Context) ([]upnpPortMapper, []error, error) {
				clients, errs, err := igd2.NewWANIPConnection2ClientsCtx(ctx)
				out := make([]upnpPortMapper, 0, len(clients))
				for _, client := range clients {
					out = append(out, client)
				}
				return out, errs, err
			},
		},
		{
			name: "igd2/wanip1",
			fn: func(ctx context.Context) ([]upnpPortMapper, []error, error) {
				clients, errs, err := igd2.NewWANIPConnection1ClientsCtx(ctx)
				out := make([]upnpPortMapper, 0, len(clients))
				for _, client := range clients {
					out = append(out, client)
				}
				return out, errs, err
			},
		},
		{
			name: "igd2/wanppp1",
			fn: func(ctx context.Context) ([]upnpPortMapper, []error, error) {
				clients, errs, err := igd2.NewWANPPPConnection1ClientsCtx(ctx)
				out := make([]upnpPortMapper, 0, len(clients))
				for _, client := range clients {
					out = append(out, client)
				}
				return out, errs, err
			},
		},
		{
			name: "igd1/wanip1",
			fn: func(ctx context.Context) ([]upnpPortMapper, []error, error) {
				clients, errs, err := igd1.NewWANIPConnection1ClientsCtx(ctx)
				out := make([]upnpPortMapper, 0, len(clients))
				for _, client := range clients {
					out = append(out, client)
				}
				return out, errs, err
			},
		},
		{
			name: "igd1/wanppp1",
			fn: func(ctx context.Context) ([]upnpPortMapper, []error, error) {
				clients, errs, err := igd1.NewWANPPPConnection1ClientsCtx(ctx)
				out := make([]upnpPortMapper, 0, len(clients))
				for _, client := range clients {
					out = append(out, client)
				}
				return out, errs, err
			},
		},
	}

	ch := make(chan result, len(discoveries))
	var wg sync.WaitGroup
	for _, discovery := range discoveries {
		discovery := discovery
		wg.Add(1)
		go func() {
			defer wg.Done()
			clients, errs, err := discovery.fn(ctx)
			ch <- result{
				clients: clients,
				errs:    errs,
				err:     err,
				name:    discovery.name,
			}
		}()
	}
	wg.Wait()
	close(ch)

	var (
		out  []upnpPortMapper
		errs []error
	)
	for res := range ch {
		out = append(out, res.clients...)
		for _, err := range res.errs {
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", res.name, err))
			}
		}
		if res.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", res.name, res.err))
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return nil, errors.New("no UPnP IGD services found")
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

func isUsableExternalIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil {
		return false
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	// 100.64.0.0/10 is carrier-grade NAT; advertising it to an Internet peer is not useful.
	return !(ip[0] == 100 && ip[1]&0xc0 == 64)
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

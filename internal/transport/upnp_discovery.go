package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huin/goupnp"
	igd1 "github.com/huin/goupnp/dcps/internetgateway1"
	igd2 "github.com/huin/goupnp/dcps/internetgateway2"
)

const (
	upnpSSDPDiscoveryWindow       = 2 * time.Second
	upnpHTTPDialTimeout           = 2 * time.Second
	upnpHTTPResponseHeaderTimeout = 2 * time.Second
	upnpDeviceFetchTimeout        = 3 * time.Second
	maxUPnPLocalInterfaces        = 16
	maxUPnPSSDPPacketSize         = 8 << 10
	maxUPnPSSDPPacketsPerSocket   = 64
	maxUPnPDiscoveryResponses     = 32
	maxUPnPConcurrentFetches      = 8
	maxUPnPPortMapperClients      = 32
	maxUPnPHTTPResponseHeaderSize = 16 << 10
	maxUPnPHTTPResponseSize       = 256 << 10
)

var (
	upnpSSDPAddress   = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	upnpSearchTargets = []string{
		igd2.URN_WANIPConnection_2,
		igd2.URN_WANIPConnection_1,
		igd2.URN_WANPPPConnection_1,
	}
)

type upnpLocalInterface struct {
	ip     net.IP
	subnet *net.IPNet
}

type upnpDiscoveryResponse struct {
	location  *url.URL
	responder net.IP
	local     upnpLocalInterface
}

type upnpInterfaceSearchResult struct {
	responses []upnpDiscoveryResponse
	err       error
}

type upnpMapperDiscoveryResult struct {
	mappers []discoveredUPnPPortMapper
	err     error
}

type upnpLimitedReadCloser struct {
	io.Reader
	io.Closer
}

type upnpRestrictedRoundTripper struct {
	base      *http.Transport
	responder net.IP
	localIP   net.IP
	subnet    *net.IPNet
}

// discoverUPnPPortMappers discovers IGD services without trusting advertised HTTP destinations.
func discoverUPnPPortMappers(ctx context.Context) ([]discoveredUPnPPortMapper, error) {
	responses, err := discoverUPnPDevices(ctx)
	if err != nil {
		return nil, err
	}

	results := make(chan upnpMapperDiscoveryResult, len(responses))
	semaphore := make(chan struct{}, maxUPnPConcurrentFetches)
	var wg sync.WaitGroup
	for _, response := range responses {
		response := response
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- upnpMapperDiscoveryResult{err: ctx.Err()}
				return
			}

			root, httpClient, fetchErr := fetchUPnPDeviceDescription(ctx, response)
			if fetchErr != nil {
				results <- upnpMapperDiscoveryResult{err: fetchErr}
				return
			}
			mappers, clientErr := newUPnPPortMappers(root, response.location, httpClient, response.local.ip)
			if clientErr != nil {
				httpClient.CloseIdleConnections()
			}
			results <- upnpMapperDiscoveryResult{mappers: mappers, err: clientErr}
		}()
	}
	wg.Wait()
	close(results)

	var (
		mappers []discoveredUPnPPortMapper
		errs    []error
	)
	for result := range results {
		mappers = append(mappers, result.mappers...)
		if result.err != nil {
			errs = append(errs, result.err)
		}
	}
	if len(mappers) > 0 {
		return mappers, nil
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return nil, errors.New("no UPnP IGD services found")
}

// discoverUPnPDevices searches for IGDs from bounded sockets tied to private IPv4 interfaces.
func discoverUPnPDevices(ctx context.Context) ([]upnpDiscoveryResponse, error) {
	interfaces, err := privateUPnPInterfaces()
	if err != nil {
		return nil, err
	}

	searchCtx, cancel := context.WithTimeout(ctx, upnpSSDPDiscoveryWindow)
	defer cancel()

	results := make(chan upnpInterfaceSearchResult, len(interfaces))
	var responseCount atomic.Int32
	var wg sync.WaitGroup
	for _, local := range interfaces {
		local := local
		wg.Add(1)
		go func() {
			defer wg.Done()
			responses, searchErr := searchUPnPInterface(searchCtx, local, &responseCount)
			results <- upnpInterfaceSearchResult{responses: responses, err: searchErr}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]struct{})
	responses := make([]upnpDiscoveryResponse, 0, responseCount.Load())
	var errs []error
	for result := range results {
		if result.err != nil {
			errs = append(errs, result.err)
		}
		for _, response := range result.responses {
			key := response.local.ip.String() + "\x00" + response.responder.String() + "\x00" + response.location.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			responses = append(responses, response)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(responses) > 0 {
		return responses, nil
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return nil, errors.New("no valid UPnP SSDP responses")
}

// privateUPnPInterfaces returns bounded, active RFC 1918 IPv4 interface addresses.
func privateUPnPInterfaces() ([]upnpLocalInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}

	seen := make(map[string]struct{})
	locals := make([]upnpLocalInterface, 0)
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		addresses, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, address := range addresses {
			ipNet, ok := address.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if !isPrivateUPnPIPv4(ip) {
				continue
			}
			key := ip.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			locals = append(locals, upnpLocalInterface{
				ip: append(net.IP(nil), ip...),
				subnet: &net.IPNet{
					IP:   append(net.IP(nil), ipNet.IP.To4()...),
					Mask: append(net.IPMask(nil), ipNet.Mask...),
				},
			})
			if len(locals) == maxUPnPLocalInterfaces {
				return locals, nil
			}
		}
	}
	if len(locals) == 0 {
		return nil, errors.New("no private IPv4 interface available for UPnP discovery")
	}
	return locals, nil
}

// searchUPnPInterface sends bounded SSDP searches from one private local interface.
func searchUPnPInterface(
	ctx context.Context,
	local upnpLocalInterface,
	responseCount *atomic.Int32,
) ([]upnpDiscoveryResponse, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: local.ip, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind UPnP SSDP socket on %s: %w", local.ip, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(upnpSSDPDiscoveryWindow)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set UPnP SSDP deadline: %w", err)
	}

	var sendErr error
	sent := false
	for _, target := range upnpSearchTargets {
		if _, err := conn.WriteToUDP(upnpSSDPRequest(target), upnpSSDPAddress); err != nil {
			sendErr = rememberFirstErr(sendErr, err)
			continue
		}
		sent = true
	}
	if !sent {
		return nil, fmt.Errorf("send UPnP SSDP search from %s: %w", local.ip, sendErr)
	}

	responses := make([]upnpDiscoveryResponse, 0)
	packet := make([]byte, maxUPnPSSDPPacketSize+1)
	for packetNumber := 0; packetNumber < maxUPnPSSDPPacketsPerSocket; packetNumber++ {
		n, responder, readErr := conn.ReadFromUDP(packet)
		if readErr != nil {
			var netErr net.Error
			if errors.As(readErr, &netErr) && netErr.Timeout() {
				break
			}
			if ctx.Err() != nil {
				break
			}
			return responses, fmt.Errorf("read UPnP SSDP response: %w", readErr)
		}
		if n > maxUPnPSSDPPacketSize || responder == nil {
			continue
		}
		location, parseErr := parseUPnPSSDPResponse(packet[:n])
		if parseErr != nil {
			continue
		}
		if err := validateUPnPPeerURL(location, responder.IP, local.ip, local.subnet); err != nil {
			continue
		}
		if !reserveUPnPResponse(responseCount) {
			break
		}
		responses = append(responses, upnpDiscoveryResponse{
			location:  location,
			responder: append(net.IP(nil), responder.IP.To4()...),
			local:     local,
		})
	}
	return responses, nil
}

// reserveUPnPResponse atomically enforces the process-wide discovery response cap.
func reserveUPnPResponse(responseCount *atomic.Int32) bool {
	for {
		current := responseCount.Load()
		if current >= maxUPnPDiscoveryResponses {
			return false
		}
		if responseCount.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// upnpSSDPRequest builds a standards-compatible search request for one trusted target constant.
func upnpSSDPRequest(target string) []byte {
	return []byte("M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 1\r\n" +
		"ST: " + target + "\r\n\r\n")
}

// parseUPnPSSDPResponse parses a bounded SSDP datagram and extracts its sole LOCATION header.
func parseUPnPSSDPResponse(packet []byte) (*url.URL, error) {
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(packet)), nil)
	if err != nil {
		return nil, fmt.Errorf("parse SSDP response: %w", err)
	}
	if response.Body != nil {
		response.Body.Close()
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected SSDP status %d", response.StatusCode)
	}
	locations := response.Header.Values("Location")
	if len(locations) != 1 || strings.TrimSpace(locations[0]) == "" {
		return nil, errors.New("SSDP response requires one LOCATION header")
	}
	location, err := url.Parse(strings.TrimSpace(locations[0]))
	if err != nil {
		return nil, fmt.Errorf("parse SSDP LOCATION: %w", err)
	}
	return location, nil
}

// validateUPnPPeerURL requires a literal HTTP IPv4 destination at the private SSDP responder.
func validateUPnPPeerURL(candidate *url.URL, responderIP, localIP net.IP, localSubnet *net.IPNet) error {
	if candidate == nil || !candidate.IsAbs() || candidate.Opaque != "" {
		return errors.New("UPnP URL must be absolute")
	}
	if !strings.EqualFold(candidate.Scheme, "http") {
		return errors.New("UPnP URL must use HTTP")
	}
	if candidate.User != nil || candidate.Fragment != "" {
		return errors.New("UPnP URL must not contain userinfo or a fragment")
	}
	responder := responderIP.To4()
	local := localIP.To4()
	if !isPrivateUPnPIPv4(responder) || !isPrivateUPnPIPv4(local) || localSubnet == nil ||
		!localSubnet.Contains(local) || !localSubnet.Contains(responder) {
		return errors.New("UPnP responder must be private and on the local subnet")
	}
	if responder.Equal(local) {
		return errors.New("UPnP responder must not be the local interface")
	}
	hostIP := net.ParseIP(candidate.Hostname()).To4()
	if hostIP == nil || !hostIP.Equal(responder) {
		return errors.New("UPnP URL host must be the literal SSDP responder IPv4 address")
	}
	if err := validateUPnPHTTPPort(candidate); err != nil {
		return err
	}
	return nil
}

// validateUPnPHTTPPort rejects malformed, zero, and out-of-range URL ports.
func validateUPnPHTTPPort(candidate *url.URL) error {
	portText := candidate.Port()
	if portText == "" {
		if strings.HasSuffix(candidate.Host, ":") {
			return errors.New("UPnP URL has an empty port")
		}
		return nil
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("UPnP URL has an invalid port")
	}
	return nil
}

// isPrivateUPnPIPv4 reports whether ip is an RFC 1918 unicast address suitable for LAN discovery.
func isPrivateUPnPIPv4(ip net.IP) bool {
	ip = ip.To4()
	return ip != nil && ip.IsPrivate() && ip.IsGlobalUnicast() &&
		!ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() && !ip.IsMulticast()
}

// fetchUPnPDeviceDescription fetches and validates one peer-pinned, size-bounded IGD description.
func fetchUPnPDeviceDescription(
	ctx context.Context,
	response upnpDiscoveryResponse,
) (*goupnp.RootDevice, *http.Client, error) {
	if err := validateUPnPPeerURL(
		response.location,
		response.responder,
		response.local.ip,
		response.local.subnet,
	); err != nil {
		return nil, nil, err
	}
	httpClient := newRestrictedUPnPHTTPClient(response.responder, response.local.ip, response.local.subnet)
	keepHTTPClient := false
	defer func() {
		if !keepHTTPClient {
			httpClient.CloseIdleConnections()
		}
	}()
	fetchCtx, cancel := context.WithTimeout(ctx, upnpDeviceFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, response.location.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create UPnP device request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch UPnP device description: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("UPnP device description returned HTTP %s", resp.Status)
	}
	body, err := readBoundedUPnPBody(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	root, err := parseUPnPDeviceDescription(
		body,
		response.location,
		response.responder,
		response.local.ip,
		response.local.subnet,
	)
	if err != nil {
		return nil, nil, err
	}
	keepHTTPClient = true
	return root, httpClient, nil
}

// newRestrictedUPnPHTTPClient returns a proxy-free client pinned to one LAN responder.
func newRestrictedUPnPHTTPClient(responderIP, localIP net.IP, localSubnet *net.IPNet) *http.Client {
	dialer := &net.Dialer{
		Timeout:   upnpHTTPDialTimeout,
		KeepAlive: 30 * time.Second,
		LocalAddr: &net.TCPAddr{IP: append(net.IP(nil), localIP.To4()...)},
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DisableCompression:     true,
		MaxIdleConns:           2,
		MaxIdleConnsPerHost:    2,
		MaxConnsPerHost:        2,
		IdleConnTimeout:        30 * time.Second,
		ResponseHeaderTimeout:  upnpHTTPResponseHeaderTimeout,
		MaxResponseHeaderBytes: maxUPnPHTTPResponseHeaderSize,
		ExpectContinueTimeout:  time.Second,
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("invalid UPnP dial address: %w", err)
		}
		ip := net.ParseIP(host).To4()
		if ip == nil || !ip.Equal(responderIP.To4()) {
			return nil, errors.New("UPnP dial destination changed from the SSDP responder")
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil || port == "0" {
			return nil, errors.New("invalid UPnP dial port")
		}
		return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
	}
	restricted := &upnpRestrictedRoundTripper{
		base:      transport,
		responder: append(net.IP(nil), responderIP.To4()...),
		localIP:   append(net.IP(nil), localIP.To4()...),
		subnet:    cloneIPNet(localSubnet),
	}
	return &http.Client{
		Transport: restricted,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("UPnP HTTP redirects are disabled")
		},
	}
}

// RoundTrip revalidates every request and bounds every HTTP response body.
func (transport *upnpRestrictedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, errors.New("nil UPnP HTTP request")
	}
	if err := validateUPnPPeerURL(req.URL, transport.responder, transport.localIP, transport.subnet); err != nil {
		return nil, err
	}
	response, err := transport.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if response.ContentLength > maxUPnPHTTPResponseSize {
		response.Body.Close()
		return nil, errors.New("UPnP HTTP response exceeds size limit")
	}
	response.Status = strconv.Itoa(response.StatusCode) + " " + http.StatusText(response.StatusCode)
	response.Body = &upnpLimitedReadCloser{
		Reader: io.LimitReader(response.Body, maxUPnPHTTPResponseSize+1),
		Closer: response.Body,
	}
	return response, nil
}

// cloneIPNet returns a defensive copy of an IPv4 network.
func cloneIPNet(network *net.IPNet) *net.IPNet {
	if network == nil {
		return nil
	}
	return &net.IPNet{
		IP:   append(net.IP(nil), network.IP...),
		Mask: append(net.IPMask(nil), network.Mask...),
	}
}

// readBoundedUPnPBody reads a complete device document without exceeding the configured cap.
func readBoundedUPnPBody(body io.Reader) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(body, maxUPnPHTTPResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read UPnP device description: %w", err)
	}
	if len(contents) > maxUPnPHTTPResponseSize {
		return nil, errors.New("UPnP device description exceeds size limit")
	}
	return contents, nil
}

// parseUPnPDeviceDescription parses an IGD document and rejects URLBase or control URL pivots.
func parseUPnPDeviceDescription(
	contents []byte,
	location *url.URL,
	responderIP, localIP net.IP,
	localSubnet *net.IPNet,
) (*goupnp.RootDevice, error) {
	if len(contents) == 0 || len(contents) > maxUPnPHTTPResponseSize {
		return nil, errors.New("invalid UPnP device description size")
	}
	if err := validateUPnPPeerURL(location, responderIP, localIP, localSubnet); err != nil {
		return nil, fmt.Errorf("invalid UPnP description location: %w", err)
	}

	root := new(goupnp.RootDevice)
	decoder := xml.NewDecoder(bytes.NewReader(contents))
	decoder.DefaultSpace = goupnp.DeviceXMLNamespace
	if err := decoder.Decode(root); err != nil {
		return nil, fmt.Errorf("parse UPnP device description: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("UPnP device description contains trailing XML")
		}
		return nil, fmt.Errorf("parse trailing UPnP XML: %w", err)
	}

	baseURL := location
	if baseText := strings.TrimSpace(root.URLBaseStr); baseText != "" {
		parsedBase, err := url.Parse(baseText)
		if err != nil {
			return nil, fmt.Errorf("parse UPnP URLBase: %w", err)
		}
		if err := validateUPnPPeerURL(parsedBase, responderIP, localIP, localSubnet); err != nil {
			return nil, fmt.Errorf("reject UPnP URLBase: %w", err)
		}
		baseURL = parsedBase
	}
	root.SetURLBase(baseURL)

	var controlErr error
	root.Device.VisitServices(func(service *goupnp.Service) {
		if controlErr != nil {
			return
		}
		service.ControlURL.Str = strings.TrimSpace(service.ControlURL.Str)
		if service.ControlURL.Str == "" {
			controlErr = errors.New("UPnP service is missing a control URL")
			return
		}
		service.ControlURL.SetURLBase(baseURL)
		if !service.ControlURL.Ok {
			controlErr = errors.New("UPnP service has an invalid control URL")
			return
		}
		if err := validateUPnPPeerURL(&service.ControlURL.URL, responderIP, localIP, localSubnet); err != nil {
			controlErr = fmt.Errorf("reject UPnP control URL: %w", err)
		}
	})
	if controlErr != nil {
		return nil, controlErr
	}
	return root, nil
}

// newUPnPPortMappers creates IGD1 and IGD2 clients using the restricted HTTP client.
func newUPnPPortMappers(
	root *goupnp.RootDevice,
	location *url.URL,
	httpClient *http.Client,
	localIP net.IP,
) ([]discoveredUPnPPortMapper, error) {
	localIP = localIP.To4()
	if root == nil || location == nil || httpClient == nil || !isPrivateUPnPIPv4(localIP) {
		return nil, errors.New("incomplete UPnP device client configuration")
	}
	wanIP2Count := len(root.Device.FindService(igd2.URN_WANIPConnection_2))
	wanIP1Count := len(root.Device.FindService(igd2.URN_WANIPConnection_1))
	wanPPP1Count := len(root.Device.FindService(igd2.URN_WANPPPConnection_1))
	mapperCount := wanIP2Count + 2*wanIP1Count + 2*wanPPP1Count
	if mapperCount == 0 {
		return nil, errors.New("device description contains no supported UPnP IGD service")
	}
	if mapperCount > maxUPnPPortMapperClients {
		return nil, errors.New("device description contains too many UPnP IGD services")
	}

	mappers := make([]discoveredUPnPPortMapper, 0, mapperCount)
	appendClient := func(serviceClient *goupnp.ServiceClient, mapper upnpPortMapper) {
		if serviceClient == nil || serviceClient.SOAPClient == nil || mapper == nil ||
			len(mappers) >= maxUPnPPortMapperClients {
			return
		}
		serviceClient.SOAPClient.HTTPClient = *httpClient
		mappers = append(mappers, discoveredUPnPPortMapper{
			client:  mapper,
			localIP: append(net.IP(nil), localIP...),
		})
	}

	if root.Device.FindService(igd2.URN_WANIPConnection_2) != nil {
		found, err := igd2.NewWANIPConnection2ClientsFromRootDevice(root, location)
		if err != nil {
			return nil, err
		}
		for _, client := range found {
			appendClient(&client.ServiceClient, client)
		}
	}
	if root.Device.FindService(igd2.URN_WANIPConnection_1) != nil {
		found, err := igd2.NewWANIPConnection1ClientsFromRootDevice(root, location)
		if err != nil {
			return nil, err
		}
		for _, client := range found {
			appendClient(&client.ServiceClient, client)
		}
		legacy, err := igd1.NewWANIPConnection1ClientsFromRootDevice(root, location)
		if err != nil {
			return nil, err
		}
		for _, client := range legacy {
			appendClient(&client.ServiceClient, client)
		}
	}
	if root.Device.FindService(igd2.URN_WANPPPConnection_1) != nil {
		found, err := igd2.NewWANPPPConnection1ClientsFromRootDevice(root, location)
		if err != nil {
			return nil, err
		}
		for _, client := range found {
			appendClient(&client.ServiceClient, client)
		}
		legacy, err := igd1.NewWANPPPConnection1ClientsFromRootDevice(root, location)
		if err != nil {
			return nil, err
		}
		for _, client := range legacy {
			appendClient(&client.ServiceClient, client)
		}
	}
	return mappers, nil
}

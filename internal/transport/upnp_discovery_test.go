package transport

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/huin/goupnp"
	igd2 "github.com/huin/goupnp/dcps/internetgateway2"
)

// TestValidateUPnPPeerURL_RestrictsDestination verifies SSDP cannot redirect HTTP beyond its responder.
func TestValidateUPnPPeerURL_RestrictsDestination(t *testing.T) {
	localIP := net.IPv4(192, 168, 1, 20)
	responderIP := net.IPv4(192, 168, 1, 1)
	localSubnet := &net.IPNet{IP: net.IPv4(192, 168, 1, 0), Mask: net.CIDRMask(24, 32)}

	tests := []struct {
		name      string
		rawURL    string
		responder net.IP
		wantErr   bool
	}{
		{name: "same private responder", rawURL: "http://192.168.1.1:5000/root.xml"},
		{name: "same private responder default port", rawURL: "http://192.168.1.1/root.xml"},
		{name: "https", rawURL: "https://192.168.1.1/root.xml", wantErr: true},
		{name: "hostname", rawURL: "http://router.local/root.xml", wantErr: true},
		{name: "userinfo", rawURL: "http://admin@192.168.1.1/root.xml", wantErr: true},
		{name: "fragment", rawURL: "http://192.168.1.1/root.xml#control", wantErr: true},
		{name: "different peer", rawURL: "http://192.168.1.2/root.xml", wantErr: true},
		{name: "different private subnet", rawURL: "http://192.168.2.1/root.xml", responder: net.IPv4(192, 168, 2, 1), wantErr: true},
		{name: "public peer", rawURL: "http://8.8.8.8/root.xml", responder: net.IPv4(8, 8, 8, 8), wantErr: true},
		{name: "loopback peer", rawURL: "http://127.0.0.1/root.xml", responder: net.IPv4(127, 0, 0, 1), wantErr: true},
		{name: "link local peer", rawURL: "http://169.254.1.1/root.xml", responder: net.IPv4(169, 254, 1, 1), wantErr: true},
		{name: "local interface", rawURL: "http://192.168.1.20/root.xml", responder: localIP, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responder := tt.responder
			if responder == nil {
				responder = responderIP
			}
			candidate, err := url.Parse(tt.rawURL)
			if err != nil {
				t.Fatalf("parse test URL: %v", err)
			}
			err = validateUPnPPeerURL(candidate, responder, localIP, localSubnet)
			if tt.wantErr && err == nil {
				t.Fatal("expected URL to be rejected")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected URL to be accepted: %v", err)
			}
		})
	}
}

// TestReadBoundedUPnPBody_EnforcesLimit verifies oversized device descriptions are rejected.
func TestReadBoundedUPnPBody_EnforcesLimit(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at limit", size: maxUPnPHTTPResponseSize},
		{name: "over limit", size: maxUPnPHTTPResponseSize + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contents, err := readBoundedUPnPBody(bytes.NewReader(make([]byte, tt.size)))
			if tt.wantErr && err == nil {
				t.Fatal("expected body to be rejected")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("expected body to be accepted: %v", err)
			}
			if !tt.wantErr && len(contents) != tt.size {
				t.Fatalf("body length = %d; want %d", len(contents), tt.size)
			}
		})
	}
}

// TestParseUPnPDeviceDescription_RejectsURLPivots verifies XML URLs remain pinned to the responder.
func TestParseUPnPDeviceDescription_RejectsURLPivots(t *testing.T) {
	localIP := net.IPv4(192, 168, 1, 20)
	responderIP := net.IPv4(192, 168, 1, 1)
	localSubnet := &net.IPNet{IP: net.IPv4(192, 168, 1, 0), Mask: net.CIDRMask(24, 32)}
	location, err := url.Parse("http://192.168.1.1:5000/root.xml")
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}

	tests := []struct {
		name       string
		urlBase    string
		controlURL string
		wantErr    bool
	}{
		{name: "relative control", controlURL: "/upnp/control/wanip"},
		{name: "same peer absolute base", urlBase: "http://192.168.1.1:5431/base/", controlURL: "control"},
		{name: "same peer absolute control", controlURL: "http://192.168.1.1:5431/control"},
		{name: "URLBase other host", urlBase: "http://192.168.1.2:5000/", controlURL: "control", wantErr: true},
		{name: "URLBase hostname", urlBase: "http://router.local:5000/", controlURL: "control", wantErr: true},
		{name: "URLBase relative", urlBase: "/alternate/", controlURL: "control", wantErr: true},
		{name: "control other host", controlURL: "http://192.168.1.2/control", wantErr: true},
		{name: "control network path pivot", controlURL: "//192.168.1.2/control", wantErr: true},
		{name: "control hostname", controlURL: "http://router.local/control", wantErr: true},
		{name: "missing control", controlURL: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			description := testUPnPDeviceDescription(tt.urlBase, tt.controlURL)
			root, parseErr := parseUPnPDeviceDescription(
				[]byte(description),
				location,
				responderIP,
				localIP,
				localSubnet,
			)
			if tt.wantErr && parseErr == nil {
				t.Fatal("expected device description to be rejected")
			}
			if !tt.wantErr && parseErr != nil {
				t.Fatalf("expected device description to be accepted: %v", parseErr)
			}
			if !tt.wantErr && root == nil {
				t.Fatal("expected parsed root device")
			}
		})
	}
}

// TestNewUPnPPortMappers_RejectsServiceFanout verifies duplicate XML services cannot create unbounded clients.
func TestNewUPnPPortMappers_RejectsServiceFanout(t *testing.T) {
	localIP := net.IPv4(192, 168, 1, 20)
	responderIP := net.IPv4(192, 168, 1, 1)
	localSubnet := &net.IPNet{IP: net.IPv4(192, 168, 1, 0), Mask: net.CIDRMask(24, 32)}
	location, err := url.Parse("http://192.168.1.1:5000/root.xml")
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	description := strings.Replace(
		testUPnPDeviceDescription("", "/upnp/control/wanip"),
		igd2.URN_WANIPConnection_2,
		igd2.URN_WANIPConnection_1,
		1,
	)
	root, err := parseUPnPDeviceDescription(
		[]byte(description),
		location,
		responderIP,
		localIP,
		localSubnet,
	)
	if err != nil {
		t.Fatalf("parse device description: %v", err)
	}
	service := root.Device.Services[0]
	for len(root.Device.Services) < maxUPnPPortMapperClients {
		root.Device.Services = append(root.Device.Services, service)
	}
	httpClient := newRestrictedUPnPHTTPClient(responderIP, localIP, localSubnet)
	t.Cleanup(httpClient.CloseIdleConnections)

	if _, err := newUPnPPortMappers(root, location, httpClient, localIP); err == nil {
		t.Fatal("expected duplicate service fanout to be rejected")
	}
}

// TestNewUPnPPortMappers_ConfiguresRestrictedHTTP verifies all supported clients retain the pinned transport.
func TestNewUPnPPortMappers_ConfiguresRestrictedHTTP(t *testing.T) {
	localIP := net.IPv4(192, 168, 1, 20)
	responderIP := net.IPv4(192, 168, 1, 1)
	localSubnet := &net.IPNet{IP: net.IPv4(192, 168, 1, 0), Mask: net.CIDRMask(24, 32)}
	location, err := url.Parse("http://192.168.1.1:5000/root.xml")
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	description := strings.Replace(
		testUPnPDeviceDescription("", "/upnp/control/wanip"),
		igd2.URN_WANIPConnection_2,
		igd2.URN_WANIPConnection_1,
		1,
	)
	root, err := parseUPnPDeviceDescription(
		[]byte(description),
		location,
		responderIP,
		localIP,
		localSubnet,
	)
	if err != nil {
		t.Fatalf("parse device description: %v", err)
	}
	httpClient := newRestrictedUPnPHTTPClient(responderIP, localIP, localSubnet)
	t.Cleanup(httpClient.CloseIdleConnections)
	mappers, err := newUPnPPortMappers(root, location, httpClient, localIP)
	if err != nil {
		t.Fatalf("newUPnPPortMappers: %v", err)
	}
	if len(mappers) != 2 {
		t.Fatalf("mapper count = %d; want IGD1 and IGD2 WANIP v1 clients", len(mappers))
	}
	for index, mapper := range mappers {
		if !mapper.localIP.Equal(localIP) {
			t.Fatalf("mapper %d local IP = %s; want %s", index, mapper.localIP, localIP)
		}
		var configuredTransport any
		switch client := mapper.client.(type) {
		case interface{ GetServiceClient() *goupnp.ServiceClient }:
			configuredTransport = client.GetServiceClient().SOAPClient.HTTPClient.Transport
		default:
			t.Fatalf("mapper %d does not expose a service client", index)
		}
		if configuredTransport != httpClient.Transport {
			t.Fatalf("mapper %d does not use the restricted HTTP transport", index)
		}
	}
}

// testUPnPDeviceDescription returns a minimal IGD description for parser tests.
func testUPnPDeviceDescription(urlBase, controlURL string) string {
	baseElement := ""
	if urlBase != "" {
		baseElement = fmt.Sprintf("<URLBase>%s</URLBase>", urlBase)
	}
	return strings.Join([]string{
		`<?xml version="1.0"?>`,
		`<root xmlns="urn:schemas-upnp-org:device-1-0">`,
		`<specVersion><major>1</major><minor>0</minor></specVersion>`,
		baseElement,
		`<device>`,
		`<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:2</deviceType>`,
		`<friendlyName>Test IGD</friendlyName>`,
		`<UDN>uuid:test-igd</UDN>`,
		`<serviceList><service>`,
		`<serviceType>urn:schemas-upnp-org:service:WANIPConnection:2</serviceType>`,
		`<serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId>`,
		`<SCPDURL>/wanip.xml</SCPDURL>`,
		"<controlURL>" + controlURL + "</controlURL>",
		`<eventSubURL>/events</eventSubURL>`,
		`</service></serviceList>`,
		`</device></root>`,
	}, "")
}

package transport

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	errInvalidRelayPin       = errors.New("relay pin must be canonical base64 of a 32-byte SHA-256 SPKI digest")
	errRelayPinRequiresHTTPS = errors.New("relay pin requires an HTTPS mailbox endpoint")
	errRelayPinMismatch      = errors.New("relay TLS SPKI pin mismatch")
)

// decodeRelaySPKIPin decodes a canonical base64 SHA-256 SPKI digest.
func decodeRelaySPKIPin(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, nil
	}
	if strings.TrimSpace(encoded) != encoded {
		return nil, errInvalidRelayPin
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(digest) != sha256.Size {
		return nil, errInvalidRelayPin
	}
	if base64.StdEncoding.EncodeToString(digest) != encoded {
		return nil, errInvalidRelayPin
	}
	return digest, nil
}

// validateRelayPinEndpoint ensures a configured pin applies only to HTTPS.
func validateRelayPinEndpoint(addr string, digest []byte) error {
	if len(digest) == 0 {
		return nil
	}
	endpoint, err := url.Parse(addr)
	if err != nil || endpoint.Host == "" || !strings.EqualFold(endpoint.Scheme, "https") {
		return errRelayPinRequiresHTTPS
	}
	return nil
}

// newRelayPinnedHTTPClient returns an HTTP client with additive SPKI verification.
func newRelayPinnedHTTPClient(timeout time.Duration, expectedDigest []byte) *http.Client {
	client := newHTTPClient(timeout)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{}
	if transport.TLSClientConfig != nil {
		tlsConfig = transport.TLSClientConfig.Clone()
	}
	// A pin supplements the platform trust store and hostname check; it never
	// turns ordinary certificate verification off.
	tlsConfig.InsecureSkipVerify = false
	previousVerifyConnection := tlsConfig.VerifyConnection
	expected := append([]byte(nil), expectedDigest...)
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if previousVerifyConnection != nil {
			if err := previousVerifyConnection(state); err != nil {
				return err
			}
		}
		return verifyRelaySPKIPin(state, expected)
	}
	transport.TLSClientConfig = tlsConfig
	client.Transport = transport
	return client
}

// verifyRelaySPKIPin compares the leaf certificate SPKI digest in constant time.
func verifyRelaySPKIPin(state tls.ConnectionState, expected []byte) error {
	if len(state.PeerCertificates) == 0 {
		return errors.New("relay TLS certificate is missing")
	}
	actual := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return errRelayPinMismatch
	}
	return nil
}

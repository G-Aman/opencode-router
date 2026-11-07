package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The OpenCode inference gateway gates the free models on the TLS ClientHello
// fingerprint of the official client (Bun's fetch stack). Requests that carry the
// same HTTP bytes but a different ClientHello are answered with
//
//	429 {"type":"error","error":{"type":"FreeUsageLimitError", ...}}
//
// even though the public free-tier API key ("Bearer public") and the HTTP headers
// are correct. Reproducing the official ClientHello byte-for-byte makes the same
// request succeed, so the upstream dialer below speaks TLS through utls with a
// hand-built spec that mirrors the captured hello:
//
//	17 cipher suites (TLS 1.3 first, then the Go/BoringSSL TLS 1.2 order),
//	13 extensions in order: SNI, EMS, renegotiation_info, supported_groups
//	(X25519MLKEM768, X25519, P-256, P-384), ec_point_formats, session_ticket,
//	ALPN (http/1.1), status_request (OCSP), signature_algorithms,
//	signed_certificate_timestamp, key_share (X25519MLKEM768 + X25519),
//	psk_key_exchange_modes, supported_versions (1.3, 1.2).
//
// Keep this in sync with the official client when it changes.
func opencodeClientHelloSpec(serverName string) *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: tls.VersionTLS12,
		TLSVersMax: tls.VersionTLS13,
		CipherSuites: []uint16{
			0x1301, // TLS_AES_128_GCM_SHA256
			0x1302, // TLS_AES_256_GCM_SHA384
			0x1303, // TLS_CHACHA20_POLY1305_SHA256
			0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
			0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
			0xc02c, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
			0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
			0xcca9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
			0xcca8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
			0xc009, // TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA
			0xc013, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA
			0xc00a, // TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA
			0xc014, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA
			0x009c, // TLS_RSA_WITH_AES_128_GCM_SHA256
			0x009d, // TLS_RSA_WITH_AES_256_GCM_SHA384
			0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
			0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{ServerName: serverName},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateNever},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []uint8{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501,
				0x0806, 0x0601, 0x0807, 0x0603, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519MLKEM768},
				{Group: utls.X25519},
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
			&utls.SupportedVersionsExtension{Versions: []uint16{tls.VersionTLS13, tls.VersionTLS12}},
		},
	}
}

// isOpenCodeHost reports whether host is served by the OpenCode gateway whose
// free tier depends on the official client's TLS fingerprint.
func isOpenCodeHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "opencode.ai" || strings.HasSuffix(host, ".opencode.ai")
}

var (
	transportMu    sync.Mutex
	transportCache = map[string]*http.Transport{}
)

// upstreamTransport returns a shared transport that dials HTTPS upstreams with a
// CLI-compatible TLS handshake and supports an optional outbound HTTP proxy.
func upstreamTransport(proxy *url.URL) *http.Transport {
	key := ""
	if proxy != nil {
		key = proxy.String()
	}
	transportMu.Lock()
	defer transportMu.Unlock()
	if tr, ok := transportCache[key]; ok {
		return tr
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialUpstreamTLS(ctx, dialer, proxy, addr)
		},
	}
	if proxy != nil {
		// HTTPS is tunnelled by dialUpstreamTLS; only plain HTTP uses the
		// transport's own proxy handling (which can send absolute-form paths).
		tr.Proxy = func(req *http.Request) (*url.URL, error) {
			if req.URL != nil && req.URL.Scheme == "https" {
				return nil, nil
			}
			return proxy, nil
		}
	}
	transportCache[key] = tr
	return tr
}

func dialUpstreamTLS(ctx context.Context, dialer *net.Dialer, proxy *url.URL, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}

	var conn net.Conn
	if proxy != nil {
		conn, err = dialProxyTunnel(ctx, dialer, proxy, addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	nextProtos := []string{"http/1.1"}
	if isOpenCodeHost(host) {
		uconn := utls.UClient(conn, &utls.Config{ServerName: host, NextProtos: nextProtos}, utls.HelloCustom)
		if err := uconn.ApplyPreset(opencodeClientHelloSpec(host)); err != nil {
			conn.Close()
			return nil, err
		}
		if err := uconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		return uconn, nil
	}

	tconn := tls.Client(conn, &tls.Config{ServerName: host, NextProtos: nextProtos})
	if err := tconn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return tconn, nil
}

// dialProxyTunnel dials an HTTP(S) proxy and opens a CONNECT tunnel to addr.
func dialProxyTunnel(ctx context.Context, dialer *net.Dialer, proxy *url.URL, addr string) (net.Conn, error) {
	proxyAddr := proxy.Host
	if proxy.Port() == "" {
		port := "80"
		if strings.EqualFold(proxy.Scheme, "https") {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(proxy.Hostname(), port)
	}

	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(proxy.Scheme, "https") {
		tconn := tls.Client(conn, &tls.Config{ServerName: proxy.Hostname()})
		if err := tconn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, err
		}
		conn = tconn
	}

	var req strings.Builder
	fmt.Fprintf(&req, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if proxy.User != nil {
		pass, _ := proxy.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + pass))
		fmt.Fprintf(&req, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	req.WriteString("\r\n")
	if _, err := io.WriteString(conn, req.String()); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s", addr, resp.Status)
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn keeps bytes the CONNECT response reader may have buffered.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

package main

// TLS fingerprint spoofing for the opencode.ai free-tier gate.
//
// The gate fingerprints the TLS client: byte-identical requests via Go's
// default TLS (OpenSSL-style hello) get 429 FreeUsageLimitError, while the
// same bytes over a hello matching opencode's runtime (Bun/BoringSSL) pass.
// We replay opencode's exact ClientHello via uTLS (blunt mimicry; key shares
// are regenerated per connection). Captured from opencode v2.2.7.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ClientHello record captured from opencode v2.2.7 (bun 1.3.x runtime) on linux/aarch64.
// Contains: 17 ciphers (TLS1.3 first), extensions in exact BoringSSL order, no GREASE,
// ALPN http/1.1 only. 562 bytes.
const spoofHelloHex = "160301022d0100022903038f3ad027561b7a024e9acbcbbc257333660d29e9478d624500d75f8e51d0040b20ff6c702c72de0cf607a8181c63d6f1a2faafbc9ac7a87dee7074309dead1c0e20022130113021303c02bc02fc02cc030cca9cca8c009c013c00ac014009c009d002f0035010001be00000010000e00000b6f70656e636f64652e6169fe0d011a000001000165002037b5f2fea622fff629c0c3dbac4e4ec6a5d566c1e6610789ff7faf1405f1931200f07f1202da3c289788306a5fe095e95887270a8f610e144ae234d68eb615eb6740c226fd46972e21c021505d9ad96164ce47f5b103101bb0cc633531cd7ec08fbe89b93f464867d29379a6ebd2e25c8f77b398fcee4e1ad08b3a57a80e06fcbc88890549c2026dc417993e5984701c93640453bc5bc578521859d55f5c6835693bbcd7bb644786ff82c8969a2ea31c4778e6ab8a44b1e984ef44fb6a1fdfc29f76eec0858a5882c7c8f9917b95189cd2d6f7ee177f00b23546c4839e27cfaed292c2f0df720e3d5fd6106ee2946a089c1d05d52d3bbccb4ef2149e11af1b0492223d83704838169fb57140d347665f009000170000ff01000100000a00080006001d00170018000b00020100002300000010000b000908687474702f312e31000500050100000000000d0014001204030804040105030805050108060601020100120000003300260024001d00206fbb373c77d701d6fe9c4527e2af077274f37af0390ae0e449a2ff9bd71a975a002d00020101002b00050403040303"

var spoofSpec *utls.ClientHelloSpec
var spoofHelloRaw []byte

func init() {
	raw, err := hex.DecodeString(spoofHelloHex)
	if err != nil || len(raw) < 10 {
		log.Printf("tls-spoof: bad embedded hello, spoofing disabled")
		return
	}
	spec, err := buildSpoofSpec(raw)
	if err != nil {
		log.Printf("tls-spoof: spec parse failed (%v), spoofing disabled", err)
		return
	}
	spoofSpec = spec
	spoofHelloRaw = raw
}

// parseClientHello extracts cipher/compression/extension bytes from a captured TLS record.
func parseClientHello(raw []byte) (ciphers, comps, exts []byte, err error) {
	if raw[0] != 0x16 || raw[5] != 0x01 {
		return nil, nil, nil, fmt.Errorf("not a ClientHello record")
	}
	chlen := int(raw[6])<<16 | int(raw[7])<<8 | int(raw[8])
	ch := raw[9 : 9+chlen]
	p := 34 // version(2) + random(32)
	sidLen := int(ch[p])
	p += 1 + sidLen
	ciphLen := int(ch[p])<<8 | int(ch[p+1])
	p += 2
	ciphers = ch[p : p+ciphLen]
	p += ciphLen
	compLen := int(ch[p])
	p += 1
	comps = ch[p : p+compLen]
	p += compLen
	extLen := int(ch[p])<<8 | int(ch[p+1])
	p += 2
	exts = ch[p : p+extLen]
	return ciphers, comps, exts, nil
}

func buildSpoofSpec(raw []byte) (*utls.ClientHelloSpec, error) {
	ciphers, comps, exts, err := parseClientHello(raw)
	if err != nil {
		return nil, err
	}
	spec := &utls.ClientHelloSpec{}
	if err := spec.ReadCipherSuites(ciphers); err != nil {
		return nil, fmt.Errorf("ciphers: %w", err)
	}
	if err := spec.ReadCompressionMethods(comps); err != nil {
		return nil, fmt.Errorf("compression: %w", err)
	}
	// allowBluntMimicry replays unknown extensions (ECH etc.) verbatim;
	// key shares are regenerated per connection by uTLS.
	if err := spec.ReadTLSExtensions(exts, true, false); err != nil {
		return nil, fmt.Errorf("extensions: %w", err)
	}
	return spec, nil
}

// proxyConnect dials target through an HTTP CONNECT proxy and returns the tunnel.
func proxyConnect(ctx context.Context, proxyURL, targetAddr string) (net.Conn, error) {
	var d net.Dialer
	pconn, err := d.DialContext(ctx, "tcp", proxyURL) // proxyURL: host:port
	if err != nil {
		return nil, fmt.Errorf("proxy dial: %w", err)
	}
	req := "CONNECT " + targetAddr + " HTTP/1.1\r\nHost: " + targetAddr + "\r\n\r\n"
	pconn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := pconn.Write([]byte(req)); err != nil {
		pconn.Close()
		return nil, fmt.Errorf("proxy connect write: %w", err)
	}
	buf := make([]byte, 4096)
	n, err := pconn.Read(buf)
	pconn.SetDeadline(time.Time{})
	if err != nil || n < 12 || !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 200") && !strings.HasPrefix(string(buf[:n]), "HTTP/1.0 200") {
		pconn.Close()
		return nil, fmt.Errorf("proxy connect rejected: %q", strings.TrimSpace(string(buf[:min(n, 64)])))
	}
	return pconn, nil
}

func spoofDialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	if spoofSpec == nil {
		return tls.Dial(network, addr, &tls.Config{ServerName: splitHost(addr)})
	}
	// Fresh spec per dial: uTLS extension objects carry per-connection state
	// (session tickets, key shares) and MUST NOT be shared across handshakes —
	// reusing one spec makes every handshake after the first fail.
	fresh, ferr := buildSpoofSpec(spoofHelloRaw)
	if ferr != nil {
		return nil, fmt.Errorf("tls-spoof spec: %w", ferr)
	}
	var rawConn net.Conn
	if proxyStr := currentOutboundProxy(); proxyStr != "" && strings.HasPrefix(proxyStr, "http") {
		proxyHost := stripScheme(proxyStr)
		pconn, err := proxyConnect(ctx, proxyHost, addr)
		if err != nil {
			return nil, err
		}
		rawConn = pconn
	} else {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		rawConn = c
	}
	cfg := &utls.Config{ServerName: splitHost(addr)}
	uconn := utls.UClient(rawConn, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(fresh); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls-spoof apply: %w", err)
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		rawConn.Close()
		return nil, fmt.Errorf("tls-spoof handshake for %s: %w", addr, err)
	}
	return uconn, nil
}

func splitHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func stripScheme(u string) string {
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "socks5://")
	u = strings.TrimPrefix(u, "socks5h://")
	if i := strings.Index(u, "@"); i >= 0 {
		u = u[i+1:] // drop userinfo
	}
	return u
}

// retryTransport wraps a transport with 429 backoff (the free-lane gate rate
// limits bursts; rejections are stateless so retrying is safe). On the final
// attempt it mints a fresh session ID in case the embedded one aged out.
type retryTransport struct {
	base http.RoundTripper
}

var retryNetErr = []string{
	"connection reset", "broken pipe", "unexpected EOF",
	"server closed connection", "use of closed connection",
}

func isConnReusedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range retryNetErr {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	backoff := []time.Duration{2 * time.Second, 6 * time.Second, 15 * time.Second}
	var lastResp *http.Response
	for attempt := 0; ; attempt++ {
		if attempt > 0 && attempt <= len(backoff) {
			select {
			case <-time.After(backoff[attempt-1]):
			case <-req.Context().Done():
				return lastResp, nil
			}
			// Aged-out session fallback: mint a fresh ID for the last try.
			if attempt == len(backoff) {
				if req.Body != nil {
					_ = req.Body.Close()
				}
				if req.GetBody != nil {
					nb, err := req.GetBody()
					if err == nil {
						req.Body = nb
					}
				}
				req.Header.Set("x-opencode-session", genSessionID())
				req.Header.Set("x-opencode-session-id", req.Header.Get("x-opencode-session"))
				req.Header.Set("x-session-affinity", req.Header.Get("x-opencode-session"))
				req.Header.Set("x-session-id", req.Header.Get("x-opencode-session"))
			}
		}
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			// Stale pooled connection (server closed it while idle): the request
			// may have been written into a dead socket — rewind and retry.
			if req.GetBody != nil && isConnReusedErr(err) && attempt < 2 {
				if nb, berr := req.GetBody(); berr == nil {
					req.Body = nb
					continue
				}
			}
			return resp, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= len(backoff) {
			return resp, nil
		}
		// Only retry on the gate's free-lane error; other 429 bodies pass through.
		peek := make([]byte, 4096)
		n, _ := io.ReadFull(resp.Body, peek)
		resp.Body.Close()
		if !bytes.Contains(peek[:n], []byte("FreeUsageLimitError")) {
			return &http.Response{
				Status: resp.Status, StatusCode: resp.StatusCode,
				Header: resp.Header, Body: io.NopCloser(bytes.NewReader(peek[:n])),
			}, nil
		}
		lastResp = resp
		// rewind body for the retry
		if req.GetBody != nil {
			nb, berr := req.GetBody()
			if berr != nil {
				return resp, nil
			}
			req.Body = nb
		}
	}
}

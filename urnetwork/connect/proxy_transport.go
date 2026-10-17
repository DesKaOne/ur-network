package connect

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

type ProxyProtocol string

const (
	ProxyProtocolSOCKS5 ProxyProtocol = "socks5"
	ProxyProtocolSOCKS4 ProxyProtocol = "socks4"
	ProxyProtocolHTTP   ProxyProtocol = "http"
)

type proxyBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *proxyBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// newProxyDialContext supports SOCKS5, SOCKS4a and plaintext HTTP CONNECT.
// Only TCP tunneling is supported. UDP over these proxy protocols requires
// separate handling and must not silently fall back to direct egress.
func (p *ProxySettings) newProxyDialContext(forward proxy.Dialer) DialContextFunction {
	return func(ctx context.Context, network, destination string) (net.Conn, error) {
		if p == nil {
			return nil, errors.New("nil proxy")
		}
		if !strings.HasPrefix(network, "tcp") {
			return nil, fmt.Errorf("proxy tunnel requires TCP, got %q", network)
		}
		if _, _, err := net.SplitHostPort(destination); err != nil {
			return nil, err
		}
		protocol := p.Protocol
		if protocol == "" {
			protocol = ProxyProtocolSOCKS5
		}
		if protocol == ProxyProtocolSOCKS5 {
			proxyNetwork := p.Network
			if proxyNetwork == "" {
				proxyNetwork = "tcp"
			}
			dialer, err := proxy.SOCKS5(proxyNetwork, p.Address, p.Auth, forward)
			if err != nil {
				return nil, err
			}
			ctxDialer, ok := dialer.(proxy.ContextDialer)
			if !ok {
				return nil, errors.New("SOCKS5 dialer must support context cancellation")
			}
			return ctxDialer.DialContext(ctx, network, destination)
		}
		if protocol != ProxyProtocolHTTP && protocol != ProxyProtocolSOCKS4 {
			return nil, fmt.Errorf("unsupported proxy protocol: %s", protocol)
		}
		ctxDialer, ok := forward.(proxy.ContextDialer)
		if !ok {
			return nil, errors.New("TCP dialer must support context cancellation")
		}
		conn, err := ctxDialer.DialContext(ctx, "tcp", p.Address)
		if err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		deadline := time.Now().Add(15 * time.Second)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, err
		}
		var tunneled net.Conn
		switch protocol {
		case ProxyProtocolHTTP:
			tunneled, err = proxyHTTPConnect(conn, destination, p.Auth)
		case ProxyProtocolSOCKS4:
			tunneled, err = proxySOCKS4aConnect(conn, destination, p.Auth)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = tunneled.SetDeadline(time.Time{})
		}
		if err != nil {
			conn.Close()
			return nil, err
		}
		return tunneled, nil
	}
}

func proxyHTTPConnect(conn net.Conn, destination string, auth *proxy.Auth) (net.Conn, error) {
	if strings.ContainsAny(destination, "\r\n") {
		return nil, errors.New("HTTP CONNECT destination contains a newline")
	}
	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", destination, destination)
	if auth != nil {
		basic := base64.StdEncoding.EncodeToString([]byte(auth.User + ":" + auth.Password))
		fmt.Fprintf(&request, "Proxy-Authorization: Basic %s\r\n", basic)
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP CONNECT failed with status %d", response.StatusCode)
	}
	return &proxyBufferedConn{Conn: conn, reader: reader}, nil
}

func proxySOCKS4aConnect(conn net.Conn, destination string, auth *proxy.Auth) (net.Conn, error) {
	host, portStr, err := net.SplitHostPort(destination)
	if err != nil {
		return nil, err
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid SOCKS4 port")
	}
	user := ""
	if auth != nil {
		if auth.Password != "" {
			return nil, errors.New("SOCKS4 does not support password auth")
		}
		user = auth.User
	}
	if host == "" || strings.ContainsRune(host, 0) || strings.ContainsRune(user, 0) {
		return nil, errors.New("invalid SOCKS4 host/user")
	}
	request := []byte{4, 1, byte(port >> 8), byte(port)}
	ip, parseErr := netip.ParseAddr(host)
	if parseErr == nil && !ip.Is4() {
		return nil, errors.New("SOCKS4 does not support IPv6 destinations")
	}
	if parseErr == nil {
		addr := ip.As4()
		request = append(request, addr[:]...)
	} else {
		// SOCKS4a: remote hostname resolution, no local destination DNS.
		request = append(request, 0, 0, 0, 1)
	}
	request = append(request, user...)
	request = append(request, 0)
	if parseErr != nil {
		request = append(request, host...)
		request = append(request, 0)
	}
	if _, err := conn.Write(request); err != nil {
		return nil, err
	}
	var reply [8]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return nil, err
	}
	if reply[0] != 0 || reply[1] != 90 {
		return nil, fmt.Errorf("SOCKS4 connect rejected: status %d", reply[1])
	}
	return conn, nil
}

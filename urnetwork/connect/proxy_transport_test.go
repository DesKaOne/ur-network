package connect

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"

	"golang.org/x/net/proxy"
)

func TestProxyHTTPConnect(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			done <- err
			return
		}
		if req.Method != http.MethodConnect || req.Host != "example.com:443" {
			done <- errors.New("wrong CONNECT target")
			return
		}
		if req.Header.Get("Proxy-Authorization") == "" {
			done <- errors.New("missing proxy auth")
			return
		}
		_, err = io.WriteString(server, "HTTP/1.1 200 Connection Established\r\n\r\nhello")
		done <- err
	}()
	defer client.Close()
	conn, err := proxyHTTPConnect(client, "example.com:443", &proxy.Auth{User: "test", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	var b [5]byte
	if _, err := io.ReadFull(conn, b[:]); err != nil || string(b[:]) != "hello" {
		t.Fatalf("buffered tunnel: %q, err=%v", b, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestProxySOCKS4a(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		var header [8]byte
		if _, err := io.ReadFull(server, header[:]); err != nil {
			done <- err
			return
		}
		if header[0] != 4 || header[1] != 1 || header[4] != 0 || header[7] != 1 {
			done <- errors.New("bad SOCKS4a header")
			return
		}
		reader := bufio.NewReader(server)
		user, err := reader.ReadString(0)
		if err != nil || user != "\x00" {
			done <- errors.New("bad SOCKS4 userid")
			return
		}
		host, err := reader.ReadString(0)
		if err != nil || host != "example.com\x00" {
			done <- errors.New("bad SOCKS4a hostname")
			return
		}
		_, err = server.Write([]byte{0, 90, 0, 0, 0, 0, 0, 0})
		done <- err
	}()
	defer client.Close()
	if _, err := proxySOCKS4aConnect(client, "example.com:443", nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

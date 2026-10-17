package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"deskaone-sdk/urnetwork/connect"
)

func TestManagedParseProxy(t *testing.T) {
	cases := []struct {
		raw string
		protocol connect.ProxyProtocol
		ok bool
	}{
		{"socks5://user:pass@localhost:1080", connect.ProxyProtocolSOCKS5, true},
		{"http://user:pass@127.0.0.1:3128", connect.ProxyProtocolHTTP, true},
		{"socks4://127.0.0.1:1080", connect.ProxyProtocolSOCKS4, true},
		{"localhost:1080", connect.ProxyProtocolSOCKS5, true},
		{"localhost:1080:user:pass", connect.ProxyProtocolSOCKS5, true},
		{"socks5://u:p%40ss@localhost:1080", connect.ProxyProtocolSOCKS5, true},
		{"socks4://user:password@localhost:1080", "", false},
		{"https://localhost:3128", "", false},
		{"socks5://localhost:99999", "", false},
		{"http://localhost:3128/path", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			cfg, err := managedParseProxy(tc.raw)
			if (err == nil) != tc.ok {
				t.Fatalf("parse(%q): err=%v want valid=%v", tc.raw, err, tc.ok)
			}
			if tc.ok && cfg.Protocol != tc.protocol {
				t.Fatalf("protocol=%s want %s", cfg.Protocol, tc.protocol)
			}
		})
	}
}

func TestProviderDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "# PROXY_HUB_URL=disabled\nMAX_CONNECTION=100\nCONCURRENCY=10\nPROXY_LIST_TXT='proxy.txt'\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	values, err := providerDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	if values["MAX_CONNECTION"] != "100" || values["CONCURRENCY"] != "10" ||
		values["PROXY_LIST_TXT"] != "proxy.txt" || values["PROXY_HUB_URL"] != "" {
		t.Fatalf("unexpected parsed .env: %#v", values)
	}
}

func TestManagedProxyFileOneGateway(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.txt")
	if err := os.WriteFile(path, []byte("# Webshare rotating gateway\nsocks5://user:pass@localhost:1080\n"), 0600); err != nil {
		t.Fatal(err)
	}
	proxies, err := managedLoadFile(path)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("file loaded %d proxies; err=%v", len(proxies), err)
	}
	// A single rotator can be selected for any number of logical providers.
	for i := 0; i < 100; i++ {
		if p := proxies[i%len(proxies)]; p.Address != "localhost:1080" {
			t.Fatal("unexpected address")
		}
	}
}

func TestManagedFlagsOverrideEnvironment(t *testing.T) {
	t.Setenv("MAX_CONNECTION", "100")
	t.Setenv("CONCURRENCY", "10")
	t.Setenv("PROXY_HUB_URL", "")
	t.Setenv("PROXY_LIST_TXT", "")
	cfg, enabled, err := managedOptions(map[string]any{
		"--max_connection": "3",
		"--concurrency": "2",
	})
	if err != nil || !enabled {
		t.Fatalf("config err=%v enabled=%v", err, enabled)
	}
	if cfg.maxConnection != 3 || cfg.concurrency != 2 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestProviderIdentityStoreRoundTrip(t *testing.T) {
	store := &providerIdentityStore{path: filepath.Join(t.TempDir(), "ids.json"), identities: map[string]providerIdentity{}}
	a, err := store.identity(0)
	if err != nil || a.InstanceID == "" {
		t.Fatalf("identity error: %v, identity=%+v", err, a)
	}
	b, err := store.identity(0)
	if err != nil || a.InstanceID != b.InstanceID {
		t.Fatalf("unstable instance ID: %v", err)
	}
	reopen := &providerIdentityStore{path: store.path, identities: map[string]providerIdentity{}}
	body, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), a.InstanceID) {
		t.Fatal("persisted state missing ID")
	}
	_ = reopen
}

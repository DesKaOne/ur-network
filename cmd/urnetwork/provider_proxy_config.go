package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"deskaone-sdk/urnetwork/connect"

	"golang.org/x/net/proxy"
)

type managedProviderConfig struct {
	maxConnection int
	concurrency   int
	proxyHubURL   string
	proxyListTXT  string
}

func providerDotEnv(path string) (map[string]string, error) {
	result := make(map[string]string)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "MAX_CONNECTION", "CONCURRENCY", "PROXY_HUB_URL", "PROXY_LIST_TXT":
		default:
			continue
		}
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'')) {
			if value[0] == '"' {
				value, err = strconv.Unquote(value)
				if err != nil {
					return nil, fmt.Errorf(".env %s: %w", key, err)
				}
			} else {
				value = value[1 : len(value)-1]
			}
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		result[key] = value
	}
	return result, scanner.Err()
}

// If a modern flag or .env key is present, provider uses the new manager;
// otherwise the old --instances/legacy proxy-file workflow is untouched.
func managedOptions(opts map[string]any) (managedProviderConfig, bool, error) {
	dotenv, err := providerDotEnv(".env")
	if err != nil {
		return managedProviderConfig{}, false, err
	}
	flags := map[string]string{
		"MAX_CONNECTION": "--max_connection",
		"CONCURRENCY":    "--concurrency",
		"PROXY_HUB_URL":  "--proxy_hub_url",
		"PROXY_LIST_TXT": "--proxy_list_txt",
	}
	enabled := false
	values := make(map[string]string)
	for key, flag := range flags {
		if _, ok := dotenv[key]; ok {
			enabled = true
		}
		if env, ok := os.LookupEnv(key); ok {
			enabled = true
			values[key] = strings.TrimSpace(env)
		} else {
			values[key] = strings.TrimSpace(dotenv[key])
		}
		if arg, ok := opts[flag].(string); ok && strings.TrimSpace(arg) != "" {
			enabled = true
			values[key] = strings.TrimSpace(arg)
		}
	}
	parsePositive := func(key string, fallback int) (int, error) {
		value := values[key]
		if value == "" {
			return fallback, nil
		}
		v, err := strconv.Atoi(value)
		if err != nil || v < 1 || v > 1000 {
			return 0, fmt.Errorf("%s must be 1..1000 (URnetwork client limit applies)", key)
		}
		return v, nil
	}
	maxConnection, err := parsePositive("MAX_CONNECTION", 1)
	if err != nil {
		return managedProviderConfig{}, enabled, err
	}
	concurrency, err := parsePositive("CONCURRENCY", 10)
	if err != nil {
		return managedProviderConfig{}, enabled, err
	}
	if concurrency > maxConnection {
		concurrency = maxConnection
	}
	return managedProviderConfig{
		maxConnection: maxConnection,
		concurrency:   concurrency,
		proxyHubURL:   values["PROXY_HUB_URL"],
		proxyListTXT:  values["PROXY_LIST_TXT"],
	}, enabled, nil
}

func managedParseProxy(raw string) (*connect.ProxySettings, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("empty proxy")
	}
	if !strings.Contains(raw, "://") {
		// Legacy shorthand: host:port:user:password (only for IPv4/DNS hosts).
		parts := strings.Split(raw, ":")
		if len(parts) == 4 {
			raw = "socks5://" + url.UserPassword(parts[2], parts[3]).String() + "@" + net.JoinHostPort(parts[0], parts[1])
		} else {
			raw = "socks5://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Hostname() == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("expected protocol://[user:password@]host:port")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("invalid proxy port")
	}
	var proto connect.ProxyProtocol
	switch strings.ToLower(u.Scheme) {
	case "socks5", "s5":
		proto = connect.ProxyProtocolSOCKS5
	case "socks4", "s4":
		proto = connect.ProxyProtocolSOCKS4
	case "http":
		proto = connect.ProxyProtocolHTTP
	default:
		return nil, errors.New("unsupported proxy protocol; use socks5, socks4 or http")
	}
	cfg := &connect.ProxySettings{
		Network:  "tcp",
		Address:  net.JoinHostPort(u.Hostname(), strconv.Itoa(port)),
		Protocol: proto,
	}
	if u.User != nil {
		pwd, ok := u.User.Password()
		if !ok {
			return nil, errors.New("proxy username requires a password field; use username:@host")
		}
		user := u.User.Username()
		if user == "" {
			return nil, errors.New("empty proxy username")
		}
		if proto == connect.ProxyProtocolSOCKS4 && pwd != "" {
			return nil, errors.New("SOCKS4 cannot use password authentication")
		}
		cfg.Auth = &proxy.Auth{User: user, Password: pwd}
	}
	return cfg, nil
}

func managedLoadFile(path string) ([]*connect.ProxySettings, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	var list []*connect.ProxySettings
	var problems []error
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		cfg, err := managedParseProxy(text)
		if err != nil {
			problems = append(problems, fmt.Errorf("line %d: %w", line, err))
			continue
		}
		list = append(list, cfg)
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, err)
	}
	return list, errors.Join(problems...)
}

func managedFetchHub(ctx context.Context, baseURL string) ([]*connect.ProxySettings, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base == nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("PROXY_HUB_URL must be an HTTP(S) base URL without credentials or query")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var list []*connect.ProxySettings
	var issues []error
	protocols := []connect.ProxyProtocol{
		connect.ProxyProtocolSOCKS5, connect.ProxyProtocolHTTP, connect.ProxyProtocolSOCKS4,
	}
	for _, protocol := range protocols {
		for page := 1; page <= 20; page++ {
			target := *base
			target.Path = strings.TrimRight(base.Path, "/") + "/proxy/" + string(protocol)
			q := target.Query()
			q.Set("alive", "true")
			q.Set("page", strconv.Itoa(page))
			q.Set("limit", "100")
			target.RawQuery = q.Encode()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
			if err != nil {
				return list, err
			}
			resp, err := client.Do(req)
			if err != nil {
				issues = append(issues, fmt.Errorf("%s page %d: %w", protocol, page, err))
				break
			}
			if resp.StatusCode != http.StatusOK {
				issues = append(issues, fmt.Errorf("%s page %d: HTTP %d", protocol, page, resp.StatusCode))
				resp.Body.Close()
				break
			}
			var data struct {
				TotalPages int `json:"total_pages"`
				Proxies    []struct {
					Address string `json:"address"`
				} `json:"proxies"`
			}
			err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&data)
			resp.Body.Close()
			if err != nil {
				issues = append(issues, fmt.Errorf("%s page %d: %w", protocol, page, err))
				break
			}
			for _, raw := range data.Proxies {
				address := strings.TrimSpace(raw.Address)
				if !strings.Contains(address, "://") {
					address = string(protocol) + "://" + address
				}
				cfg, err := managedParseProxy(address)
				if err == nil && cfg.Protocol == protocol {
					list = append(list, cfg)
				}
			}
			if page >= data.TotalPages || data.TotalPages < 1 {
				break
			}
		}
		if err := ctx.Err(); err != nil {
			return list, err
		}
	}
	return list, errors.Join(issues...)
}

func managedProxyKey(p *connect.ProxySettings) string {
	if p == nil {
		return ""
	}
	key := string(p.Protocol) + "\x00" + p.Address
	if p.Auth != nil {
		key += "\x00" + p.Auth.User + "\x00" + p.Auth.Password
	}
	return key
}

func managedLoadSources(ctx context.Context, cfg managedProviderConfig) ([]*connect.ProxySettings, error) {
	var all []*connect.ProxySettings
	if cfg.proxyListTXT != "" {
		proxies, err := managedLoadFile(cfg.proxyListTXT)
		if err != nil {
			fmt.Printf("[proxy] file warning: %v\n", err)
		}
		fmt.Printf("[proxy] file loaded: %d\n", len(proxies))
		all = append(all, proxies...)
	}
	if cfg.proxyHubURL != "" {
		proxies, err := managedFetchHub(ctx, cfg.proxyHubURL)
		if err != nil {
			fmt.Printf("[proxy] hub warning: %v\n", err)
		}
		fmt.Printf("[proxy] hub loaded: %d\n", len(proxies))
		all = append(all, proxies...)
	}
	seen := make(map[string]bool, len(all))
	result := make([]*connect.ProxySettings, 0, len(all))
	for _, p := range all {
		key := managedProxyKey(p)
		if !seen[key] {
			seen[key] = true
			result = append(result, p)
		}
	}
	if (cfg.proxyHubURL != "" || cfg.proxyListTXT != "") && len(result) == 0 {
		return nil, errors.New("proxy mode requested but no valid proxy sources loaded")
	}
	return result, nil
}

// providerProxyCheck only lists configured proxy endpoints; it never creates
// URnetwork clients or verifies live IP/transport reachability.
func providerProxyCheck(opts map[string]any) {
	cfg, _, err := managedOptions(opts)
	if err != nil {
		fmt.Printf("Configuration error: %v\n", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	proxies, err := managedLoadSources(ctx, cfg)
	if err != nil {
		fmt.Printf("Proxy load error: %v\n", err)
		return
	}
	fmt.Printf("Proxy sources OK. Unique proxies: %d | MAX_CONNECTION=%d | CONCURRENCY=%d\n",
		len(proxies), cfg.maxConnection, cfg.concurrency)
	if len(proxies) == 0 {
		fmt.Println("No proxy sources enabled: managed provider will connect directly.")
	}
}

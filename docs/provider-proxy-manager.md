# Provider Proxy Manager (development branch)

This is an upgrade to the **working public-provider binary**, not to
`github.com/urnetwork/connect/connectctl sink`.

Executable: `go run ./cmd/urnetwork`

## Configuration

Create `.env` in the repo working directory:

```dotenv
MAX_CONNECTION=1
CONCURRENCY=1
PROXY_HUB_URL=http://127.0.0.1:8080
PROXY_LIST_TXT=proxy.txt
```

Both proxy sources can be commented out for **direct** provider mode.
Flags `--max_connection`, `--concurrency`, `--proxy_hub_url`, and
`--proxy_list_txt` override the process environment, which overrides
`.env`. The new provider manager is enabled when one of those four values
is explicitly configured. Without any of them, the legacy
`provider provide --instances` behavior is preserved.

`MAX_CONNECTION` is the number of **public provider clients**, each with
a separate client ID and saved instance ID. `CONCURRENCY` limits
simultaneous **initial provider setup/auth** attempts, not long-running
provider connections. The manager accepts 1..100, subject to URnetwork
plan/network limits; do not assume 100 accounts/devices are permitted.

Provider identities are saved under
`.urnetwork/provider_manager.json`, to reuse an existing client instead
of allocating another on each restart. Treat the state file as persistent;
do not delete it to clear errors.

## Proxy files

One entry per line. SOCKS5 is the default for bare host:port entries:

```text
# Webshare rotating gateway (one address can service multiple sessions)
socks5://username:password@proxy.example.net:1080
# Plain HTTP CONNECT tunnel (HTTP proxy server itself is not TLS encrypted)
http://username:password@127.0.0.1:3128
socks4://127.0.0.1:1081
```

Legacy `host:port` and `host:port:user:pass` are also accepted. Encode
reserved username/password characters in URLs. HTTPS proxy URLs and UDP proxy
carriers are **not** supported.

When file and ProxyHub are both enabled, entries from the file are first,
then ProxyHub entries, deduplicated, and initial provider instances are
assigned round-robin. If a provider fails initialization, the manager retries
another proxy after backoff. If an authenticated provider stays unconnected
for 90 seconds and another proxy is available, it rebuilds the session
with the **same** saved client ID. Active sessions are never rotated.

A single Webshare rotating gateway can be reused by all instances when it is
the only source. The number of distinct outbound IPs is controlled by the
proxy provider and rotation policy; it does not equal MAX_CONNECTION.

ProxyHub is read via:
`GET /proxy/{socks5,http,socks4}?alive=true&page=N&limit=100`,
up to 20 pages per protocol. Loading is bounded. `proxy-check` only
tests reading/parsing, not live reachability.

## Authentication and commands (Windows PowerShell)

Run from the repository root:

```powershell
git switch feature/provider-proxy-manager
git pull origin feature/provider-proxy-manager
go test ./...
go run ./cmd/urnetwork --help
```

The old provider stores a **network JWT** in `.urnetwork/jwt`.
If you have not logged in, generate a fresh dashboard auth code and run:

```powershell
go run ./cmd/urnetwork auth
```

The command prompts for the code. Do not paste client JWTs, network JWTs,
or live proxy credentials into chat logs.

Start with a single **direct** provider by commenting out the two proxy
sources in `.env`:

```powershell
go run ./cmd/urnetwork provide --max_connection=1 --concurrency=1
```

Or test a single proxy (with proxy settings enabled):

```powershell
go run ./cmd/urnetwork proxy-check
go run ./cmd/urnetwork provide --max_connection=1 --concurrency=1
```

Only after provider status and actual traffic have been verified should you
increase to `MAX_CONNECTION=3`, then larger sizes. Stop with Ctrl+C.
`[provider N] connected=true` is the transport's authenticated H1
availability signal, while appearance on the manager website is separate.
The legacy `--port` status handler is a process health endpoint,
**not** a per-client connectivity indicator.

## Routing caveats and safety

- HTTP CONNECT, SOCKS4a and SOCKS5 in this branch support **TCP only**.
  When a proxy is selected, the platform transport is forced to H1/TCP.
- This does **not** prove that all public provider payloads, UDP datagrams,
  ICMP, DNS, or SDK bootstrap paths can be routed through a TCP proxy.
  The NAT's network settings are forwarded to the proxy-aware TCP dialer;
  unsupported protocols may fail. Independently verify egress with packet
  capture and test UDP traffic separately before publishing a provider.
- Never assume a rotating gateway gives unique or sticky IP identities.
  Reconnects may change external IP; test against provider reputation,
  server policy, and account limits.
- `.env`, `proxy.txt` and the `.urnetwork` directory are gitignored.
  HTTP proxy credentials are transported in plaintext to the proxy gateway,
  so prefer a secure trusted network for this mode.
- Provisioning multiple clients consumes network/client slots and may
  trigger authentication throttles or plan limits. Begin with 1.

This branch has source and unit tests but **has not been independently
compiled or exercised against the live URnetwork backend by the author**.
Run `go test ./...` and live tests before considering merge into main.

# spiffec

`spiffec` is a per-connection `stdio` ↔ SPIFFE mTLS client tunnel for tools that
can spawn a transport command (such as OpenSSH `ProxyCommand` or `mbsync`
`Tunnel`). It fetches the calling workload's X.509-SVID and trust bundle from
the local SPIRE agent Workload API, dials the target TCP endpoint over mTLS,
verifies the server's expected SPIFFE ID, and relays `stdin`/`stdout`.

## Flags

| Flag | Default | Description |
|---|---|---|
| `-addr` | *(required)* | mTLS endpoint address (`host:port`) |
| `-server-id` | *(required)* | Expected server SPIFFE ID (`spiffe://...`) |
| `-spire-socket` | `$SPIFFE_ENDPOINT_SOCKET`, else `/run/spire/agent/public/api.sock` | SPIRE Workload API socket path or URI |

Usage errors exit `2`; operational failures exit `1`. Diagnostics are written
only to `stderr`.

## Usage

### OpenSSH `ProxyCommand`

```sshconfig
Host server
  ProxyCommand spiffec -addr 192.0.2.1:2222 -server-id spiffe://example.org/server/frontdoor
```

### `mbsync` (`isync`) `Tunnel`

`mbsync` supports a `Tunnel` directive to run a transport command instead of dialing a direct TCP socket:

```ini
Tunnel "spiffec -addr 192.0.2.1:1430 -server-id spiffe://example.org/server/imap-proxy"
```

## Build & Test

```console
$ go test ./...
$ go build
$ nix flake check -L
```

# REST API

`fastvmd` exposes the same JSON API over two transports:

- **Unix socket** at `socket_path` (default `/run/fastvm/fastvm.sock`), mode `0600`
- **TCP with mutual TLS**, optional

The unix socket is always enabled. Generate local test certificates with:

```sh
make certs
```

This writes:

```text
/tmp/fastvm/certs/ca/{ca.pem,ca.key}
/tmp/fastvm/certs/server/{ca.pem,cert.pem,key.pem}
/tmp/fastvm/certs/client/{ca.pem,cert.pem,key.pem}
```

Enable TCP in config:

```yaml
api:
  host: 127.0.0.1:8443
  cert_path: /tmp/fastvm/certs/server
```

`cert_path` must contain:

```text
ca.pem      CA used to verify client certificates
cert.pem    server certificate
key.pem     server private key
```

All three files and `cert_path` are required when `host` is set. There is no
plaintext TCP mode.

## Authentication

TCP clients must present a certificate signed by `ca.pem`. The server requires
and verifies client certificates; requests without one are rejected during the
TLS handshake.

Every trusted client has the same access. Use a dedicated CA and keep its key
off the API hosts.

## Example

```sh
curl --cacert ca.crt \
  --cert client.crt \
  --key client.key \
  https://127.0.0.1:8443/v1/version
```

The server certificate must include a SAN matching the address clients use.

## Endpoints

Same paths as the unix API, for example:

```text
GET  /v1/version
GET  /v1/sandboxes
POST /v1/sandboxes
PATCH /v1/sandboxes/{ref}/resources
POST /v1/sandboxes/{ref}/exec
GET  /v1/events
GET  /v1/kernels
POST /v1/kernels/download
POST /v1/kernels/use
GET  /v1/sandboxes/{ref}/shell/ws
```

The WebSocket shell endpoint also works over the mTLS listener, using `wss://`.

## CLI client

`fastvmctl` uses the unix socket by default. Set `FASTVM_HOST` to use the
mTLS listener:

```sh
export FASTVM_HOST=https://127.0.0.1:8443
export FASTVM_CERT_PATH=/tmp/fastvm/certs/client

fastvmctl ps
fastvmctl exec my-sandbox -- uname -a
```

`FASTVM_CERT_PATH` must be a directory containing:

```text
ca.pem      CA used to verify the server certificate
cert.pem    client certificate
key.pem     client private key
```

`FASTVM_HOST` may omit the scheme; `127.0.0.1:8443` is treated as
`https://127.0.0.1:8443`.

An explicit `--socket` overrides `FASTVM_HOST` for local use.

## Metrics

`GET /metrics` exposes Prometheus text metrics over the same unix socket and
optional mTLS listener:

```sh
curl --unix-socket /run/fastvm/fastvm.sock http://fastvm/metrics
# or
curl --cacert ... --cert ... --key ... https://127.0.0.1:8443/metrics
```

Metrics include:

- daemon sandbox counts by state
- image and writable-volume cache counts/bytes
- per-sandbox CPU, VMM RSS, network rx/tx
- per-sandbox wake, sleep, and exec counts

See [Prometheus metrics](METRICS.md) for the full metric list and scrape config.

## Security notes

- No token or password auth; mTLS is the only TCP authentication.
- The unix socket remains the local-only transport.
- Bind `listen` to a private address; a public interface implies exposing full
  sandbox control to every client certificate signed by `ca.pem`.

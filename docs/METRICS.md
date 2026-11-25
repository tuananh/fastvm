# Prometheus metrics

`fastvmd` exposes Prometheus text metrics at:

```text
GET /metrics
```

The endpoint is available on both transports:

```sh
# unix socket
sudo curl --unix-socket /run/fastvm/fastvm.sock http://fastvm/metrics

# mTLS API
curl --cacert ca.pem --cert cert.pem --key key.pem \
  https://127.0.0.1:8443/metrics
```

## Daemon metrics

| Metric | Type | Description |
|---|---|---|
| `fastvm_up` | gauge | Always `1` while the daemon serves metrics |
| `fastvm_sandboxes{state}` | gauge | Sandbox count by state |
| `fastvm_images` | gauge | Number of base images |
| `fastvm_image_cache_bytes` | gauge | Logical size of base images |
| `fastvm_volumes` | gauge | Number of writable sandbox volumes |
| `fastvm_volume_cache_bytes` | gauge | Logical size of writable volumes |

## Per-sandbox metrics

All are labeled with `sandbox_id`.

| Metric | Type | Description |
|---|---|---|
| `fastvm_sandbox_info` | gauge | Identity/config; labels include name, state, memory_mb, vcpus |
| `fastvm_sandbox_cpu_percent` | gauge | Cached CPU utilization |
| `fastvm_sandbox_memory_rss_bytes` | gauge | VMM process RSS |
| `fastvm_sandbox_guest_memory_used_bytes` | gauge | Guest-used memory (`free` semantics) |
| `fastvm_sandbox_guest_memory_total_bytes` | gauge | Guest-visible total memory |
| `fastvm_sandbox_guest_memory_ballooned_bytes` | gauge | Memory inflated into the virtio balloon |
| `fastvm_sandbox_network_rx_bytes_total` | counter | Guest receive bytes |
| `fastvm_sandbox_network_tx_bytes_total` | counter | Guest transmit bytes |
| `fastvm_sandbox_wakes_total` | counter | Wake count |
| `fastvm_sandbox_sleeps_total` | counter | Sleep count |
| `fastvm_sandbox_execs_total` | counter | Exec attempts |

Example:

```text
fastvm_sandbox_info{sandbox_id="abc123",sandbox_name="dev",state="Running",memory_mb="1024",vcpus="2"} 1
fastvm_sandbox_cpu_percent{sandbox_id="abc123"} 0.4
fastvm_sandbox_memory_rss_bytes{sandbox_id="abc123"} 548000000
fastvm_sandbox_guest_memory_used_bytes{sandbox_id="abc123"} 24117248
fastvm_sandbox_guest_memory_total_bytes{sandbox_id="abc123"} 100909056
fastvm_sandbox_guest_memory_ballooned_bytes{sandbox_id="abc123"} 0
fastvm_sandbox_wakes_total{sandbox_id="abc123"} 2
fastvm_sandbox_sleeps_total{sandbox_id="abc123"} 1
fastvm_sandbox_execs_total{sandbox_id="abc123"} 4
```

## Prometheus scrape config

```yaml
scrape_configs:
  - job_name: fastvm
    scheme: https
    static_configs:
      - targets: ["127.0.0.1:8443"]
    tls_config:
      ca_file: /tmp/fastvm/certs/ca/ca.pem
      cert_file: /tmp/fastvm/certs/client/cert.pem
      key_file: /tmp/fastvm/certs/client/key.pem
```

## Notes

- Metrics use cached samples from the daemon; values may lag one sample interval.
- Counters reset when a sandbox or VMM process is recreated.
- `sandbox_info` carries the current state and capacity as labels.
- Image and volume cache bytes are logical sizes, not physical disk usage.
- The mTLS endpoint requires a client certificate signed by `ca.pem`.

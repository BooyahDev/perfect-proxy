# perfect-proxy

`perfect-proxy` is a small TCP/UDP port forwarder for exposing services on an
edge host and forwarding them to another host over a private network such as
WireGuard.

Example topology:

- A environment: externally reachable edge host, WireGuard IP `172.31.255.1`
- B environment: service host, WireGuard IP `172.31.255.2`
- External client connects to A, A forwards raw TCP/UDP traffic to B

## Build

```sh
go build -o perfect-proxy .
```

## Release

GitHub Actions runs tests on pushes and pull requests. Pushing a tag like
`v0.1.0` builds release archives and uploads them to GitHub Releases.

```sh
git tag v0.1.0
git push origin v0.1.0
```

Release assets include Linux, macOS, Windows, and Raspberry Pi-friendly
`linux/arm/v6` and `linux/arm/v7` builds.

## Configure

Create `config.json` from `config.example.json`.

```json
{
  "udp_idle_timeout": "2m",
  "routes": [
    {
      "name": "web-to-b",
      "proto": "tcp",
      "listen": "0.0.0.0:80",
      "target": "172.31.255.2:80"
    },
    {
      "name": "dns-to-b",
      "proto": "udp",
      "listen": "0.0.0.0:53",
      "target": "172.31.255.2:53"
    }
  ]
}
```

Run it on A:

```sh
sudo ./perfect-proxy -config config.json
```

Ports below 1024 usually require root or `CAP_NET_BIND_SERVICE`.

## systemd

Install the binary and config:

```sh
sudo install -m 0755 perfect-proxy /usr/local/bin/perfect-proxy
sudo install -m 0644 config.json /etc/perfect-proxy.json
sudo install -m 0644 systemd/perfect-proxy.service /etc/systemd/system/perfect-proxy.service
sudo systemctl daemon-reload
sudo systemctl enable --now perfect-proxy
```

## Notes

- TCP routes forward streams bidirectionally until either side closes.
- UDP routes create a temporary upstream socket per client address and remove it
  after `udp_idle_timeout`.
- HTTP routes work as an HTTP reverse proxy. Use this when another reverse proxy
  terminates HTTPS and forwards plain HTTP to `perfect-proxy`.
- This works for many raw TCP/UDP protocols, but protocols that embed IP
  addresses or ports inside their payload may still need protocol-specific
  handling.
- The B-side service must listen on an address reachable from A, for example
  `172.31.255.2` or `0.0.0.0`.

## HTTPS Front Proxy

If HTTPS is terminated by a front reverse proxy, route that proxy to an HTTP
route instead of a raw TCP route:

```json
{
  "name": "lodge-home",
  "proto": "tcp",
  "listen": "0.0.0.0:8123",
  "target": "172.31.255.2:8123"
}
```

For raw TCP and UDP routes, `target` is a network address. Use
`172.31.255.2:8123`, not `http://172.31.255.2:8123`. URL-style targets are
accepted for compatibility, but only the host and port are used.

Use an HTTP route only when this process needs to rewrite HTTP headers before
forwarding to B:

```json
{
  "name": "lodge-home-http",
  "proto": "http",
  "listen": "127.0.0.1:8080",
  "target": "http://172.31.255.2:8123"
}
```

Then configure the front proxy upstream to `http://127.0.0.1:8080`.

By default, HTTP routes strip `Forwarded`, `X-Forwarded-*`, and `X-Real-IP`
headers so the upstream sees a request similar to a direct request. This is
useful for services such as Home Assistant unless `trusted_proxies` is
configured on the upstream.

`host_header` is optional. Set it only when the B-side HTTP server needs a
specific Host value. `forward_headers` is also optional and defaults to `false`;
set it to `true` only when the upstream is explicitly configured to trust this
proxy.

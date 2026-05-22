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
- This works for many raw TCP/UDP protocols, but protocols that embed IP
  addresses or ports inside their payload may still need protocol-specific
  handling.
- The B-side service must listen on an address reachable from A, for example
  `172.31.255.2` or `0.0.0.0`.

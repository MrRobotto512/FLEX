# Remote access with Tailscale

## How the server joins your tailnet

`server/tsnet.go` embeds Tailscale with [tsnet](https://tailscale.com/kb/1244/tsnet). When `FLEX_TS_AUTHKEY` is set the server starts a second listener on your tailnet as the node `FLEX_TS_HOSTNAME` (default `flex`). Any Tailscale device then reaches it at `http://flex:8484` (MagicDNS) or `http://flex.<tailnet>.ts.net:8484`.

- No port forwarding and no separate Tailscale install on the PC are required (the normal Tailscale client can still be installed alongside).
- Traffic inside the tailnet is WireGuard-encrypted end to end, so the API stays plain HTTP.
- Every request's source address is resolved with `WhoIs`, so the server knows which of your devices is asking. That is the whole auth story for remote access.
- Requests arriving over the tailnet are flagged `remote: true` in `GET /server`; clients default to the `remote` transcode profile, sized to your home upload speed.

Generate the auth key in the Tailscale admin console → Settings → Keys: **reusable**, **pre-authorized**, optionally tagged `tag:flex` so ACLs can target it.

## Phones and laptops

Install Tailscale, sign in to the same tailnet, open `http://flex:8484`. Done.

## The PS5

The PS5 cannot run Tailscale and no payload is likely to change that. Away from home it needs another device on the remote network to carry it into your tailnet:

1. A **travel router with Tailscale built in** (GL.iNet's current firmware has it). Join it to the tailnet, enable "use exit node" pointing at your PC or accept the home subnet route, and connect the PS5 to the router's Wi-Fi. The PS5 then sees `flex` exactly as if it were at home, apart from discovery.
2. On the PC, advertise your home LAN as a subnet route (`tailscale up --advertise-routes=192.168.0.0/24`) **or** advertise it as an exit node; approve the route in the admin console.
3. Discovery broadcasts don't cross the tunnel, so on the PS5 enter the server address once (`flex` resolves via MagicDNS if the travel router hands out Tailscale DNS; otherwise the server's `100.x.y.z` address). The client remembers it and falls back to it whenever LAN discovery finds nothing.

## Bandwidth

The `remote` profile defaults to ≤1080p H.264 at 6 Mbps. If your home upload is below ~10 Mbps, set it to 720p/3 Mbps in the desktop UI. Tailscale uses a relay (DERP) when it can't punch through NAT; expect somewhat higher latency on first connection and lower throughput there.

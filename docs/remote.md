# Remote access with Tailscale

## Turning it on (desktop app)

1. In FLEX.exe, tick **Enable remote access through Tailscale** and press **Save & restart server**.
2. The status line changes to "Waiting for you to sign in to Tailscale". Press **Sign in to Tailscale…** (or use the tray menu's *Remote access* item) and sign in with your Tailscale account in the browser that opens. This happens once; FLEX keeps its node identity in its data folder (`tsnet`).
3. The status shows **Connected as flex.<your-tailnet>.ts.net (100.x.y.z)**, and the tray menu shows the same address.

Headless (`flexd`): set `FLEX_TS_ENABLE=1`, start it, and open the sign-in URL from the log at `-debug` level, or set `FLEX_TS_AUTHKEY` to a reusable, pre-authorized key from the Tailscale admin console instead.

## How it works

`server/tsnet.go` embeds Tailscale with [tsnet](https://tailscale.com/kb/1244/tsnet): the server is its own device on your tailnet (default name `flex`), separate from any Tailscale client installed on the PC. It serves the same API there.

- No port forwarding.
- Tailnet traffic is WireGuard-encrypted end to end, so the API stays plain HTTP.
- Each request's Tailscale identity is resolved with `WhoIs`. Only devices on your tailnet can reach the node at all.
- Requests arriving over the tailnet use the `remote` transcode profile (≤1080p H.264, 6 Mbps), sized for home upload speeds, even when the PS5 client asks for `ps5`.

## Phones and laptops

Install Tailscale, sign in to the same tailnet, and open `http://flex:8484` (or the full `flex.<tailnet>.ts.net` name). The web UI works as at home.

## The PS5

The PS5 can't run Tailscale. Away from home it needs a **travel router with Tailscale built in** (GL.iNet's firmware has it) that is signed in to your tailnet. Connect the PS5 to that router's Wi-Fi.

The PS5 client remembers the server's Tailscale IP each time it finds the server at home: the server includes it in the discovery reply and in `/server`, and the client writes it to `/data/homebrew/FLEX/server.txt`. Away from home, discovery and the saved LAN address fail fast (3 s connect timeout), and the client connects to the Tailscale IP. The status line then says "(remote)". To use this, open FLEX on the PS5 at home once after enabling remote access.

## Bandwidth

The `remote` profile defaults to ≤1080p H.264 at 6 Mbps. If your home upload is below ~10 Mbps, expect buffering. Tailscale uses a relay (DERP) when it can't punch through NAT; throughput is lower there.

# HDHR Monitor

A small web app for watching HDHomeRun tuner signal live and changing channels,
built for standing on a roof with a phone while aiming an antenna.

- Signal strength, SNR quality, symbol quality and errors per second, updated
  twice a second while the page is open, with a 1, 5 or 15 minute graph
- Optional beep whose pitch follows SNR, so you can aim without looking
- Change channels by station, network name or RF channel, with favorites saved
  on the server
- Watch tuners other apps are using; taking one over needs a press and hold
- Several HDHomeRuns at once
- Installs to an Android home screen and keeps the screen on

The app talks to each HDHomeRun on your network the same way `hdhomerun_config`
does, and only while a page is open.

## Run it

```yaml
services:
  hdhr-monitor:
    image: ghcr.io/yesterdayforgotten/hdhr-monitor:latest
    restart: unless-stopped
    network_mode: host
    environment:
      HDHR_DEVICES: 10.10.0.54
    volumes:
      - hdhr-data:/data

volumes:
  hdhr-data:
```

`docker compose up -d`, then open `http://<host>:8089` on your phone.

Host networking lets the app see each browser's real address, which decides who
needs a login (see [Access](#access)). The image runs on amd64 and arm64.

## Settings

| Variable | Default | |
| --- | --- | --- |
| `HDHR_DEVICES` | required | HDHomeRun IPs or hostnames, comma-separated |
| `HDHR_LISTEN` | `:8089` | Listen address |
| `HDHR_PASSWORD` | unset | Password for access from outside your network; unset refuses it |
| `HDHR_USERNAME` | `admin` | Username for that login |
| `HDHR_AUTH` | `on` | `off` lets every request in, for when a proxy or firewall in front already limits who can connect |
| `HDHR_TRUSTED_NETS` | private ranges | CIDRs that need no login |
| `HDHR_TRUSTED_PROXIES` | none | Reverse proxy IPs or CIDRs whose `X-Forwarded-For` is believed |
| `HDHR_LEASE` | `60s` | How long a tuner stays taken after its page stops polling |
| `HDHR_GUIDE` | on | `0` stops fetching network names and logos from Silicondust's guide service |
| `HDHR_DATA_DIR` | `/data` | Where favorites and the guide cache are saved |
| `HDHR_MEASURE` | on | `0` stops measuring ATSC 3.0 bitrate (see [ATSC 3.0](#atsc-30)) |
| `HDHR_STREAM_PORT` | `8089` | UDP port the measurement streams arrive on |
| `HDHR_STREAM_HOST` | found per device | Address the HDHomeRun should send them to |

## Tuners

Tuning from the page locks the tuner on the HDHomeRun, so a DVR or another
viewer can't take it mid-adjustment. The lock is released when you leave the
page, or a minute after it stops polling (phone asleep, tab closed).

A tuner already in use, by a TV app, a DVR or another phone, can be watched but
not changed until you press and hold to take it, which cuts off whoever was
using it.

## ATSC 3.0

ATSC 3.0 tuners (FLEX 4K, CONNECT 4K) report a station's bitrate only while
streaming it. So while a page holds a 3.0 tuner, the app has the tuner stream
the station to the app, which counts it and throws it away. That's the
station's bitrate in network traffic (a few Mbps) per tuner, only while a page
is watching it; `HDHR_MEASURE=0` turns it off. ATSC 1.0 stations need none of
this.

The HDHomeRun has to reach the app on UDP port 8089. With host networking it
does, and the app works out its own address. Behind Docker's bridge network,
publish the port (`8089:8089/udp`) and set `HDHR_STREAM_HOST` to the Docker
host's address.

## Access

Requests from private networks need no login. Anything else, including any
request that came through a reverse proxy, needs `HDHR_PASSWORD`, and is
refused when it isn't set.

### A LAN-only subdomain

To reach it at `https://hdhr.example.com` from home, with HTTPS (which lets
Android keep the screen on properly), but not from the internet:

1. **Proxy it** and refuse other addresses at the proxy. For Apache with an
   existing certificate:

   ```apache
   <VirtualHost *:443>
      ServerName hdhr.example.com

      <Location />
         Require ip 10.10.0.0/23
      </Location>

      ProxyPreserveHost On
      ProxyPass / http://10.10.0.2:8089/
      ProxyPassReverse / http://10.10.0.2:8089/
      RequestHeader set X-Forwarded-Proto "https"

      SSLCertificateFile /etc/letsencrypt/live/example.com/fullchain.pem
      SSLCertificateKeyFile /etc/letsencrypt/live/example.com/privkey.pem
      Include /etc/letsencrypt/options-ssl-apache.conf
   </VirtualHost>
   ```

2. **Let the proxy's requests in.** Either set `HDHR_AUTH=off` and leave access
   to the proxy alone, or set `HDHR_TRUSTED_PROXIES` to the proxy's address so
   the app also checks the client address the proxy reports. With Docker bridge
   networking, the proxy shows up as Docker's gateway (in `172.16.0.0/12`), not
   the host's own address.

3. **Point the name at the proxy on your network** with a DNS override in your
   router, so phones at home connect directly instead of through your public
   IP.

## Development

```sh
HDHR_DEVICES=10.10.0.54 HDHR_DATA_DIR=./data go run ./cmd/hdhr-monitor
go test ./...
```

Tests run against a fake HDHomeRun (`internal/hdhrtest`).

## License

MIT

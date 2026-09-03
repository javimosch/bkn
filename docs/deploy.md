# Deploying bkn

The reference deployment is `bkn.intrane.fr` on dk1: systemd runs the binary on
loopback, Traefik terminates TLS and forwards, Cloudflare holds the DNS record.

## The one thing that will bite you

`bkn serve` binds `127.0.0.1` by default, and with no `BKN_ADMIN_TOKEN` set it
treats a loopback bind as "only a co-resident process can reach me". That is
true on a laptop and **false the moment a reverse proxy listens publicly and
forwards to it** — which is exactly this deployment.

bkn refuses the loopback exemption for any request carrying a forwarding
header (`X-Forwarded-For`, `X-Forwarded-Host`, `X-Real-Ip`, `Forwarded`,
`X-Forwarded-Proto`), so a proxied request can never inherit local trust, and
it says so on startup. **Set `BKN_ADMIN_TOKEN` anyway**: it is what actually
gates the admin routes, and it does not depend on a proxy sending the headers
it is supposed to.

## Layout

```
/opt/bkn/bkn              the binary
/opt/bkn/release/bkn      what GET /version and /dl/bkn publish to other machines
/opt/bkn/home/            data: bkn.db, files/
/etc/bkn/bkn.env          secrets, mode 600
/etc/systemd/system/bkn.service
```

## The unit

```ini
[Unit]
Description=bkn — single-binary backend core
After=network.target

[Service]
User=dk1
WorkingDirectory=/opt/bkn
EnvironmentFile=/etc/bkn/bkn.env
ExecStart=/opt/bkn/bkn serve --host 127.0.0.1 --port 8804
Restart=on-failure
RestartSec=2

[Install]
WantedBy=multi-user.target
```

## The environment

```sh
BKN_HOME=/opt/bkn/home
BKN_DATA=/opt/bkn/home/bkn.db
BKN_FILES_DIR=/opt/bkn/home/files
BKN_RELEASE_DIR=/opt/bkn/release
BKN_ADMIN_TOKEN=<openssl rand -hex 32>     # gates every admin route
BKN_ENCRYPTION_KEY=<openssl rand -hex 32>  # encrypted kv values
BKN_AUTH_SECRET=<openssl rand -hex 32>     # token signing; shared across instances
BKN_URL=https://bkn.intrane.fr             # where `bkn feedback` posts
BKN_SERVER=https://bkn.intrane.fr          # where `bkn update` looks
BKN_NO_NUDGE=1                             # a server does not need a nudge
```

Losing `BKN_ENCRYPTION_KEY` orphans every encrypted kv value; losing
`BKN_AUTH_SECRET` invalidates every outstanding access token. Back up
`/etc/bkn/bkn.env` **and** `/opt/bkn/home` — either alone is not much use.

## Routing (hotify on dk1)

```sh
hotify-cli add -id bkn -name "bkn backend core" -domain bkn -port 8804 \
  -cmd "sudo systemctl restart bkn" -local -setup-dns
hotify-cli setup-traefik -id bkn -local --challenge-type dns
```

The HTTP-01 challenge timed out on first issue here; DNS-01 succeeded
immediately, since hotify already holds the Cloudflare credentials. Try
`--challenge-type dns` first rather than waiting HTTP-01 out.

`-cmd` delegates to systemd on purpose. Letting hotify spawn its own copy
would put a second process on the same port, which fails to bind and looks
like a routing problem.

## Publishing an update for other machines

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o bkn .
scp bkn dk1:/opt/bkn/release/bkn
```

`GET /version` re-hashes the file on every request, so a copy is the whole
publish step — there is no version to bump. Clients then:

```sh
BKN_SERVER=https://bkn.intrane.fr bkn update
```

Updating the server's own binary is a separate step (`/opt/bkn/bkn`), so
publishing an artifact never restarts the thing serving it.

## Verifying a deployment

```sh
curl -s https://bkn.intrane.fr/_health
curl -s https://bkn.intrane.fr/version
curl -s -o /dev/null -w '%{http_code}\n' https://bkn.intrane.fr/v1/kv    # must be 403
```

That last one is the important one: `403` means the admin surface is closed.
A `200` means the token is missing and the data is public.

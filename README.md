# nanogateway

Every channel, one door: the adapters that turn a chat platform into the platform's
own message shape.

It is one of the eight first-class services. It owns no data — it authenticates,
resolves who is talking, and hands the message to the bus.

## What it is

- **Five channels out of the box.** Web chat, WeCom, Feishu, WeChat customer
  service and a CLI — each one an adapter behind the same interface.
- **Identity resolution.** A platform identity becomes a user and a binding; a
  first-time sender is onboarded on the spot.
- **Three paths.** Inbound to the agent, outbound to the channel, and the streamed
  path the web socket uses for a live answer.
- **Acknowledgements that you can turn off.** One config item decides whether a
  channel answers "received" before the agent starts thinking.

## Where it sits

| | |
|---|---|
| Collections | none — the gateway stores nothing |
| Port | `8080` in the network, `14402` on the host (loopback) |
| Upstream | the chat platforms, and the browser for web chat |
| Downstream | `nanobus` (messages), `nanoadmin` (channel configuration), `nanoportal` (who owns an agent), `nanoauth` (token validation) |
| Health | `GET /healthz` and `GET /metrics` on the same port |

## Quick start

```bash
go build ./cmd/nanogateway
NANOSEC_GATEWAY_BUS_URL="http://localhost:9000" \
NANOSEC_BUS_INTERNAL_TOKEN="…" \
  ./nanogateway
```

At least one channel has to be enabled in `admin_config`, or the gateway exits with
"no channels enabled". The web channel is on by default. For a one-shot smoke test,
the CLI channel sends a single message and exits:

```bash
./nanogateway --bus-url http://localhost:9000 --message "hello"
```

## Configuration

| Variable | Meaning |
|---|---|
| `NANOSEC_GATEWAY_BUS_URL` | The bus, HTTP and WebSocket |
| `NANOSEC_BUS_INTERNAL_TOKEN` | Shared secret for the bus's internal surfaces |
| `NANOSEC_ADMIN_URL` | Where the channel configuration is read |
| `NANOSEC_PORTAL_URL` | Where agent rows and the conversation projection live |
| `NANOSEC_PROVISIONER_URL` | Where tunnels terminate (the terminal bridge dials it first) |
| `NANOAUTH_JWKS_URL` | Key set the web channel validates browser tokens against |

Channel secrets are not environment variables: they live in `admin_config` under
`channels.<name>.*` (corp id, app secret, callback token, encoding key) and are
edited in the console.

## Design decisions

- **One interface, five adapters.** A channel implements receive, send and stream;
  everything else — deduplication, acknowledgement, the outbound poll — is shared.
- **The gateway does not own state.** Reconnects rebuild from the bus's cursor, so
  any replica can serve any conversation.
- **Only the web channel authenticates browsers.** The other four trust the platform
  signature; the web channel validates the JWT against nanoauth's JWKS.
- **Streaming is SSE, not polling.** The console subscribes; the gateway forwards
  what the bus already wrote.

## Docs

- [`docs/ARCHITECTURE.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/ARCHITECTURE.md) — the platform around this module
- [`docs/DEPLOYMENT.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/DEPLOYMENT.md) — ports and operations
- [`docs/README.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/README.md) — every document

## Status

Released as part of `v0.1.0`; the version of this module is pinned in the
super-repo's `versions.lock`.

## Contributing and security

[`CONTRIBUTING.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/CONTRIBUTING.md)
for setup and commit rules; report vulnerabilities privately through
[`SECURITY.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/SECURITY.md), never
in a public issue.

## License

[AGPL-3.0](./LICENSE). The name and logo are covered separately — see
[`NOTICE`](./NOTICE).

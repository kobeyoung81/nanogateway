# nanogateway

Chat platform adapter service for MyriadMind.

## Overview

`nanogateway` connects to multiple chat platforms, routes inbound messages to the central bus (`nanobus`), polls / subscribes for outbound responses, and delivers them back to the correct platform. It handles user identity resolution, optional message acknowledgements, JWT authentication for the browser channel, and media attachment routing.

## Architecture

```
Chat Platforms:
  CLI (stdin) -->  nanogateway  <--WebSocket/HTTP--> nanobus
  WeCom       -->    |-- gateway/
  Feishu      -->    |   |-- gateway.go (poll/outbound loop)
  Web Chat    -->    |   |-- client.go  (BusClient)
  WeChat KF   -->
                     |-- channel/
                          |-- channel.go  (Channel + StreamSender interfaces)
                          |-- cli/         (stdin/stdout)
                          |-- wecom/       (HTTP callback, go-workwx)
                          |-- feishu/      (WebSocket, oapi-sdk-go)
                          |-- web/         (browser WebSocket chat, JWT + streaming)
                          |-- wechatkf/    (polling + HTTP callback)
```

## Supported Channels

| Channel | Name | Protocol | Description |
|---------|------|----------|-------------|
| CLI | `cli` | stdin/stdout | Interactive development mode, one-shot via `--message` |
| WeCom | `wecom` | HTTP callback | Enterprise WeChat (go-workwx fork) |
| Feishu | `feishu` | WebSocket | Lark/Feishu (oapi-sdk-go/v3) |
| Web Chat | `web` | WebSocket | Authenticated browser chat with streaming (JWT via JWKS) |
| WeChat KF | `wechatkf` | Polling + HTTP | WeChat Customer Service with cursor-based sync |

## Message Flow

### Inbound (User -> Agent)
1. Channel adapter receives message from platform
2. `ResolveUser(channel, senderID)` -> bus returns `{userID, bindingID, isNew}`
3. Gateway posts `InboundMessage` with `userID`, `bindingID`, content, media to bus

### Outbound (Agent -> User)
1. Gateway polls bus every 500ms per channel: `PollOutbound(channel, gatewayID)` (SSE subscription for streaming channels)
2. Bus returns claimed `OutboundMessage` (atomically claimed via `FOR UPDATE`)
3. Gateway calls `ch.Send()` on the matching channel adapter

### Streaming (Web Chat)
The web channel implements `StreamSender` (`SendChunk` / `SendComplete`) so the agent's
streaming tokens are forwarded to the browser in near-real-time.

### Acknowledgement
Optional ack text sent immediately when an inbound message is received:
```json
{ "ack": { "enabled": true, "text": "Message received, processing..." } }
```

## Channel Interface

```go
type Channel interface {
    Name() string
    Start(ctx context.Context, handler InboundHandler) error
    Send(ctx context.Context, msg OutboundMessage) error
    Stop() error
}

// Optional for streaming delivery (e.g. the browser WebSocket channel).
type StreamSender interface {
    SendChunk(ctx context.Context, chunk nanoprotocol.OutboundChunk) error
    SendComplete(ctx context.Context, complete nanoprotocol.OutboundComplete) error
}
```

## Adding a New Channel

1. Create `internal/channel/<name>/adapter.go`
2. Implement the `Channel` interface
3. Add config struct to `internal/config/config.go`
4. Wire it up in `cmd/nanogateway/main.go`

## CLI Reference

```bash
nanogateway                              # Start gateway with configured channels
nanogateway --bus-url ws://bus:9000      # Specify bus URL
nanogateway --message "Hello"            # One-shot CLI mode (sends and exits)
nanogateway version                      # Print version info
```

## Configuration

Loaded from the bus at startup via `GetConfigItems("gateway")` (seeded by `nanoadmin config seed` and editable in the portal → Configs).

```json
{
  "channels": {
    "cli": { "enabled": true, "userID": "alice" },
    "wecom": { "enabled": false, "corpID": "", "corpSecret": "", "agentID": 0, "token": "", "encodingAESKey": "", "callbackAddr": ":8081" },
    "feishu": { "enabled": false, "appID": "", "appSecret": "", "encryptKey": "", "verificationToken": "" },
    "web": { "enabled": true, "listenAddr": ":8080" },
    "wechatkf": { "enabled": false, "corpID": "", "kfSecret": "", "openKfID": "", "token": "", "encodingAESKey": "", "callbackAddr": ":8082" }
  },
  "ack": { "enabled": false, "text": "Message received, processing..." },
  "auth": { "jwksUrl": "http://nanoauth:8081/auth/v1/.well-known/jwks" }
}
```

Environment prefix: `NANOSEC_GATEWAY_` (e.g., `NANOSEC_GATEWAY_BUS_URL`). The `auth.jwksUrl` item enables JWT validation for the web channel.

> The web channel is **enabled by default** (`channels.web.enabled=true`, seeded by
> `nanoadmin config seed`). It validates browser tokens through the bus
> (`validate_token` → nanoauth) and lists agents via the bus `list_agents` RPC, so the
> portal Chat page works out of the box when the gateway publishes port `8080`.

## Deployment

### Docker
```bash
docker build -t nanogateway .
docker run nanogateway --bus-url ws://nanobus:9000
```

### Docker Compose
Part of top-level `docker-compose.yml`. Configure at least one channel in the bus
config items first, otherwise the process exits with "no channels enabled".

### Standalone
```bash
go build ./cmd/nanogateway
./nanogateway --bus-url ws://localhost:9000
```

## Packages

| Package | Purpose |
|---------|---------|
| `cmd/nanogateway` | CLI entry point |
| `internal/gateway` | Gateway loop, outbound polling, BusClient |
| `internal/channel` | Channel + StreamSender interfaces, 5 adapter implementations |
| `internal/auth` | JWKS validator for the web channel |
| `internal/config` | Configuration types and bus-backed loader |
| `internal/log` | Custom slog log level |
| `go-workwx/` | Vendored WeCom API fork |

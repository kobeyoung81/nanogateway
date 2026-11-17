# nanogateway

所有渠道，一个门：把聊天平台的消息变成平台自己的形状。

它是八个一级服务之一。它不持有任何数据 —— 只负责认人、找到谁来对话，然后把消息交给总线。

## 它是什么

- **开箱五个渠道。** 网页对话、企业微信、飞书、微信客服与 CLI，各是一个适配器，同一个接口。
- **身份解析。** 平台身份变成用户与绑定；第一次发言的人当场完成开户。
- **三条路径。** 入站给智能体、出站回渠道、以及网页 socket 用的流式路径。
- **可以关掉的回执。** 一个配置项决定渠道要不要在智能体开始思考前先回一句「收到」。

## 它在哪

| | |
|---|---|
| 集合 | 无 —— 网关不存任何东西 |
| 端口 | 网络内 `8080`，宿主机 `14402`（绑 loopback） |
| 上游 | 各聊天平台；网页对话时还有浏览器 |
| 下游 | `nanobus`（消息）、`nanoadmin`（渠道配置）、`nanoportal`（智能体归属）、`nanoauth`（令牌校验） |
| 健康 | 同一端口上的 `GET /healthz` 与 `GET /metrics` |

## 快速开始

```bash
go build ./cmd/nanogateway
NANOSEC_GATEWAY_BUS_URL="http://localhost:9000" \
NANOSEC_BUS_INTERNAL_TOKEN="…" \
  ./nanogateway
```

`admin_config` 里至少要启用一个渠道，否则网关会以「no channels enabled」退出。
网页渠道默认开启。想快速冒烟，CLI 渠道发一条就退出：

```bash
./nanogateway --bus-url http://localhost:9000 --message "hello"
```

## 配置

| 变量 | 含义 |
|---|---|
| `NANOSEC_GATEWAY_BUS_URL` | 总线地址，HTTP 与 WebSocket |
| `NANOSEC_BUS_INTERNAL_TOKEN` | 总线内部面用的共享密钥 |
| `NANOSEC_ADMIN_URL` | 去哪儿读渠道配置 |
| `NANOSEC_PORTAL_URL` | 智能体行与会话投影在哪儿 |
| `NANOSEC_PROVISIONER_URL` | 隧道在哪儿终止（终端桥优先拨它） |
| `NANOAUTH_JWKS_URL` | 网页渠道校验浏览器令牌用的公钥集 |

渠道密钥不是环境变量：它们在 `admin_config` 的 `channels.<name>.*` 下（企业 id、应用密钥、
回调令牌、加密密钥），在控制台编辑。

## 设计决策

- **一个接口，五个适配器。** 渠道只实现收、发、流；去重、回执、出站轮询都是共用的。
- **网关不持有状态。** 重连按总线的游标重建，所以任何副本都能服务任何会话。
- **只有网页渠道校验浏览器。** 另外四个信任平台签名；网页渠道按 nanoauth 的 JWKS 验 JWT。
- **流式走 SSE，不轮询。** 控制台订阅，网关转发总线已经写下的内容。

## 文档

- [`docs/ARCHITECTURE.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/ARCHITECTURE.md) —— 它所在的整个平台
- [`docs/DEPLOYMENT.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/DEPLOYMENT.md) —— 端口与运维
- [`docs/README.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/docs/README.md) —— 全部文档索引

## 状态

随 `v0.1.0` 发布；本模块的版本钉在超仓的 `versions.lock` 里。

## 贡献与安全

开发与提交规则见
[`CONTRIBUTING.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/CONTRIBUTING.md)；
漏洞请按
[`SECURITY.md`](https://gitee.com/kobeyoung81/nanosec/blob/master/SECURITY.md) 私下报告，
不要开公开 issue。

## 许可证

[AGPL-3.0](./LICENSE)。名称与标志另有说明 —— 见 [`NOTICE`](./NOTICE)。

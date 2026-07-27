# cpa-plugin-anti-model-fallback

[![CI](https://github.com/lkangd/cpa-plugin-anti-model-fallback/actions/workflows/ci.yml/badge.svg)](https://github.com/lkangd/cpa-plugin-anti-model-fallback/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

CLIProxyAPI(cpa)插件。阻止上游"模型兜底"—— 即请求 A 模型、上游却用 B 模型处理并以 HTTP 200 返回。

对受保护模型:检测到兜底 → 不返回给客户端 → 重试 → 仍兜底则返回 503 错误。

## 为什么需要它

兜底响应的状态码是 **200**,不是错误。cpa 内置的 `request-retry` 只在 403/408/5xx 时触发,
因此完全抓不到这种情况 —— 客户端会静默拿到另一个模型的输出。

## 工作原理

插件同时注册 **Model Router** 和 **Executor**:

1. Router 只对配置里声明的受保护模型返回 `Handled=true, TargetKind=self`,其它请求原样交还宿主,零干预。
2. Executor 掌控执行循环,通过 `host.model.execute` / `host.model.execute_stream` 重放请求,
   解析响应中的**处理模型**,不匹配就重试。

响应拦截器做不到这件事:`ResponseInterceptResponse` 只能改写已到手的响应,没有"重新执行"的返回路径。
详见 [ADR 0001](docs/adr/0001-router-executor-over-interceptor.md)。

### 处理模型从哪读

- **流式**:SSE 第一个事件 `message_start` 的 `message.model`(OpenAI 风格则读顶层 `model`)。
- **非流式**:响应 body 顶层 `model`。
- **不在响应 header 里** —— 实测确认。

流式路径在判定通过前**不向客户端发送任何字节**:先缓冲到能读出模型的那个 chunk,
判定不匹配就关掉上游流重试,客户端看不到半个兜底响应。因为模型名在流的最开头,
缓冲量只有几百字节,重试也几乎不烧 token。

### 递归防护

Executor 调 `host.model.execute` 会重新走宿主执行链,可能再次命中本插件的 Router 造成死循环。
防护有两层:

1. 重放请求带上 `X-Cpa-Anti-Model-Fallback-Bypass` 头,Router 见到就放行(`Handled=false`)。
2. 熔断:并发执行数超过 64 时 Router 一律放行,万一 header 被宿主丢弃也不会无限递归。

## 构建

需要 Go(CGO)+ 编译器。cpa 必须是带插件支持的构建 —— 验证:

```bash
curl -s -o /dev/null -D - http://127.0.0.1:8317/v0/management/ | grep -i x-cpa-support-plugin
# X-Cpa-Support-Plugin: 1
```

```bash
make test      # 单元测试
make build     # 产出 dist/<goos>/<goarch>/anti-model-fallback-v0.1.0.<ext>
make install   # 复制到 ~/.cli-proxy-api/plugins/<goos>/<goarch>/
```

`make install` 的目标目录必须与 config.yaml 里的 `plugins.dir` 一致,通过 `INSTALL_DIR=` 覆盖。

## 配置

```yaml
plugins:
  enabled: true
  dir: "~/.cli-proxy-api/plugins"
  configs:
    anti-model-fallback:
      enabled: true
      priority: 1
      max_retries: 10        # 全局默认重试预算
      retry_delay_ms: 200    # 两次重试之间的固定延迟
      protected_models:
        - model: "my-glm-5.2"      # 客户端请求的模型名
          expect_model: "glm-5.2"  # 期望上游报告的处理模型名
        - model: "my-glm-5.1"
          expect_model: "glm-5.1"
          max_retries: 3           # 可选,覆盖全局预算
```

### `expect_model` 什么时候要写

cpa 的 alias 会把 `my-glm-5.2` 改写成 `glm-5.2` 再发给上游,上游报告的也是 `glm-5.2`。
匹配器能自动桥接这种"别名前缀"(`my-glm-5.2` 以 `glm-5.2` 结尾即视为命中),
所以上面的 `expect_model` 其实可省。当别名与上游名毫无字面关系时才必须显式写。

### 匹配规则

处理模型与期望模型比对,大小写不敏感,满足任一即视为命中:

| 情况 | 期望 | 实际 | 结果 |
|---|---|---|---|
| 完全相等 | `glm-5.2` | `glm-5.2` | ✅ |
| 上游加版本后缀 | `glm-5.2` | `glm-5.2-0722` | ✅ |
| 别名前缀被剥离 | `my-glm-5.2` | `glm-5.2` | ✅ |
| 兜底 | `glm-5.2` | `kimi-for-coding` | ❌ 重试 |
| 邻近版本 | `glm-5.2` | `glm-5.1` | ❌ 重试 |

前缀/后缀放宽都要求边界字符(`-` `_` `.` `/` `:`),所以 `glm-5.2` 不会误配 `glm-5.21`。

## 行为约定

| 场景 | 行为 |
|---|---|
| 未受保护的模型 | 完全不干预,Router 直接放行 |
| 检测到兜底 | 丢弃该次响应,延迟后重试 |
| 上游真错误(4xx/5xx) | **立即透传**,不消耗重试预算 |
| 响应里没有模型字段 | **视为正常放行**(fail-open),不误重试 |
| 重试预算耗尽(非流式) | 返回 `model_fallback_blocked` 错误 + **HTTP 503** |
| 重试预算耗尽(流式) | 返回单条 SSE error 事件,状态码由宿主决定(实测 500) |

流式的状态码插件控制不了:`host.stream.close` 这个 RPC 只有 `Error string` 字段,没有状态位。
但因为判定通过前不发送任何字节,兜底内容不会泄露给客户端。

## 已知局限

重试有效的前提是兜底为**间歇性**。若某个上游是 100% 确定性兜底,插件会烧满 `max_retries`
后必然返回 503 —— 此时它的价值是"把静默的错误模型变成响亮的失败",而非恢复可用性。

开发本插件的那个上游就接近这种情况:历史日志 222 次全部兜底、零命中,
实测 11 次重试仍全数兜底,最终 503。详见 [CONTEXT.md](CONTEXT.md)。
恢复可用性需换渠道或找渠道商补额度。

非流式每次重试都完整烧一遍 token(要读完整个 body 才知道模型);流式在开头就能判定,成本很低。
因此 `max_retries` 不宜设过大。

## License

[MIT](LICENSE)

# CONTEXT — Anti Model Fallback 插件术语表

本项目是 CLIProxyAPI(cpa)的一个自定义插件,目的是阻止"模型兜底"。
本文件只是术语表(glossary),不放实现细节。

## 术语

### 模型兜底 (Model Fallback)
上游在收到某个模型请求后,实际用**另一个不同的模型**来处理并返回,且 HTTP 状态为 200。
例:上游收到 `glm-5.2`,响应里体现的处理模型却是 `kimi-for-coding`。
因为状态是 200 而非错误码,cpa 内置的 `request-retry`(仅对 403/408/5xx 生效)抓不到它。

### 客户端模型 (Client Model)
客户端发出的模型名,即 cpa alias 改写**之前**的名字(SDK 里的 `RequestedModel`)。
例:`my-glm-5.2`。

### 上游模型 (Upstream Model)
cpa 经 alias/model-pool 改写**之后**、实际发给上游的模型名(SDK 里的 `Model`)。
例:`glm-5.2`。**兜底判定以此名为基准**,因为这才是上游收到的请求。

### 处理模型 (Processing Model)
上游实际用来处理并在响应中体现的模型名。发生兜底时,它 ≠ 上游模型。

位置(实测确认):
- **流式**:SSE 第一个事件 `message_start` 的 `message.model` 字段。
- **非流式**:响应 body 顶层的 `model` 字段。
- **不在响应 header 里** — 上游响应头只有 `Cache-Control` / `Content-Type` / `Date` /
  `Server: YKYW` / `Server-Timing` / `Set-Cookie` / `Strict-Transport-Security` /
  `Vary` / `X-Trace-Id`,零模型信息。

### 受保护模型 (Protected Model)
被显式声明"禁止兜底"的模型(例:`glm-5.2`)。只有受保护模型才会触发插件的检测与重试;
其它模型原样放行。

## 已确认事实(2026-07-27 实测,基于 cpa request-log)

- 兜底发生在**远端上游** `upstream.example.com/cn-cch`。
  cpa 只做了 `my-glm-5.2` → `glm-5.2` 的 alias 重命名,之后忠实转发 → 插件是正确的干预点。
- 客户端**同时存在 streaming 与非 streaming** 两种请求。
- **该上游的 glm 兜底命中率极低**,历史日志中为零:

  | 客户端模型 | 上游模型 | 处理模型 | 命中率 |
  |---|---|---|---|
  | `my-glm-5.1` | `glm-5.1` | `kimi-for-coding` | 0 / 162 |
  | `my-glm-5.2` | `glm-5.2` | `kimi-for-coding` | 0 / 60 |
  | `gpt-5.6-sol` | — | `gpt-5.6-sol` ✅ | 91 / 91 |
  | `gpt-5.4` | — | `gpt-5.4` ✅ | 1 / 1 |

  同域名的 gpt 系模型完全正常 → 不是域名问题,是 **glm 系模型在该渠道近乎无货**。

  补充(2026-07-27 手工测试):并非绝对确定性 —— 一次 16-token 的小请求正常返回了 `glm-5.2`,
  但同样条件下再测,连续 11 次尝试全部兜底成 `kimi-for-coding`。
  推论:重试有命中机会但概率极低,对这个上游多数情况仍会烧满 `max_retries` 后返回 503
  (见 ADR 0001 的"已知局限")。对其它间歇性兜底的渠道,重试仍然有效。

## 相关设计决策

- 插件形态与重试机制见 [ADR 0001](docs/adr/0001-router-executor-over-interceptor.md)。

# 0001 — 用 ModelRouter+Executor 掌控执行循环,而非响应拦截器

- 状态:已接受
- 日期:2026-07-24(2026-07-27 依据 request-log 实测修订)

## 背景

需求:对受保护模型(如 `glm-5.2`)阻止上游"模型兜底"。检测到处理模型 ≠ 上游模型时,
先不返回给客户端,而是重试请求直到处理模型一致;重试达到阈值仍兜底则返回错误。

直觉上"检测响应并重试"像是响应拦截器的活,但 cpa 的响应拦截器做不到。

## 决策

采用 `ModelRouter → self + Executor` 模式(参考官方示例
`examples/plugin/claude-web-search-router`):

1. 插件注册为 **Model Router**,只对受保护模型返回 `Handled=true, TargetKind=self`;
   其它模型 `Handled=false`,原样交还宿主,零干预。
2. 插件的 **Executor** 掌控执行循环:通过 **`host.model.execute` / `host.model.execute_stream`**
   重放请求 → 解析响应取出**处理模型** → 用**前缀/规范化匹配**判断是否兜底
   → 不一致则(等待固定小延迟后)重试,直到匹配或达到阈值。

关键规则:
- **判定基准**:拿处理模型与**上游模型**(alias 改写后,SDK 的 `Model`)比,
  不是与客户端模型(`RequestedModel`)比 —— 上游收到的是前者。
- **匹配**:处理模型以上游模型为前缀即视为命中(容忍 `glm-5.2-0722` 之类后缀),大小写不敏感。
- **上游真错误(429/5xx/超时)**:立即透传,不计入重试预算(插件只管兜底)。
- **模型字段缺失**:无法判断即视为正常放行(fail-open),不误重试。
- **阈值**:全局默认 `max_retries`,可按模型覆盖。
- **重试间隔**:固定小延迟,默认约 200ms。
- **耗尽**:返回标准错误 JSON(OpenAI/Claude 风格)+ HTTP 503。
- **重试通道**:走 `host.model.execute`,复用 cpa 凭据/路由,插件不硬编码上游 URL。

### 处理模型的提取(2026-07-27 实测修订)

早期假设"处理模型在响应 header 里"**已被实测推翻**。实际位置:

- **流式**:SSE 第一个事件 `message_start` 的 `message.model`。
  → 因此 streaming **需要缓冲**到第一个 `message_start` 事件才能判断。
    该事件在流的最开头且体积极小(几百字节),缓冲成本可忽略。
    判定不匹配 → 关流重试,**几乎不烧 token**;判定匹配 → 补发该 chunk,再转发后续。
- **非流式**:响应 body 顶层的 `model` 字段,需读完整 body。
  → 每次重试上游都已完整生成,**token 已烧**。

## 考虑过的替代方案

- **Response Interceptor(`response.intercept_after` / `intercept_stream_chunk`)**:
  否决。其返回类型 `ResponseInterceptResponse` 只有 Headers/Body/ClearHeaders,
  只能**改写**已到手的响应,**没有"重新执行"的返回路径**,无法重试。
  且流式拦截器是逐 chunk 改写,发现兜底时首个 chunk 往往已经发给客户端了。
- **靠 cpa 内置 `request-retry`**:无效。兜底响应是 **HTTP 200**,
  而 `request-retry` 只在 403/408/500/502/503/504 时触发。
- **改 cpa 配置(alias 指向别的源)**:能解决当前 `/cn-cch` 无货问题,
  但不构成通用护栏;与本插件不冲突,可并行。

## 后果

- 好处:能真正"检测→重试→耗尽报错",且只影响受保护模型,其它流量零开销。
- 代价:插件比一个拦截器重(要实现 Router + Executor 两套接口和流式桥接)。
- 成本:非流式每次重试都完整烧一遍 token;流式可在 `message_start` 处早停,成本低。
  故 `max_retries` 不宜设过大。

## 运行时验证(2026-07-27,cpa 7.2.100)

插件已装入 `~/.cli-proxy-api/plugins/darwin/arm64/` 并实测:

| 场景 | 结果 |
|---|---|
| 非流式 + 受保护模型被兜底 | 11 次尝试(1+10)后 **HTTP 503** `model_fallback_blocked`,耗时 21.4s |
| 流式 + 受保护模型被兜底 | 11 次尝试后单条 SSE error 事件,**未泄露任何兜底内容**,HTTP 500 |
| 未受保护模型(`gpt-5.6-sol`) | 完全不干预,HTTP 200,模型正确 |
| 递归防护 | 上游恰好收到 N 次请求(非指数爆炸)→ bypass 生效,无递归 |

两点实现修正来自这轮验证:

- `ExecutorResponse.Metadata["status_code"]` **不被宿主采纳**,返回的仍是 200。
  正确做法是返回错误信封并设置 ABI `Error.http_status` 字段。

- 流式耗尽时插件**不应自己 emit SSE error 事件**:宿主在 `host.stream.close`
  带错误信息时已经会生成一条,插件再发就重复了。
- 流式错误的状态码由宿主决定(实测 500),插件无法通过 `host.stream.close` 指定,
  因为该 RPC 只有 `Error string` 字段。非流式路径不受影响,仍是 503。

## 已知局限(重要)

本设计的收益前提是**兜底为间歇性**(重试有命中机会)。

上游 `upstream.example.com/cn-cch` 的 glm 兜底**接近但不完全是确定性**的:
历史日志 222 次全兜底、零命中,但 2026-07-27 手工测试中有一次小请求正常返回了 `glm-5.2`
(详见 [CONTEXT.md](../../CONTEXT.md))。因此重试**理论上有命中机会,实际命中率极低** ——
实测 11 次尝试仍全部兜底,最终 503。

这是**用户在知情后明确选择的行为** —— 宁可显式报错,也不接受静默串模型。
插件在此场景下的价值是"把静默的错误模型变成响亮的失败",而非恢复可用性。
恢复 glm 可用性需换渠道(config 里已有智谱官方源 `open.bigmodel.cn`)或找渠道商补额度。

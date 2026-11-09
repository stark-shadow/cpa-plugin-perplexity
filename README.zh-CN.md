# Perplexity — CLIProxyAPI 原生提供商插件

将已登录的 **Perplexity 网页会话**接入 CPA 的 OpenAI 兼容聊天接口。
独立个人仓库：`github.com/lzt404/cpa-plugin-perplexity`，并非 Sonar API，
不使用 API Key，也不伪造浏览器 OAuth 登录。

## 功能与限制

- 非流式 `chat.completion` 和增量 `chat.completion.chunk`；来源链接可选。
- 仅处理文本。多模态数组保留 `text`，忽略 `image_url`；不支持工具、附件、
  跨请求线程续接。`temperature`、`max_tokens` 可解析，但不传入网页请求。
- 模型目录默认缓存 6 小时，尽力持久化到 `<auth_dir>/.perplexity-catalog.json`（0600）；
  获取失败使用四个内置模型。不会把 ASI、浏览器代理、agentic research 等当成聊天模型。
- 使用宿主 HTTP 传输和浏览器常规请求头，**不添加 TLS 指纹依赖**。
- 用量及 token 计数是 **ceil(Unicode 字符数 / 4) 的估算**，不是账单数据。
- 网页接口可能变化；付费模型仍受账号套餐限制。仅使用你有权使用的账号。
  所有本地测试均为离线模拟，不宣称已通过真实账号验收。

## 安装与配置

需要 CPA v8 原生 ABI 1、schema 10，支持宿主 HTTP operation、流和认证存储回调。
SDK 固定为 `v8.0.61`，于 2026-11-09 通过 `go list -m -versions` 核实。
源码构建需要 Go 1.26.10+、C 编译器、Python 3。
将发行 ZIP 中的 `perplexity.so` 放入 CPA 插件目录；macOS 包内同名文件为 Mach-O 动态库。

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    perplexity:
      enabled: true
      priority: 0
      language: en-US
      sources: [web]               # web / scholar / social
      include_sources: true
      catalog_ttl: 6h
      request_timeout: 180s        # 深度研究建议提升到 10m 等更长时间
      max_prompt_chars: 48000      # 从开头截断，并加 [...truncated...]
      max_concurrency: 2           # 每个会话的并发提问上限
      pro_query_limit: 0           # 0 表示分母未知，不生成比例桶
      research_query_limit: 0
      exclude_models: []          # 精确匹配公开 ID 或内部 ID
      base_url: https://www.perplexity.ai
      user_agent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
```

未知字段、重复键、多份 YAML 文档、无效时长/参数会被拒绝；接受宿主注入的
`enabled`、`priority`、`store`。`base_url` 必须是带主机的 HTTP(S) URL，可含镜像路径前缀，不能含
用户名、查询串或片段。`max_prompt_chars` 至少为 17，以容纳截断标记。
镜像会收到账号 Cookie，请仅配置可信地址。

## 导入会话与管理

打开 CPA 的 `/v0/resource/plugins/perplexity/manage`。页面静态资源无需认证，
但管理操作必须通过 CPA 管理认证。按需填写管理密钥；仅保留于页面内存，不写入浏览器存储。

先在浏览器登录 perplexity.ai，再打开开发者工具 → Application → Cookies，复制
`__Secure-next-auth.session-token` 的值。支持粘贴纯 token、完整 `Cookie:` 头、
Cookie 文本或 JSON Cookie 对象。提交前清空输入框，不回显 token。
Cookie 等同密码，请勿发到聊天、日志、截图或公开 issue。

`auth.login.start` 明确返回 `unsupported_operation`（501）；真正登录入口是会话导入。
刷新仅探测 `/api/auth/session`，不会生成新 token；过期后需重新导入。

以下路径在 `/v0/management` 下，均受 CPA 管理认证保护：

| 方法 | 路径 | 请求 / 响应 |
| --- | --- | --- |
| POST | `/plugins/perplexity/auth/import` | `{session_token?, cookies?, label?}` → 201 `{email, plan, file}` |
| GET | `/plugins/perplexity/auth/status` | `{accounts:[{file,email,plan,validated_at,valid}]}` |
| POST | `/plugins/perplexity/auth/test` | `{file}` → 单个账号状态 |
| GET | `/plugins/perplexity/catalog` | `{tier, models}` |

`session_token` 优先于 Cookie jar 中的同名值；额外 Cookie 保留。
通过 `host.auth.save` 存为 `perplexity-<会话 SHA-256 前 12 位>.json`，重复导入更新同一文件。
删除账号仍使用 CPA 常规认证文件管理，本插件不提供 DELETE。
每次探测最长 8 秒；某个账号失败不导致整个状态接口失败。
`valid:false` 也可能是暂时网络不可达，不一定意味着会话已过期。
静态页面、CSS、JS 都由插件自身提供，无 CDN 或外部脚本。

## 模型、流与错误

| 公开 ID | mode | model_preference |
| --- | --- | --- |
| `pplx-auto` | concise | turbo |
| `pplx-pro` | copilot | pplx_pro |
| `pplx-reasoning` | copilot | pplx_reasoning |
| `pplx-deep-research` | copilot | pplx_alpha |
| `pplx-<内部 ID>` | copilot | 目录内部 ID |

四个基础模型排在前面，其余按标签与 ID 排序；Max 账号可见所有套餐级别。
配置排除的模型既不发布也不执行。未知模型返回 `unknown_model`（400）。
单条用户文本直接发送，多轮消息转为 `System: ...\n\nUser: ...\n\nAssistant: ...`。
按 Unicode 字符限制提示词，不切断 UTF-8 字符。

非流式结果含 `perplexity.thread_url` 与 `sources`。
开启来源时，两种模式均追加 `\n\n---\n**Sources**\n- [title](url)`。
插件通过 `host.stream.emit` 发送裸 JSON，由 CPA 的 OpenAI 转换器与处理器补上 SSE
前缀及 `[DONE]`。空答案、协议垃圾或上游生成失败不会伪装成成功，流中错误通过
`error_detail` 关闭并保留状态。

401/403 或 LOGIN 引导映射为 `account_unavailable`（401，提示导入路径）；
429 映射 `upstream_rate_limited` 并传递 Retry-After 秒数；协议错误 502，传输超时 504，
客户端取消 `client_closed`（499），不应惩罚账号。
刷新时网络故障返回 503，仅失效会话返回 `reauth_required`（401）。
token 计数无需联网，在 SDK executor 响应 Payload 中返回 `{input_tokens:N}`，
RPC 结果顶层也提供该字段。

## 配额

会话探测提供 plan；`/rest/rate-limit/all` 和 `/rest/user/settings` 提供 Pro、Research、
Labs、agentic research、上传和页面原始计数。只有验证会话有效后才查询付费 credits，
查询失败可忽略。Pro/Research 仅在配置分母大于 0 时生成桶，比例为剩余数/配置限额。
默认不编造月额度；手工配置的分母不代表上游官方承诺。

## 构建与验收

```sh
export PATH="$HOME/.local/go/bin:$PATH"
bash build.sh
go test -race -v ./...
python3 acceptance/abi_smoke.py dist/1.0.0-linux-amd64/perplexity.so
```

构建先运行离线 race 测试，再执行 `CGO_ENABLED=1 go build -trimpath -buildmode=c-shared`，
删除生成的 `.h` 并打包。ZIP 根目录严格为动态库、manifest、两份 README 与 LICENSE。
可用 `CPA_CORE=/path/to/CLIProxyAPI bash build.sh` 临时生成 `.build/go.work`，不修改模块依赖。
Makefile 提供 `build`、`test`、`package`、`clean`。

`v*` 标签工作流构建 Linux amd64/arm64（arm64 使用 `aarch64-linux-gnu-gcc`）、
macOS amd64/arm64，发布 `perplexity_<version>_<goos>_<goarch>.zip` 和 `checksums.txt`。
跨平台构建的测试始终在构建机原生架构上运行。标签版本必须与 manifest 一致。
本地执行记录、缺失的参考资料及待真实账号验证的项目见 `acceptance/REPORT.md`。

MIT 许可证。除 Go 标准库、CPA SDK 与 yaml.v3 外不引入依赖；不复制参考项目的托管目标或凭据。

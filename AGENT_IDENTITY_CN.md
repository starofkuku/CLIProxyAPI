# Codex Agent Identity 接口说明

> 分支：`feat/agent-identity`  
> 镜像：`dx95/cliproxy:v7.2.71.agentIdentity`  
> 说明：将 OpenAI/Codex **Agent Identity** 作为一种 Codex 账号导入并使用。

## 1. 概述

Agent Identity 不是传统 OAuth `access_token` / `refresh_token` 模式。

| 项目 | 传统 Codex OAuth | Agent Identity |
|---|---|---|
| 存储内容 | access/refresh token | Ed25519 私钥 + runtime 身份 |
| 上游鉴权 | `Authorization: Bearer ...` | `Authorization: AgentAssertion ...` |
| Token 刷新 | 需要 | 不需要 |
| 账号类型 | `type=codex` | 仍为 `type=codex`，额外 `auth_mode=agentIdentity` |

导入后，账号仍按 **Codex 账号** 参与调度；仅鉴权方式不同。

---

## 2. 导入接口（路径与原认证文件一致）

### 2.1 单文件 / 粘贴 JSON 上传

```http
POST /v0/management/auth-files?name=<filename>.json
Content-Type: application/json
Authorization: Bearer <management-key>
```

或 multipart 上传：

```http
POST /v0/management/auth-files
Content-Type: multipart/form-data
Authorization: Bearer <management-key>
```

### 2.2 压缩包导入

```http
POST /v0/management/auth-files/archive
Content-Type: multipart/form-data
Authorization: Bearer <management-key>
```

压缩包内每个 JSON 若识别为 Agent Identity，会自动规范化为 CPA Codex 文件后保存。

### 2.3 行为说明

1. 后端识别 Agent Identity 载荷  
2. 校验必填字段与私钥格式  
3. 规范化并写入本地 auth 文件（`type=codex`）  
4. 注册到运行时 Auth Manager  

**不需要新接口**；与现有认证文件导入路径完全一致。

---

## 3. 请求体格式

### 3.1 推荐格式（嵌套）

```json
{
  "auth_mode": "agentIdentity",
  "agent_identity": {
    "agent_runtime_id": "<runtime-id>",
    "agent_private_key": "<base64-pkcs8-ed25519-private-key>",
    "account_id": "<chatgpt-account-id>",
    "chatgpt_user_id": "<chatgpt-user-id>",
    "task_id": "<optional-task-id>",
    "email": "optional@example.com",
    "plan_type": "optional-plan"
  }
}
```

### 3.2 扁平格式

```json
{
  "authMode": "agentIdentity",
  "agentRuntimeId": "<runtime-id>",
  "agentPrivateKey": "<base64-pkcs8-ed25519-private-key>",
  "accountId": "<chatgpt-account-id>",
  "chatgptUserId": "<chatgpt-user-id>",
  "taskId": "<optional-task-id>"
}
```

字段同时支持 **snake_case** 与 **camelCase**。

### 3.3 字段说明

| 字段 | 必填 | 说明 |
|---|---|---|
| `auth_mode` / `authMode` | 建议 | 固定 `agentIdentity` |
| `agent_runtime_id` | 是 | Agent Runtime ID |
| `agent_private_key` | 是 | Base64 编码的 PKCS#8 **Ed25519** 私钥 |
| `account_id` | 是 | ChatGPT Account ID（也会用于 `Chatgpt-Account-Id` 头） |
| `chatgpt_user_id` | 是 | ChatGPT User ID |
| `task_id` | 否 | 已有 task；缺省时运行时自动注册 |
| `email` | 否 | 展示/元数据 |
| `plan_type` | 否 | 套餐元数据 |

可选运营字段（若传入会保留）：

- `disabled`
- `priority`
- `proxy_url`
- `prefix`
- `note`
- `websockets`

### 3.4 私钥要求

`agent_private_key` 必须满足：

1. Base64 编码  
2. PKCS#8 DER  
3. 算法为 Ed25519  

不接受 PEM 文本、公钥、或其他算法私钥。

---

## 4. 规范化后的落盘格式

导入后本地 auth 文件大致为：

```json
{
  "type": "codex",
  "auth_mode": "agentIdentity",
  "agent_runtime_id": "<runtime-id>",
  "agent_private_key": "<base64-pkcs8-ed25519-private-key>",
  "account_id": "<chatgpt-account-id>",
  "chatgpt_user_id": "<chatgpt-user-id>",
  "task_id": "<optional-or-registered>",
  "email": "optional@example.com",
  "plan_type": "optional-plan"
}
```

说明：

- **不会**写入 `access_token` / `refresh_token`
- 列表/调度时仍显示为 Codex 账号
- 识别条件：`type=codex` 且 `auth_mode=agentIdentity`

---

## 5. 运行时鉴权流程

### 5.1 检测

```text
Metadata.auth_mode == agentIdentity
```

### 5.2 Task 注册（缺 task_id 时）

```http
POST https://auth.openai.com/api/accounts/v1/agent/{agent_runtime_id}/task/register
Content-Type: application/json
Accept: application/json

{
  "timestamp": "<RFC3339 UTC>",
  "signature": "<Base64 Ed25519 signature of agent_runtime_id:timestamp>"
}
```

成功后把 `task_id` 写回该账号 auth 文件与内存 Metadata。

### 5.3 每次上游请求

```http
Authorization: AgentAssertion <base64url-json>
Chatgpt-Account-Id: <account_id>
```

`AgentAssertion` 解码后为：

```json
{
  "agent_runtime_id": "<runtime-id>",
  "task_id": "<task-id>",
  "timestamp": "<RFC3339 UTC>",
  "signature": "<Base64 Ed25519 signature of agent_runtime_id:task_id:timestamp>"
}
```

### 5.4 与普通 Codex 的差异

| 行为 | 普通 OAuth Codex | Agent Identity |
|---|---|---|
| `Authorization` | Bearer access_token | AgentAssertion |
| Token 自动刷新 | 有 | 跳过 |
| Task 注册 | 无 | 有 |
| Chatgpt-Account-Id | 有（若存在 account_id） | 有 |

HTTP 与 WebSocket Codex 路径均支持。

---

## 6. 错误与限制

| 场景 | 结果 |
|---|---|
| 缺少必填字段 | 导入失败，返回错误 |
| 私钥格式非法 | 导入失败 |
| Task 注册失败 | 请求失败，该次上游调用不可用 |
| 普通 Codex JSON | 不受影响，原逻辑继续 |

当前实现范围：

- 支持导入与上游签名鉴权
- 支持 task 自动注册并回写
- **未**单独做 invalid-task 自动重试循环（后续可增强）
- 前端管理面板如需专用录入 UI，需另行适配；当前可直接上传 JSON

---

## 7. 调用示例

### 7.1 curl 上传

```bash
curl -X POST \
  -H "Authorization: Bearer <management-key>" \
  -H "Content-Type: application/json" \
  --data-binary @agent-identity.json \
  "http://127.0.0.1:8317/v0/management/auth-files?name=codex-agent-1.json"
```

`agent-identity.json` 示例：

```json
{
  "auth_mode": "agentIdentity",
  "agent_identity": {
    "agent_runtime_id": "runtime_xxx",
    "agent_private_key": "MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg...",
    "account_id": "acct_xxx",
    "chatgpt_user_id": "user_xxx",
    "email": "demo@example.com"
  }
}
```

### 7.2 验证列表

```bash
curl -H "Authorization: Bearer <management-key>" \
  "http://127.0.0.1:8317/v0/management/auth-files"
```

应能看到对应 Codex 认证文件；文件内容中 `auth_mode` 为 `agentIdentity`。

---

## 8. 相关代码

| 文件 | 作用 |
|---|---|
| `internal/auth/codex/agent_identity.go` | 解析、规范化、签名、task 注册 |
| `internal/auth/codex/agent_identity_test.go` | 单测 |
| `internal/api/handlers/management/auth_files.go` | 导入路径规范化 |
| `internal/runtime/executor/codex_executor.go` | HTTP 鉴权 / 跳过 OAuth refresh |
| `internal/runtime/executor/codex_websockets_executor.go` | WebSocket 鉴权 |

---

## 9. 安全注意

1. `agent_private_key` 属于高敏感凭据，禁止提交到公开仓库或日志。  
2. 管理接口必须使用 management key，并限制公网暴露。  
3. 备份 auth 目录时按密钥材料同等保护。  
4. 文档与 issue 中只使用占位符，不要粘贴真实私钥。

---

## 10. 版本与镜像

```bash
# 拉取本功能镜像
docker pull dx95/cliproxy:v7.2.71.agentIdentity
```

GitHub 触发 tag：

```text
v7.2.71.agentIdentity
```

对应分支：

```text
feat/agent-identity
```

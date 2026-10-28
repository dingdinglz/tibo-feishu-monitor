---
id: tibo-config
type: module-design
status: draft
title: 配置解析与启用条件
parent: tibo-architecture
---

# 配置解析与启用条件

## 职责

把单个 YAML 文档转成已验证配置，提供默认值、来源归一化、路径解析及凭据展开。先解码 YAML 再展开环境变量，避免变量内容注入 YAML 结构；拒绝未知字段。相对文件路径以配置文件目录为基准，裸解释器名称保留 PATH 查找语义。

只解析当前来源需要的 X/TypeSafe 配置；探测和状态查询不要求 Webhook 或推理凭据，禁用目标不要求其凭据。启用的 Webhook 必须是允许的官方 HTTPS 地址，目标 ID 唯一，相同启用地址不能重复配置。

## 边界

配置加载不探测网络、不打开业务库、不导入账号、不加载 `.env`，也不热重载。文件存在性和进程锁属于启动检查，而不是 YAML 校验。稳定目标 ID 的运行语义见 `tibo-architecture`；秘密处理规则见 [SECURITY.md](../../SECURITY.md)。

**未确认的长期约束（由现有导入关系推断）：**配置包保持无项目内依赖，不调用采集、发送或编排代码。

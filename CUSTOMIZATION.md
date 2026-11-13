# Nightingale custom backend

基于 Nightingale v9.3.0 的二次开发后端。上游基线：`98653573c386949985acc9abe6e5957ce6017904`，https://github.com/ccfos/nightingale 。保留原许可证及版权声明。

配套前端：https://github.com/Steelki/nightingale-frontend-custom （初始配套提交 `00b7bc9`）。官方前端不包含本项目新增的管理页面。

## 已实现

- 独立值班组、团队成员快照导入、轮班规则及临时替班。
- 通知规则支持原有接收模式或单个值班组，按当前排班解析接收人。
- 告警认领、取消认领、手动转交及处理进展，认领后停止重复提醒。
- 认领群通知、转交/取消认领通知、恢复个人通知按认领人路由。
- 值班组/系统发送失败兜底，持久化任务及发送结果记录。
- 可配置分钟/小时的值班提醒，使用值班组全部公共IM渠道，动态解析当班人和电话。

电话最终回执、逐级升级、跨严重度关联及多平台语音等仍是待办。已有本地专项、隔离API和浏览器验收，不代表真实供应商送达联调完成。

## 构建

Go要求见 `go.mod`，当前为1.25.0。本地已验证macOS arm64构建：

```sh
go build -o build/n9e ./cmd/center
```

Go模块路径中的 `/v6` 是上游模块标识，不代表产品版本。Linux或其他架构需要针对目标环境重新构建和验证，不能直接运行macOS二进制。

前端在配套仓库中构建，已验证的本地环境是Node.js22.23.2：

```sh
npm ci
NODE_OPTIONS=--max-old-space-size=8192 ./node_modules/.bin/vite build
```

前端输出为 `pub/`；首次干净依赖安装未在此次上传中重新验收，直接Vite构建不执行完整TypeScript检查。原有发布脚本/Actions可能引用官方前端和官方发布地址，尚未适配本项目，不应直接用它们发布配套二次开发包。

## 部署目录与启动

运行目录至少包含下列内容：

```text
runtime/
  n9e
  etc/config.toml
  pub/
  integrations/
  data/
```

从本仓库复制 `integrations/`，从配套前端构建结果复制 `pub/`。以 `etc/` 为配置参考，按目标环境配置数据库、Redis、监听地址、站点URL和数据源；不要使用默认凭据暴露公网服务。设置 `Center.UseFileAssets=true` 使用磁盘前端资源。

已验收的本机实例使用 `127.0.0.1:8772`、SQLite `n9e.db`、内置miniredis及TSDB。这些本地运行数据不在本仓库中。生产数据库、高可用和Linux发布尚未验收。

在运行目录中临时前台启动：

```sh
./n9e --configs etc
```

需要日志时可重定向到本地日志文件。不要从其他工作目录启动，以免读取错误的数据库或创建另一份密钥。当前不配置托管、自启或守护进程。

## 更新、备份和回滚

1. 在暂存目录完成新程序和前端构建，按改动范围验收。
2. 核对并停止目标实例，备份旧程序、`pub/`、`etc/`、数据库、`data/`及`integrations/`。
3. SQLite使用backup API或停机一致性备份，不能在运行时仅复制主DB文件并遗漏WAL数据。
4. 替换程序和前端，保留配置、数据库、密钥及模板定制，重新启动并检查登录、值班组、通知规则、模板中心及静态资源。
5. 回滚前再次停机并保存故障现场，按同一备份恢复程序/静态及相匹配的数据。恢复旧数据库会丢失备份之后的操作，需先评估。

`data/duty-notification.key` 是值班机器人地址/签名密钥的加密密钥，必须与数据库配套备份恢复，不能重新生成后沿用旧数据库。漏部署 `integrations/` 会导致监控和仪表盘模板无法加载。

## 验证与隔离

通知worker专项使用隔离SQLite及模拟HTTP：

```sh
go test ./center/dutynotify ./center/claimnotify -count=1
```

还有 `alert/dispatch/duty_test.go`、`models/duty_rotation_test.go` 和 `alert/sender/provider/duty_email_test.go` 等相关测试。SMTP专项使用本机模拟SMTP服务器，不连接真实邮件供应商。

API/UI验证应使用独立端口和数据库，并移除或禁用真实通知目标。`N9E_DISABLE_DUTY_DELIVERY=1` 仅禁用新的dutynotify worker，**不是全系统禁发开关**，不会禁用原有告警/认领发送链路。

GitHub Actions保留上游定义；本次不推版本标签、不执行发布。新仓库首次push可能触发上游模板检查，但不代表本项目已配置自动测试/部署流水线。

## 数据库初始化与升级维护

见 [数据库初始化与升级](docker/migratesql/custom/README.md)。三个数据库基础初始化SQL已同步二次开发模型，另提供版本化增量SQL；`go run ./cmd/duty-schema --check` 检查SQL与模型的一致性。SQL导入后仍需运行同版本程序完成上游迁移和应用初始化。

# 二次开发数据库初始化与升级

本目录维护二次开发的增量SQL。完整安装使用上游基础初始化加本项目启动迁移，不能只导入这几个增量文件。

## 全新安装

推荐：配置 `etc/config.toml` 的数据库连接，在空数据库上启动对应版本的程序。启动顺序：

1. `pkg/ormx` 创建基础表及初始用户/权限等种子数据。
2. `models/migrate` 更新当前上游表结构，包括 `notify_rule`。
3. `models.InitDutySchema` 创建二次开发的8张表及索引，并初始化本实例的通知加密密钥。
4. 后续初始化默认账户/配置等应用数据。

如运维要求先手动导入SQL，三种数据库的基础文件为：

| 数据库 | 初始化文件 | 注意 |
| --- | --- | --- |
| MySQL | `docker/initsql/a-n9e.sql` | 文件包含创建并选择 `n9e_v6` 数据库，仅用于新库 |
| PostgreSQL | `docker/compose-postgres/initsql_for_postgres/a-n9e-for-Postgres.sql` | 先建目标数据库；Ibex配套使用同目录 `b-ibex-for-Postgres.sql` |
| SQLite | `docker/sqlite.sql` | 仅导入新的空SQLite文件 |

这些基础文件已包含本项目当前完整的 `notify_rule` 和8张 `response_*` 表及索引。上游仍有其他表/列通过启动迁移管理，所以导入后仍须启动同版本程序完成上游迁移及应用种子初始化，不能把“SQL导入成功”当作全部启动验收完成。不要向已有生产库重复执行基础初始化文件，也不要同时执行基础文件和001增量文件。

SQLite导入示例（`new-n9e.db` 必须尚不存在）：

```sh
test ! -e new-n9e.db && sqlite3 new-n9e.db < docker/sqlite.sql
```

随后配置程序使用该文件。基础脚本包含上游初始账户，部署时应立即修改初始密码，不将默认账户暴露公网。

## 从已经运行过的上游v9.3.0升级

先停机并备份数据库、程序、配置和加密密钥。首选启动新版本，让GORM按实际表/列存在情况执行增量迁移，避免手工重复加列。

需要预先手工建表时，选择对应方言的 `001-duty-*.sql`，**仅执行一次**。前提是上游数据库已经完整初始化、`notify_rule` 存在，并且还没有本项目新增字段和 `response_*` 表。脚本新增通知规则接收模式/值班组字段，以及8张二次开发表，不删除旧规则；已有规则默认继续使用 `original` 模式。

部分旧SQLite基础SQL没有 `notify_rule`，这种仅导入旧SQL但从未运行的数据库不满足001前提，应使用程序启动迁移。已有任一二次开发表或字段的数据库也不要直接执行001，应使用启动迁移。DDL是否支持事务因数据库而异，执行中途失败时需检查实际完成状态，不能盲目重跑。

## 现有二次开发版本升级

使用启动迁移，已覆盖旧响应表补 `assignment_version`、旧认领通知表补 `group_id`、创建发送任务及提醒时钟等。重复启动迁移不应清空已有数据。

提醒间隔、单位、成员、轮班、渠道配置保存在值班组 `config` JSON文本内，不是独立SQL列；它们由应用兼容旧默认值。发送目标和任务内容为加密数据，不能用初始化SQL复制其他环境密钥或目标。`data/duty-notification.key` 必须和数据库配套备份。

## 二次开发表

| 表 | 用途 |
| --- | --- |
| response_duty_group | 值班组及配置 |
| response_duty_audit | 值班组配置变更审计 |
| response_duty_settings | 系统失败兜底配置 |
| response_event | 告警认领及恢复状态 |
| response_event_log | 处理进展及操作日志 |
| response_claim_notice | 认领群通知持久任务 |
| response_duty_notice | 转交、取消认领、失败兜底、值班提醒任务 |
| response_duty_reminder_clock | 各组下次提醒时间 |

## 后续维护要求

运行迁移和SQL生成共用 `models.DutySchemaModels()`。修改表模型后在仓库根目录执行：

```sh
go run ./cmd/duty-schema
go run ./cmd/duty-schema --check
go test ./internal/dutyschema -count=1
```

生成器更新三个基础SQL的标记区块及当前 `notify_rule`，不连接生产数据库。已经存在的001版本增量脚本不会被重写；后续需要手工SQL升级的结构变化应增加002、003等新文件并补对应旧库升级测试。

验证已覆盖：SQLite完整基础SQL导入、模型字段/索引一致、上游规则数据升级保留、旧二次开发表补列及重复迁移。MySQL/PostgreSQL已按GORM方言生成并做一致性检查，**尚未在真实MySQL/PostgreSQL服务上执行验收**。不能据此宣称这两类生产环境已验证。

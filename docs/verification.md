# 第一阶段开发版验证记录

日期：2026-10-13。环境：Debian 13.5、amd64、systemd，root隔离测试安装。
核心：sing-box v1.14.3，commit 7054cac5124465ffdb145870d7c01885566711b1，Go1.27.2，开启with_v2ray_api。
不包含生产凭据、配置副本或数据库。

| 检查 | 结果与范围 |
| --- | --- |
| Go全包测试、go vet | 通过 |
| 应用管理员 | Argon2id固定成本、随机盐、恶意PHC拒绝、stdin输入、凭据0600、JSON/审计不泄露均通过；v1/v2到v3保留账本/绑定，改名及密码轮换不调用systemd |
| 管理员开发升级检查 | 原临时脚本把SQLite BLOB直接JSON序列化，在预检查失败后错误触发停服；用户已手动恢复三项服务及原无限账号。修正版改用配置摘要，预检查失败零停服，增加产物校验及完整入口备份；语法、BLOB和跨月旧账期检查、故障门禁模拟通过。实际v3升级仍待执行 |
| Bash分派、危险旧路径拦截、root安装防覆盖 | 通过；本机与服务器rootfixture |
| SOCKS / VMess / VLESS / Trojan / SS / TUIC / Hysteria2 / AnyTLS | 8类实际HTTP上传下载均通过；每次上行196840、下行327896字节 |
| 15个上游传输/REALITY变体 | 均通过后端实际转发、入站计量；HTTPUpgrade三项修正测试客户端Host字段后复测通过 |
| 7种SS方法 | 均通过实际转发与计量，包括3种2022方法 |
| UDP计量 | SOCKS、SS、TUIC、Hysteria2、VMess、VLESS、Trojan：各发送5个1500字节数据报，上传/下载各7500字节；均通过 |
| 多账号绑定 | 同一逻辑SOCKS节点生成不同凭据、端口；独立账号进程 |
| 配额保护 | 降低额度后该账号全部入口停止，另一账号进程保持运行；重置恢复；手动禁用不被重置解除 |
| 账期 / 水位 / 持久化 | SQLite测试通过；月初使用模拟时间验证，未修改服务器时钟 |
| 旧配置发现 | 只读识别真实旧Hysteria2配置，原服务持续运行 |
| 模拟旧服务迁移 | config.json内入站+conf节点均识别；import无影响；apply保留端口/凭据；未归属身份停用；零额度关闭全部旧入口 |
| 真实旧节点接管 | Hysteria2归属无限额度admin，保留UDP52613、旧凭据和证书；原base/conf文件逐字节未变；sb/备用入口及原脚本统一受管，旧unit masked；账号进程active且实际流量持续记账 |
| 真实旧节点连接与WARP | 原认证信息+显式信任原证书、正常名称校验，HTTP转发通过；临时www.cloudflare.com规则命中后Cloudflare trace返回warp=on；无限账号仍计量；临时规则恢复为空 |
| 恢复后旧节点健康复核 | 用户确认公网旧节点可正常访问；服务器不停服验证原Hysteria2认证、原证书及SNI，64KiB上传、256KiB下载及HTTPS均通过。账号上传增加79703、下载增加323519字节；前后三项服务active，admin允许访问、无限额度、无统计故障；临时客户端清理成功。该次HTTPS走普通出口（trace warp=off），不代表本轮重新验证WARP分流 |
| 单节点分享与日志 | 8类分享格式、传输/REALITY、SS2022双密钥、IPv6/字符编码、TLS默认校验均通过模块测试；CLI读入口和交互菜单通过Shell回归 |
| WARP故障路由 | HTTP Host sniff命中固定域名后，本地出口不可用时请求阻断；非命中流量仍直连 |
| 计量daemon死亡 | 强制kill后BindsTo停止账号进程；故障状态持久化；原生产unit持续active |
| 数据库查询故障 | SQLite可打开但账号表缺失、账本读失败、账期写失败均触发保护；故障标志落库失败时保留旧进程身份，防止自动恢复 |
| 统计API | 仅127.0.0.1；gRPC协议、延迟启动、deadline、异常计数测试通过 |
| Caddy | 片段、Host/path隔离、配置校验及恢复通过模块测试；公网TLS前端未验证 |

八类协议及所有矩阵传输测试验证的是账号专属进程的全部入站统计，不证明共享进程的原生按用户统计能力。
TCP/UDP统计为转发负载，不包含协议加密开销；轮询超额、崩溃尾量及跨月采样差额边界见phase1.md。
真实迁移按用户指定的admin无限额度执行，与模拟迁移分别记录。原配置及原脚本均有root私有备份。
真实WARP最小安装已验证：私有运行库及NSS校验文件加载、官方daemon active，未执行运行库包维护脚本。
经用户明确授权接受条款、注册并连接，`sb warp status` 通过本地SOCKS及正常HTTPS验证返回
`daemon_connected=true`、`proxy_reachable=true`、`warp=on`，本次出口地区JP；不保证地区固定。
代理仅监听127.0.0.1:40000，/etc/passwd及/etc/group摘要与操作前一致。
恢复后的健康复核使用固定版本`scripts/test-legacy-health.py`，只读取当前已安装sb和旧私有模板，
没有重启或修改受管服务、配额、WARP规则；结果保存在服务器
`/etc/sing-box-enhanced/legacy-health-verification/result.json`。本轮未执行管理员v3升级。

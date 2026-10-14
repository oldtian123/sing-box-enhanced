# 233Boy sing-box Enhanced：第一阶段开发版

原脚本入口新增本地 Go 分派；增强业务不依赖 Agent/API。部署使用 root，不创建 Linux 用户。
当前是可测试的开发版本。账号订阅、Web、独立 Agent、多服务器管理后置。

## 使用和构建

Go 1.25.5 或更高；测试 `go test ./...`、`bash scripts/test-shell.sh`。

```bash
CGO_ENABLED=0 go build -trimpath -o build/sbe ./cmd/sbe
GO_BIN=go CGO_ENABLED=0 bash scripts/build-core.sh /absolute/new-output-directory
bash install.sh --enhanced --core /absolute/new-output-directory/sing-box \
  --helper /absolute/build/sbe --entry /usr/local/bin/sb-enhanced
```

核心固定到上游 v1.14.3、commit `7054cac5124465ffdb145870d7c01885566711b1`，
保留上游默认构建标签，并增加 `with_v2ray_api`；关闭 CGO 时增加 `with_purego`。
Naive 出站仍依赖 `libcronet.so`，此构建不代表该附加出站已经可用。
安装器只使用本地二进制，不替换现有 `sb`、不迁移或重启现有 sing-box。

```bash
sb-enhanced account create --name alice --bytes 107374182400
sb-enhanced admin show
sb-enhanced node add --name hy2 --template /path/to/single-inbound-node.json
sb-enhanced node bind --node hy2 --account alice
sb-enhanced node endpoints --account alice
sb-enhanced node url --account alice --server YOUR_SERVER_ADDRESS
sb-enhanced account quota --id alice --bytes 53687091200
sb-enhanced account disable --id alice
sb-enhanced account enable --id alice
sb-enhanced account reset --id alice
sb-enhanced warp add --domain example.com
sb-enhanced warp list
sb-enhanced service status
```

新逻辑节点可关联多个账号。每个绑定生成独立凭据和监听端口；一个账号的全部节点在同一专属进程内。
账号独立进程是断流兜底，需要更多内存；本期不实现共用一个端口的多账号分派。
模板必须只包含根级 `inbounds/outbounds`，保留入站 TLS、传输及未知字段；节点私有 route/dns 等明确拒绝。
服务器级 `base.json` 的全局路由、DNS 等保留。匿名 Direct、无认证 SOCKS 不准入。
Caddy 模板的新绑定需要已有匹配域名站点；追加受管路径并验证配置。未配置的前端会明确拒绝。
动态 ACME 模板不直接复制；须先使用共享证书文件，避免每账号重复申请证书。
当前创建节点使用 JSON 模板，原菜单的所有创建/修改便利功能尚未移植。

增强菜单提供账号创建、启停、配额/重置、模板导入和绑定、WARP和服务操作。
无参数 `sb` 使用分层中文菜单，数字选择对象，结果按键后返回原页面；详细结构见
[交互式菜单说明](interactive-menu.md)。带参数命令保留JSON。交互额度使用十进制GB（1GB=10^9字节），
保留既有字节额度及北京时间自然月账期。
原 `info/i`、`url/qr`、`log` 映射受管逻辑。`node url/qr` 需要明确服务器地址；
Caddy需要公网端口。默认保留客户端TLS校验，自签场景只有显式 `--insecure` 才放宽。
二维码使用已经安装的 qrencode；没有该工具时可用 URL，不自动安装。

`node endpoints` 含实际认证信息，仅供本机 root 查看；没有订阅 HTTP 链接。
SQLite、凭据、配置、备份均在私有数据目录。管理操作与守护进程共用文件锁。
接管后的增强入口拦截原脚本写入/裸启动/更新路径，避免直接覆盖受管配置。

## 应用管理员

安装默认创建名为 `admin` 的无限额度应用账号和管理员身份。该身份用于后续面板登录，
本期没有登录 API；本机管理仍由 root 执行。应用密码与协议凭据、Linux root 密码完全独立。
默认生成随机密码，数据库仅存 Argon2id 哈希；初始名字和密码写入数据目录的
`admin.initial-credentials.json`，权限0600。命令和审计不输出密码/哈希。
取走初始凭据后可删除该文件；自定义密码不会写入这个明文文件。

```bash
sb admin show
sb admin rename --name YOUR_ADMIN_NAME
sb admin passwd                    # 生成新随机密码，输出私有凭据文件路径
sb admin passwd --password-stdin   # 从标准输入读取一行密码，也可使用隐藏输入菜单
```

自定义密码为12到1024字节，不允许换行；不要把密码写到命令参数或Shell历史里。
安装支持 `--admin-name`、`--admin-password-stdin`。已有实例升级后需一次性执行
`sb admin setup [--name admin]`；若存在同名账号，会保留其ID、账本、绑定和保护状态，
仅设置无限额度及管理员哈希。重复 setup 拒绝覆盖，改名和密码轮换不会重启核心。
SQLite v1/v2 自动迁移到v3；运行中的旧计量程序不能打开v3，所以替换旧程序前应备份数据库、
停止计量服务，升级完成再启动并核实计划停机造成的故障保护后恢复账号。

## 计量与配额

统计 API 只监听各账号的 `127.0.0.1` 端口。采集所有受管 inbound 的累计 uplink/downlink，
`reset=false`，checkpoint 与用量同一 SQLite 事务提交，不叠加 user/outbound 计数。
一个账号上传下载合计固定限额；额度为零时拒绝访问。`--unlimited` 明确表示只计量、不设上限，
与 `--bytes` 互斥。`account quota --id ... --unlimited` 切为无限，重新设置 `--bytes` 恢复固定额度。
无限账号仍遵守手动禁用和统计故障保护。北京时间每月 1 日零点进入新账期，无结转。
超额停止该账号的全部进程入口；新账期恢复仅因配额停用的账号。
手动禁用、统计故障不随重置恢复。统计故障需确认后执行 `account recover --id ...`。

每秒轮询，故障 RPC 最多等待两秒；有限超额和进程异常崩溃后的尾量损失仍可能发生。
月底跨边界的采样差额按采集时账期记账；不是逐字节时间戳计费。
不能把进程隔离称为零丢量、零超额保证。协议有流量时必须实际验证计数；空计数在无流量时属正常。
手动重置不清 checkpoint，避免下一次重复记账。配置失败会保留前一文件，配额账本不随配置回滚。

计量 daemon 死亡时，systemd `BindsTo` 停止账号进程；ExecStopPost 记录故障保护。
直接重启计量 daemon 后需确认账号再 recover。使用 `sb service` 管理受管核心，避免裸启动。
`Restart=no` 防止核心自重启清零后继续免费使用。检测 boot/PID/start ticks 变化时保护停用。

## 开发迁移

新安装无迁移菜单。仅开发者模式且能识别旧安装时使用：

```bash
SBE_DEVELOPER=1 sb-enhanced migrate inspect --legacy-root /etc/sing-box
SBE_DEVELOPER=1 sb-enhanced migrate import --legacy-root /etc/sing-box --owner alice
SBE_DEVELOPER=1 sb-enhanced migrate apply
# 全部旧身份归一个账号时，可一条命令导入并接管：
SBE_DEVELOPER=1 sb-enhanced migrate run --legacy-root /etc/sing-box --owner alice \
  --takeover-entry /usr/local/bin/sb
```

inspect 只输出节点名称、认证状态，不显示凭据。import 复制私有配置备份、保留原凭据/端口/证书引用，
不修改旧服务。省略 owner 时全部先不绑定；逐一 `node bind` 指定归属。
apply 先真实核心校验，再停用并 mask 旧 unit，启动受管账号；未归属节点不启动。
证书资源仍引用原路径，开发迁移后不要删除原证书目录。

失败切换仅在不存在未归属或受限身份时允许恢复原服务；否则保持停用，避免绕过额度。
成功接管后 `migrate rollback` 不会恢复无计量的旧服务；会保护停用并保留账本、原配置备份。
默认保留旧 `sb` 入口，测试入口使用 `sb-enhanced`。指定 `--takeover-entry` 才会在成功切换后
备份并替换已识别的旧入口及其原脚本目标，使原 `sb` 别名、第二符号链接和直接脚本调用
统一进入增强管理，拒绝覆盖其他命令。原脚本内容保存在私有 `legacy-script.original` 备份。
升级包分发尚未实现。

## WARP

固定域名规则覆盖所有受管账号，包括迁移身份。先 sniff 受管入站，再精确域名匹配，再执行原路由。
仅规则命中的流量送本机 SOCKS 出口，出口失败不自动回退直连。配额在入口统计，WARP不重复计费。
纯 IP、ECH 等不可观察域名的流量无法保证按域名识别；不承诺通用 UDP 经官方代理可用。

增加或删除规则会重启配置变化的账号实例，现有连接可能短暂断开。规则变更由独立
systemd 作业执行，SSH 断开后仍继续；完成结果保存在私有 `jobs/warp-*/result.json`，
命令等待完成后返回错误或成功。所有账号的候选配置先通过实际核心检查，才保存规则并切换。
预检失败保留原规则和运行实例；切换失败尝试恢复旧规则和仍健康的账号，保留统计故障保护，
不自动清除故障、重置流量或放宽配额。

`warp status/connect` 先检查官方客户端处于指定端口的 WarpProxy 模式，拒绝全局 tunnel 模式。
状态探测显式走本地 SOCKS，正常验证 HTTPS，并显示实际出口 IP/地区；地区不保证固定。
安装和外部状态探测在管理锁外执行，避免等待包管理器期间停止配额采集。

`sb warp install` 为 Debian 13 amd64/arm64 的可选最小安装：通过官方签名 APT 元数据下载，
仅安装官方 warp-cli/warp-svc 二进制；运行库提取至私有目录，独立 root 服务。
不执行桌面及运行库包的维护脚本，不激活其 sysusers 配置。配置仓库所需 GPG 的新依赖包若包含账户创建操作会拒绝。不自动接受条款或注册/连接。官方说明：
[软件仓库](https://pkg.cloudflareclient.com/)、[Linux 注册流程](https://developers.cloudflare.com/warp-client/get-started/linux/)。

阅读条款后，只有明确执行 `warp register --accept-terms` 才记录同意并注册、设置本地代理，
不自动连接。已经手工注册的安装可执行 `warp accept-terms` 记录同意；后续客户端命令才带
官方 `--accept-tos`。然后使用 `warp connect` 与 `warp status`。没有同意时不默认添加该旗标。

## 验证边界

Go 单元/集成测试：SQLite持久化、月账期/手动重置、禁用状态、计数回滚/溢出保护、文件权限、
节点凭据隔离、预留端口冲突、Caddy标记片段、配置失败恢复、gRPC协议、WARP全局模式拦截。
`scripts/test-live.py` 是真实服务器隔离实例的八类协议转发及配额停用测试；需要 root、systemd、curl、openssl。
`test-transports.py` 覆盖15个传输/REALITY变体及7种SS方法；`test-udp.py`验证支持UDP协议的计量。
`test-migration.py`仅操作临时旧服务；`test-protection.py`验证域名故障阻断及计量服务死亡保护。
所有业务测试账号均为应用记录，不是 Linux 用户。生产迁移、Caddy公网TLS链路、真实WARP出口须单独标明结果。
实际测试记录见 [verification.md](verification.md)，其中包含真实Hysteria2迁移与WARP分流。
`test-managed-legacy.py`在明确已迁移的身份上验证旧凭据和证书及临时WARP规则，必须从SSH会话外独立运行，
因为路由变更重启账号进程，可能中断依赖该节点的管理连接。它不修改账号配额/凭据，结束时恢复临时规则。

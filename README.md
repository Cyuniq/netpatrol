# 网巡 NetPatrol

网巡可以通过 Web 管理区域和设备，由后台定时采集 Ping、TCP 和近一小时 Ping 丢包率。设备列表支持按区域、状态、关键字筛选，状态记录支持按时间、区域和设备筛选。

## 构建与试跑

需要 Go 1.26 或更新版本。首次构建需要获取 `go.mod` 固定的依赖，生成的二进制可以在离线环境运行。

```sh
go test ./...
go build -o netpatrol ./cmd/netpatrol
export NETPATROL_ADMIN_PASSWORD='替换为实际密码'
./netpatrol -listen 127.0.0.1:8080 -db netpatrol.db
```

用 `admin` 和设置的密码访问 `http://127.0.0.1:8080`。通过内网反代访问时，可保持服务监听回环地址，由反代终止 TLS。HTMX 和 CSS 随二进制提供。

设置 `NETPATROL_NO_AUTH=1` 可启用免登录模式。免登录名单留空时，能访问入口的用户均可执行管理操作。

需要指定免登录的来源地址时，可同时填写 `NETPATROL_NO_AUTH_CIDRS`，值为访问者的出口 IP 或 CIDR，多个条目用逗号分隔。网巡按直连请求的来源 IP 匹配名单。启用名单时，其他来源用管理员密码登录，密码留空时其他来源会收到拒绝。本机直接访问回环地址仍可免登录。

可在「区域 → 登记网段」维护独立的网段列表，再给每个区域关联零个或多个网段并填写路径。从「设备」可手动添加或上传 CSV。区域留空时，按设备 IP 唯一匹配已关联的网段。导入默认仅新增，选择「新增并更新」后按设备编号更新。设备列表每 10 秒刷新当前筛选和页码，可按相同筛选导出资产 CSV 或状态 CSV。

CSV 模板和导出文件采用带 BOM 的 UTF-8。导入兼容 UTF-8、GBK／GB18030，以及 UTF-8 表头配 GBK 数据行的文件。表尾的空白列会自动忽略。

网段发现可以手动填写 IPv4 CIDR，也可从区域关联网段中选取。管理员和已指定的免登录来源可以选择任意 IPv4 网段。普通免登录访问可从本机或同网段直连后发现服务器直连的内网，升级时兼容原配置中已有的发现范围。一次任务可填写最多 16 个 TCP 端口，每个 IP 发一次 Ping，并逐端口展示结果。默认每次最多 1,024 个地址，可通过 `-discovery-limit` 调整。勾选候选后选择纳管区域及每台设备的日常探测端口。

设备、路径和区域列表均提供「修改」和「删除」。删除设备或路径时，确认页会说明将清理的探测数据。区域有设备时须先调整设备归属，删除区域后已登记网段和历史事件仍可查看。

「探测设置」可调整所有已启用设备的常规间隔、降频间隔、双失败降频门槛，以及 Ping 和 TCP 超时。保存后后台自动采用新配置，运行中的探测会先完成。新建的网段发现任务使用当前设置中的超时。

顶部「路径检测」可独立添加 IPv4／IPv6 目标 IP，查看从网巡服务发出的 ICMP 逐跳结果。最多 24 跳，每跳采样 3 次，单次最多等待 1 秒。结果显示回应地址、发送与回应次数、未回应率和延迟统计。中间节点可能对 ICMP 回应限速或过滤，请结合后续跳和目标的回应判断。

每个路径目标都可立即检测，也可开启后台持续检测。默认间隔为 300 秒，支持设置 60～86,400 秒。后台保存最近一次结果和最近 50 条可见路径变化。

设备和路径详情的「延迟趋势」支持快捷时间范围、自定义最近时长和固定起止时间。设备显示 Ping 与 TCP 握手延迟，路径可查看目标端、每一跳及各回应 IP 的延迟。折线显示平均延迟，阴影表示最小至最大值。可通过图下方滑块缩放，并查看悬浮提示中的回应、超时和本地异常次数。相对时间以服务器时钟为准，每分钟刷新。

设备列表的「Ping 探测」和「TCP 探测」两列分别显示最新结果和延迟缩略图。「趋势时长」可以选最近 15 分钟、30 分钟、1 小时、6 小时、24 小时、7 天或 30 天，默认 30 分钟。较长时段会合并采样，红色底线表示该时段有超时，点击可打开完整趋势。只要 TCP 连接成功，设备状态即为「正常」。

延迟历史从启用此功能后开始积累，最长查询 30 天。设备的分钟统计保留 24 小时，小时统计保留 30 天。设备 IP 相同时，换 TCP 端口前后的趋势按采样时间接续显示。详情图的悬浮提示注明每个点对应的端口，同一时段有多个端口时分别列出统计。更换 IP 时显示当前 IP 的历史。路径的手动和持续检测均保存逐轮统计，查询时按时段和检测间隔聚合。

单设备试跑可直接输出逐轮 JSON。

```sh
./netpatrol -ip 127.0.0.1 -port 554 -once
./netpatrol -ip ::1 -port 554
```

Linux 运行账号需要创建 ICMP 原始套接字的权限（`CAP_NET_RAW`）。权限不足时，Ping 结果显示 `local_error`，TCP 仍独立执行。IPv6 链路本地地址需带接口名，例如 `fe80::21%eth0`。

连续运行时，每 10 秒取一轮结果。双失败立即记为不可达，持续满 10 分钟后的下一轮改为 60 秒，任一项恢复或出现缺样后回到 10 秒。丢包率使用最近 60 个分钟桶的实发包数，零样本输出 `null`。

Web 服务将配置、状态和历史保存在 SQLite。重启后设备重新评估，一小时丢包统计延续使用仍在窗口内的样本。单设备命令行试跑的数据保存在进程内存。

## 银河麒麟 V10 x86_64 离线部署

将麒麟安装包复制到运行 systemd 的银河麒麟高级服务器操作系统 V10 x86_64。安装包包含网巡程序、SQLite 和网页资源，安装时使用系统自带的 Bash、systemd 和基础命令。可一并复制同名的 `.sha256` 文件进行校验。下面以 `v1.0.0` 为例。

```sh
sha256sum -c netpatrol-kylin-v10-x86_64-20261125-v1.0.0.run.sha256
sudo bash netpatrol-kylin-v10-x86_64-20261125-v1.0.0.run
```

首次安装默认免登录并监听 8080，可从内网浏览器打开 `http://服务器IP:8080/devices`。服务由 `netpatrol.service` 启动并设为开机自启。可在首次安装时传入 `--port 18080` 更换端口，或用 `--local-only` 绑定回环地址。首次安装时，如果 firewalld 正在运行，脚本会在默认区域开放服务端口。若网卡位于其他区域，可传入 `--firewall-zone 区域名`。由现场统一配置防火墙时可使用 `--no-firewall`。升级沿用现场防火墙规则，传入 `--firewall-zone` 时才为当次升级指定放行区域。

配置位于 `/etc/netpatrol/netpatrol.env`，数据位于 `/var/lib/netpatrol/netpatrol.db`。修改免登录来源后执行 `sudo systemctl restart netpatrol`。按相同方式重跑安装包可升级标准路径下的网巡，保留监听、配置和数据，升级备份保存在 `/var/backups/netpatrol/`。安装失败可用 `sudo journalctl -u netpatrol.service -n 50` 检查启动日志。SELinux、KYSEC 和上游网络策略由现场维护。

在构建机执行 `python3 deploy/package.py --kylin-offline --version v1.0.0` 可生成安装包和校验文件。命名格式为 `netpatrol-kylin-v10-x86_64-YYYYMMDD-v主版本.次版本.修订号.run`。日期默认取北京时间，可用 `--release-date 20261125` 指定。同一批次的麒麟和 Windows 安装包使用同一日期和版本号。

## Windows x64 离线使用

解压 Windows x64 安装包后，双击 `install.cmd` 并确认管理员授权。首次安装会注册开机自动启动的 `NetPatrol` 服务，默认在 `http://127.0.0.1:8080/devices` 提供免登录页面。重复运行安装脚本可升级，安装前会生成程序和 SQLite 快照备份，并保留现有配置、端口和历史。

在管理员 PowerShell 中，可在首次安装时通过以下命令指定端口并允许局域网访问。脚本会给应用添加一条仅允许本地子网访问该 TCP 端口的 Windows 防火墙规则。

```powershell
.\install.cmd -Port 18080 -Lan
```

服务程序位于 `%ProgramFiles%\NetPatrol`，配置在 `%ProgramData%\NetPatrol\netpatrol.env`，数据库和日志位于同目录下的 `data`。修改免登录来源后，以管理员权限运行 `Restart-Service NetPatrol`。Windows 使用系统 ICMP API 采集 IPv4／IPv6 Ping 与逐跳路径。

本机临时使用可直接双击 `run-local.cmd`，关闭窗口后结束运行。该模式使用当前用户的 `%LOCALAPPDATA%\NetPatrol`，访问入口为 `http://127.0.0.1:8080/devices`。

在构建机执行 `python deploy/package.py --windows-offline --version v1.0.0` 可生成 `netpatrol-windows-x64-YYYYMMDD-v1.0.0.zip` 及同名 SHA-256 校验文件。

## GitHub 自动发版

向 GitHub 推送形如 `v1.2.3` 的版本标签后，GitHub Actions 在 AMD64 和 ARM64 上运行 Go 测试，构建下列附件并校验 SHA-256。全部成功后自动创建 Release。安装包和二进制只作为 Release 附件分发。手动运行同一工作流会生成版本号为 `v0.0.0` 的测试产物，供 Actions 页面下载。

| 平台 | Release 附件 |
| --- | --- |
| Windows x64 | 离线安装 ZIP |
| 银河麒麟 V10 x86_64 | 自解压安装包 `.run` |
| Debian 13 AMD64、ARM64 | 每种架构一个原生 `.deb` |
| 通用 Linux AMD64、ARM64 | 每种架构一个可直接运行的静态二进制，以及含相同程序、部署示例和许可证的 `.tar.gz` |

所有附件都有同名 `.sha256`。直接下载的 Linux 二进制需先执行 `chmod +x 文件名`。本地仅需麒麟安装包时，使用上文的 `--kylin-offline` 命令即可。

## Debian 13 软件包安装

运行 `dpkg --print-architecture` 查看本机架构，下载对应的 `.deb`。在下载目录执行以下命令，将文件名替换为实际名称：

```sh
sudo apt install ./netpatrol-debian13-amd64-YYYYMMDD-v1.2.3.deb
sudoedit /etc/netpatrol/netpatrol.env
sudo systemctl enable --now netpatrol.service
```

首次启动前在配置文件中设置 `NETPATROL_ADMIN_PASSWORD`。服务默认监听 `127.0.0.1:8080`，数据库位于 `/var/lib/netpatrol/netpatrol.db`。同一软件包可用 `apt install ./新版本文件名.deb` 升级。

## 通用 Linux 归档部署

运行 `python3 deploy/package.py` 生成 Linux amd64 发布包。可用 `--arch arm64` 生成 arm64 版本。包内含二进制、systemd unit、环境配置示例和依赖许可证。

安装二进制到 `/usr/local/bin/netpatrol`，创建独立的 `netpatrol` 系统账号和同名组。以仅管理员可读的权限将 `netpatrol.env.example` 复制到 `/etc/netpatrol/netpatrol.env`，设置密码或启用免登录。将 `netpatrol.service` 安装到 `/etc/systemd/system/` 后执行 `systemctl daemon-reload` 和 `systemctl enable --now netpatrol`。这两个文件位于发布包根目录，源码中位于 `deploy/`。服务使用本地磁盘的 `/var/lib/netpatrol`，并通过 systemd 获得 ICMP 权限。

生成 SQLite 一致性备份时可保持 Web 服务运行。备份目标须是可写目录中的新文件。

```sh
./netpatrol -db /var/lib/netpatrol/netpatrol.db -backup-to /path/to/backup.db
```

启动恢复实例前，可用 SQLite 检查备份中的 `areas`、`devices`、`latest_results`、`ping_minutes` 和 `state_events`。恢复实例使用独立监听端口，启动后按正常流程重新评估设备状态。

## 容量验收

`/diagnostics.json` 沿用管理页面的访问设置。它提供已调度设备数、在途轮次、完成和缺样计数、待写数量、最老待写时长，以及调度延迟、实际探测间隔、写入耗时和设备页延迟的 P95。P95 使用每项最近最多 4,096 个样本，时间单位为毫秒。

源代码可运行 5,000 台设备的可重复模拟。它使用真实调度、SQLite、HTTP、导出和发现任务，模拟探测结果和超时等待。

```sh
NETPATROL_LOADTEST=full go test -count=1 -timeout=16m -run '^TestCapacity5000$' -v ./internal/web
```

目标虚机验收需要在可控测试网段运行真实 ICMP 和 TCP。按健康、全量超时超过十分钟、批量恢复的顺序运行，每阶段保持设备页、导出和允许范围内的发现任务。记录诊断指标和 Linux 服务进程的 CPU、RSS、文件描述符，以及数据库和 WAL 大小。以阶段性结果标明支持规模，并核对备份中的资产、分钟桶和历史。场景和预期节奏见 [设计文档](netpatrol-design.md)。

# 网巡 NetPatrol

网巡用于监测内网设备的在线状态。它从部署网巡的主机向设备发送 Ping，并尝试连接指定 TCP 端口。运维人员可以在网页上按区域查看设备状态、近一小时丢包率和历史延迟，也可以逐跳检测到目标的网络路径。网页资源随程序提供，监测数据保存在本机 SQLite 数据库中。

[安装与启动](#安装与启动) · [添加设备与查看监测结果](#添加设备与查看监测结果) · [访问控制与运维](#访问控制与运维) · [源码构建与测试](#源码构建与测试)

## 安装与启动

| 平台 | 选择的文件 | 默认访问方式 |
| --- | --- | --- |
| 银河麒麟 V10 x86_64 | `.run` 离线安装包 | 内网浏览器访问 `http://服务器IP:8080/devices`，首次安装默认免登录 |
| Windows x64 | `.zip` 离线安装包 | 本机访问 `http://127.0.0.1:8080/devices`，首次安装默认免登录 |
| Debian 13（AMD64、ARM64） | 对应架构的 `.deb` 软件包 | 设置管理员密码并启动服务后，本机访问 `http://127.0.0.1:8080/devices` |
| 通用 Linux（AMD64、ARM64） | 独立二进制或 `.tar.gz` 归档 | 命令行可直接运行，归档另含 systemd 服务文件、配置示例和依赖许可证 |

以下命令中的日期、版本和架构均为示例，请替换为下载的文件名。各平台的安装包和独立二进制均提供同名 `.sha256` 校验文件。

### 银河麒麟 V10

将安装包和校验文件复制到运行 systemd 的目标服务器，在同一目录执行：

```sh
sha256sum -c netpatrol-kylin-v10-x86_64-20261125-v1.0.0.run.sha256
sudo bash netpatrol-kylin-v10-x86_64-20261125-v1.0.0.run
```

安装脚本会启动 `netpatrol.service` 并设为开机自启。首次安装可用 `--port 18080` 更换端口，或用 `--local-only` 限制为本机访问。默认对外监听时，脚本会在运行中的 firewalld 默认区域放行服务端口。可用 `--firewall-zone 区域名` 指定区域，或用 `--no-firewall` 交由现场统一配置。SELinux、KYSEC 和上游网络策略由现场维护。

升级时运行新版安装包即可。标准路径下的监听配置和数据库会保留，升级备份存放在 `/var/backups/netpatrol/`。升级沿用已有防火墙规则，也可显式指定放行区域。

### Windows x64

- **临时试用**：解压后双击 `run-local.cmd`，通过本机浏览器访问设备页。数据写入 `%LOCALAPPDATA%\NetPatrol`，关闭窗口即可结束运行。
- **安装为服务**：双击 `install.cmd` 并确认管理员授权，安装程序会注册开机自启的 `NetPatrol` 服务。升级时再次运行安装脚本，脚本会备份程序和数据库快照，并保留配置、端口和历史。
- **允许局域网访问**：首次安装时，在解压目录的管理员 PowerShell 中运行以下命令。防火墙规则仅允许本地子网访问指定端口。

```powershell
.\install.cmd -Port 18080 -Lan
```

### Debian 13

运行 `dpkg --print-architecture` 查看架构，选择 `amd64` 或 `arm64` 软件包。先通过 APT 安装，再编辑配置文件，填写 `NETPATROL_ADMIN_PASSWORD` 后启动服务：

```sh
sudo apt install ./netpatrol-debian13-amd64-20261125-v1.0.0.deb
sudoedit /etc/netpatrol/netpatrol.env
sudo systemctl enable --now netpatrol.service
```

在本机设备页使用 `admin` 和设置的密码登录。升级时用同一条 `apt install` 命令指定新版本文件。

### 通用 Linux

**直接运行**时，先给下载的二进制添加执行权限，再指定监听地址和数据库路径。以下示例在当前目录创建数据库：

```sh
chmod +x netpatrol-linux-amd64-20261125-v1.0.0
export NETPATROL_ADMIN_PASSWORD='替换为实际密码'
./netpatrol-linux-amd64-20261125-v1.0.0 -listen 127.0.0.1:8080 -db netpatrol.db
```

**安装为 systemd 服务**时，解压 `.tar.gz`，然后按以下顺序配置：

1. 创建 `netpatrol` 系统账号及同名组，将程序安装到 `/usr/local/bin/netpatrol`。
2. 将归档中的 `netpatrol.env.example` 复制为 `/etc/netpatrol/netpatrol.env`，将文件属主和属组设为 `root:root`，权限设为 `0600`，并填写管理员密码。
3. 将 `netpatrol.service` 安装到 `/etc/systemd/system/`，执行 `sudo systemctl daemon-reload` 和 `sudo systemctl enable --now netpatrol`。

Linux 上的 Ping 和逐跳检测需要 `CAP_NET_RAW`，随包提供的 systemd 服务已配置此权限。直接运行时，若权限不足，Ping 会显示 `local_error`，TCP 仍会执行。Windows 使用系统 ICMP API。

## 添加设备与查看监测结果

### 登记区域和设备

先在「区域 → 登记网段」维护网段，再给区域关联所需网段并填写路径。一个区域可关联多个网段，也可留空。随后在「设备」中选择添加方式：

| 方式 | 操作 |
| --- | --- |
| 手动添加 | 填写设备 IP 和日常探测的 TCP 端口。区域留空时，系统按 IP 唯一匹配已关联的网段。 |
| CSV 导入 | 下载模板并填写设备。默认仅新增，选择「新增并更新」后按设备编号更新。模板和导出文件使用带 BOM 的 UTF-8，导入也支持 GBK、GB18030 以及 UTF-8 表头配 GBK 数据行。表尾空白列会自动忽略。 |
| 网段发现 | 输入 IPv4 CIDR 或选择区域关联的网段，最多指定 16 个 TCP 端口。系统对每个地址发一次 Ping，并逐端口展示结果。勾选候选设备后，选择纳管区域和各设备的日常探测端口。 |

网段发现默认每次最多 1,024 个地址，可用 `-discovery-limit` 调整。管理员和指定的免登录来源可手动选择任意 IPv4 网段。普通免登录用户从本机或同网段直连时，可发现服务器直连的内网。升级后也可使用原配置中已有的发现范围。

设备、路径和区域均可修改或删除。删除设备或路径前，确认页会列出将清理的数据。删除区域前须先调整其中设备的归属，已登记的网段和历史事件会保留。

### 判断设备状态

「设备」页每 10 秒刷新并保留筛选条件和页码。可按区域、状态和关键字筛选设备，按当前筛选条件导出资产或状态 CSV。要追溯某次异常，可在状态记录中按时间、区域和设备查询。

- **在线状态**：只要 TCP 连接成功，设备即显示「正常」。Ping 和 TCP 均有效失败时，当轮即显示不可达。
- **探测间隔**：默认每 10 秒探测一次。双失败持续满 10 分钟后，下一轮间隔延长到 60 秒。任一项恢复或出现缺样后回到 10 秒。「探测设置」可调整全局间隔、降频门槛和超时。已开始的探测按原设置完成，新建发现任务使用当前超时。
- **一小时丢包率**：按最近 60 个分钟桶的实际发包数计算。窗口内没有实发样本时显示无数据，JSON 中为 `null`。服务重启后重新评估设备状态，窗口内已有样本继续参与计算。

### 查看延迟与路径

设备列表中的「Ping 探测」和「TCP 探测」同时显示最新结果和趋势缩略图，默认展示最近 30 分钟，可在 15 分钟至 30 天之间选择。较长时段会合并采样，红色底线表示该时段出现过超时。

点击缩略图可查看平均延迟折线及最小、最大值的阴影范围。详情页支持快捷时段、自定义最近时长和固定起止时间。拖动滑块可缩放，悬浮可查看回应、超时和本地异常次数。相对时间以服务器时钟为准，每分钟刷新。设备分钟统计保留 24 小时，小时统计保留 30 天。同一 IP 更换 TCP 端口后，历史按时间接续显示，提示中标明采样端口。更换 IP 后展示当前 IP 的历史。

在「路径检测」中添加 IPv4 或 IPv6 目标，可立即检测，也可开启持续检测。默认每 300 秒一次，可调范围为 60～86,400 秒。每轮最多 24 跳，每跳采样 3 次，单次最多等待 1 秒。详情页可查看各跳及其回应 IP 的延迟、未回应率，以及最近 50 条可见路径变化。手动和持续检测都会保存逐轮统计。中间节点可能限制 ICMP 回应，判断异常时请结合后续跳和目标端的结果。

## 访问控制与运维

### 登录与免登录来源

| 配置项 | 用途 |
| --- | --- |
| `NETPATROL_ADMIN_PASSWORD` | 设置 `admin` 的密码。通过内网反向代理访问时，可让网巡监听回环地址，由代理提供 HTTPS。 |
| `NETPATROL_NO_AUTH` | 设为 `1` 启用免登录。若来源名单留空，所有能访问入口的用户均可执行管理操作。 |
| `NETPATROL_NO_AUTH_CIDRS` | 指定免登录来源，支持以逗号分隔的 IP 或 CIDR。启用免登录后按直连请求来源匹配。名单外的用户需用管理员密码登录，未设密码时会被拒绝。本机直接访问回环地址仍可免登录。 |

### 配置位置与日志

| 安装方式 | 配置文件 | 数据与诊断 |
| --- | --- | --- |
| 麒麟或 Debian 服务 | `/etc/netpatrol/netpatrol.env` | 数据库为 `/var/lib/netpatrol/netpatrol.db`。用 `sudo journalctl -u netpatrol.service -n 50` 查看最近日志。 |
| Windows 服务 | `%ProgramData%\NetPatrol\netpatrol.env` | 程序位于 `%ProgramFiles%\NetPatrol`，数据库和日志位于 `%ProgramData%\NetPatrol\data`。 |

修改服务配置后，Linux 执行 `sudo systemctl restart netpatrol`，Windows 在管理员 PowerShell 中执行 `Restart-Service NetPatrol`。

### 备份与恢复检查

**在另一台电脑上用浏览器查历史**，请在「设备」页点击 **导出完整离线浏览包**。ZIP 包包含导出时数据库里保留的全部区域、设备、状态记录、路径和延迟采样，设备列表上的筛选条件仅影响 CSV 导出。

1. 把 ZIP 复制到目标电脑，完整解压，双击 `index.html`。
2. 在页面中选择同目录的 `netpatrol.db`。查询和绘图都在本机浏览器内完成，可断网使用，无需安装网巡、SQLite 或启动服务。
3. 在「设备」或「路径」中筛选目标并点击「查看」，可切换时段、缩放趋势图，或按跳点及回应 IP 查看路径延迟。相对时间以该包的快照时间为基准。「状态记录」支持按区域、状态、关键字和日期筛选。
4. 「数据表与 SQL」可浏览快照中的所有业务表，或执行自定义 `SELECT` 查询，结果按页显示。数据库单独保存在包内，也可交给 SQLite 工具处理。浏览器会把数据库载入内存，大文件可优先在内存充足的 64 位浏览器中打开。

**备份服务数据库**可使用以下命令。

Web 服务运行期间可生成 SQLite 一致性备份。以有权读取数据库的账号执行，备份目标应为可写目录中的新文件：

```sh
netpatrol -db /var/lib/netpatrol/netpatrol.db -backup-to /path/to/backup.db
```

可先用 SQLite 检查备份中的 `areas`、`devices`、`latest_results`、`ping_minutes` 和 `state_events`，再用独立监听端口启动恢复实例，核对设备与历史记录。

## 源码构建与测试

### 编译与单设备试跑

需要 Go 1.26 或更新版本。首次构建会获取 `go.mod` 固定的依赖，生成的程序可离线运行。

```sh
go test ./...
go build -o netpatrol ./cmd/netpatrol
```

启动 Web 服务时可使用上文通用 Linux 的监听和数据库参数。命令行单设备试跑会逐轮输出 JSON，数据保存在进程内存中。第一条命令只探测一轮，第二条持续探测。IPv6 链路本地地址需带接口名，例如 `fe80::21%eth0`。

```sh
./netpatrol -ip 127.0.0.1 -port 554 -once
./netpatrol -ip ::1 -port 554
```

### 构建离线安装包

按目标平台选择一条命令。麒麟和 Windows 安装包的文件名包含北京时间日期和版本号，并附带同名 `.sha256` 文件。可用 `--release-date 20261125` 指定日期。

```sh
# 银河麒麟 V10 x86_64
python3 deploy/package.py --kylin-offline --version v1.0.0

# Windows x64
python deploy/package.py --windows-offline --version v1.0.0

# 通用 Linux 归档，--arch 可选 amd64 或 arm64
python3 deploy/package.py --arch amd64
```

### 容量验证

`/diagnostics.json` 沿用管理页面的访问设置。可查看已调度设备数、在途轮次、完成与缺样计数、待写数量及最老待写时长。调度延迟、实际探测间隔、写入耗时和设备页响应时间的 P95 取自各自最近最多 4,096 个样本，单位为毫秒。

以下命令运行 5,000 台设备的模拟测试，覆盖调度、SQLite、HTTP、导出和发现任务。探测结果和超时等待由测试模拟。

```sh
NETPATROL_LOADTEST=full go test -count=1 -timeout=16m -run '^TestCapacity5000$' -v ./internal/web
```

在目标虚机验证实际容量时，可在可控网段依次测试健康、全量超时超过 10 分钟和批量恢复三个阶段。各阶段保持设备页查询、导出和网段发现，记录诊断指标、进程 CPU、RSS、文件描述符及数据库和 WAL 大小，并核对备份中的资产、分钟桶和历史。[设计文档与验收场景](netpatrol-design.md)

#!/bin/bash
set -euo pipefail
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
umask 027

die() { printf '%s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
用法  sudo bash <安装包文件名>.run [选项]

  --port PORT           首次安装的 HTTP 端口，默认 8080
  --local-only          首次安装时仅监听 127.0.0.1
  --no-firewall         跳过 firewalld 端口配置
  --firewall-zone ZONE  指定 firewalld 区域，默认使用当前默认区域
  --extract DIR         解出程序、文档和许可证到新目录
  --help                查看用法

首次安装默认对外监听、免登录、开机自启。升级保留现有监听、配置和数据库。
EOF
}

port=
local_only=0
skip_firewall=0
zone=
while (( $# )); do
    case "$1" in
        --port) [[ $# -ge 2 ]] || die '请提供 HTTP 端口。'; port=$2; shift ;;
        --firewall-zone) [[ $# -ge 2 ]] || die '请提供 firewalld 区域。'; zone=$2; shift ;;
        --local-only) local_only=1 ;;
        --no-firewall) skip_firewall=1 ;;
        --help|-h) usage; exit 0 ;;
        *) die "未知选项 $1，可用 --help 查看用法。" ;;
    esac
    shift
done

valid_port() {
    [[ $1 =~ ^[1-9][0-9]{0,4}$ ]] && (( $1 <= 65535 ))
}

[[ -z $port ]] || valid_port "$port" || die '端口须为 1～65535 的整数。'
[[ -z $zone || $zone =~ ^[a-zA-Z0-9_-]+$ ]] || die 'firewalld 区域名称无效。'
(( EUID == 0 )) || die '请以 root 身份运行安装包。'
[[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || die '此安装包需要 Linux x86_64。'
for tool in systemctl install getent groupadd useradd nologin runuser flock timeout awk sed; do
    command -v "$tool" >/dev/null || die "缺少系统命令 $tool。"
done
systemctl show -p Version >/dev/null || die '请在运行 systemd 的服务器上安装。'

exec 9>/run/netpatrol-install.lock
flock -n 9 || die '另一个网巡安装正在进行。'
source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
program=/usr/local/bin/netpatrol
unit=/etc/systemd/system/netpatrol.service
config=/etc/netpatrol/netpatrol.env
state=/var/lib/netpatrol
database=$state/netpatrol.db
service=netpatrol.service

fragment=$(systemctl show "$service" -p FragmentPath --value || true)
existing=0
active=0
if [[ -n $fragment ]]; then
    existing=1
    command_line=$(systemctl show "$service" -p ExecStart --value)
    [[ $command_line == *"path=$program ;"* && $command_line == *" -db $database "* ]] ||
        die '现有网巡服务使用自定义程序或数据库路径，请先核对部署路径。'
    [[ $command_line =~ [[:space:]]-listen[[:space:]]+([^[:space:]]+) ]] ||
        die '无法识别现有网巡监听参数，请先核对 systemd 配置。'
    listen=${BASH_REMATCH[1]}
    listen_port=${listen##*:}
    valid_port "$listen_port" || die '现有网巡端口无效。'
    [[ -z $port || $port == "$listen_port" ]] || die '升级保留现有端口。请通过 systemd 配置修改监听。'
    (( !local_only )) || [[ $listen == 127.0.0.1:* ]] || die '请通过 systemd 配置修改现有服务的监听地址。'
    if systemctl is-active --quiet "$service"; then active=1; fi
else
    listen_port=${port:-8080}
    listen=:$listen_port
    if (( local_only )); then listen=127.0.0.1:$listen_port; fi
fi

backup=
staged=
db_stage=
changed=0
new_unit=0
validated=0
finish() {
    local status=$?
    trap - EXIT
    if (( status != 0 && changed && !validated )); then
        set +e
        systemctl stop "$service"
        if (( new_unit )); then
            systemctl disable "$service" >/dev/null 2>&1
            rm -f -- "$unit"
        fi
        if [[ -n $backup && -f $backup/netpatrol ]]; then
            install -m 0755 "$backup/netpatrol" "$program"
            if command -v restorecon >/dev/null; then restorecon "$program"; fi
            printf '已恢复上一版程序，备份在 %s\n' "$backup" >&2
        else
            rm -f -- "$program"
        fi
        systemctl daemon-reload
        if (( active )); then systemctl start "$service"; fi
    fi
    if [[ -n $staged ]]; then rm -f -- "$staged"; fi
    if [[ -n $db_stage ]]; then
        rm -f -- "$db_stage/netpatrol.db"
        rmdir -- "$db_stage"
    fi
    exit "$status"
}
trap finish EXIT

if ! getent group netpatrol >/dev/null; then groupadd --system netpatrol; fi
if ! getent passwd netpatrol >/dev/null; then
    nologin=$(command -v nologin)
    useradd --system --gid netpatrol --home-dir "$state" --no-create-home --shell "$nologin" netpatrol
fi
[[ -d $state ]] || install -d -m 0750 -o netpatrol -g netpatrol "$state"
[[ -d /etc/netpatrol ]] || install -d -m 0750 /etc/netpatrol
[[ -d /usr/local/bin ]] || install -d -m 0755 /usr/local/bin

if [[ -e $program ]]; then
    install -d -m 0700 /var/backups/netpatrol
    backup=$(mktemp -d /var/backups/netpatrol/upgrade.XXXXXXXX)
    install -m 0700 "$program" "$backup/netpatrol"
    if [[ -e $database ]]; then
        db_stage=$(mktemp -d "$state/.snapshot.XXXXXXXX")
        chown netpatrol:netpatrol "$db_stage"
        runuser -u netpatrol -- "$program" -db "$database" -backup-to "$db_stage/netpatrol.db"
        mv -T -- "$db_stage/netpatrol.db" "$backup/netpatrol.db"
        chown root:root "$backup/netpatrol.db"
        rmdir -- "$db_stage"
        db_stage=
    fi
fi

if [[ ! -e $config ]]; then
    sed 's/^NETPATROL_NO_AUTH=0$/NETPATROL_NO_AUTH=1/' "$source_dir/netpatrol.env.example" >"$source_dir/netpatrol.env"
    install -m 0600 "$source_dir/netpatrol.env" "$config"
fi
staged=$(mktemp /usr/local/bin/.netpatrol.XXXXXXXX)
install -m 0755 "$source_dir/netpatrol" "$staged"
mv -fT -- "$staged" "$program"
staged=
changed=1
if command -v restorecon >/dev/null; then restorecon "$program" "$config" "$state"; fi
if (( !existing )); then
    new_unit=1
    unit_options=(-e "s/-listen 127\.0\.0\.1:8080/-listen $listen/")
    if (( listen_port < 1024 )); then
        unit_options+=(-e '/^AmbientCapabilities=/s/$/ CAP_NET_BIND_SERVICE/' -e '/^CapabilityBoundingSet=/s/$/ CAP_NET_BIND_SERVICE/')
    fi
    sed "${unit_options[@]}" "$source_dir/netpatrol.service" >"$source_dir/service"
    install -m 0644 "$source_dir/service" "$unit"
    if command -v restorecon >/dev/null; then restorecon "$unit"; fi
fi

host=${listen%:*}
case "$host" in
    ''|0.0.0.0) health_host=127.0.0.1 ;;
    '[::]') health_host=::1 ;;
    *) health_host=${host#\[}; health_host=${health_host%\]} ;;
esac

systemctl daemon-reload
systemctl restart "$service"
healthy=0
for (( attempt=0; attempt<15; attempt++ )); do
    sleep 1
    if systemctl is-active --quiet "$service" && timeout 2 bash -c '
        exec 3<>"/dev/tcp/$1/$2"
        printf "GET /diagnostics.json HTTP/1.0\r\nHost: localhost\r\n\r\n" >&3
        read -r version status rest <&3
        [[ $version == HTTP/* && ( $status == 200 || $status == 401 ) ]]
    ' _ "$health_host" "$listen_port" 2>/dev/null; then
        healthy=1
        break
    fi
done
(( healthy )) || die '网巡未通过 HTTP 检查。请查看 journalctl -u netpatrol.service -n 50。'
pid=$(systemctl show "$service" -p MainPID --value)
caps=$(awk '$1 == "CapEff:" { print $2 }' "/proc/$pid/status")
[[ $caps =~ ^[0-9a-fA-F]+$ ]] && (( (16#$caps & 0x2000) != 0 )) ||
    die '网巡进程未获得 CAP_NET_RAW，无法运行 ICMP 探测。请核对 systemd 和主机安全策略。'
if (( !existing )); then systemctl enable "$service"; fi
validated=1

if (( !skip_firewall )) && { (( !existing )) || [[ -n $zone ]]; } &&
    [[ $host != 127.* && $host != '[::1]' ]] &&
    command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
    zone=${zone:-$(firewall-cmd --get-default-zone)}
    rule=$listen_port/tcp
    added_permanent=0
    if ! firewall-cmd --quiet --permanent --zone="$zone" --query-port="$rule"; then
        firewall-cmd --quiet --permanent --zone="$zone" --add-port="$rule" ||
            die "网巡已启动。firewalld 无法在 $zone 区域持久开放 $rule。"
        added_permanent=1
    fi
    if ! firewall-cmd --quiet --zone="$zone" --query-port="$rule" &&
        ! firewall-cmd --quiet --zone="$zone" --add-port="$rule"; then
        if (( added_permanent )); then firewall-cmd --quiet --permanent --zone="$zone" --remove-port="$rule" || true; fi
        die "网巡已启动。firewalld 无法在 $zone 区域开放 $rule。"
    fi
    printf 'firewalld 区域 %s 已开放 %s。\n' "$zone" "$rule"
fi

printf '网巡服务运行中，HTTP 监听 %s。\n' "$listen"
if [[ $host == 127.* || $host == '[::1]' ]]; then
    printf '访问 http://%s:%s/devices\n' "$host" "$listen_port"
else
    printf '访问 http://服务器IP:%s/devices\n' "$listen_port"
fi
printf '配置 %s\n数据 %s\n' "$config" "$database"
if [[ -n $backup ]]; then printf '升级备份 %s\n' "$backup"; fi

package discovery

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

type Prober interface {
	ProbeDiscovery(context.Context, netip.Addr, []uint16, monitor.Settings) monitor.DiscoveryProbe
}

type Manager struct {
	db       *store.Store
	prober   Prober
	canProbe func() bool
	allowed  []netip.Prefix
	limit    int
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	jobs     sync.WaitGroup
}

type Plan struct {
	Prefix    netip.Prefix
	Ports     []uint16
	PortText  string
	Total     int
	TCPChecks int
	first     netip.Addr
}

func New(ctx context.Context, db *store.Store, prober Prober, allowed string, limit int, canProbe func() bool) (*Manager, error) {
	if limit < 1 || limit > 65536 {
		return nil, errors.New("单次发现上限必须为 1～65536")
	}
	var prefixes []netip.Prefix
	for _, raw := range strings.FieldsFunc(allowed, func(r rune) bool {
		return strings.ContainsRune(" ,\r\n\t", r)
	}) {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || !prefix.Addr().Is4() {
			return nil, fmt.Errorf("无效 IPv4 发现允许网段 %q", raw)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	if err := db.InterruptDiscovery(ctx, 0, "服务重启，任务中断"); err != nil {
		return nil, err
	}
	service, cancel := context.WithCancel(ctx)
	return &Manager{db: db, prober: prober, allowed: prefixes, limit: limit, canProbe: canProbe, ctx: service, cancel: cancel}, nil
}

func (m *Manager) Enabled() bool {
	return m != nil
}

func (m *Manager) Preview(raw, portText string, access Access) (Plan, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
	if err != nil || !prefix.Addr().Is4() {
		return Plan{}, errors.New("请输入有效的 IPv4 CIDR")
	}
	prefix = prefix.Masked()
	var ports []uint16
	var normalized []string
	seen := make(map[uint16]bool)
	for _, raw := range strings.FieldsFunc(portText, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，、", r)
	}) {
		value, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || value == 0 {
			return Plan{}, fmt.Errorf("TCP 端口 %q 无效，必须为 1～65535", raw)
		}
		port := uint16(value)
		if seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
		normalized = append(normalized, strconv.Itoa(int(port)))
	}
	if len(ports) < 1 || len(ports) > 16 {
		return Plan{}, errors.New("请填写 1～16 个不同的 TCP 端口")
	}
	first := prefix.Addr()
	total := uint64(1) << (32 - prefix.Bits())
	if prefix.Bits() <= 30 {
		first = first.Next()
		total -= 2
	}
	if total > uint64(m.limit) {
		return Plan{}, fmt.Errorf("此网段需探测 %d 个地址，超过上限 %d，请拆分网段", total, m.limit)
	}
	if !access.Trusted && !contained(m.allowed, prefix) && !directDiscovery(connectedIPv4(), prefix, access) {
		return Plan{}, errors.New("直连内网可直接发现；探测其他网段请使用管理员登录或已配置的免登录来源")
	}
	return Plan{Prefix: prefix, Ports: ports, PortText: strings.Join(normalized, ", "),
		Total: int(total), TCPChecks: int(total) * len(ports), first: first}, nil
}

func contained(networks []netip.Prefix, prefix netip.Prefix) bool {
	for _, network := range networks {
		if network.Bits() <= prefix.Bits() && network.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func (m *Manager) Start(ctx context.Context, areaID int64, raw, ports string, access Access) (int64, error) {
	plan, err := m.Preview(raw, ports, access)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return 0, err
	}
	settings, err := m.db.Settings(ctx)
	if err != nil {
		return 0, err
	}
	id, err := m.db.CreateDiscovery(ctx, areaID, plan.Prefix, plan.Ports, plan.Total)
	if err != nil {
		return 0, err
	}
	m.jobs.Go(func() {
		if err := m.run(id, plan, settings); err != nil {
			reason := "探测或保存进度失败"
			if !errors.Is(err, context.Canceled) {
				log.Printf("discovery %d: %v", id, err)
			} else {
				reason = "服务停止，任务中断"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := m.db.InterruptDiscovery(ctx, id, reason); err != nil {
				log.Printf("discovery %d: %v", id, err)
			}
		}
	})
	return id, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	m.jobs.Wait()
}

func (m *Manager) run(id int64, plan Plan, settings monitor.Settings) error {
	maxInFlight := 16 / len(plan.Ports)
	ctx, cancel := context.WithCancel(m.ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	results := make(chan monitor.DiscoveryProbe, maxInFlight)
	ticker := time.NewTicker(time.Duration(len(plan.Ports)) * time.Second / 10)
	defer ticker.Stop()
	ip := plan.first
	launched, completed := 0, 0
	for completed < plan.Total {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if launched < plan.Total && launched-completed < maxInFlight && (m.canProbe == nil || m.canProbe()) {
				target := ip
				workers.Go(func() {
					round := m.prober.ProbeDiscovery(ctx, target, plan.Ports, settings)
					select {
					case results <- round:
					case <-ctx.Done():
					}
				})
				ip, launched = ip.Next(), launched+1
			}
		case round := <-results:
			if err := m.db.RecordDiscovery(ctx, id, round); err != nil {
				return err
			}
			completed++
		}
	}
	return nil
}

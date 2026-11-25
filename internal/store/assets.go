package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"netpatrol/internal/monitor"
)

type Area struct {
	ID       int64
	Name     string
	Path     string
	Networks []netip.Prefix
}

type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) SaveArea(ctx context.Context, id int64, name, path, networks string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("区域名称不能为空")
	}
	prefixes := make(map[netip.Prefix]struct{})
	for _, text := range strings.FieldsFunc(networks, func(r rune) bool {
		return strings.ContainsRune(" ,，\n\r\t", r)
	}) {
		prefix, err := parseNetwork(text)
		if err != nil {
			return 0, err
		}
		prefixes[prefix] = struct{}{}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if id == 0 {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO areas(id, name, path)
			VALUES (MAX(COALESCE((SELECT MAX(id) FROM areas), 0),
			  COALESCE((SELECT last_id FROM deleted_id_floors WHERE kind = 'area'), 0)) + 1, ?, ?)
			RETURNING id`, name, path).Scan(&id)
	} else {
		err = tx.QueryRowContext(ctx, "UPDATE areas SET name = ?, path = ? WHERE id = ? RETURNING id", name, path, id).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM area_networks WHERE area_id = ?", id); err != nil {
		return 0, err
	}
	for prefix := range prefixes {
		if _, err := tx.ExecContext(ctx, "INSERT INTO networks(prefix) VALUES (?) ON CONFLICT(prefix) DO NOTHING", prefix.String()); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO area_networks(area_id, network_id)
			SELECT ?, id FROM networks WHERE prefix = ?`, id, prefix.String()); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

func parseNetwork(raw string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
	if err != nil || prefix.Addr().Is4In6() {
		return netip.Prefix{}, fmt.Errorf("无效网段 %q", raw)
	}
	return prefix.Masked(), nil
}

func (s *Store) AddNetwork(ctx context.Context, raw string) error {
	prefix, err := parseNetwork(raw)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO networks(prefix) VALUES (?) ON CONFLICT(prefix) DO NOTHING", prefix.String())
	return err
}

func (s *Store) Networks(ctx context.Context) ([]netip.Prefix, error) {
	rows, err := s.reader.QueryContext(ctx, "SELECT prefix FROM networks ORDER BY prefix")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prefixes []netip.Prefix
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, rows.Err()
}

func (s *Store) Areas(ctx context.Context) ([]Area, error) {
	return readAreas(ctx, s.reader)
}

func readAreas(ctx context.Context, query querier) ([]Area, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT a.id, a.name, a.path, n.prefix FROM areas a
		LEFT JOIN area_networks l ON l.area_id = a.id LEFT JOIN networks n ON n.id = l.network_id
		ORDER BY a.name, a.id, n.prefix`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var areas []Area
	for rows.Next() {
		var area Area
		var text sql.NullString
		if err := rows.Scan(&area.ID, &area.Name, &area.Path, &text); err != nil {
			return nil, err
		}
		if len(areas) == 0 || areas[len(areas)-1].ID != area.ID {
			areas = append(areas, area)
		}
		if !text.Valid {
			continue
		}
		prefix, err := netip.ParsePrefix(text.String)
		if err != nil {
			return nil, err
		}
		last := &areas[len(areas)-1]
		last.Networks = append(last.Networks, prefix)
	}
	return areas, rows.Err()
}

type Device struct {
	ID      int64
	Code    string
	Name    string
	AreaID  int64
	Target  monitor.Target
	Enabled bool
	Version int64
}

type DeviceInput struct {
	Code    string
	Name    string
	AreaID  int64
	Address string
	Port    int
	Enabled bool
}

func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	return readDevices(ctx, s.reader)
}

func readDevices(ctx context.Context, query querier) ([]Device, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT id, code, name, area_id, address, port, enabled, version
		FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []Device
	for rows.Next() {
		var device Device
		var address string
		var port int
		if err := rows.Scan(&device.ID, &device.Code, &device.Name, &device.AreaID, &address, &port,
			&device.Enabled, &device.Version); err != nil {
			return nil, err
		}
		device.Target, err = monitor.ParseTarget(address, port)
		if err != nil {
			return nil, err
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Store) SaveDevice(ctx context.Context, id int64, input DeviceInput) (int64, error) {
	input, target, err := validateDevice(input)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	areas, err := readAreas(ctx, tx)
	if err != nil {
		return 0, err
	}
	area, err := resolveArea(areas, input.AreaID, target.IP)
	if err != nil {
		return 0, err
	}
	input.AreaID = area.ID
	id, err = writeDevice(ctx, tx, id, input, target)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func validateDevice(input DeviceInput) (DeviceInput, monitor.Target, error) {
	target, err := monitor.ParseTarget(input.Address, input.Port)
	input.Code = strings.TrimSpace(input.Code)
	input.Name = strings.TrimSpace(input.Name)
	if err == nil && input.Name == "" {
		err = errors.New("设备名称不能为空")
	}
	return input, target, err
}

func writeDevice(ctx context.Context, tx *sql.Tx, id int64, input DeviceInput, target monitor.Target) (int64, error) {
	var err error
	if id == 0 {
		code := input.Code
		if code == "" {
			var token [8]byte
			if _, err := rand.Read(token[:]); err != nil {
				return 0, err
			}
			code = "CAM-" + strings.ToUpper(hex.EncodeToString(token[:]))
		}
		err = tx.QueryRowContext(ctx, `
			INSERT INTO devices(id, code, name, area_id, address, port, enabled)
			VALUES (MAX(COALESCE((SELECT MAX(id) FROM devices), 0),
			  COALESCE((SELECT last_id FROM deleted_id_floors WHERE kind = 'device'), 0)) + 1, ?, ?, ?, ?, ?, ?)
			RETURNING id`,
			code, input.Name, input.AreaID, target.IP.String(), target.Port, input.Enabled).Scan(&id)
	} else {
		err = tx.QueryRowContext(ctx, `
			UPDATE devices SET name = ?, area_id = ?,
			  version = version + (address <> ? OR port <> ? OR enabled <> ?),
			  address = ?, port = ?, enabled = ?
			WHERE id = ? RETURNING id`, input.Name, input.AreaID,
			target.IP.String(), target.Port, input.Enabled,
			target.IP.String(), target.Port, input.Enabled, id).Scan(&id)
	}
	return id, err
}

func resolveArea(areas []Area, id int64, ip netip.Addr) (Area, error) {
	if id != 0 {
		for _, area := range areas {
			if area.ID == id {
				return area, nil
			}
		}
		return Area{}, errors.New("指定区域不存在")
	}
	ip = ip.WithZone("")
	var matched Area
	for _, area := range areas {
		for _, prefix := range area.Networks {
			if prefix.Contains(ip) {
				if matched.ID != 0 {
					return Area{}, errors.New("IP 匹配多个区域，请手动选择")
				}
				matched = area
				break
			}
		}
	}
	if matched.ID == 0 {
		return Area{}, errors.New("IP 未匹配任何区域，请手动选择")
	}
	return matched, nil
}

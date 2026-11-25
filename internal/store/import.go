package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"netpatrol/internal/monitor"
)

type ImportRow struct {
	Line int
	Area string
	DeviceInput
	Error string
}

type ImportAction string

const (
	ImportAdd    ImportAction = "新增"
	ImportUpdate ImportAction = "更新"
	ImportSkip   ImportAction = "跳过"
	ImportError  ImportAction = "错误"
)

type ImportEntry struct {
	ImportRow
	Action   ImportAction
	Message  string
	previous Device
	target   monitor.Target
}

func (s *Store) PreviewImport(ctx context.Context, rows []ImportRow, update bool) ([]ImportEntry, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	plan, err := planImport(ctx, tx, rows, update)
	if err != nil {
		return nil, err
	}
	return plan, tx.Commit()
}

// ConfirmImport checks the preview against the same transaction as the writes.
// A stale preview is returned for review without applying any of its rows.
func (s *Store) ConfirmImport(ctx context.Context, rows []ImportRow, update bool, preview []ImportEntry) ([]ImportEntry, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	plan, err := planImport(ctx, tx, rows, update)
	if err != nil || !slices.Equal(plan, preview) {
		return plan, false, err
	}
	for _, entry := range plan {
		if entry.Action != ImportAdd && entry.Action != ImportUpdate {
			continue
		}
		if _, err := writeDevice(ctx, tx, entry.previous.ID, entry.DeviceInput, entry.target); err != nil {
			return nil, false, fmt.Errorf("CSV 第 %d 行: %w", entry.Line, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return plan, true, nil
}

func planImport(ctx context.Context, tx *sql.Tx, rows []ImportRow, update bool) ([]ImportEntry, error) {
	areas, err := readAreas(ctx, tx)
	if err != nil {
		return nil, err
	}
	devices, err := readDevices(ctx, tx)
	if err != nil {
		return nil, err
	}
	byCode := make(map[string]Device, len(devices))
	byAddress := make(map[netip.Addr]Device, len(devices))
	byAreaName := make(map[string]int64, len(areas))
	for _, device := range devices {
		byCode[device.Code], byAddress[device.Target.IP] = device, device
	}
	for _, area := range areas {
		byAreaName[area.Name] = area.ID
	}
	codeCounts := make(map[string]int)
	addressCounts := make(map[netip.Addr]int)
	plan := make([]ImportEntry, len(rows))
	for i, row := range rows {
		input, target, err := validateDevice(row.DeviceInput)
		row.DeviceInput = input
		row.Area = strings.TrimSpace(row.Area)
		entry := ImportEntry{ImportRow: row, target: target}
		if row.Error != "" {
			entry.Action, entry.Message = ImportError, row.Error
		} else if err != nil {
			entry.Action, entry.Message = ImportError, err.Error()
		}
		if entry.Code != "" {
			codeCounts[entry.Code]++
		}
		if target.IP.IsValid() {
			entry.Address = target.IP.String()
			addressCounts[target.IP]++
		}
		plan[i] = entry
	}
	for i := range plan {
		entry := &plan[i]
		if entry.Action == ImportError {
			continue
		}
		switch {
		case codeCounts[entry.Code] > 1:
			entry.Action, entry.Message = ImportError, "文件内设备编号重复"
			continue
		case addressCounts[entry.target.IP] > 1:
			entry.Action, entry.Message = ImportError, "文件内 IP 重复"
			continue
		case entry.Area != "":
			entry.AreaID = byAreaName[entry.Area]
			if entry.AreaID == 0 {
				entry.Action, entry.Message = ImportError, "指定区域不存在"
				continue
			}
		}
		addressOwner := byAddress[entry.target.IP]
		if entry.Code == "" && addressOwner.ID != 0 {
			entry.previous = addressOwner
			entry.Action, entry.Message = ImportSkip, "IP 已纳管"
			continue
		}
		area, err := resolveArea(areas, entry.AreaID, entry.target.IP)
		if err != nil {
			entry.Action, entry.Message = ImportError, err.Error()
			continue
		}
		entry.AreaID, entry.Area = area.ID, area.Name
		existing := byCode[entry.Code]
		switch {
		case addressOwner.ID != 0 && addressOwner.ID != existing.ID:
			entry.Action, entry.Message = ImportError, "IP 已由其他设备编号使用"
		case existing.ID == 0:
			entry.Enabled = true
			entry.Action = ImportAdd
		case !update:
			entry.previous = existing
			entry.Action, entry.Message = ImportSkip, "设备编号已存在，当前模式仅新增"
		default:
			entry.previous = existing
			entry.Enabled = existing.Enabled
			if existing.Name == entry.Name && existing.AreaID == entry.AreaID && existing.Target == entry.target {
				entry.Action, entry.Message = ImportSkip, "设备内容无变化"
			} else {
				entry.Action = ImportUpdate
			}
		}
	}
	return plan, nil
}

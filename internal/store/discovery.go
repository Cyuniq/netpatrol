package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"netpatrol/internal/monitor"
)

var ErrDiscoveryBusy = errors.New("已有发现任务正在运行")

type DiscoveryJob struct {
	ID, PreferredAreaID                           int64
	Prefix, State, Reason                         string
	Ports                                         []uint16
	Total, Completed, CandidateCount, LocalErrors int
}

type Candidate struct {
	Address  string
	Probe    monitor.DiscoveryProbe
	Existing bool
}

func (s *Store) CreateDiscovery(ctx context.Context, areaID int64, prefix netip.Prefix, ports []uint16, total int) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var preferred sql.NullInt64
	if areaID > 0 {
		if err := tx.QueryRowContext(ctx, "SELECT id FROM areas WHERE id = ?", areaID).Scan(&preferred); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, errors.New("所选区域不存在")
			}
			return 0, err
		}
	}
	var running bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM discovery_jobs WHERE state = 'running')").Scan(&running); err != nil {
		return 0, err
	}
	if running {
		return 0, ErrDiscoveryBusy
	}
	encoded, err := json.Marshal(ports)
	if err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO discovery_jobs(preferred_area_id, prefix, ports, total, state, created_ns)
		VALUES (?, ?, ?, ?, 'running', ?) RETURNING id`,
		preferred, prefix.String(), string(encoded), total, time.Now().UnixNano(),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) RecordDiscovery(ctx context.Context, jobID int64, probe monitor.DiscoveryProbe) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	localError := probe.Ping.Outcome == monitor.LocalError
	discovered := probe.Ping.Outcome == monitor.Passed
	for _, port := range probe.Ports {
		localError = localError || port.Result.Outcome == monitor.LocalError
		discovered = discovered || port.Result.Outcome == monitor.Passed
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE discovery_jobs SET completed = completed + 1, local_errors = local_errors + ?,
		  state = CASE WHEN completed + 1 = total THEN 'done' ELSE state END,
		  finished_ns = CASE WHEN completed + 1 = total THEN ? ELSE finished_ns END
		WHERE id = ? AND state = 'running'`, localError, time.Now().UnixNano(), jobID)
	if err != nil {
		return err
	}
	touched, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if touched == 0 {
		return sql.ErrNoRows
	}
	if discovered {
		data, err := json.Marshal(probe)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO discovery_candidates(job_id, address, result) VALUES (?, ?, ?)",
			jobID, probe.IP.String(), string(data)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) InterruptDiscovery(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE discovery_jobs SET state = 'interrupted', finished_ns = ?, reason = ?
		WHERE state = 'running' AND (? = 0 OR id = ?)`, time.Now().UnixNano(), reason, id, id)
	return err
}

func (s *Store) Discovery(ctx context.Context, id int64) (DiscoveryJob, []Candidate, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return DiscoveryJob{}, nil, err
	}
	defer tx.Rollback()
	job, err := readDiscovery(ctx, tx, id)
	if err != nil {
		return job, nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.address, c.result, d.id IS NOT NULL FROM discovery_candidates c
		LEFT JOIN devices d ON d.address = c.address WHERE c.job_id = ?`, id)
	if err != nil {
		return job, nil, err
	}
	defer rows.Close()
	var candidates []Candidate
	for rows.Next() {
		var candidate Candidate
		var data string
		if err := rows.Scan(&candidate.Address, &data, &candidate.Existing); err != nil {
			return job, nil, err
		}
		if err := json.Unmarshal([]byte(data), &candidate.Probe); err != nil {
			return job, nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return job, nil, err
	}
	job.CandidateCount = len(candidates)
	return job, candidates, tx.Commit()
}

func readDiscovery(ctx context.Context, tx *sql.Tx, id int64) (DiscoveryJob, error) {
	var job DiscoveryJob
	var ports string
	err := tx.QueryRowContext(ctx, `
		SELECT id, COALESCE(preferred_area_id, 0), prefix, ports, state, total, completed, local_errors, reason
		FROM discovery_jobs WHERE id = ?`, id,
	).Scan(&job.ID, &job.PreferredAreaID, &job.Prefix, &ports, &job.State, &job.Total, &job.Completed, &job.LocalErrors, &job.Reason)
	if err != nil {
		return job, err
	}
	err = json.Unmarshal([]byte(ports), &job.Ports)
	return job, err
}

type DiscoverySelection struct {
	Address, Name string
	Port          uint16
}

func (s *Store) AddDiscovered(ctx context.Context, id, areaID int64, selected []DiscoverySelection) (added, existing int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	job, err := readDiscovery(ctx, tx, id)
	if err != nil {
		return 0, 0, err
	}
	if job.State == "running" {
		return 0, 0, errors.New("请等待发现任务结束")
	}
	if areaID == 0 {
		areaID = job.PreferredAreaID
	}
	var areaName string
	if err := tx.QueryRowContext(ctx, "SELECT name FROM areas WHERE id = ?", areaID).Scan(&areaName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, errors.New("请选择有效的纳管区域")
		}
		return 0, 0, err
	}
	for _, selection := range selected {
		var belongs, managed bool
		err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM discovery_candidates WHERE job_id = ? AND address = ?),
			  EXISTS(SELECT 1 FROM devices WHERE address = ?)`, id, selection.Address, selection.Address,
		).Scan(&belongs, &managed)
		if err != nil {
			return 0, 0, err
		}
		if !belongs {
			return 0, 0, fmt.Errorf("IP %q 不属于该任务", selection.Address)
		}
		if managed {
			existing++
			continue
		}
		if selection.Port == 0 && len(job.Ports) == 1 {
			selection.Port = job.Ports[0]
		}
		if !slices.Contains(job.Ports, selection.Port) {
			return 0, 0, fmt.Errorf("%s 的纳管端口必须为本次探测的端口之一", selection.Address)
		}
		name := strings.TrimSpace(selection.Name)
		if name == "" {
			name = areaName + "-" + selection.Address
		}
		input, target, err := validateDevice(DeviceInput{Name: name, AreaID: areaID, Address: selection.Address, Port: int(selection.Port), Enabled: true})
		if err != nil {
			return 0, 0, fmt.Errorf("%s: %w", selection.Address, err)
		}
		if _, err := writeDevice(ctx, tx, 0, input, target); err != nil {
			return 0, 0, fmt.Errorf("%s: %w", selection.Address, err)
		}
		added++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return added, existing, nil
}

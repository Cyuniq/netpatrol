CREATE TABLE IF NOT EXISTS areas (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    path TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE IF NOT EXISTS networks (
    id INTEGER PRIMARY KEY,
    prefix TEXT NOT NULL UNIQUE
) STRICT;

CREATE TABLE IF NOT EXISTS area_networks (
    area_id INTEGER NOT NULL REFERENCES areas(id) ON DELETE CASCADE,
    network_id INTEGER NOT NULL REFERENCES networks(id),
    PRIMARY KEY (area_id, network_id)
) STRICT;

CREATE TABLE IF NOT EXISTS devices (
    id INTEGER PRIMARY KEY,
    code TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    area_id INTEGER NOT NULL REFERENCES areas(id),
    address TEXT NOT NULL UNIQUE,
    port INTEGER NOT NULL CHECK (port BETWEEN 1 AND 65535),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0)
) STRICT;

CREATE INDEX IF NOT EXISTS devices_area_name ON devices(area_id, name, id);
CREATE INDEX IF NOT EXISTS devices_name ON devices(name, id);

CREATE TABLE IF NOT EXISTS deleted_id_floors (
    kind TEXT PRIMARY KEY,
    last_id INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS probe_paths (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    address TEXT NOT NULL UNIQUE,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    interval_seconds INTEGER NOT NULL CHECK (interval_seconds BETWEEN 60 AND 86400),
    generation INTEGER NOT NULL DEFAULT 1,
    next_check_ns INTEGER NOT NULL,
    signature TEXT,
    latest TEXT
) STRICT;

CREATE INDEX IF NOT EXISTS probe_paths_due ON probe_paths(next_check_ns, id) WHERE enabled = 1;

CREATE TABLE IF NOT EXISTS probe_path_changes (
    id INTEGER PRIMARY KEY,
    path_id INTEGER NOT NULL REFERENCES probe_paths(id) ON DELETE CASCADE,
    sample TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS probe_path_changes_path ON probe_path_changes(path_id, id DESC);

CREATE TABLE IF NOT EXISTS probe_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    ping_timeout_ns INTEGER NOT NULL,
    tcp_timeout_ns INTEGER NOT NULL,
    fast_interval_ns INTEGER NOT NULL,
    slow_interval_ns INTEGER NOT NULL,
    slow_after_ns INTEGER NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS latest_results (
    device_id INTEGER PRIMARY KEY REFERENCES devices(id),
    version INTEGER NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('unknown', 'healthy', 'partial', 'down', 'disabled')),
    state_since_ns INTEGER NOT NULL,
    completed_ns INTEGER NOT NULL,
    snapshot TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS ping_minutes (
    device_id INTEGER NOT NULL REFERENCES devices(id),
    version INTEGER NOT NULL,
    minute INTEGER NOT NULL,
    sent INTEGER NOT NULL CHECK (sent > 0),
    replies INTEGER NOT NULL CHECK (replies BETWEEN 0 AND sent),
    PRIMARY KEY (device_id, version, minute)
) STRICT;

CREATE INDEX IF NOT EXISTS ping_minutes_retention ON ping_minutes(minute);

CREATE TABLE IF NOT EXISTS device_latency (
    device_id INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    address TEXT NOT NULL,
    port INTEGER NOT NULL,
    step INTEGER NOT NULL CHECK (step IN (60, 3600)),
    at INTEGER NOT NULL,
    protocol TEXT NOT NULL CHECK (protocol IN ('ping', 'tcp')),
    replies INTEGER NOT NULL,
    timeouts INTEGER NOT NULL,
    failures INTEGER NOT NULL,
    errors INTEGER NOT NULL,
    total_ns INTEGER NOT NULL,
    min_ns INTEGER,
    max_ns INTEGER,
    PRIMARY KEY (device_id, address, port, step, at, protocol)
) WITHOUT ROWID, STRICT;

CREATE INDEX IF NOT EXISTS device_latency_retention ON device_latency(step, at);
CREATE INDEX IF NOT EXISTS device_latency_history ON device_latency(device_id, address, step, at);

CREATE TABLE IF NOT EXISTS path_latency (
    path_id INTEGER NOT NULL REFERENCES probe_paths(id) ON DELETE CASCADE,
    address TEXT NOT NULL,
    at_ns INTEGER NOT NULL,
    sample TEXT NOT NULL,
    PRIMARY KEY (path_id, address, at_ns)
) WITHOUT ROWID, STRICT;

CREATE INDEX IF NOT EXISTS path_latency_retention ON path_latency(at_ns);

CREATE TABLE IF NOT EXISTS state_events (
    id INTEGER PRIMARY KEY,
    device_id INTEGER NOT NULL REFERENCES devices(id),
    area_id INTEGER NOT NULL,
    area_name TEXT NOT NULL,
    version INTEGER NOT NULL,
    at_ns INTEGER NOT NULL,
    event TEXT NOT NULL
) STRICT;

CREATE INDEX IF NOT EXISTS state_events_device_time ON state_events(device_id, at_ns DESC, id DESC);
CREATE INDEX IF NOT EXISTS state_events_area_time ON state_events(area_id, at_ns DESC, id DESC);
CREATE INDEX IF NOT EXISTS state_events_time ON state_events(at_ns DESC, id DESC);

CREATE TABLE IF NOT EXISTS discovery_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    preferred_area_id INTEGER REFERENCES areas(id),
    prefix TEXT NOT NULL,
    ports TEXT NOT NULL CHECK (json_array_length(ports) BETWEEN 1 AND 16),
    state TEXT NOT NULL CHECK (state IN ('running', 'done', 'interrupted')),
    created_ns INTEGER NOT NULL,
    finished_ns INTEGER,
    total INTEGER NOT NULL CHECK (total > 0),
    completed INTEGER NOT NULL DEFAULT 0 CHECK (completed BETWEEN 0 AND total),
    local_errors INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE UNIQUE INDEX IF NOT EXISTS discovery_one_running ON discovery_jobs(state) WHERE state = 'running';
CREATE INDEX IF NOT EXISTS discovery_retention ON discovery_jobs(finished_ns);

CREATE TABLE IF NOT EXISTS discovery_candidates (
    job_id INTEGER NOT NULL REFERENCES discovery_jobs(id) ON DELETE CASCADE,
    address TEXT NOT NULL,
    result TEXT NOT NULL,
    PRIMARY KEY (job_id, address)
) STRICT;

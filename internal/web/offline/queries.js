// The browser runs these queries against the exported SQLite file. Times in the
// chart use milliseconds. Storage uses Unix seconds (device) or nanoseconds (path).
class OfflineQueries {
  constructor(db, at) {
    this.db = db;
    this.at = at;
  }

  rows(sql, params = []) {
    const stmt = this.db.prepare(sql);
    try {
      stmt.bind(params);
      const rows = [];
      while (stmt.step()) rows.push(stmt.getAsObject());
      return rows;
    } finally {
      stmt.free();
    }
  }

  page(sql, params, page, size) {
    // Keep database integer values exact, including nanosecond timestamps.
    const stmt = this.db.prepare("SELECT * FROM (\n" + sql + "\n) LIMIT ? OFFSET ?");
    try {
      stmt.bind([...params, size + 1, page * size]);
      const columns = stmt.getColumnNames();
      const rows = [];
      while (stmt.step()) rows.push(stmt.get(null, {useBigInt: true}));
      return {columns, rows: rows.slice(0, size), more: rows.length > size};
    } finally {
      stmt.free();
    }
  }

  trend(path, id, address, interval, query) {
    const relative = query.get("minutes");
    const to = relative ? this.at : Date.parse(query.get("to"));
    const from = relative ? to - Number(relative) * 60000 : Date.parse(query.get("from"));
    const source = from < Math.floor((this.at - 86400000) / 60000) * 60000 ? 3600 : 60;
    const minimum = path ? Math.min(interval, Math.max(60, (to - from) / 2000)) : source;
    const step = [60,120,300,600,900,1800,3600,7200,14400,28800,43200,86400].find(
      s => s >= minimum && Math.floor(to / (1000*s)) - Math.floor(from / (1000*s)) < 1200
    ) || 86400;
    const period = {from, to, step};
    if (path) return this.pathTrend(id, address, period, query);
    return this.deviceTrend(id, address, period, step === 3600 ? step : source);
  }

  deviceTrend(id, address, period, source) {
    const {from, to, step} = period;
    const rows = this.rows(`SELECT at / ? * ? AS bucket, protocol,
      CASE WHEN protocol='tcp' THEN port ELSE 0 END AS service,
      sum(replies) r, sum(timeouts) t, sum(failures) f, sum(errors) e,
      sum(total_ns) sum, min(min_ns) min, max(max_ns) max
      FROM device_latency WHERE device_id=? AND address=? AND step=? AND at>=? AND at<?
      GROUP BY bucket,protocol,service ORDER BY bucket,service`,
      [step,step,id,address,source,Math.floor(from/(source*1000))*source,Math.ceil(to/1000)]);
    const ping = [], tcp = [];
    for (const row of rows) {
      const at = row.bucket * 1000;
      if (row.protocol === "ping") {
        ping.push({at, ...chartStats(row)});
      } else {
        let point = tcp.at(-1);
        if (!point || point.at !== at) {
          point = {at, totals: {}, tcpPorts: []};
          tcp.push(point);
        }
        addStats(point.totals, row);
        point.tcpPorts.push({port: row.service, ...chartStats(row)});
      }
    }
    return {...period, series: [{name:"Ping", points:ping}, {name:"TCP", points:
      tcp.map(({at,totals,tcpPorts}) => ({at, ...chartStats(totals), tcpPorts}))}]};
  }

  pathTrend(id, address, period, query) {
    const hop = Number(query.get("hop")), responder = query.get("responder");
    const options = new Map(), buckets = new Map();
    const stmt = this.db.prepare(`SELECT at_ns / 1000000, sample FROM path_latency
      WHERE path_id=? AND address=? AND at_ns>=?*1000000 AND at_ns<?*1000000 ORDER BY at_ns`);
    try {
      stmt.bind([id,address,period.from,period.to]);
      while (stmt.step()) {
        const [at,text] = stmt.get(), sample = JSON.parse(text);
        let stats = sample.target, other = 0;
        let found = !hop;
        for (const h of sample.hops || []) {
          if (!options.has(h.n)) options.set(h.n, new Set());
          for (const ip of Object.keys(h.responders || {})) options.get(h.n).add(ip);
          if (h.n !== hop) continue;
          found = true;
          stats = h.stats;
          if (responder) {
            const selected = h.responders?.[responder] || {};
            other = (stats.r || 0) - (selected.r || 0);
            stats = {...stats, r:selected.r, sum:selected.sum, min:selected.min, max:selected.max};
          }
        }
        if (!found) continue;
        const time = Math.floor(at / (period.step*1000)) * period.step*1000;
        const p = buckets.get(time) || {at:time, totals:{}, checks:0, otherReplies:0, unreached:0, localChecks:0};
        addStats(p.totals, stats);
        p.checks++;
        p.otherReplies += other;
        if (!hop && !sample.reached) {
          if (sample.local) p.localChecks++; else p.unreached++;
        }
        buckets.set(time,p);
      }
    } finally {
      stmt.free();
    }
    const hops = [];
    for (const number of [...options.keys()].sort((a,b)=>a-b)) {
      hops.push({number});
      for (const ip of [...options.get(number)].sort()) hops.push({number,address:ip});
    }
    return {...period, hops, series:[{name:hop ? "跳点" : "目标端",
      points:Array.from(buckets.values(), ({totals,...p})=>({...p,...chartStats(totals)}))}]};
  }
}

function addStats(acc, part) {
  if (part.r) {
    acc.min = acc.r ? Math.min(acc.min, part.min) : part.min;
    acc.max = acc.r ? Math.max(acc.max, part.max) : part.max;
  }
  for (const key of ["r","t","f","e","sum"]) acc[key] = (acc[key] || 0) + (part[key] || 0);
}

function chartStats(s) {
  return {replies:s.r || 0, timeouts:s.t || 0, failures:s.f || 0, errors:s.e || 0,
    rtt:s.r ? [s.min/1e6, s.sum/s.r/1e6, s.max/1e6] : null};
}

(() => {
  "use strict";
  const byId = id => document.getElementById(id);
  const fields = byId("filters").elements;
  const at = Number(document.body.dataset.at);
  const states = {healthy:"正常", partial:"部分异常", down:"不可达", unknown:"未知", disabled:"停用"};
  const titles = {devices:"设备", paths:"路径", events:"状态记录", tables:"数据表与 SQL"};
  const outcomes = {passed:"通过", timeout:"超时", refused:"连接拒绝", unreachable:"不可达", local_error:"本地异常"};
  const size = 100;
  let query, tab = "devices", page = 0, disposeChart;
  const timestamp = ms => new Date(Number(ms)).toLocaleString("zh-CN", {hour12:false});
  byId("info").textContent = "快照时间 " + timestamp(at);

  function error(message = "") {
    byId("error").textContent = message;
    byId("error").hidden = !message;
  }
  function table(columns, rows, action) {
    const el = document.createElement("table");
    const head = el.createTHead().insertRow();
    for (const name of columns) {
      const th = document.createElement("th");
      th.textContent = name;
      head.append(th);
    }
    if (action) {
      const th = document.createElement("th");
      th.textContent = "详情";
      head.append(th);
    }
    const body = el.createTBody();
    for (const row of rows) {
      const tr = body.insertRow();
      for (const cell of row) tr.insertCell().textContent = cell == null ? "—" : String(cell);
      if (action) {
        const button = document.createElement("button");
        button.className = "button";
        button.textContent = "查看";
        button.onclick = () => { try { action(row); } catch (ex) { error(ex.message); } };
        tr.insertCell().append(button);
      }
    }
    if (!rows.length) {
      const cell = body.insertRow().insertCell();
      cell.colSpan = columns.length + Number(Boolean(action));
      cell.textContent = "没有匹配记录";
    }
    return el;
  }
  function closeDetail() {
    disposeChart?.();
    disposeChart = undefined;
    byId("detail").hidden = true;
    byId("chart").replaceChildren();
    byId("extra").replaceChildren();
  }
  function showDetail(row) {
    closeDetail();
    const path = tab === "paths";
    const id = Number(row[0]);
    const name = path ? row[1] : row[2], address = path ? row[2] : row[4];
    const interval = path ? Number(row[4]) : 0;
    byId("detail-name").textContent = name + " · " + address;
    byId("detail").hidden = false;
    byId("chart").append(byId("trend-template").content.cloneNode(true));
    const panel = byId("chart").querySelector(".latency-panel");
    disposeChart = initLatencyPanel(panel, {isPath:path, now:at, live:false,
      read: params => query.trend(path, id, address, interval, params)});
    if (path) {
      const extra = byId("extra");
      const [watch] = query.rows("SELECT latest FROM probe_paths WHERE id=?", [id]);
      const latest = watch.latest && JSON.parse(watch.latest);
      if (latest) {
        const heading = document.createElement("h3");
        heading.textContent = "最近一次持续检测 · " + timestamp(Date.parse(latest.completed));
        extra.append(heading);
        const hops = latest.result?.Hops || [];
        extra.append(table(["跳数","回应地址","结果"],hops.map(h => [h.Number,h.Address || "—",h.Result?.outcome])));
      }
      const changesHeading = document.createElement("h3");
      changesHeading.textContent = "路径变化";
      extra.append(changesHeading);
      const changes = query.rows("SELECT sample FROM probe_path_changes WHERE path_id=? ORDER BY id DESC", [id]);
      extra.append(table(["完成时间","目标","结果","逐跳路径"], changes.map(({sample})=>{
        const p=JSON.parse(sample);
        return [timestamp(Date.parse(p.completed)),p.address,p.message || (p.result.Reached?"已到达":"未到达"),
          (p.result.Hops || []).map(h=>h.Address || "*").join(" → ")];
      })));
    }
    byId("detail").scrollIntoView({block:"start"});
  }
  function day(raw, nextDay) {
    const d = new Date(raw + "T00:00:00");
    if (nextDay) d.setDate(d.getDate()+1);
    return d.getTime();
  }
  function records() {
    const filters = [], params = [];
    function filter(sql, ...args) { filters.push(sql); params.push(...args); }
    const key = fields.keyword.value.trim();
    let sql, order;
    if (tab === "tables") {
      sql = byId("sql").value.trim().replace(/;\s*$/, "");
      if (!/^(select|with)\b/i.test(sql)) throw new Error("请输入 SELECT 或 WITH 查询。");
      return query.page(sql, [], page, size);
    }
    const deviceState = "CASE WHEN d.enabled=0 THEN 'disabled' ELSE coalesce(r.state,'unknown') END";
    if (tab === "paths") {
      sql = "SELECT id AS ID,name AS 名称,address AS 目标地址,enabled AS 持续检测,interval_seconds AS 间隔秒 FROM probe_paths";
      if (key) filter("instr(lower(name),lower(?)) OR instr(address,?)",key,key);
      order = "id";
    } else {
      if (key) filter("instr(lower(d.name),lower(?)) OR instr(lower(d.code),lower(?)) OR instr(lower(d.address),lower(?))",key,key,key);
      if (tab === "devices") {
        sql = `SELECT d.id AS ID,d.code AS 设备编号,d.name AS 名称,a.name AS 区域,d.address AS IP,d.port AS TCP端口,
          ${deviceState} AS 状态,r.completed_ns/1000000 AS 最后探测,
          json_extract(r.snapshot,'$.last_round.ping.outcome') AS Ping,json_extract(r.snapshot,'$.last_round.tcp.outcome') AS TCP
          FROM devices d JOIN areas a ON a.id=d.area_id LEFT JOIN latest_results r ON r.device_id=d.id AND r.version=d.version`;
        if (fields.area.value) filter("d.area_id=?",Number(fields.area.value));
        if (fields.state.value) filter(deviceState+"=?",fields.state.value);
        order = "d.id";
      } else {
        const from = fields.from.value, to = fields.to.value;
        if (from && to && from > to) throw new Error("结束日期须晚于或等于起始日期。");
        sql = `SELECT e.at_ns/1000000 AS 时间,d.name AS 设备,e.area_name AS 区域,
          json_extract(e.event,'$.previous') AS 此前状态,json_extract(e.event,'$.current') AS 当前状态,
          json_extract(e.event,'$.round.target.ip') AS IP,json_extract(e.event,'$.round.target.port') AS TCP端口,
          json_extract(e.event,'$.round.ping.outcome') AS Ping,json_extract(e.event,'$.round.tcp.outcome') AS TCP
          FROM state_events e JOIN devices d ON d.id=e.device_id`;
        if (fields.area.value) filter("e.area_id=?",Number(fields.area.value));
        if (fields.state.value) filter("json_extract(e.event,'$.current')=?",fields.state.value);
        if (from) filter("e.at_ns>=?*1000000",day(from,false));
        if (to) filter("e.at_ns<?*1000000",day(to,true));
        order = "e.at_ns DESC,e.id DESC";
      }
    }
    if (filters.length) sql += " WHERE "+filters.map(f=>"("+f+")").join(" AND ");
    const result = query.page(sql+" ORDER BY "+order, params, page, size);
    for (const row of result.rows) result.columns.forEach((name,i)=>{
      const value=row[i];
      if (value == null) return;
      if (["时间","最后探测"].includes(name)) row[i]=timestamp(value);
      if (["状态","此前状态","当前状态"].includes(name)) row[i]=states[value] || value;
      if (["Ping","TCP"].includes(name)) row[i]=outcomes[value] || value;
      if (name==="持续检测") row[i]=Number(value) ? "已启用" : "未启用";
    });
    return result;
  }
  function render() {
    closeDetail();
    error();
    try {
      const data=records();
      byId("records").replaceChildren(table(data.columns,data.rows,["devices","paths"].includes(tab)?showDetail:null));
      byId("count").textContent = data.rows.length ? "第 "+(page+1)+" 页 · 第 "+(page*size+1)+"–"+(page*size+data.rows.length)+" 条" : "0 条";
      byId("prev").disabled = page===0;
      byId("next").disabled = !data.more;
    } catch (ex) {
      error(ex.message);
      byId("records").replaceChildren();
      byId("count").textContent="";
      byId("next").disabled=true;
      byId("prev").disabled=page===0;
    }
  }
  function selectTab(name) {
    tab=name;
    page=0;
    for (const button of byId("tabs").querySelectorAll("button"))
      button.setAttribute("aria-pressed",String(button.dataset.tab===tab));
    byId("heading").textContent=titles[tab];
    byId("filters").hidden=tab==="tables";
    byId("workbench").hidden=tab!=="tables";
    byId("area-label").hidden=byId("state-label").hidden=tab==="paths";
    byId("from-label").hidden=byId("to-label").hidden=tab!=="events";
    render();
  }
  function selectTable() {
    byId("sql").value = 'SELECT * FROM "'+byId("tables").value.replaceAll('"','""')+'"';
  }
  byId("file").onchange=async event=>{
    const file=event.target.files[0];
    if (!file) return;
    const picker=event.target;
    picker.disabled=true;
    closeDetail();
    query?.db.close();
    query=undefined;
    byId("workspace").hidden=true;
    error();
    byId("info").textContent="正在读取 "+file.name;
    let db;
    try {
      const SQL=await initSqlJs();
      db=new SQL.Database(new Uint8Array(await file.arrayBuffer()));
      db.run("PRAGMA trusted_schema=OFF; PRAGMA query_only=ON");
      query=new OfflineQueries(db,at);
      const areas=query.rows("SELECT id,name FROM areas UNION SELECT area_id,area_name FROM state_events ORDER BY name");
      fields.area.replaceChildren(new Option("全部区域",""),...areas.map(a=>new Option(a.name,a.id)));
      const tables=query.rows("SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name");
      byId("tables").replaceChildren(...tables.map(t=>new Option(t.name,t.name)));
      byId("tables").value="devices";
      selectTable();
      byId("workspace").hidden=false;
      byId("info").textContent=file.name+" · 快照时间 "+timestamp(at);
      selectTab("devices");
    } catch (ex) {
      db?.close();
      query=undefined;
      byId("info").textContent="";
      error("无法打开快照。请选择同一浏览包中的 netpatrol.db。"+ex.message);
    } finally { picker.disabled=false; picker.value=""; }
  };
  fields.state.replaceChildren(new Option("全部状态",""), ...Object.entries(states).map(([v,s])=>new Option(s,v)));
  for (const button of byId("tabs").querySelectorAll("button")) button.onclick=()=>selectTab(button.dataset.tab);
  for (const id of ["filters","workbench"]) byId(id).onsubmit=event=>{event.preventDefault();page=0;render();};
  byId("tables").onchange=()=>{selectTable();page=0;render();};
  byId("prev").onclick=()=>{page--;render();};
  byId("next").onclick=()=>{page++;render();};
  byId("close").onclick=closeDetail;
})();

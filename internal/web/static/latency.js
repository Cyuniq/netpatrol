(() => {
  const panel = document.querySelector("#latency-history");
  if (!panel) return;

  const form = panel.querySelector("form");
  const fields = form.elements;
  const chartHost = panel.querySelector(".latency-chart");
  const summary = panel.querySelector("[data-summary]");
  const errorBox = panel.querySelector("[data-error]");
  const isPath = panel.dataset.url.startsWith("/paths/");
  const minute = 60000;
  const maxRange = 30 * 24 * 60 * minute;
  const timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const format = new Intl.DateTimeFormat("zh-CN", {
    month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hourCycle: "h23",
  });
  const colors = ["#00796b", "#3b5fa8"];
  let chart, request, applied;

  function localInput(timestamp) {
    const date = new Date(timestamp);
    return new Date(timestamp - date.getTimezoneOffset() * minute).toISOString().slice(0, 16);
  }

  function showFields() {
    for (const kind of ["relative", "fixed"]) {
      for (const label of form.querySelectorAll(`[data-${kind}]`)) {
        const active = fields.range.value === kind;
        label.hidden = !active;
        label.control.disabled = !active;
        label.control.required = active;
      }
    }
  }

  function pickRange() {
    const choice = fields.range.value;
    const hop = fields.hop.value;
    if (choice === "fixed") {
      const from = new Date(fields.from.value).getTime();
      const to = new Date(fields.to.value).getTime();
      if (!Number.isFinite(from) || !Number.isFinite(to) || from >= to || to - from > maxRange) {
        throw new Error("请选择有效的起止时间，单次查询最长 30 天。");
      }
      return {from, to, hop};
    }
    const minutes = choice === "relative" ? Number(fields.amount.value) * Number(fields.unit.value) : Number(choice);
    if (!Number.isInteger(minutes) || minutes < 1 || minutes * minute > maxRange) {
      throw new Error("最近时长须在 1 分钟到 30 天之间。");
    }
    return {minutes, hop};
  }

  function hopOptions(hops, selected) {
    if (!isPath) return;
    fields.hop.replaceChildren(new Option("目标端", ""));
    for (const hop of hops) {
      const name = `第 ${hop.number} 跳`;
      fields.hop.add(hop.address
        ? new Option(`${name} · ${hop.address}`, `${hop.number}@${hop.address}`)
        : new Option(`${name} · 全部回应地址`, String(hop.number)));
    }
    if (selected && !Array.from(fields.hop.options).some(option => option.value === selected)) {
      const [hop, address] = selected.split("@");
      fields.hop.add(new Option(`第 ${hop} 跳${address ? ` · ${address}` : ""}（本时段未出现）`, selected));
    }
    fields.hop.value = selected;
  }

  function quality(point, target) {
    if (!point) return null;
    if (target) {
      const observed = point.checks - (point.localChecks || 0);
      return observed > 0 ? (point.unreached || 0) * 100 / observed : null;
    }
    const failed = (point.timeouts || 0) + (point.failures || 0);
    const observed = failed + point.replies + (point.otherReplies || 0);
    return observed > 0 ? failed * 100 / observed : null;
  }

  function describe(point, target) {
    if (!point) return "未采样";
    const latency = point.rtt
      ? `平均 ${point.rtt[1].toFixed(2)} ms（${point.rtt[0].toFixed(2)}–${point.rtt[2].toFixed(2)} ms）`
      : "无延迟样本";
    const details = [`${point.replies} 次回应`];
    if (point.otherReplies) details.push(`其他地址回应 ${point.otherReplies}`);
    if (point.timeouts) details.push(`超时 ${point.timeouts}`);
    if (point.failures) details.push(`失败 ${point.failures}`);
    if (point.errors) details.push(`本地异常 ${point.errors}`);
    if (target) {
      details.push(`检测 ${point.checks} 轮，未到达 ${point.unreached || 0} 轮`);
      if (point.localChecks) details.push(`本地异常中断 ${point.localChecks} 轮`);
    }
    return `${latency}\n${details.join(" · ")}`;
  }

  function draw(data, selected, resetZoom) {
    const populated = data.series.some(series => series.points.length > 0);
    const resolution = data.step >= 3600 ? `${data.step / 3600} 小时` : `${data.step / 60} 分钟`;
    const span = `${format.format(data.from)} 至 ${format.format(data.to)} · ${timeZone}`;
    summary.textContent = populated
      ? `${span} · 每点 ${resolution} · 折线为平均值，阴影为最小至最大值`
      : `${span} · 此时段没有延迟历史`;
    chartHost.hidden = !populated;
    if (!populated) {
      chart?.clear();
      return;
    }

    const step = data.step * 1000;
    const buckets = [];
    for (let at = Math.floor(data.from / step) * step; at < data.to; at += step) buckets.push(at);
    const target = isPath && !selected;
    const rateName = target ? "目标未到达率" : isPath ? "跳点未回应率" : "探测失败率";
    const byTime = data.series.map(item => new Map(item.points.map(point => [point.at, point])));
    const series = [];
    data.series.forEach(({name}, index) => {
      const points = buckets.map(at => byTime[index].get(at));
      const color = colors[index];
      const line = {type: "line", xAxisIndex: 0, yAxisIndex: 0, connectNulls: false};
      const bounds = {symbol: "none", stack: `bounds-${index}`, silent: true, lineStyle: {opacity: 0}};
      series.push(
        {...line, ...bounds, data: points.map(point => point?.rtt?.[0] ?? null), areaStyle: {opacity: 0}},
        {...line, ...bounds, data: points.map(point => point?.rtt ? point.rtt[2] - point.rtt[0] : null),
          areaStyle: {color, opacity: 0.17}},
        {...line, name, data: points.map(point => point?.rtt?.[1] ?? null), symbolSize: 4,
          lineStyle: {width: 2, color}, itemStyle: {color}},
        {type: "bar", name, xAxisIndex: 1, yAxisIndex: 1, barMaxWidth: 9,
          data: points.map(point => quality(point, target)), itemStyle: {color, opacity: 0.7}},
      );
    });
    const rangeAxis = {
      type: "category", data: buckets, boundaryGap: true,
      axisLabel: {hideOverlap: true, formatter: value => format.format(Number(value))},
    };
    chart ??= echarts.init(chartHost);
    const zoom = resetZoom ? undefined : chart.getOption()?.dataZoom?.[0];
    chart.setOption({
      animation: false,
      legend: {top: 0, data: data.series.map(item => item.name), selectedMode: false},
      grid: [
        {left: 56, right: 20, top: 60, bottom: 171},
        {left: 56, right: 20, bottom: 72, height: 59},
      ],
      xAxis: [
        {...rangeAxis, gridIndex: 0, axisLabel: {show: false}},
        {...rangeAxis, gridIndex: 1},
      ],
      yAxis: [
        {gridIndex: 0, type: "value", name: "延迟（ms）", min: 0},
        {gridIndex: 1, type: "value", name: `${rateName}（%）`, min: 0, max: 100, interval: 50},
      ],
      axisPointer: {link: [{xAxisIndex: [0, 1]}]},
      tooltip: {
        trigger: "axis", renderMode: "richText", confine: true,
        formatter: items => {
          if (!items.length) return "";
          const at = Number(items[0].axisValue);
          const lines = [`${format.format(at)}  ·  ${resolution}`];
          data.series.forEach(({name}, index) => {
            const point = byTime[index].get(at);
            if (point?.tcpPorts?.length) {
              for (const port of point.tcpPorts) {
                lines.push(`TCP 端口 ${port.port}  ${describe(port, false)}`);
                const rate = quality(port, false);
                if (rate !== null) lines.push(`${rateName} ${rate.toFixed(1)}%`);
              }
              return;
            }
            const rate = quality(point, target);
            lines.push(`${name}  ${describe(point, target)}`);
            if (rate !== null) lines.push(`${rateName} ${rate.toFixed(1)}%`);
          });
          return lines.join("\n");
        },
      },
      dataZoom: [
        {type: "slider", xAxisIndex: [0, 1], height: 19, bottom: 12, filterMode: "none",
          start: zoom?.start ?? 0, end: zoom?.end ?? 100,
          labelFormatter: (_value, label) => format.format(Number(label))},
        {type: "inside", xAxisIndex: [0, 1], filterMode: "none",
          zoomOnMouseWheel: "ctrl", moveOnMouseWheel: false, moveOnMouseMove: true},
      ],
      series,
    }, {notMerge: true});
    chart.resize();
  }

  function showError(message) {
    errorBox.textContent = message;
    errorBox.hidden = false;
  }

  async function load(range, resetZoom) {
    request?.abort();
    const controller = new AbortController();
    request = controller;
    const query = range.minutes
      ? new URLSearchParams({minutes: range.minutes})
      : new URLSearchParams({from: new Date(range.from).toISOString(), to: new Date(range.to).toISOString()});
    const [hop, responder] = range.hop.split("@");
    if (hop) query.set("hop", hop);
    if (responder) query.set("responder", responder);
    panel.setAttribute("aria-busy", "true");
    errorBox.hidden = true;
    if (resetZoom) {
      summary.textContent = "正在查询延迟历史…";
      chartHost.hidden = true;
    }
    try {
      const response = await fetch(`${panel.dataset.url}?${query}`, {signal: controller.signal});
      if (response.status === 400) throw new Error((await response.text()).trim());
      if (!response.ok) throw new Error("无法读取延迟历史，请重试。");
      const data = await response.json();
      if (!controller.signal.aborted) {
        hopOptions(data.hops ?? [], range.hop);
        draw(data, range.hop, resetZoom);
      }
    } catch (error) {
      if (!controller.signal.aborted) {
        if (resetZoom) summary.textContent = "";
        showError(error.message || "无法读取延迟历史，请重试。");
      }
    } finally {
      if (request === controller) {
        request = undefined;
        panel.removeAttribute("aria-busy");
      }
    }
  }

  function apply() {
    try {
      applied = pickRange();
      load(applied, true);
    } catch (error) {
      showError(error.message);
    }
  }

  if (!window.echarts) {
    showError("图表组件加载失败，请刷新页面。");
    return;
  }
  const now = Number(panel.dataset.serverNow);
  fields.from.value = localInput(now - 60 * minute);
  fields.to.value = localInput(now);
  form.querySelector("[data-hop]").hidden = !isPath;
  form.hidden = false;
  showFields();
  form.addEventListener("submit", event => {
    event.preventDefault();
    apply();
  });
  fields.range.addEventListener("change", () => {
    showFields();
    if (fields.range.value !== "fixed" && fields.range.value !== "relative") apply();
  });
  fields.hop.addEventListener("change", () => {
    applied = {...applied, hop: fields.hop.value};
    load(applied, true);
  });
  document.body.addEventListener("path-latency-recorded", () => {
    if (applied?.minutes) load(applied, false);
  });
  setInterval(() => {
    if (applied?.minutes && !document.hidden && !request) load(applied, false);
  }, minute);
  new ResizeObserver(() => chart?.resize()).observe(chartHost);
  apply();
})();

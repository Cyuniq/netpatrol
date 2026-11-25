(() => {
  function drawTrends() {
    for (const canvas of document.querySelectorAll("canvas[data-probe-trend]")) {
      const points = JSON.parse(canvas.dataset.probeTrend);
      const ctx = canvas.getContext("2d");
      if (!ctx) continue;

      const {width, height} = canvas;
      const slot = width / points.length;
      const maximum = Math.max(1, ...points.map(point => point.ms ?? 0));
      ctx.clearRect(0, 0, width, height);
      ctx.strokeStyle = "#00665e";
      ctx.fillStyle = ctx.strokeStyle;
      ctx.lineWidth = 2.8;
      ctx.lineJoin = "round";
      ctx.beginPath();
      points.forEach((point, index) => {
        if (point.ms == null) return;
        const x = (index + 0.5) * slot;
        const y = height - 10 - (height - 16) * point.ms / maximum;
        if (points[index - 1]?.ms == null) ctx.moveTo(x, y);
        else ctx.lineTo(x, y);
        if (points[index - 1]?.ms == null && points[index + 1]?.ms == null) {
          ctx.fillRect(x - 2, y - 2, 4, 4);
        }
      });
      ctx.stroke();

      ctx.fillStyle = "#be2942";
      points.forEach((point, index) => {
        if (point.timeout) {
          const from = Math.round(index * slot);
          const to = Math.round((index + 1) * slot);
          ctx.fillRect(from, height - 4, to - from, 4);
        }
      });
    }
  }

  drawTrends();
  document.addEventListener("htmx:afterSwap", drawTrends);
  document.querySelector("select[name=trend]")?.addEventListener("change", event => {
    event.target.form.requestSubmit();
  });
  document.addEventListener("change", event => {
    if (event.target.matches(".device-page-size")) event.target.form.requestSubmit();
  });
  document.addEventListener("htmx:beforeSwap", event => {
    if (event.detail.target.id === "device-results" &&
        document.activeElement?.closest(".device-pagination")) {
      event.detail.shouldSwap = false;
    }
  });
})();

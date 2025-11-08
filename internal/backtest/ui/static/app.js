const TF_OPTIONS = ["15m", "1h", "4h", "1d", "3d", "7d"];
const PROFILE_OPTIONS = ["one_hour", "four_hour", "one_day"];
let currentRunId = "";

function fillTimeframes(select) {
  TF_OPTIONS.forEach((tf) => {
    const option = document.createElement("option");
    option.value = tf;
    option.textContent = tf;
    select.appendChild(option);
  });
}

function fillProfiles(select) {
  PROFILE_OPTIONS.forEach((profile) => {
    const option = document.createElement("option");
    option.value = profile;
    option.textContent = profile;
    select.appendChild(option);
  });
}

function tsFromInput(value) {
  if (!value) return 0;
  const ms = Date.parse(value);
  return Number.isNaN(ms) ? 0 : ms;
}

function formatTs(ms) {
  if (!ms) return "-";
  const date = new Date(ms);
  return date.toLocaleString();
}

function formatTsValue(val) {
  if (!val) return "-";
  if (typeof val === "number") {
    return formatTs(val);
  }
  const ts = Date.parse(val);
  if (Number.isNaN(ts)) {
    return val;
  }
  return formatTs(ts);
}

function escapeHTML(str = "") {
  return str
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

function previewText(str = "", max = 80) {
  if (!str) return "-";
  return str.length > max ? `${str.slice(0, max)}…` : str;
}

function toLocalInput(date) {
  const offset = date.getTimezoneOffset() * 60000;
  const local = new Date(date.getTime() - offset);
  return local.toISOString().slice(0, 16);
}

async function safeFetch(url, options = {}) {
  const res = await fetch(url, options);
  if (!res.ok) {
    const msg = await res.text();
    throw new Error(msg || res.statusText);
  }
  return res.json();
}

function setMessage(el, text, type = "") {
  el.textContent = text;
  el.className = `message ${type}`;
}

function formatPercent(val) {
  if (typeof val !== "number" || Number.isNaN(val)) return "-";
  return (val * 100).toFixed(2) + "%";
}

function formatNumber(val, digits = 2) {
  if (typeof val !== "number" || Number.isNaN(val)) return "-";
  return val.toFixed(digits);
}

function statusText(run) {
  let text = run.status || "-";
  if (run.message) {
    text += `<br/><small>${run.message}</small>`;
  }
  return text;
}

function updateRunsTable(runs) {
  const body = document.getElementById("runsBody");
  body.innerHTML = "";
  if (!runs.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="6" class="muted">暂无记录</td>`;
    body.appendChild(row);
    return;
  }
  runs.forEach((run) => {
    const row = document.createElement("tr");
    if (run.id === currentRunId) {
      row.classList.add("selected");
    }
    const profit = typeof run.profit === "number" ? formatNumber(run.profit, 2) : "-";
    const winRate =
      typeof run.win_rate === "number"
        ? formatPercent(run.win_rate)
        : run.stats
        ? formatPercent(run.stats.win_rate)
        : "-";
    const updated = run.updated_at ? formatTs(Date.parse(run.updated_at)) : "-";
    row.innerHTML = `
      <td>${run.id.slice(0, 8)}…</td>
      <td>${run.symbol}/${run.profile}</td>
      <td>${statusText(run)}</td>
      <td>${profit}</td>
      <td>${winRate}</td>
      <td>${updated}</td>
    `;
    row.addEventListener("click", () => loadRunDetail(run.id));
    body.appendChild(row);
  });
}

function showRunSummary(run) {
  const summary = document.getElementById("runSummary");
  currentRunId = run.id;
  const stats = run.stats || {};
  summary.innerHTML = `
    <p><strong>ID</strong>: ${run.id}</p>
    <p><strong>交易对</strong>: ${run.symbol} (${run.profile})</p>
    <p><strong>时间</strong>: ${formatTs(run.start_ts)} → ${formatTs(run.end_ts)}</p>
    <p><strong>资金</strong>: 初始 ${formatNumber(run.initial_balance || 0, 2)} · 结束 ${formatNumber(run.final_balance || stats.final_balance || 0, 2)}</p>
    <p><strong>PnL</strong>: ${formatNumber(run.profit ?? stats.profit ?? 0, 2)} (${formatPercent(run.return_pct ?? stats.return_pct ?? 0)})</p>
    <p><strong>胜率</strong>: ${formatPercent(run.win_rate ?? stats.win_rate ?? 0)} · <strong>最大回撤</strong>: ${formatPercent(run.max_drawdown_pct ?? stats.max_drawdown_pct ?? 0)}</p>
    ${run.message ? `<p><strong>进度</strong>: ${run.message}</p>` : ""}
  `;
}

function updatePositionsTable(list) {
  const body = document.getElementById("positionsBody");
  body.innerHTML = "";
  if (!list.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="10" class="muted">暂无持仓记录</td>`;
    body.appendChild(row);
    return;
  }
  list.forEach((pos) => {
    const row = document.createElement("tr");
    const tp = pos.take_profit ? formatNumber(pos.take_profit, 4) : "-";
    const sl = pos.stop_loss ? formatNumber(pos.stop_loss, 4) : "-";
    const rr = pos.expected_rr ? formatNumber(pos.expected_rr, 2) : "-";
    row.innerHTML = `
      <td>${pos.opened_at ? formatTs(Date.parse(pos.opened_at)) : "-"}</td>
      <td>${pos.side}</td>
      <td>${formatNumber(pos.entry_price || 0, 4)}</td>
      <td>${formatNumber(pos.exit_price || 0, 4)}</td>
      <td>${formatNumber(pos.quantity || 0, 4)}</td>
      <td>${formatNumber(pos.pnl || 0, 2)}</td>
      <td>${formatPercent(pos.pnl_pct || 0)}</td>
      <td>${tp}</td>
      <td>${sl}</td>
      <td>${rr}</td>
    `;
    body.appendChild(row);
  });
}

function updateSnapshotsTable(list) {
  const body = document.getElementById("snapshotsBody");
  body.innerHTML = "";
  if (!list.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="5" class="muted">暂无快照</td>`;
    body.appendChild(row);
    return;
  }
  list.slice(-200).forEach((snap) => {
    const row = document.createElement("tr");
    row.innerHTML = `
      <td>${formatTs(snap.ts)}</td>
      <td>${formatNumber(snap.equity || 0, 2)}</td>
      <td>${formatNumber(snap.balance || 0, 2)}</td>
      <td>${formatPercent(snap.drawdown || 0)}</td>
      <td>${formatPercent(snap.exposure || 0)}</td>
    `;
    body.appendChild(row);
  });
}

function summarizeDecisions(decisions = []) {
  const list = Array.isArray(decisions) ? decisions : [];
  if (!list.length) {
    return "无";
  }
  return list
    .map((d) => `${d.symbol || ""} ${d.action || ""}`.trim())
    .join("; ");
}

function renderPromptDetails(log) {
  return `
    <details>
      <summary>查看</summary>
      <div class="prompt-block">
        <strong>System</strong>
        <pre>${escapeHTML(log.system_prompt || "-")}</pre>
      </div>
      <div class="prompt-block">
        <strong>User</strong>
        <pre>${escapeHTML(log.user_prompt || "-")}</pre>
      </div>
      <div class="prompt-block">
        <strong>Raw</strong>
        <pre>${escapeHTML(log.raw_output || log.raw_json || "-")}</pre>
      </div>
    </details>
  `;
}

function formatSymbols(list = []) {
  if (!list || !list.length) return "-";
  return list.join(", ");
}

function updateLogsTable(list) {
  list = Array.isArray(list) ? list : [];
  const body = document.getElementById("logsBody");
  body.innerHTML = "";
  if (!list.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="6" class="muted">暂无 AI 记录</td>`;
    body.appendChild(row);
    return;
  }
  list.forEach((log) => {
    const row = document.createElement("tr");
    const provider = `${log.provider_id || "-"} / ${log.stage || "-"}`;
    const status =
      log.error && log.error.length
        ? `❗ ${escapeHTML(previewText(log.error, 80))}`
        : "OK";
    const meta =
      log.meta_summary && log.meta_summary.length
        ? `<br/><small>${escapeHTML(previewText(log.meta_summary, 120))}</small>`
        : "";
    const note =
      log.note && log.note.length
        ? `<br/><small>${escapeHTML(previewText(log.note, 80))}</small>`
        : "";
    row.innerHTML = `
      <td>${formatTs(log.candle_ts)}</td>
      <td>${log.timeframe}</td>
      <td>${provider}</td>
      <td>${summarizeDecisions(log.decisions)}</td>
      <td>${status}${meta}${note}</td>
      <td>${renderPromptDetails(log)}</td>
    `;
    body.appendChild(row);
  });
}

function updateLiveLogsTable(list) {
  list = Array.isArray(list) ? list : [];
  const body = document.getElementById("liveLogsBody");
  body.innerHTML = "";
  if (!list.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="6" class="muted">暂无实时记录</td>`;
    body.appendChild(row);
    return;
  }
  list.forEach((log) => {
    const row = document.createElement("tr");
    const provider = `${log.provider_id || "-"} / ${log.stage || "-"}`;
    const status =
      log.error && log.error.length
        ? `❗ ${escapeHTML(previewText(log.error, 80))}`
        : "OK";
    const meta =
      log.meta_summary && log.meta_summary.length
        ? `<br/><small>${escapeHTML(previewText(log.meta_summary, 120))}</small>`
        : "";
    row.innerHTML = `
      <td>${formatTs(log.ts)}</td>
      <td>${escapeHTML(formatSymbols(log.symbols))}</td>
      <td>${provider}</td>
      <td>${summarizeDecisions(log.decisions)}</td>
      <td>${status}${meta}</td>
      <td>${renderPromptDetails(log)}</td>
    `;
    body.appendChild(row);
  });
}

function updateLiveOrdersTable(list) {
  list = Array.isArray(list) ? list : [];
  const body = document.getElementById("liveOrdersBody");
  body.innerHTML = "";
  if (!list.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="6" class="muted">暂无记录</td>`;
    body.appendChild(row);
    return;
  }
  list.forEach((order) => {
    const row = document.createElement("tr");
    const notes = [];
    if (typeof order.take_profit === "number" && order.take_profit > 0) {
      notes.push(`TP ${formatNumber(order.take_profit, 4)}`);
    }
    if (typeof order.stop_loss === "number" && order.stop_loss > 0) {
      notes.push(`SL ${formatNumber(order.stop_loss, 4)}`);
    }
    if (order.decision) {
      notes.push("JSON");
    }
    row.innerHTML = `
      <td>${formatTsValue(order.decided_at)}</td>
      <td>${order.symbol || "-"}</td>
      <td>${order.action || "-"}</td>
      <td>${formatNumber(order.price || 0, 4)}</td>
      <td>${formatNumber(order.notional || order.quantity || 0, 2)}</td>
      <td>${notes.join(" · ") || "-"}</td>
    `;
    body.appendChild(row);
  });
}

async function refreshRunsList() {
  try {
    const data = await safeFetch("/api/backtest/runs");
    updateRunsTable(data.runs || []);
  } catch (err) {
    console.error(err);
  }
}

async function refreshLiveDecisions() {
  const msg = document.getElementById("liveLogsMessage");
  if (msg) setMessage(msg, "加载中…");
  try {
    const params = new URLSearchParams();
    const symbol = document.getElementById("liveSymbol").value.trim();
    const provider = document.getElementById("liveProvider").value.trim();
    const stage = document.getElementById("liveStage").value;
    const limit = document.getElementById("liveLimit").value || "100";
    if (symbol) params.set("symbol", symbol);
    if (provider) params.set("provider", provider);
    if (stage) params.set("stage", stage);
    if (limit) params.set("limit", limit);
    const query = params.toString();
    const data = await safeFetch(`/api/live/decisions${query ? `?${query}` : ""}`);
    updateLiveLogsTable(data.logs || []);
    if (msg) setMessage(msg, `已加载 ${data.logs?.length || 0} 条`, "success");
  } catch (err) {
    if (msg) setMessage(msg, err.message, "error");
  }
}

async function refreshLiveOrders() {
  const msg = document.getElementById("liveOrdersMessage");
  if (msg) setMessage(msg, "加载中…");
  try {
    const params = new URLSearchParams();
    const symbol = document.getElementById("liveOrdersSymbol").value.trim();
    const limit = document.getElementById("liveOrdersLimit").value || "100";
    if (symbol) params.set("symbol", symbol);
    if (limit) params.set("limit", limit);
    const data = await safeFetch(`/api/live/orders${params.size ? `?${params.toString()}` : ""}`);
    updateLiveOrdersTable(data.orders || []);
    if (msg) setMessage(msg, `已加载 ${data.orders?.length || 0} 条`, "success");
  } catch (err) {
    if (msg) setMessage(msg, err.message, "error");
  }
}

async function loadRunDetail(runId) {
  try {
    const detail = await safeFetch(`/api/backtest/runs/${runId}`);
    showRunSummary(detail.run);
    const positions = await safeFetch(`/api/backtest/runs/${runId}/positions?limit=200`);
    updatePositionsTable(positions.positions || []);
    const snaps = await safeFetch(`/api/backtest/runs/${runId}/snapshots?limit=400`);
    updateSnapshotsTable(snaps.snapshots || []);
    const logs = await safeFetch(`/api/backtest/runs/${runId}/logs?limit=200`);
    updateLogsTable(logs.logs || []);
    refreshRunsList();
  } catch (err) {
    setMessage(document.getElementById("runMessage"), err.message, "error");
  }
}

function updateJobsTable(jobs) {
  const body = document.getElementById("jobsBody");
  body.innerHTML = "";
  if (!jobs.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="6" class="muted">暂无任务</td>`;
    body.appendChild(row);
    return;
  }
  jobs
    .sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at))
    .forEach((job) => {
      const row = document.createElement("tr");
      const total = job.total || 0;
      const completed = job.completed || 0;
      const percent =
        total > 0 ? ((completed / total) * 100).toFixed(1) + "%" : "-";
      row.innerHTML = `
        <td>${job.id.slice(0, 8)}…</td>
        <td>${job.params.symbol}/${job.params.timeframe}</td>
        <td>${formatTs(job.params.start)} → ${formatTs(job.params.end)}</td>
        <td>${job.status}${job.message ? `<br/><small>${job.message}</small>` : ""}</td>
        <td>${completed}/${total} (${percent})</td>
        <td>${formatTs(new Date(job.updated_at).getTime())}</td>
      `;
      body.appendChild(row);
    });
}

function sanitizeNumber(val) {
  const num = Number(val);
  if (Number.isNaN(num)) {
    return 0;
  }
  return num;
}

function updateCandlesTable(list) {
  const body = document.getElementById("candlesBody");
  body.innerHTML = "";
  if (!list.length) {
    const row = document.createElement("tr");
    row.innerHTML = `<td colspan="6" class="muted">没有数据</td>`;
    body.appendChild(row);
    return;
  }
  list.forEach((candle) => {
    const row = document.createElement("tr");
    const close = sanitizeNumber(candle.close).toFixed(4);
    const high = sanitizeNumber(candle.high).toFixed(4);
    const low = sanitizeNumber(candle.low).toFixed(4);
    const volume = sanitizeNumber(candle.volume).toFixed(2);
    const trades = sanitizeNumber(candle.trades);
    row.innerHTML = `
      <td>${formatTs(candle.open_time)}</td>
      <td>${close}</td>
      <td>${high}</td>
      <td>${low}</td>
      <td>${volume}</td>
      <td>${trades}</td>
    `;
    body.appendChild(row);
  });
}

async function refreshJobs() {
  try {
    const data = await safeFetch("/api/backtest/jobs");
    updateJobsTable(data.jobs || []);
  } catch (err) {
    console.error(err);
  }
}

function init() {
  fillTimeframes(document.getElementById("timeframe"));
  fillTimeframes(document.getElementById("dataTimeframe"));
  fillTimeframes(document.getElementById("executionTf"));
  fillProfiles(document.getElementById("profile"));

  document
    .getElementById("fetchForm")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const msg = document.getElementById("fetchMessage");
      setMessage(msg, "提交中…");
      try {
        const payload = {
          exchange: document.getElementById("exchange").value.trim(),
          symbol: document.getElementById("symbol").value.trim(),
          timeframe: document.getElementById("timeframe").value,
          start_ts: tsFromInput(document.getElementById("start").value),
          end_ts: tsFromInput(document.getElementById("end").value),
        };
        if (!payload.symbol || !payload.start_ts || !payload.end_ts) {
          setMessage(msg, "请填写完整参数", "error");
          return;
        }
        const res = await safeFetch("/api/backtest/fetch", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        setMessage(
          msg,
          `任务 ${res.job.id} 已创建，状态：${res.job.status}`,
          "success"
        );
        refreshJobs();
      } catch (err) {
        setMessage(msg, err.message, "error");
      }
    });

  document
    .getElementById("refreshJobs")
    .addEventListener("click", refreshJobs);

  document
    .getElementById("candlesForm")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const symbol = document.getElementById("dataSymbol").value.trim();
      const timeframe = document.getElementById("dataTimeframe").value.trim();
      if (!symbol || !timeframe) {
        alert("请填写交易对与周期");
        return;
      }
      const params = new URLSearchParams({ symbol, timeframe });
      try {
        const manifestRes = await safeFetch(
          `/api/backtest/data?${params.toString()}`
        );
        const manifest = manifestRes.manifest;
        const info = document.getElementById("manifestInfo");
        if (manifest) {
          info.textContent = `本地共有 ${manifest.rows} 根，时间范围 ${formatTs(
            manifest.min_time
          )} → ${formatTs(manifest.max_time)}`;
        } else {
          info.textContent = "未找到 manifest 信息";
        }
        const res = await safeFetch(
          `/api/backtest/candles/all?${params.toString()}`
        );
        updateCandlesTable(res.candles || []);
      } catch (err) {
        alert(err.message);
      }
    });

  document
    .getElementById("runForm")
    .addEventListener("submit", async (e) => {
      e.preventDefault();
      const msg = document.getElementById("runMessage");
      setMessage(msg, "提交中…");
      try {
        const payload = {
          symbol: document.getElementById("runSymbol").value.trim(),
          profile: document.getElementById("profile").value,
          execution_timeframe: document.getElementById("executionTf").value,
          start_ts: tsFromInput(document.getElementById("runStart").value),
          end_ts: tsFromInput(document.getElementById("runEnd").value),
          initial_balance: Number(document.getElementById("initialBalance").value || 0),
          fee_rate: Number(document.getElementById("feeRate").value || 0),
        };
        if (!payload.symbol || !payload.start_ts || !payload.end_ts) {
          setMessage(msg, "请填写完整参数", "error");
          return;
        }
        const res = await safeFetch("/api/backtest/runs", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        setMessage(msg, `回测 ${res.run.id} 已创建`, "success");
        refreshRunsList();
      } catch (err) {
        setMessage(msg, err.message, "error");
      }
    });

  document
    .getElementById("refreshRunsList")
    .addEventListener("click", refreshRunsList);

  document
    .getElementById("liveFilterForm")
    .addEventListener("submit", (e) => {
      e.preventDefault();
      refreshLiveDecisions();
    });

  document
    .getElementById("refreshLiveLogs")
    .addEventListener("click", refreshLiveDecisions);

  document
    .getElementById("liveOrdersForm")
    .addEventListener("submit", (e) => {
      e.preventDefault();
      refreshLiveOrders();
    });

  document
    .getElementById("refreshLiveOrders")
    .addEventListener("click", refreshLiveOrders);

  // 默认时间：拉取任务 24h、回测任务 7 天
  const now = new Date();
  const dayAgo = new Date(now.getTime() - 24 * 3600 * 1000);
  document.getElementById("end").value = toLocalInput(now);
  document.getElementById("start").value = toLocalInput(dayAgo);

  const weekAgo = new Date(now.getTime() - 7 * 24 * 3600 * 1000);
  document.getElementById("runEnd").value = toLocalInput(now);
  document.getElementById("runStart").value = toLocalInput(weekAgo);

  refreshJobs();
  refreshRunsList();
  refreshLiveDecisions();
  refreshLiveOrders();
}

document.addEventListener("DOMContentLoaded", init);

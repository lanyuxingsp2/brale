(() => {
  const { createApp, ref, reactive, computed, onMounted } = Vue;

  const fetchJSON = async (url, opts = {}) => {
    const res = await fetch(url, opts);
    if (!res.ok) {
      let msg = res.statusText;
      try {
        const body = await res.json();
        msg = body.error || body.message || msg;
      } catch (e) {}
      throw new Error(msg || '请求失败');
    }
    return res.json();
  };

  const formatNumber = (val) => {
    if (val === null || val === undefined || Number.isNaN(val)) return '--';
    if (Math.abs(val) >= 1000) return val.toFixed(2);
    if (Math.abs(val) >= 1) return val.toFixed(4);
    return val.toFixed(6);
  };

  const formatUSD = (val) => {
    if (val === null || val === undefined || Number.isNaN(val)) return '--';
    const prefix = val > 0 ? '+' : '';
    return `${prefix}$${val.toFixed(2)}`;
  };

  const formatPercent = (val) => {
    if (val === null || val === undefined || Number.isNaN(val)) return '--';
    return `${(val * 100).toFixed(2)}%`;
  };

  const normalizeStatus = (status) => (status || '').toString().trim().toLowerCase();

  const statusLabel = (status) => {
    const s = normalizeStatus(status);
    switch (s) {
      case 'closed':
        return '已平仓';
      case 'partial':
      case 'closing_partial':
        return '部分平仓';
      case 'opening':
      case 'closing_full':
        return '待确认';
      case 'open':
      case 'opened':
      case 'active':
        return '进行中';
      default:
        return s ? s.toUpperCase() : 'UNKNOWN';
    }
  };

  const statusBadgeClass = (status) => {
    const s = normalizeStatus(status);
    if (s === 'closed') return 'status-closed';
    if (s === 'partial' || s === 'closing_partial') return 'status-partial';
    if (s === 'opening' || s === 'closing_full' || s === 'pending') return 'status-pending';
    return 'status-open';
  };

  const formatDuration = (ms) => {
    if (!ms || Number.isNaN(ms)) return '--';
    const d = Number(ms);
    const totalMinutes = Math.floor(d / 60000);
    const hours = Math.floor(totalMinutes / 60);
    const minutes = totalMinutes % 60;
    if (hours >= 24) {
      const days = Math.floor(hours / 24);
      const h = hours % 24;
      return `${days}天${h}时`;
    }
    if (hours > 0) return `${hours}时${minutes}分`;
    return `${minutes}分`;
  };

  const formatDateTime = (ts) => {
    const d = parseTs(ts);
    if (!d) return '--';
    return d.toLocaleString('zh-CN', { hour12: false });
  };

  const formatTime = (ts) => {
    const d = parseTs(ts);
    if (!d) return '--';
    return d.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false });
  };

  const formatDate = (ts) => {
    const d = parseTs(ts);
    if (!d) return '--';
    return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric' });
  };

  const parseTs = (ts) => {
    if (!ts) return null;
    if (typeof ts === 'string') {
      const parsed = Date.parse(ts);
      if (!Number.isNaN(parsed)) return new Date(parsed);
    }
    if (typeof ts === 'number') {
      const ms = ts > 1e12 ? ts : ts * 1000;
      return new Date(ms);
    }
    if (ts instanceof Date) return ts;
    return null;
  };

  const badgeClass = (action) => {
    const a = (action || '').toLowerCase();
    if (a.includes('long')) return 'badge-green';
    if (a.includes('short')) return 'badge-red';
    return 'badge-neutral';
  };

  const pickAction = (log, trace) => {
    if (log && Array.isArray(log.decisions) && log.decisions.length) {
      const act = log.decisions[0].action || 'hold';
      return act.toUpperCase();
    }
    const steps = trace?.steps || [];
    for (let i = steps.length - 1; i >= 0; i -= 1) {
      const step = steps[i];
      if (step.decisions && step.decisions.length) {
        return (step.decisions[0].action || 'hold').toUpperCase();
      }
    }
    return 'HOLD';
  };

  const formatAction = (action) => {
    if (!action) return 'HOLD';
    return action.replace(/_/g, ' ').toUpperCase();
  };

  const operationLabel = (op) => {
    const map = {
      1: '开仓',
      2: 'Tier1 触发',
      3: 'Tier2 触发',
      4: 'Tier3 触发',
      5: '止盈触发',
      6: '止损触发',
      7: '调整止盈/止损',
      8: '更新分段',
      10: '执行失败',
    };
    const key = Number(op);
    return map[key] || `OP-${op || '未知'}`;
  };

  const pickReason = (log, trace) => {
    const steps = trace?.steps || [];
    const last = steps[steps.length - 1];
    if (last?.meta_summary) return last.meta_summary;
    if (log?.meta_summary) return log.meta_summary;
    if (log?.meta) return log.meta;
    const dec = log?.decisions && log.decisions[0];
    if (dec?.reasoning) return dec.reasoning;
    if (last?.raw_output) return last.raw_output;
    return '—';
  };

  const decodeMultiline = (text) => {
    if (!text) return '';
    return text.replace(/\\n/g, '\n');
  };

  const providerCount = (trace) => {
    if (!trace || !trace.steps) return 0;
    const set = new Set();
    trace.steps.forEach((s) => {
      if (s.provider_id) set.add(s.provider_id);
    });
    return set.size || trace.steps.length || 0;
  };

  const normalizeDecisionCard = (log, trace) => {
    const ts = log.ts || log.timestamp;
    if (ts && !log.ts) {
      log.ts = ts;
    }
    const action = pickAction(log, trace);
    return {
      log,
      trace,
      action,
      actionLabel: formatAction(action),
      summary: decodeMultiline(pickReason(log, trace)),
      providerCount: providerCount(trace),
      stepCount: trace?.steps?.length || 0,
      hasError: !!(log?.error || (trace?.steps || []).some((s) => s.error)),
    };
  };

  const normalizePositionDetail = (p) => {
    if (!p) return p;
    const normLogs = (p.tier_logs || p.TierLogs || []).map((log) => ({
      timestamp: log.timestamp || log.Timestamp,
      field: log.field || log.Field,
      old_value: log.old_value || log.OldValue,
      new_value: log.new_value || log.NewValue,
      reason: log.reason || log.Reason,
    }));
    const normEvents = (p.events || p.Events || []).map((ev) => ({
      timestamp: ev.timestamp || ev.Timestamp,
      operation: ev.operation || ev.Operation,
      details: ev.details || ev.Details || {},
    }));
    return {
      ...p,
      tier_logs: normLogs,
      events: normEvents,
    };
  };

  const app = createApp({
    template: '#app-template',
    setup() {
      const init = window.__APP_INIT__ || {};
      const view = ref(init.view || 'desk');
      const navCollapsed = ref(false);
      const defaultSymbols = ref(init.defaultSymbols || []);

      const desk = reactive({ decisions: [], positions: [], loading: false, error: '' });
      const decisions = reactive({
        items: [],
        page: 1,
        pageSize: 12,
        total: 0,
        symbols: init.selectedSymbols || [],
        loading: false,
        error: '',
        providers: [],
        providersLoading: false,
        providersError: '',
      });
      const positions = reactive({
        items: [],
        page: 1,
        pageSize: 12,
        total: 0,
        symbol: init.symbol || '',
        loading: false,
        error: '',
      });
      const logs = reactive({
        name: 'app',
        lines: [],
        available: [],
        loading: false,
        error: '',
      });
      const decisionDetail = reactive({ id: init.decisionId || null, data: null, loading: false, error: '' });
      const decisionFilters = reactive({ stage: 'all', provider: '' });
      const positionDetail = reactive({ tradeId: init.tradeId || null, data: null, loading: false, error: '' });
      const tierForm = reactive({
        stop_loss: '',
        take_profit: '',
        tier1_target: '',
        tier1_ratio: '',
        tier2_target: '',
        tier2_ratio: '',
        tier3_target: '',
        tier3_ratio: '',
        reason: '',
      });
      const symbolFallback =
        defaultSymbols.value && defaultSymbols.value.length ? defaultSymbols.value[0] : '';
      const manualOpen = reactive({
        symbol: init.symbol || symbolFallback,
        side: 'short',
        leverage: 5,
        position_size_usd: 100,
        stop_loss: '',
        take_profit: '',
        tier1_target: '',
        tier1_ratio: 0.33,
        tier2_target: '',
        tier2_ratio: 0.33,
        tier3_target: '',
        tier3_ratio: 0.34,
        reason: '',
        price: { last: 0, high: 0, low: 0 },
        loadingPrice: false,
        submitting: false,
      });
      const toast = reactive({ message: '', type: 'info' });

      const loadingAny = computed(
        () =>
          desk.loading ||
          decisions.loading ||
          positions.loading ||
          decisionDetail.loading ||
          positionDetail.loading ||
          logs.loading ||
          manualOpen.submitting ||
          manualOpen.loadingPrice,
      );

      const decisionTotalPages = computed(() => Math.max(1, Math.ceil(decisions.total / decisions.pageSize)));
      const positionTotalPages = computed(() => Math.max(1, Math.ceil(positions.total / positions.pageSize)));

      const decisionProviders = computed(() => {
        const steps = decisionDetail.data?.trace?.steps || [];
        const set = new Set();
        steps.forEach((s) => {
          if (s.provider_id) set.add(s.provider_id);
        });
        return Array.from(set);
      });

      const manualTierSum = computed(() => {
        const r1 = Number(manualOpen.tier1_ratio) || 0;
        const r2 = Number(manualOpen.tier2_ratio) || 0;
        const r3 = Number(manualOpen.tier3_ratio) || 0;
        return r1 + r2 + r3;
      });

      const filteredDecisionSteps = computed(() => {
        const steps = decisionDetail.data?.trace?.steps || [];
        const stageFilter = (decisionFilters.stage || 'all').toLowerCase();
        const provider = (decisionFilters.provider || '').trim();
        return steps.filter((step) => {
          const stage = (step.stage || '').toLowerCase();
          let stageOK = true;
          if (stageFilter === 'provider') stageOK = stage === 'provider';
          else if (stageFilter === 'agent') stageOK = stage.includes('agent');
          else if (stageFilter === 'final') stageOK = stage === 'final';
          else if (stageFilter === 'other') stageOK = !(stage === 'provider' || stage.includes('agent') || stage === 'final');
          if (!stageOK) return false;
          if (provider && step.provider_id !== provider) return false;
          return true;
        });
      });

      const headline = computed(() => {
        switch (view.value) {
          case 'decisions':
            return { eyebrow: 'Blue Book', title: '决策档案', desc: '多 Agent 思维链' };
          case 'positions':
            return { eyebrow: 'Red Book', title: '仓位 Command Center', desc: '实盘持仓 / 止盈止损 / tiers' };
          case 'manualOpen':
            return { eyebrow: 'Manual', title: '手动开仓', desc: '跳过 AI 审查，直接推送 Freqtrade' };
          case 'decisionDetail':
            return { eyebrow: 'Decision Trace', title: `决策 #${decisionDetail.id || ''}`, desc: '完整的 prompt + 输出' };
          case 'positionDetail':
            return { eyebrow: 'Position', title: `Trade #${positionDetail.tradeId || ''}`, desc: '止盈止损 + tiers + 日志' };
          case 'logs':
            return { eyebrow: 'Logs', title: '实时日志', desc: '读取后端 log 文件尾部' };
          default:
            return { eyebrow: 'Desk', title: 'Live Desk', desc: '最近决策 + 仓位快照' };
        }
      });

      const showToast = (message, type = 'info') => {
        toast.message = message;
        toast.type = type;
        setTimeout(() => {
          toast.message = '';
        }, 3200);
      };

      const ensureManualSymbol = () => {
        if (!manualOpen.symbol && defaultSymbols.value && defaultSymbols.value.length) {
          manualOpen.symbol = defaultSymbols.value[0];
        }
      };

      const loadDesk = async () => {
        desk.loading = true;
        desk.error = '';
        try {
          const [decRes, posRes] = await Promise.all([
            fetchJSON('/api/live/decisions?limit=6&stage=final'),
            fetchJSON('/api/live/freqtrade/positions?limit=6&include_logs=0'),
          ]);
          const traceMap = Object.fromEntries((decRes.traces || []).map((t) => [t.trace_id, t]));
          desk.decisions = (decRes.logs || []).map((log) => normalizeDecisionCard(log, traceMap[log.trace_id]));
          desk.positions = posRes.positions || [];
        } catch (e) {
          desk.error = e.message;
          showToast(e.message, 'error');
        } finally {
          desk.loading = false;
        }
      };

      const buildDecisionParams = () => {
        const params = new URLSearchParams();
        params.set('page', decisions.page);
        params.set('page_size', decisions.pageSize);
        params.set('stage', 'final');
        decisions.symbols.forEach((s) => params.append('symbol', s));
        return params.toString();
      };

      const buildProviderParams = () => {
        const params = new URLSearchParams();
        params.set('limit', '9');
        params.set('stage', 'provider');
        decisions.symbols.forEach((s) => params.append('symbol', s));
        return params.toString();
      };

      const loadDecisions = async (force = false) => {
        if (!force && decisions.items.length && view.value !== 'decisions') return;
        decisions.loading = true;
        decisions.error = '';
        try {
          const res = await fetchJSON(`/api/live/decisions?${buildDecisionParams()}`);
          const traceMap = Object.fromEntries((res.traces || []).map((t) => [t.trace_id, t]));
          decisions.items = (res.logs || []).map((log) => normalizeDecisionCard(log, traceMap[log.trace_id]));
          decisions.total = res.total_count || 0;
        } catch (e) {
          decisions.error = e.message;
          showToast(e.message, 'error');
        } finally {
          decisions.loading = false;
        }
      };

      const normalizeProviderOutput = (log) => {
        const ts = log.ts || log.timestamp;
        const provider = (log.provider_id || '').trim() || 'provider';
        const dec = (log.decisions && log.decisions[0]) || {};
        const action = dec.action || 'hold';
        const symbols = (log.symbols && log.symbols.length ? log.symbols : dec.symbol ? [dec.symbol] : []) || [];
        let summary = decodeMultiline(dec.reasoning || log.meta || log.raw_output || '').trim();
        if (!summary) summary = '（无输出）';
        const symbolLabel = symbols.length ? symbols.join(' / ') : '多标的';
        return {
          id: log.id,
          ts,
          provider,
          action,
          actionLabel: formatAction(action),
          symbols,
          symbolLabel,
          summary,
        };
      };

      const loadProviderOutputs = async (force = false) => {
        if (!force && decisions.providers.length && view.value !== 'decisions') return;
        decisions.providersLoading = true;
        decisions.providersError = '';
        try {
          const res = await fetchJSON(`/api/live/decisions?${buildProviderParams()}`);
          decisions.providers = (res.logs || []).map((log) => normalizeProviderOutput(log));
        } catch (e) {
          decisions.providersError = e.message;
          showToast(e.message, 'error');
        } finally {
          decisions.providersLoading = false;
        }
      };

      const switchView = (next) => {
        view.value = next;
        if (next === 'desk') loadDesk();
        if (next === 'decisions') {
          loadDecisions();
          loadProviderOutputs();
        }
        if (next === 'positions') loadPositions();
        if (next === 'manualOpen') {
          ensureManualSymbol();
          if (manualOpen.symbol) loadManualPrice();
        }
        if (next === 'decisionDetail' && decisionDetail.id) loadDecisionDetail(decisionDetail.id);
        if (next === 'positionDetail' && positionDetail.tradeId) loadPositionDetail(positionDetail.tradeId);
        if (next === 'logs') loadLogs();
      };

      const refreshCurrent = () => switchView(view.value);

      const changeDecisionPage = (delta) => {
        const next = decisions.page + delta;
        if (next < 1 || next > decisionTotalPages.value) return;
        decisions.page = next;
        loadDecisions(true);
      };

      const toggleSymbol = (sym) => {
        const idx = decisions.symbols.indexOf(sym);
        if (idx >= 0) {
          decisions.symbols.splice(idx, 1);
        } else {
          decisions.symbols.push(sym);
        }
        decisions.page = 1;
        loadDecisions(true);
        loadProviderOutputs(true);
      };

      const clearSymbols = () => {
        decisions.symbols = [];
        decisions.page = 1;
        loadDecisions(true);
        loadProviderOutputs(true);
      };

      const openDecision = (id) => {
        decisionDetail.id = id;
        view.value = 'decisionDetail';
        loadDecisionDetail(id);
      };

      const decisionHeadline = (log, trace) => formatAction(pickAction(log, trace));
      const decisionHeadlineAction = (log, trace) => pickAction(log, trace);

      const loadDecisionDetail = async (id) => {
        decisionDetail.loading = true;
        decisionDetail.error = '';
        decisionDetail.data = null;
        decisionFilters.stage = 'all';
        decisionFilters.provider = '';
        try {
          const res = await fetchJSON(`/api/live/decisions/${id}`);
          decisionDetail.data = res;
        } catch (e) {
          decisionDetail.error = e.message;
          showToast(e.message, 'error');
        } finally {
          decisionDetail.loading = false;
        }
      };

      const loadPositions = async (resetPage = false) => {
        if (resetPage) positions.page = 1;
        positions.loading = true;
        positions.error = '';
        try {
          const params = new URLSearchParams();
          params.set('page', positions.page);
          params.set('page_size', positions.pageSize);
          params.set('include_logs', '0');
          if (positions.symbol) params.set('symbol', positions.symbol.trim());
          const res = await fetchJSON(`/api/live/freqtrade/positions?${params.toString()}`);
          positions.items = res.positions || [];
          positions.total = res.total_count || 0;
        } catch (e) {
          positions.error = e.message;
          showToast(e.message, 'error');
        } finally {
          positions.loading = false;
        }
      };

      const changePositionPage = (delta) => {
        const next = positions.page + delta;
        if (next < 1 || next > positionTotalPages.value) return;
        positions.page = next;
        loadPositions();
      };

      const openPosition = (tradeId) => {
        positionDetail.tradeId = tradeId;
        view.value = 'positionDetail';
        loadPositionDetail(tradeId);
      };

      const loadPositionDetail = async (tradeId) => {
        positionDetail.loading = true;
        positionDetail.error = '';
        positionDetail.data = null;
        try {
          const res = await fetchJSON(`/api/live/freqtrade/positions/${tradeId}?logs_limit=120`);
          positionDetail.data = normalizePositionDetail(res.position);
          const d = positionDetail.data || {};
          tierForm.stop_loss = d.stop_loss ?? '';
          tierForm.take_profit = d.take_profit ?? '';
          tierForm.tier1_target = d.tier1?.target ?? '';
          tierForm.tier1_ratio = d.tier1?.ratio ?? '';
          tierForm.tier2_target = d.tier2?.target ?? '';
          tierForm.tier2_ratio = d.tier2?.ratio ?? '';
          tierForm.tier3_target = d.tier3?.target ?? '';
          tierForm.tier3_ratio = d.tier3?.ratio ?? '';
        } catch (e) {
          positionDetail.error = e.message;
          showToast(e.message, 'error');
        } finally {
          positionDetail.loading = false;
        }
      };

      const loadLogs = async (force = false) => {
        if (!force && logs.lines.length && view.value !== 'logs') return;
        logs.loading = true;
        logs.error = '';
        try {
          const params = new URLSearchParams();
          if (logs.name) params.set('name', logs.name);
          params.set('limit', '400');
          const res = await fetchJSON(`/api/live/logs?${params.toString()}`);
          logs.name = res.name || logs.name;
          logs.lines = res.lines || [];
          const av = res.available || [];
          logs.available = av.length ? av : logs.available.length ? logs.available : [logs.name].filter(Boolean);
        } catch (e) {
          logs.error = e.message;
          showToast(e.message, 'error');
        } finally {
          logs.loading = false;
        }
      };

      const loadManualPrice = async () => {
        const symbol = (manualOpen.symbol || '').trim();
        if (!symbol) {
          showToast('请选择交易对', 'error');
          return;
        }
        manualOpen.loadingPrice = true;
        try {
          const res = await fetchJSON(`/api/live/freqtrade/price?symbol=${encodeURIComponent(symbol)}`);
          manualOpen.price.last = res.last || 0;
          manualOpen.price.high = res.high || 0;
          manualOpen.price.low = res.low || 0;
        } catch (e) {
          showToast(e.message, 'error');
        } finally {
          manualOpen.loadingPrice = false;
        }
      };

      const tierRows = (pos) => {
        if (!pos) return [];
        const t1 = pos.tier1 || {};
        const t2 = pos.tier2 || {};
        const t3 = pos.tier3 || {};
        return [
          { name: 'Tier1', target: formatNumber(t1.target), ratio: formatPercent(t1.ratio), done: !!t1.done },
          { name: 'Tier2', target: formatNumber(t2.target), ratio: formatPercent(t2.ratio), done: !!t2.done },
          { name: 'Tier3', target: formatNumber(t3.target), ratio: formatPercent(t3.ratio), done: !!t3.done },
        ];
      };

      const tierFieldLabelCN = (f) => {
        switch (f) {
          case 1:
            return '止盈价格';
          case 2:
            return '止损价格';
          case 3:
            return '一阶价格';
          case 4:
            return '二阶价格';
          case 5:
            return '三阶价格';
          case 6:
            return '一阶比例';
          case 7:
            return '二阶比例';
          case 8:
            return '三阶比例';
          default:
            return `字段-${f}`;
        }
      };

      const updateTierSettings = async () => {
        if (!positionDetail.data) return;
        const payload = {
          trade_id: positionDetail.tradeId,
          symbol: positionDetail.data.symbol,
          side: positionDetail.data.side,
          stop_loss: Number(tierForm.stop_loss) || 0,
          take_profit: Number(tierForm.take_profit) || 0,
          tier1_target: Number(tierForm.tier1_target) || 0,
          tier1_ratio: Number(tierForm.tier1_ratio) || 0,
          tier2_target: Number(tierForm.tier2_target) || 0,
          tier2_ratio: Number(tierForm.tier2_ratio) || 0,
          tier3_target: Number(tierForm.tier3_target) || 0,
          tier3_ratio: Number(tierForm.tier3_ratio) || 0,
          reason: tierForm.reason || '前端手动修改',
        };
        try {
          await fetchJSON('/api/live/freqtrade/tiers', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
          });
          showToast('已提交修改');
          loadPositionDetail(positionDetail.tradeId);
        } catch (e) {
          showToast(e.message, 'error');
        }
      };

      const forceClosePosition = async () => {
        if (!positionDetail.data) return;
        try {
          await fetchJSON('/api/live/freqtrade/close', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              symbol: positionDetail.data.symbol,
              side: positionDetail.data.side,
              close_ratio: 1,
            }),
          });
          showToast('已发起全平');
          loadPositionDetail(positionDetail.tradeId);
        } catch (e) {
          showToast(e.message, 'error');
        }
      };

      const submitManualOpen = async () => {
        const symbol = (manualOpen.symbol || '').trim().toUpperCase();
        if (!symbol) {
          showToast('请选择交易对', 'error');
          return;
        }
        if (Math.abs(manualTierSum.value - 1) > 1e-3) {
          showToast('Tier 比例需等于 100%', 'error');
          return;
        }
        manualOpen.submitting = true;
        try {
          const payload = {
            symbol,
            side: manualOpen.side,
            leverage: Number(manualOpen.leverage) || 0,
            position_size_usd: Number(manualOpen.position_size_usd) || 0,
            stop_loss: Number(manualOpen.stop_loss) || 0,
            take_profit: Number(manualOpen.take_profit) || 0,
            tier1_target: Number(manualOpen.tier1_target) || 0,
            tier1_ratio: Number(manualOpen.tier1_ratio) || 0,
            tier2_target: Number(manualOpen.tier2_target) || 0,
            tier2_ratio: Number(manualOpen.tier2_ratio) || 0,
            tier3_target: Number(manualOpen.tier3_target) || 0,
            tier3_ratio: Number(manualOpen.tier3_ratio) || 0,
            reason: manualOpen.reason || '手动信号',
          };
          await fetchJSON('/api/live/freqtrade/manual-open', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
          });
          showToast('已提交手动开仓');
          loadPositions(true);
        } catch (e) {
          showToast(e.message, 'error');
        } finally {
          manualOpen.submitting = false;
        }
      };

      onMounted(() => {
        switchView(view.value);
      });

      return {
        navCollapsed,
        view,
        headline,
        desk,
        decisions,
        positions,
        decisionDetail,
        decisionFilters,
        decisionProviders,
        filteredDecisionSteps,
        positionDetail,
        logs,
        manualOpen,
        defaultSymbols,
        toast,
        loadingAny,
        decisionTotalPages,
        positionTotalPages,
        switchView,
        refreshCurrent,
        openDecision,
        openPosition,
        changeDecisionPage,
        changePositionPage,
        toggleSymbol,
        clearSymbols,
        loadPositions,
        badgeClass,
        formatTime,
        formatDate,
        formatDateTime,
        formatNumber,
        formatUSD,
        formatPercent,
        statusLabel,
        statusBadgeClass,
        tierRows,
        decisionHeadline,
        decisionHeadlineAction,
        tierFieldLabelCN,
        tierForm,
        updateTierSettings,
        forceClosePosition,
        manualTierSum,
        loadManualPrice,
        submitManualOpen,
        formatDuration,
        setStageFilter: (val) => {
          decisionFilters.stage = val;
        },
        toggleNav: () => {
          navCollapsed.value = !navCollapsed.value;
        },
        operationLabel,
        loadLogs,
      };
    },
  });

  app.config.compilerOptions.delimiters = ['[[', ']]'];
  app.mount('#app');
})();

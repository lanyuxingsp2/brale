const App = {
  data() {
    return {
      traces: [],
      positions: [],
      expanded: new Set(),
      activeTab: {},
      page: 1,
      pageSize: 8,
      hasNext: false,
      symbolFilter: '',
      symbolInput: '',
      stageFilter: 'core',
      stageOptions: [
        { value: 'core', label: '核心阶段 (Agent + Provider)' },
        { value: 'decision', label: '仅最终决策' },
        { value: 'provider', label: '仅模型阶段' },
        { value: 'executor', label: '仅执行器' },
        { value: 'freqtrade', label: '仅 Freqtrade' },
        { value: 'all', label: '全部阶段' },
      ],
      loading: false,
      error: '',
      refreshTimer: null,
      refreshInterval: 30,
      closing: {},
      tierEditors: {},
      tierForms: {},
      savingTier: {},
      tierLogs: {},
      tierLogsLoading: {},
      historyLimit: 5,
    };
  },
  mounted() {
    this.initSymbolFilter();
    this.fetchAll();
    this.setupAutoRefresh();
  },
  watch: {
    refreshInterval() {
      this.setupAutoRefresh();
    },
    stageFilter() {
      this.page = 1;
      this.fetchDecisions();
    },
  },
  computed: {
    openPositionCount() {
      if (!Array.isArray(this.positions)) return 0;
      return this.positions.filter((pos) => !this.isClosed(pos)).length;
    },
    historyCount() {
      if (!Array.isArray(this.positions)) return 0;
      return this.positions.filter((pos) => this.isClosed(pos)).length;
    },
    firstHistoryIndex() {
      if (!Array.isArray(this.positions)) return -1;
      return this.positions.findIndex((pos) => this.isClosed(pos));
    },
  },
  methods: {
    async fetchAll() {
      this.loading = true;
      this.error = '';
      try {
        await Promise.all([this.fetchDecisions(), this.fetchPositions()]);
      } catch (err) {
        console.error(err);
        this.error = err.message || '拉取数据失败';
      } finally {
        this.loading = false;
      }
    },
    async fetchDecisions(options = {}) {
      const { fromPagination = false } = options;
      const previousTraces = [...this.traces];
      const params = new URLSearchParams({
        limit: this.pageSize.toString(),
        offset: Math.max(0, (this.page - 1) * this.pageSize).toString(),
      });
      if (this.symbolFilter) {
        params.set('symbol', this.symbolFilter);
      }
      const stageValue = this.stageQueryParam();
      if (stageValue) {
        params.set('stage', stageValue);
      }
      const res = await fetch(`/api/live/decisions?${params.toString()}`);
      if (!res.ok) throw new Error(await res.text());
      const data = await res.json();
      const traceList = Array.isArray(data.traces) ? data.traces : [];
      const traces = traceList.map((trace) => ({
        ...trace,
        ts: trace.ts || Date.now(),
        steps: Array.isArray(trace.steps) ? trace.steps.map((step) => ({
          ...step,
          ts: step.ts || trace.ts,
          images: step.images || [],
          image_count: typeof step.image_count === 'number'
            ? step.image_count
            : (step.images ? step.images.length : 0),
          vision_supported: Boolean(step.vision_supported),
          decisions: step.decisions || [],
        })) : [],
      }));
      if (fromPagination && this.page > 1 && traces.length === 0) {
        this.page = Math.max(1, this.page - 1);
        this.traces = previousTraces;
        this.hasNext = false;
        return;
      }
      this.traces = traces;
      this.hasNext = traces.length === this.pageSize;
      // 默认 tab 为 first stage
      this.traces.forEach((trace) => {
        const activeKey = this.activeTab[trace.trace_id];
        if (!activeKey) {
          this.activeTab[trace.trace_id] = 'ALL';
          return;
        }
        const hasMatch = trace.steps.some((step) => this.stageKey(step) === activeKey);
        if (!hasMatch && activeKey !== 'ALL') {
          this.activeTab[trace.trace_id] = 'ALL';
        }
      });
    },
    stageQueryParam() {
      switch (this.stageFilter) {
        case 'core':
          return 'core';
        case 'decision':
          return 'final';
        case 'provider':
          return 'provider';
        case 'executor':
          return 'executor';
        case 'freqtrade':
          return 'freqtrade';
        case 'all':
          return 'all';
        default:
          return '';
      }
    },
    async fetchPositions() {
      const params = new URLSearchParams({
        limit: '100',
        history_limit: String(this.historyLimit || 5),
        include_logs: '1',
        logs_limit: '20',
      });
      if (this.symbolFilter) {
        params.set('symbol', this.symbolFilter);
      }
      const res = await fetch(`/api/live/freqtrade/positions?${params.toString()}`);
      if (!res.ok) throw new Error(await res.text());
      const data = await res.json();
      const list = Array.isArray(data.positions) ? data.positions : [];
      const normalized = list.filter((pos) => pos && typeof pos.trade_id === 'number');
      const nextLogs = { ...this.tierLogs };
      normalized.forEach((pos) => {
        if (Array.isArray(pos.tier_logs) && pos.tier_logs.length) {
          nextLogs[pos.trade_id] = pos.tier_logs;
        }
      });
      this.tierLogs = nextLogs;
      this.positions = normalized;
      this.syncTierForms();
    },
    syncTierForms() {
      const next = { ...this.tierForms };
      const active = new Set();
      this.positions.forEach((pos) => {
        const id = pos.trade_id;
        const key = id != null ? id.toString() : '';
        if (!key || this.isClosed(pos)) {
          return;
        }
        active.add(key);
        if (!next[id]) {
          next[id] = this.buildTierForm(pos);
        }
      });
      Object.keys(next).forEach((key) => {
        if (!active.has(key)) {
          delete next[key];
        }
      });
      this.tierForms = next;
      const cleanedEditors = { ...this.tierEditors };
      Object.keys(cleanedEditors).forEach((key) => {
        if (!active.has(key)) {
          delete cleanedEditors[key];
        }
      });
      this.tierEditors = cleanedEditors;
    },
    buildTierForm(pos) {
      return {
        tier1_target: pos?.tier1?.target || pos.entry_price || 0,
        tier1_ratio: pos?.tier1?.ratio || 0.33,
        tier2_target: pos?.tier2?.target || pos.entry_price || 0,
        tier2_ratio: pos?.tier2?.ratio || 0.33,
        tier3_target: pos?.tier3?.target || pos.entry_price || 0,
        tier3_ratio: pos?.tier3?.ratio || 0.34,
        reason: '',
        logs_limit: 20,
      };
    },
    setupAutoRefresh() {
      if (this.refreshTimer) {
        clearInterval(this.refreshTimer);
        this.refreshTimer = null;
      }
      if (this.refreshInterval > 0) {
        this.refreshTimer = setInterval(() => {
          this.fetchAll();
        }, this.refreshInterval * 1000);
      }
    },
    manualRefresh() {
      this.page = 1;
      this.fetchAll();
    },
    initSymbolFilter() {
      const searchParams = new URLSearchParams(window.location.search);
      const symbol = (searchParams.get('symbol') || '').toUpperCase().trim();
      if (symbol) {
        this.symbolFilter = symbol;
        this.symbolInput = symbol;
      }
    },
    applySymbolFilter() {
      const normalized = (this.symbolInput || '').toUpperCase().trim();
      if (this.symbolFilter === normalized) {
        return;
      }
      this.symbolFilter = normalized;
      this.page = 1;
      this.updateQuerySymbol();
      this.fetchAll();
    },
    clearSymbolFilter() {
      if (!this.symbolFilter && !this.symbolInput) {
        return;
      }
      this.symbolFilter = '';
      this.symbolInput = '';
      this.page = 1;
      this.updateQuerySymbol();
      this.fetchAll();
    },
    updateQuerySymbol() {
      const url = new URL(window.location.href);
      if (this.symbolFilter) {
        url.searchParams.set('symbol', this.symbolFilter);
      } else {
        url.searchParams.delete('symbol');
      }
      window.history.replaceState({}, '', url.toString());
    },
    toggleTrace(id) {
      if (this.expanded.has(id)) {
        this.expanded.delete(id);
      } else {
        this.expanded.add(id);
      }
    },
    nextPage() {
      if (this.loading || !this.hasNext) {
        return;
      }
      this.page += 1;
      this.fetchDecisions({ fromPagination: true });
    },
    prevPage() {
      if (this.loading || this.page === 1) {
        return;
      }
      this.page -= 1;
      this.fetchDecisions({ fromPagination: true });
    },
    setActiveTab(traceId, stage) {
      this.activeTab = {
        ...this.activeTab,
        [traceId]: stage,
      };
    },//hello worldn
    filteredSteps(trace) {
      const key = this.activeTab[trace.trace_id];
      if (!key || key === 'ALL') return trace.steps;
      return trace.steps.filter((step) => this.stageKey(step) === key);
    },
    traceStages(trace) {
      const stages = [];
      const seen = new Set();
      trace.steps.forEach((step) => {
        const key = this.stageKey(step);
        if (seen.has(key)) {
          return;
        }
        seen.add(key);
        stages.push({
          key,
          stage: step.stage || '未命名',
          provider: step.provider_id || '',
        });
      });
      return stages;
    },
    formatStageLabel(info) {
      if (!info) return '';
      return info.provider ? `${info.stage} · ${info.provider}` : info.stage;
    },
    stageKey(step) {
      const stage = (step.stage || '未命名').trim();
      const provider = (step.provider_id || '').trim();
      return provider ? `${stage}::${provider}` : stage;
    },
    shouldShowHistoryDivider(idx) {
      return this.firstHistoryIndex !== -1 && idx === this.firstHistoryIndex;
    },
    formatTraceTitle(trace) {
      const symbols = (trace.symbols && trace.symbols.length)
        ? trace.symbols.join(', ')
        : (trace.candidates || []).join(', ');
      return `${trace.trace_id.slice(0, 8)} · ${symbols || '未知'}`;
    },
    formatTs(ts) {
      if (!ts) return '-';
      const date = typeof ts === 'number' ? new Date(ts) : new Date(Number(ts));
      if (Number.isNaN(date.getTime())) return '-';
      return date.toLocaleString();
    },
    formatNumber(num, digits = 2) {
      if (typeof num !== 'number' || Number.isNaN(num)) return '-';
      return num.toFixed(digits);
    },
    formatPercent(num, digits = 2) {
      if (typeof num !== 'number' || Number.isNaN(num)) return '-';
      return (num * 100).toFixed(digits) + '%';
    },
    formatDuration(ms) {
      if (!ms || ms <= 0) return '-';
      const total = Math.floor(ms / 1000);
      const h = Math.floor(total / 3600);
      const m = Math.floor((total % 3600) / 60);
      const s = total % 60;
      const parts = [];
      if (h) parts.push(h + 'h');
      if (m) parts.push(m + 'm');
      if (!h && s) parts.push(s + 's');
      return parts.join(' ') || s + 's';
    },
    summarizeDecisions(decisions) {
      if (!decisions || !decisions.length) return '';
      return decisions
        .map((d) => `${d.symbol || ''} ${d.action || ''}`.trim())
        .join(' · ');
    },
    preview(text, max = 160) {
      if (!text) return '';
      return text.length > max ? text.slice(0, max) + '…' : text;
    },
    isClosing(tradeId) {
      if (!tradeId && tradeId !== 0) return false;
      return Boolean(this.closing[tradeId]);
    },
    isClosed(pos) {
      if (!pos || !pos.status) return false;
      return String(pos.status).toLowerCase() === 'closed';
    },
    setClosing(tradeId, value) {
      if (!tradeId && tradeId !== 0) return;
      const next = { ...this.closing };
      if (value) {
        next[tradeId] = true;
      } else {
        delete next[tradeId];
      }
      this.closing = next;
    },
    isTierEditorOpen(tradeId) {
      return Boolean(this.tierEditors[tradeId]);
    },
    isSavingTier(tradeId) {
      return Boolean(this.savingTier[tradeId]);
    },
    isTierLogsLoading(tradeId) {
      return Boolean(this.tierLogsLoading[tradeId]);
    },
    toggleTierEditor(pos) {
      if (!pos) return;
      const id = pos.trade_id;
      if (!this.tierForms[id]) {
        this.tierForms = { ...this.tierForms, [id]: this.buildTierForm(pos) };
      }
      const next = { ...this.tierEditors };
      next[id] = !next[id];
      this.tierEditors = next;
      if (next[id]) {
        if (Array.isArray(pos.tier_logs) && pos.tier_logs.length) {
          this.tierLogs = { ...this.tierLogs, [id]: pos.tier_logs };
        }
        if (!this.isClosed(pos)) {
          this.fetchTierLogs(pos);
        }
      }
    },
    async saveTierConfig(pos) {
      if (!pos) return;
      if (this.isClosed(pos)) {
        alert('该仓位已平仓，无法调整三段配置。');
        return;
      }
      const id = pos.trade_id;
      const form = this.tierForms[id];
      if (!form) return;
      this.savingTier = { ...this.savingTier, [id]: true };
      try {
        const payload = {
          trade_id: id,
          symbol: pos.symbol,
          side: pos.side,
          tier1_target: Number(form.tier1_target) || 0,
          tier1_ratio: Number(form.tier1_ratio) || 0,
          tier2_target: Number(form.tier2_target) || 0,
          tier2_ratio: Number(form.tier2_ratio) || 0,
          tier3_target: Number(form.tier3_target) || 0,
          tier3_ratio: Number(form.tier3_ratio) || 0,
          reason: form.reason || '手动调整',
        };
        const res = await fetch('/api/live/freqtrade/tiers', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload),
        });
        if (!res.ok) throw new Error(await res.text());
        await this.fetchPositions();
      } catch (err) {
        console.error(err);
        alert(`更新失败：${err.message || err}`);
      } finally {
        const { [id]: removed, ...rest } = this.savingTier;
        this.savingTier = rest;
      }
    },
    async fetchTierLogs(pos) {
      if (!pos) return;
      const id = pos.trade_id;
      const form = this.tierForms[id];
      const limit = form?.logs_limit || 20;
      this.tierLogsLoading = { ...this.tierLogsLoading, [id]: true };
      try {
        const params = new URLSearchParams({ trade_id: id.toString(), limit: limit.toString() });
        const res = await fetch(`/api/live/freqtrade/tier-logs?${params.toString()}`);
        if (!res.ok) throw new Error(await res.text());
        const data = await res.json();
        this.tierLogs = { ...this.tierLogs, [id]: data.logs || [] };
      } catch (err) {
        console.error(err);
        alert(`获取日志失败：${err.message || err}`);
      } finally {
        const { [id]: _, ...rest } = this.tierLogsLoading;
        this.tierLogsLoading = rest;
      }
    },
    formatTierStatus(tier) {
      if (!tier) return '-';
      const target = this.formatNumber(tier.target);
      const ratio = this.formatPercent(tier.ratio);
      const status = tier.done ? '✅ 已完成' : '⏳ 待触发';
      return `${target} (${ratio}) ${status}`;
    },
    formatPositionStatus(pos) {
      if (!pos) return '-';
      if (this.isClosed(pos)) {
        const ts = pos.closed_at ? this.formatTs(pos.closed_at) : '-';
        const reason = pos.exit_reason ? ` · ${pos.exit_reason}` : '';
        return `已平仓 ${ts}${reason}`;
      }
      return '持仓中';
    },
    formatTierLog(log) {
      if (!log) return '-';
      const ts = log.created_at ? this.formatTs(log.created_at) : '-';
      const target = `${this.formatNumber(log.old_target)} → ${this.formatNumber(log.new_target)}`;
      const ratio = `${this.formatPercent(log.old_ratio)} → ${this.formatPercent(log.new_ratio)}`;
      return `${ts} · ${log.tier_name?.toUpperCase() || ''} 价格 ${target} 比例 ${ratio} · ${log.source || ''} ${log.reason || ''}`;
    },
    formatTradeEvent(ev) {
      if (!ev) return '-';
      const ts = ev.created_at ? this.formatTs(ev.created_at) : '-';
      const event = ev.event ? ev.event.toUpperCase() : 'EVENT';
      const parts = [];
      if (ev.detail) parts.push(ev.detail);
      if (typeof ev.stop_loss === 'number' && ev.stop_loss) {
        parts.push(`SL=${this.formatNumber(ev.stop_loss)}`);
      }
      if (typeof ev.take_profit === 'number' && ev.take_profit) {
        parts.push(`TP=${this.formatNumber(ev.take_profit)}`);
      }
      const tierInfo = [ev.tier1_target, ev.tier2_target, ev.tier3_target]
        .map((t) => (typeof t === 'number' && t ? this.formatNumber(t) : ''))
        .filter(Boolean);
      if (tierInfo.length) {
        parts.push(`Tiers=${tierInfo.join('/')}`);
      }
      if (ev.source) parts.push(ev.source);
      return `${ts} · ${event}${parts.length ? ' · ' + parts.join(' · ') : ''}`;
    },
    async quickClose(pos) {
      if (!pos) return;
      if (this.isClosed(pos)) {
        alert('该仓位已平仓');
        return;
      }
      const tradeId = pos.trade_id;
      const symbol = (pos.symbol || '').toUpperCase().trim();
      const side = (pos.side || '').toLowerCase().trim();
      if (!symbol || !side) {
        alert('无法关闭：缺少交易对或方向');
        return;
      }
      if (this.isClosing(tradeId)) {
        return;
      }
      if (!window.confirm(`确认关闭 ${symbol} ${side.toUpperCase()} 仓位？`)) {
        return;
      }
      this.setClosing(tradeId, true);
      try {
        const res = await fetch('/api/live/freqtrade/close', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            trade_id: tradeId,
            symbol,
            side,
          }),
        });
        if (!res.ok) throw new Error(await res.text());
        await this.fetchPositions();
      } catch (err) {
        console.error(err);
        alert(`关闭失败：${err.message || err}`);
      } finally {
        this.setClosing(tradeId, false);
      }
    },
  },
};

Vue.createApp(App).mount('#app');

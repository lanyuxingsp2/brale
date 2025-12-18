(() => {
  const { createApp, ref, reactive, computed, onMounted, onBeforeUnmount, watch } = Vue;

  const fetchJSON = async (url, opts = {}) => {
    const res = await fetch(url, opts);
    if (!res.ok) {
      let msg = res.statusText;
      try {
        const body = await res.json();
        msg = body.error || body.message || msg;
      } catch (e) { }
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

  const normalizeComboKey = (val) => (val || '').toString().trim().toLowerCase();

  const exitComboLabel = (key) => {
    switch (normalizeComboKey(key)) {
      case 'tp_tiers__sl_single':
        return '分段止盈 + 固定止损';
      case 'tp_tiers__sl_tiers':
        return '分段止盈 + 分段止损';
      case 'tp_single__sl_single':
        return '单止盈 + 单止损';
      case 'tp_atr__sl_atr':
        return 'ATR 追踪止盈 + ATR 追踪止损';
      case 'sl_atr__tp_tiers':
        return 'ATR 止损 + 分段止盈';
      case 'tp_atr__sl_tiers':
        return 'ATR 止盈 + 分段止损';
      case 'sl_atr__tp_single':
        return 'ATR 止损 + 单止盈';
      case 'tp_atr__sl_single':
        return 'ATR 止盈 + 单止损';
      default:
        return (key || '').toString().trim() || '未知组合';
    }
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
    if (ms === null || ms === undefined || Number.isNaN(ms)) return '--';
    const d = Number(ms);
    if (!Number.isFinite(d) || d < 0) return '--';
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
    if (a.includes('update')) return 'badge-indigo';
    if (a === 'hold' || a.includes('hold')) return 'badge-amber';
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

  const csvBlockRegex = () => /\[([A-Z0-9_]+)_START\]([\s\S]*?)\[\1_END\]/gi;

  const extractCsvBlocks = (text) => {
    if (!text) return [];
    const regex = csvBlockRegex();
    const blocks = [];
    let match;
    while ((match = regex.exec(text)) !== null) {
      const tag = match[1];
      const payload = (match[2] || '').trim();
      if (!payload) continue;
      const lines = payload
        .split(/\r?\n/)
        .map((ln) => ln.trim())
        .filter((ln) => ln.length);
      if (!lines.length) continue;
      const headers = lines[0].split(',').map((h) => h.trim());
      const rows = lines.slice(1).map((line) => line.split(',').map((cell) => cell.trim()));
      blocks.push({
        tag,
        label: tag.replace(/^DATA_?/, '') || tag,
        headers,
        rows,
      });
    }
    return blocks;
  };

  const stripCsvBlocks = (text) => {
    if (!text) return '';
    const regex = csvBlockRegex();
    return text.replace(regex, (_, tag) => `[${tag} 数据见下方表格]\n`);
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
    const normEvents = (p.events || p.Events || []).map((ev) => ({
      timestamp: ev.timestamp || ev.Timestamp,
      operation: ev.operation || ev.Operation,
      details: ev.details || ev.Details || {},
    }));
    return {
      ...p,
      events: normEvents,
    };
  };

  const normalizePlanChanges = (list) => {
    if (!Array.isArray(list)) return [];
    return list
      .map((raw, idx) => ({
        tradeId: raw.trade_id ?? raw.TradeID ?? null,
        instanceId: raw.instance_id ?? raw.InstanceID ?? null,
        planId: raw.plan_id ?? raw.PlanID ?? '',
        planComponent: raw.plan_component ?? raw.PlanComponent ?? '',
        changedField: raw.changed_field ?? raw.ChangedField ?? '',
        oldValue: raw.old_value ?? raw.OldValue ?? '',
        newValue: raw.new_value ?? raw.NewValue ?? '',
        triggerSource: raw.trigger_source ?? raw.TriggerSource ?? '',
        reason: raw.reason ?? raw.Reason ?? '',
        decisionTraceId: raw.decision_trace_id ?? raw.DecisionTraceID ?? '',
        createdAt: raw.created_at ?? raw.CreatedAt ?? '',
        _idx: idx,
      }))
      .filter((item) => item.planId || item.planComponent || item.changedField || item.createdAt);
  };

  const parseJSONSafe = (raw, fallback = {}) => {
    if (raw === null || raw === undefined) return fallback;
    if (typeof raw === 'object') return raw;
    const text = String(raw).trim();
    if (!text) return fallback;
    try {
      const parsed = JSON.parse(text);
      if (parsed && typeof parsed === 'object') return parsed;
      return fallback;
    } catch (e) {
      return fallback;
    }
  };

  const planStatusLabel = (status) => {
    if (status === null || status === undefined) return '未知';
    if (typeof status === 'string') {
      const lower = status.trim().toLowerCase();
      switch (lower) {
        case 'waiting':
          return '等待';
        case 'pending':
          return '执行中';
        case 'done':
          return '完成';
        case 'paused':
          return '暂停';
        default:
          if (!Number.isNaN(Number(status))) {
            return planStatusLabel(Number(status));
          }
          return status.toUpperCase();
      }
    }
    const num = Number(status);
    switch (num) {
      case 0:
        return '等待';
      case 1:
        return '执行中';
      case 2:
        return '完成';
      case 3:
        return '暂停';
      default:
        return `状态${num}`;
    }
  };

  const normalizePlanInstances = (list) => {
    if (!Array.isArray(list)) return [];
    return list
      .map((raw, idx) => {
        const planId = raw.plan_id ?? raw.PlanID ?? '';
        const component = raw.plan_component ?? raw.PlanComponent ?? '';
        const stateJSON = raw.state_json ?? raw.StateJSON ?? '';
        const paramsJSON = raw.params_json ?? raw.ParamsJSON ?? '';
        const parsedState = parseJSONSafe(stateJSON, {});
        const parsedParams = parseJSONSafe(paramsJSON, {});
        const status = raw.status ?? raw.Status ?? 'waiting';
        const item = {
          id: raw.id ?? raw.ID ?? idx,
          tradeId: raw.trade_id ?? raw.TradeID ?? null,
          planId,
          planComponent: component,
          planVersion: raw.plan_version ?? raw.PlanVersion ?? 1,
          paramsJSON,
          stateJSON,
          status,
          statusLabel: planStatusLabel(status),
          decisionTraceId: raw.decision_trace_id ?? raw.DecisionTraceID ?? '',
          lastEvalAt: raw.last_eval_at ?? raw.LastEvalAt ?? '',
          nextEvalAfter: raw.next_eval_after ?? raw.NextEvalAfter ?? '',
          createdAt: raw.created_at ?? raw.CreatedAt ?? '',
          updatedAt: raw.updated_at ?? raw.UpdatedAt ?? '',
          parsedState,
          parsedParams,
        };
        return item;
      })
      .filter((item) => item.planId);
  };

  const pickStateValue = (state, ...keys) => {
    if (!state || typeof state !== 'object') return undefined;
    for (const key of keys) {
      if (key in state) {
        const val = state[key];
        if (val !== undefined) return val;
      }
      const camel =
        key.indexOf('_') !== -1
          ? key.replace(/_([a-z])/g, (_, c) => c.toUpperCase())
          : key;
      if (camel in state) {
        const val = state[camel];
        if (val !== undefined) return val;
      }
    }
    return undefined;
  };

  const formatParamValue = (val) => {
    if (val === null || val === undefined) return '--';
    if (Array.isArray(val)) return val.join('/');
    if (typeof val === 'object') {
      const parts = [];
      Object.entries(val).forEach(([k, v]) => {
        parts.push(`${k}=${formatParamValue(v)}`);
      });
      return parts.join('; ');
    }
    return String(val);
  };

  const normalizeMiddlewareDisplay = (list) => {
    if (!Array.isArray(list)) return [];
    const shorten = (text) => {
      const max = 90;
      if (!text || text.length <= max) return text;
      return `${text.slice(0, max)}…`;
    };
    return list
      .map((raw, idx) => {
        const text = raw === null || raw === undefined ? '' : String(raw).trim();
        if (!text) return null;
        const match = text.match(/^([^\s]+)\s+(.+)$/);
        const name = match ? match[1] : text;
        const paramText = match ? match[2].trim() : '';
        const parsed = paramText ? parseJSONSafe(paramText, null) : null;
        const paramsLabel =
          parsed && typeof parsed === 'object' && !Array.isArray(parsed)
            ? Object.entries(parsed)
              .map(([k, v]) => `${k}: ${formatParamValue(v)}`)
              .join(' · ')
            : paramText;
        return {
          key: `${name || 'mw'}-${idx}`,
          name: name || 'middleware',
          paramsLabel: shorten(paramsLabel || ''),
        };
      })
      .filter(Boolean);
  };

  const normalizeStrategyDisplay = (list) => {
    if (!Array.isArray(list)) return [];
    return list
      .map((raw, idx) => {
        const text = raw === null || raw === undefined ? '' : String(raw).trim();
        if (!text) return null;
        const colonIdx = text.indexOf(':');
        let id = text;
        let desc = '';
        if (colonIdx >= 0) {
          id = text.slice(0, colonIdx).trim();
          desc = text.slice(colonIdx + 1).trim();
        }
        return {
          key: `${id || 'plan'}-${idx}`,
          id: id || 'plan',
          desc,
        };
      })
      .filter(Boolean);
  };

  const decorateStrategyCard = (symbol, detail = {}) => {
    const mws = detail.middlewares || detail.Middlewares || [];
    const strategies = detail.strategies || detail.Strategies || [];
    const sysPrompt = detail.system_prompt || detail.systemPrompt || detail.SystemPrompt || '';
    const userPrompt = detail.user_prompt || detail.userPrompt || detail.UserPrompt || '';
    const profileLabel =
      detail.profile || detail.Profile || detail.profile_name || detail.ProfileName || '';
    const exitSummary = detail.exit_summary || detail.ExitSummary || '';
    const exitCombos = detail.exit_combos || detail.ExitCombos || [];
    const strategyItems = normalizeStrategyDisplay(strategies);
    return {
      ...detail,
      symbol,
      profileLabel: profileLabel || 'N/A',
      systemPromptLabel: sysPrompt || '—',
      userPromptLabel: userPrompt || '—',
      middlewares: mws,
      strategies,
      exitSummary,
      exitCombos,
      middlewareItems: normalizeMiddlewareDisplay(mws),
      strategyItems,
    };
  };

  const app = createApp({
    template: '#app-template',
    setup() {
      const init = window.__APP_INIT__ || {};
      const view = ref(init.view || 'desk');
      const isMobile = ref(typeof window !== 'undefined' ? window.innerWidth <= 768 : false);
      const navCollapsed = ref(isMobile.value);
      const showSystemPrompt = ref(true);
      const showUserPrompt = ref(true);
      const csvCollapse = reactive({});
      const defaultSymbols = ref(init.defaultSymbols || []);
      const symbolDetails = ref(init.symbolDetails || {});

      const desk = reactive({
        decisions: [],
        latestDecisions: [],
        positions: [],
        loading: false,
        error: '',
      });
      const decisions = reactive({
        items: [],
        itemsAll: [],
        flowPage: 1,
        flowPerSymbol: 2,
        fetchPage: 0,
        fetchPageSize: 0,
        rawLogs: [],
        traceMap: {},
        total: 0,
        symbols: init.selectedSymbols || [],
        loading: false,
        error: '',
        providers: [],
        providersRaw: [],
        providersLoading: false,
        providersError: '',
      });
      const positions = reactive({
        items: [],
        closed: [],
        page: 1,
        pageSize: 12,
        total: 0,
        symbol: init.symbol || '',
        loading: false,
        refreshing: false,
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
      const decisionFilters = reactive({ stage: 'provider', provider: '' });
      const positionDetail = reactive({
        tradeId: init.tradeId || null,
        data: null,
        loading: false,
        error: '',
        pnlRefreshing: false,
        planChanges: [],
        planChangesLoading: false,
        planInstances: [],
        planInstancesLoading: false,
        planInstancesError: '',
        tradeEvents: [],
        tradeEventsLoading: false,
        tradeEventsError: '',
      });
      const planAdjustForm = reactive({
        visible: false,
        submitting: false,
        planId: '',
        planComponent: '',
        entryPrice: 0,
        side: '',
        stop_loss_price: '',
        final_take_profit: '',
        final_stop_loss: '',
      });
      const tierEdits = reactive({});
      const atrEdits = reactive({});
      const symbolFallback =
        defaultSymbols.value && defaultSymbols.value.length ? defaultSymbols.value[0] : '';
      const manualOpen = reactive({
        symbol: init.symbol || symbolFallback,
        side: 'short',
        leverage: 5,
        position_size_usd: 100,
        entry_price: '',
        stop_loss: '',
        take_profit: '',
        exit_combo: '',
        tier1_target: '',
        tier1_ratio: 0.33,
        tier2_target: '',
        tier2_ratio: 0.33,
        tier3_target: '',
        tier3_ratio: 0.34,
        sl_tier1_target: '',
        sl_tier1_ratio: 0.33,
        sl_tier2_target: '',
        sl_tier2_ratio: 0.33,
        sl_tier3_target: '',
        sl_tier3_ratio: 0.34,
        reason: '',
        price: { last: 0, high: 0, low: 0 },
        loadingPrice: false,
        submitting: false,
        showConfirm: false,
      });
      const toast = reactive({ message: '', type: 'info' });
      const imagePreview = reactive({ visible: false, src: '', desc: '' });
      const expandMobileSteps = ref(false);
      const mobileStepLimit = 2;

      const loadingAny = computed(
        () =>
          desk.loading ||
          decisions.loading ||
          positions.loading ||
          positions.refreshing ||
          decisionDetail.loading ||
          positionDetail.loading ||
          logs.loading ||
          manualOpen.submitting ||
          manualOpen.loadingPrice,
      );

      const positionTotalPages = computed(() => Math.max(1, Math.ceil(positions.total / positions.pageSize)));

      const decisionFlowGroups = computed(() => {
        const groups = {};
        const cards = decisions.itemsAll || [];
        cards.forEach((card) => {
          const key = (card?.primarySymbol || '').toUpperCase() || 'MULTI';
          if (!groups[key]) groups[key] = [];
          groups[key].push(card);
        });
        Object.values(groups).forEach((list) => list.sort((a, b) => (b.log.ts || 0) - (a.log.ts || 0)));
        return groups;
      });

      const decisionFlowHasPrev = computed(() => decisions.flowPage > 1);
      const decisionFlowHasNext = computed(() => {
        const target = decisions.flowPage * decisions.flowPerSymbol;
        const symbols = decisions.symbols && decisions.symbols.length ? decisions.symbols : defaultSymbols.value || [];
        const groups = decisionFlowGroups.value || {};
        const hasEnoughForNext = symbols.some((sym) => (groups[String(sym).toUpperCase()] || []).length > target);
        const hasMoreOnServer = decisions.total > 0 && (decisions.rawLogs || []).length < decisions.total;
        return hasEnoughForNext || hasMoreOnServer;
      });
      const decisionFlowCanPage = computed(() => decisionFlowHasPrev.value || decisionFlowHasNext.value);

      const decisionProviders = computed(() => {
        const steps = decisionDetail.data?.trace?.steps || [];
        const set = new Set();
        steps.forEach((s) => {
          if (s.provider_id) set.add(s.provider_id);
        });
        return Array.from(set);
      });

      const rawDecisionSteps = computed(() => {
        const steps = decisionDetail.data?.trace?.steps || [];
        const stageFilter = (decisionFilters.stage || 'all').toLowerCase();
        const provider = (decisionFilters.provider || '').trim();
        return steps.filter((step) => {
          const stage = (step.stage || '').toLowerCase();
          let stageOK = true;
          if (stageFilter === 'provider') stageOK = stage === 'provider';
          else if (stageFilter === 'agent') stageOK = stage.includes('agent');
          else if (stageFilter === 'final') stageOK = stage === 'final';
          else if (stageFilter === 'other')
            stageOK = !(stage === 'provider' || stage.includes('agent') || stage === 'final');
          if (!stageOK) return false;
          if (provider && step.provider_id !== provider) return false;
          return true;
        });
      });

      const filteredDecisionSteps = computed(() => {
        const steps = rawDecisionSteps.value;
        if (isMobile.value && !expandMobileSteps.value && steps.length > mobileStepLimit) {
          return steps.slice(0, mobileStepLimit);
        }
        return steps;
      });

      const sharedPrompts = computed(() => {
        const steps = rawDecisionSteps.value || [];
        for (const step of steps) {
          const systemPrompt = (step.system_prompt || '').trim();
          const userPrompt = (step.user_prompt || '').trim();
          if (systemPrompt || userPrompt) {
            return { systemPrompt, userPrompt };
          }
        }
        return { systemPrompt: '', userPrompt: '' };
      });

      const stepExpandAvailable = computed(
        () => isMobile.value && rawDecisionSteps.value.length > mobileStepLimit,
      );

      const availableExitCombos = computed(() => {
        const sym = (manualOpen.symbol || '').trim().toUpperCase();
        if (!sym) return [];
        const detail = symbolDetails.value?.[sym] || {};
        const combos = detail.exit_combos || detail.exitCombos || detail.ExitCombos || [];
        if (!Array.isArray(combos)) return [];
        return combos
          .map((key) => ({ key, label: exitComboLabel(key) }))
          .filter((item) => item.key && item.key.toString().trim().length);
      });

      const manualExitCombo = computed(() => normalizeComboKey(manualOpen.exit_combo));

      const manualOpenFlags = computed(() => {
        const key = manualExitCombo.value;
        return {
          hasTpTiers: key.includes('tp_tiers'),
          hasTpSingle: key.includes('tp_single'),
          hasSlTiers: key.includes('sl_tiers'),
          hasSlSingle: key.includes('sl_single'),
          hasATR: key.includes('tp_atr') || key.includes('sl_atr'),
        };
      });

      const manualTierSum = computed(() => {
        const targets = [manualOpen.tier1_target, manualOpen.tier2_target, manualOpen.tier3_target].map((v) =>
          Number(v),
        );
        const filled = targets.filter((v) => Number.isFinite(v) && v > 0);
        if (!filled.length) return 0;
        if (filled.length === 1) return 1;
        const ratios = [manualOpen.tier1_ratio, manualOpen.tier2_ratio, manualOpen.tier3_ratio]
          .map((v) => Number(v))
          .filter((v) => Number.isFinite(v) && v > 0);
        if (!ratios.length) return 0;
        return ratios.reduce((acc, v) => acc + v, 0);
      });

      const manualSlTierSum = computed(() => {
        const targets = [manualOpen.sl_tier1_target, manualOpen.sl_tier2_target, manualOpen.sl_tier3_target].map((v) =>
          Number(v),
        );
        const filled = targets.filter((v) => Number.isFinite(v) && v > 0);
        if (!filled.length) return 0;
        if (filled.length === 1) return 1;
        const ratios = [manualOpen.sl_tier1_ratio, manualOpen.sl_tier2_ratio, manualOpen.sl_tier3_ratio]
          .map((v) => Number(v))
          .filter((v) => Number.isFinite(v) && v > 0);
        if (!ratios.length) return 0;
        return ratios.reduce((acc, v) => acc + v, 0);
      });

      const manualOpenPreview = computed(() => {
        const errors = [];
        const key = manualExitCombo.value;
        const comboLabel = key ? exitComboLabel(key) : '';
        const entry = Number(manualOpen.entry_price);
        const side = (manualOpen.side || '').toString().trim().toLowerCase();
        const symbol = (manualOpen.symbol || '').trim().toUpperCase();

        if (!symbol) errors.push('请选择交易对');
        if (!key) errors.push('请选择 exit plan 策略组合');
        if (!entry || entry <= 0) errors.push('请填写开仓价格（可点击“获取当前价”）');
        if (side !== 'long' && side !== 'short') errors.push('方向必须为 long 或 short');

        const flags = manualOpenFlags.value;
        if (flags.hasATR) {
          errors.push('当前手动开仓暂不支持 ATR 组合，请改用非 ATR 策略或开仓后在仓位详情中调整');
        }

        const lines = [];
        const addLine = (kind, label, price, ratio) => {
          const p = Number(price);
          const r = Number(ratio);
          if (!Number.isFinite(p) || p <= 0 || !Number.isFinite(r) || r <= 0) return;
          lines.push({
            kind,
            label,
            price: p,
            ratio: r,
            pct: priceToRelativePct(p, entry, side),
          });
        };

        const validateDirection = (kind, p) => {
          if (!entry || entry <= 0) return;
          if (side === 'long') {
            if (kind === 'TP' && !(p > entry)) errors.push(`${kind} 目标价需 > 开仓价`);
            if (kind === 'SL' && !(p < entry)) errors.push(`${kind} 目标价需 < 开仓价`);
          }
          if (side === 'short') {
            if (kind === 'TP' && !(p < entry)) errors.push(`${kind} 目标价需 < 开仓价`);
            if (kind === 'SL' && !(p > entry)) errors.push(`${kind} 目标价需 > 开仓价`);
          }
        };

        const tpTiers = [];
        if (flags.hasTpSingle) {
          const tp = Number(manualOpen.take_profit);
          if (!tp || tp <= 0) errors.push('请填写止盈目标价');
          if (tp > 0) {
            validateDirection('TP', tp);
            addLine('TP', 'TP', tp, 1);
          }
        } else if (flags.hasTpTiers) {
          const tierDefs = [
            { label: 'TP1', target: manualOpen.tier1_target, ratio: manualOpen.tier1_ratio },
            { label: 'TP2', target: manualOpen.tier2_target, ratio: manualOpen.tier2_ratio },
            { label: 'TP3', target: manualOpen.tier3_target, ratio: manualOpen.tier3_ratio },
          ];
          tierDefs.forEach((t) => {
            const target = Number(t.target);
            const ratio = Number(t.ratio);
            if (Number.isFinite(target) && target > 0) {
              tpTiers.push({ label: t.label, target, ratio });
            }
          });
          if (!tpTiers.length) {
            errors.push('请至少填写一个止盈 Tier 目标价');
          } else {
            if (tpTiers.length === 1) {
              tpTiers[0].ratio = 1;
            } else {
              const sum = tpTiers.reduce((acc, t) => acc + (Number.isFinite(t.ratio) ? t.ratio : 0), 0);
              if (Math.abs(sum - 1) > 1e-6) errors.push(`止盈 Tier 比例合计需为 1，当前=${sum.toFixed(4)}`);
            }
            const sorted = [...tpTiers].sort((a, b) => a.target - b.target);
            const expectAsc = side === 'long';
            const ordered = expectAsc ? sorted : sorted.reverse();
            for (let i = 0; i < ordered.length; i++) {
              validateDirection('TP', ordered[i].target);
              if (i > 0) {
                const ok = expectAsc ? ordered[i].target > ordered[i - 1].target : ordered[i].target < ordered[i - 1].target;
                if (!ok) errors.push('止盈 Tier 目标价需按方向严格单调（多头递增/空头递减）');
              }
            }
            ordered.forEach((t) => addLine('TP', t.label, t.target, t.ratio));
          }
        }

        if (flags.hasSlSingle) {
          const sl = Number(manualOpen.stop_loss);
          if (!sl || sl <= 0) errors.push('请填写止损目标价');
          if (sl > 0) {
            validateDirection('SL', sl);
            addLine('SL', 'SL', sl, 1);
          }
        } else if (flags.hasSlTiers) {
          const slTiers = [];
          const slDefs = [
            { label: 'SL1', target: manualOpen.sl_tier1_target, ratio: manualOpen.sl_tier1_ratio },
            { label: 'SL2', target: manualOpen.sl_tier2_target, ratio: manualOpen.sl_tier2_ratio },
            { label: 'SL3', target: manualOpen.sl_tier3_target, ratio: manualOpen.sl_tier3_ratio },
          ];
          slDefs.forEach((t) => {
            const target = Number(t.target);
            const ratio = Number(t.ratio);
            if (Number.isFinite(target) && target > 0) {
              slTiers.push({ label: t.label, target, ratio });
            }
          });
          if (!slTiers.length) {
            errors.push('请至少填写一个止损 Tier 目标价（SL1/2/3）');
          } else {
            if (slTiers.length === 1) {
              slTiers[0].ratio = 1;
            } else {
              const sum = slTiers.reduce((acc, t) => acc + (Number.isFinite(t.ratio) ? t.ratio : 0), 0);
              if (Math.abs(sum - 1) > 1e-6) errors.push(`止损 Tier 比例合计需为 1，当前=${sum.toFixed(4)}`);
            }
            const expectAsc = side === 'short';
            const sorted = [...slTiers].sort((a, b) => a.target - b.target);
            const ordered = expectAsc ? sorted : sorted.reverse();
            for (let i = 0; i < ordered.length; i++) {
              validateDirection('SL', ordered[i].target);
              if (i > 0) {
                const ok = expectAsc ? ordered[i].target > ordered[i - 1].target : ordered[i].target < ordered[i - 1].target;
                if (!ok) errors.push('止损 Tier 目标价需按方向严格单调（多头递减/空头递增）');
              }
            }
            ordered.forEach((t) => addLine('SL', t.label, t.target, t.ratio));
          }
        }

        return {
          symbol,
          side,
          entryPrice: entry,
          comboKey: key,
          comboLabel,
          lines,
          errors,
        };
      });

      const enabledSymbols = computed(() => defaultSymbols.value || []);

      const strategyCards = computed(() => {
        const details = symbolDetails.value || {};
        const ordered = [];
        const added = new Set();
        enabledSymbols.value.forEach((sym) => {
          const key = (sym || '').toUpperCase();
          if (!key || added.has(key)) return;
          const detail = details[sym] || details[key];
          if (detail) {
            ordered.push(decorateStrategyCard(sym, detail));
            added.add(key);
          }
        });
        Object.entries(details).forEach(([sym, detail]) => {
          const key = (sym || '').toUpperCase();
          if (added.has(key)) return;
          ordered.push(decorateStrategyCard(sym, detail));
          added.add(key);
        });
        return ordered;
      });

      // Filter desk data by profile symbols (defaultSymbols)
      const filteredDeskDecisions = computed(() => {
        const allowed = new Set((defaultSymbols.value || []).map((s) => s.toUpperCase()));
        const list = desk.latestDecisions.length ? desk.latestDecisions : desk.decisions;
        if (allowed.size === 0) return list;
        return list.filter((item) => {
          const symbols = item.log?.symbols || [];
          if (!symbols.length) return true; // Keep multi-symbol items
          return symbols.some((s) => allowed.has(s.toUpperCase()));
        });
      });

      const filteredDeskPositions = computed(() => {
        const allowed = new Set((defaultSymbols.value || []).map((s) => s.toUpperCase()));
        if (allowed.size === 0) return desk.positions;
        return desk.positions.filter((p) => {
          const sym = (p.symbol || '').toUpperCase();
          return allowed.has(sym);
        });
      });

      const cleanStageText = (text) => {
        if (!text) return '';
        return text
          .replace(/[_-]+/g, ' ')
          .replace(/\s+/g, ' ')
          .trim();
      };

      const describeStage = (stageRaw) => {
        const raw = (stageRaw || '').trim();
        if (!raw) {
          return { label: 'OTHER', detail: '', className: 'stage-pill-other' };
        }
        const lower = raw.toLowerCase();
        if (lower === 'provider') {
          return { label: 'PROVIDER', detail: '', className: 'stage-pill-provider' };
        }
        if (lower === 'final') {
          return { label: 'FINAL', detail: 'AGGREGATE', className: 'stage-pill-final' };
        }
        if (lower.startsWith('agent')) {
          const detail = cleanStageText(raw.split(':').slice(1).join(':'));
          return { label: 'AGENT', detail, className: 'stage-pill-agent' };
        }
        return { label: cleanStageText(raw).toUpperCase() || 'OTHER', detail: '', className: 'stage-pill-other' };
      };

      const stageLabel = (stage) => describeStage(stage).label || 'OTHER';
      const stageDetail = (stage) => {
        const detail = describeStage(stage).detail;
        return detail ? detail.toUpperCase() : '';
      };
      const stageBadgeClass = (stage) => describeStage(stage).className || 'stage-pill-other';

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

      const buildDeskDecisionParams = () => {
        const params = new URLSearchParams();
        const base = Math.max(6, (enabledSymbols.value?.length || 0) * 3);
        params.set('limit', String(base));
        params.set('stage', 'final');
        enabledSymbols.value.forEach((sym) => params.append('symbol', sym));
        return params.toString();
      };

      const pickLatestDeskDecisions = (cards) => {
        const allowed = new Set((enabledSymbols.value || []).map((s) => s.toUpperCase()));
        if (allowed.size === 0) return cards;
        const picked = [];
        const seen = new Set();
        for (const card of cards) {
          const symbols = (card.log?.symbols || []).map((s) => s.toUpperCase());
          const matched = symbols.filter((s) => allowed.has(s));
          if (!matched.length) continue;
          let captured = false;
          for (const sym of matched) {
            if (seen.has(sym)) continue;
            seen.add(sym);
            picked.push(card);
            captured = true;
            break;
          }
          if (captured && seen.size >= allowed.size) {
            break;
          }
        }
        return picked.length ? picked : cards;
      };

      const loadDesk = async () => {
        desk.loading = true;
        desk.error = '';
        try {
          const [decRes, posRes] = await Promise.all([
            fetchJSON(`/api/live/decisions?${buildDeskDecisionParams()}`),
            fetchJSON('/api/live/freqtrade/positions?limit=6&include_logs=0'),
          ]);
          const traceMap = Object.fromEntries((decRes.traces || []).map((t) => [t.trace_id, t]));
          desk.decisions = (decRes.logs || []).map((log) => normalizeDecisionCard(log, traceMap[log.trace_id]));
          desk.latestDecisions = pickLatestDeskDecisions(desk.decisions);
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
        params.set('page', String(Math.max(1, decisions.fetchPage || 1)));
        params.set('page_size', String(Math.max(1, decisions.fetchPageSize || 60)));
        params.set('stage', 'final');
        decisions.symbols.forEach((s) => params.append('symbol', s));
        return params.toString();
      };

      const buildProviderParams = () => {
        const params = new URLSearchParams();
        params.set('page', '1');
        const symCount = decisions.symbols.length ? decisions.symbols.length : (defaultSymbols.value || []).length || 1;
        params.set('page_size', String(Math.min(120, Math.max(30, symCount * 12))));
        params.set('stage', 'provider');
        decisions.symbols.forEach((s) => params.append('symbol', s));
        return params.toString();
      };

      const extractPrimarySymbol = (log, trace) => {
        const symbols = log?.symbols && log.symbols.length ? log.symbols : trace?.symbols || [];
        if (symbols && symbols.length) return String(symbols[0]).toUpperCase();
        const decSym = log?.decisions?.[0]?.symbol;
        if (decSym) return String(decSym).toUpperCase();
        return 'MULTI';
      };

      const rebuildDecisionItems = () => {
        const all = decisions.itemsAll || [];
        const groups = {};
        all.forEach((card) => {
          const key = (card?.primarySymbol || '').toUpperCase() || 'MULTI';
          if (!groups[key]) groups[key] = [];
          groups[key].push(card);
        });
        Object.values(groups).forEach((list) => list.sort((a, b) => (b.log.ts || 0) - (a.log.ts || 0)));

        const symbols = decisions.symbols && decisions.symbols.length ? decisions.symbols : defaultSymbols.value || [];
        const start = (decisions.flowPage - 1) * decisions.flowPerSymbol;
        const end = start + decisions.flowPerSymbol;
        const pageItems = [];
        symbols.forEach((sym) => {
          const list = groups[String(sym).toUpperCase()] || [];
          pageItems.push(...list.slice(start, end));
        });
        // keep Multi-symbol decisions at the end (optional)
        const multi = groups.MULTI || [];
        if (symbols.length === 0) {
          pageItems.push(...multi.slice(start, end));
        }
        decisions.items = pageItems.slice().sort((a, b) => (b.log.ts || 0) - (a.log.ts || 0));
      };

      const fetchDecisionHistoryPage = async (page) => {
        decisions.fetchPage = page;
        const res = await fetchJSON(`/api/live/decisions?${buildDecisionParams()}`);
        const traceMap = Object.fromEntries((res.traces || []).map((t) => [t.trace_id, t]));
        decisions.total = res.total_count || decisions.total || 0;
        decisions.traceMap = { ...(decisions.traceMap || {}), ...traceMap };
        const existing = new Set((decisions.rawLogs || []).map((l) => l.id));
        const appended = [];
        (res.logs || []).forEach((log) => {
          if (!log || !log.id || existing.has(log.id)) return;
          decisions.rawLogs.push(log);
          appended.push(log);
        });
        return appended.length;
      };

      const loadDecisions = async (force = false) => {
        if (!force && decisions.items.length && view.value !== 'decisions') return;
        decisions.loading = true;
        decisions.error = '';
        try {
          const symCount = decisions.symbols.length ? decisions.symbols.length : (defaultSymbols.value || []).length || 1;
          decisions.flowPage = 1;
          decisions.fetchPage = 0;
          decisions.rawLogs = [];
          decisions.traceMap = {};
          decisions.fetchPageSize = Math.min(200, Math.max(40, symCount * decisions.flowPerSymbol * 6)); // ~3 pages per symbol

          await fetchDecisionHistoryPage(1);
          decisions.itemsAll = (decisions.rawLogs || []).map((log) => {
            const trace = (decisions.traceMap || {})[log.trace_id];
            const card = normalizeDecisionCard(log, trace);
            card.primarySymbol = extractPrimarySymbol(log, trace);
            return card;
          });
          rebuildDecisionItems();
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
        const traceId = log.trace_id || log.traceId || '';
        return {
          id: log.id,
          traceId,
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
          decisions.providersRaw = (res.logs || []).map((log) => normalizeProviderOutput(log));
          const symbols = decisions.symbols && decisions.symbols.length ? decisions.symbols : defaultSymbols.value || [];
          const latestTraceBySymbol = {};
          symbols.forEach((sym) => {
            const key = String(sym).toUpperCase();
            let bestTs = -1;
            let bestTrace = '';
            (decisions.providersRaw || []).forEach((item) => {
              if (!item.traceId) return;
              const hasSymbol = (item.symbols || []).some((s) => String(s).toUpperCase() === key);
              if (!hasSymbol) return;
              const ts = Number(item.ts) || 0;
              if (ts > bestTs) {
                bestTs = ts;
                bestTrace = item.traceId;
              }
            });
            if (bestTrace) latestTraceBySymbol[key] = bestTrace;
          });
          const filtered = [];
          (decisions.providersRaw || []).forEach((item) => {
            const hit = (item.symbols || []).some((s) => {
              const key = String(s).toUpperCase();
              const wantTrace = latestTraceBySymbol[key];
              return wantTrace && item.traceId === wantTrace;
            });
            if (hit) filtered.push(item);
          });
          decisions.providers = filtered.length ? filtered : decisions.providersRaw.slice(0, 12);
        } catch (e) {
          decisions.providersError = e.message;
          showToast(e.message, 'error');
        } finally {
          decisions.providersLoading = false;
        }
      };

      const switchView = (next) => {
        view.value = next;
        if (isMobile.value) navCollapsed.value = true;
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

      const refreshPositionsPagePnL = async () => {
        if (positions.refreshing || positions.loading) return;
        const ids = (positions.items || [])
          .map((p) => Number(p.trade_id))
          .filter((id) => Number.isFinite(id) && id > 0);
        if (!ids.length) return;
        positions.refreshing = true;
        try {
          const queue = ids.slice();
          const workerCount = Math.min(3, queue.length);
          const workers = Array.from({ length: workerCount }, async () => {
            while (queue.length) {
              const id = queue.shift();
              try {
                const res = await fetchJSON(`/api/live/freqtrade/positions/${id}/refresh`, { method: 'POST' });
                const next = res?.position || null;
                if (!next) continue;
                const idx = positions.items.findIndex((p) => Number(p.trade_id) === id);
                if (idx >= 0) {
                  positions.items[idx] = { ...positions.items[idx], ...next };
                }
              } catch (e) { }
            }
          });
          await Promise.all(workers);
          showToast('已刷新当前页盈亏');
        } finally {
          positions.refreshing = false;
        }
      };

      const changeDecisionPage = async (delta) => {
        const next = decisions.flowPage + delta;
        if (next < 1) return;
        decisions.loading = true;
        decisions.error = '';
        try {
          const symbols = decisions.symbols && decisions.symbols.length ? decisions.symbols : defaultSymbols.value || [];
          const needed = next * decisions.flowPerSymbol;
          let safe = 0;
          while (safe < 4) {
            const groups = decisionFlowGroups.value || {};
            const missing = symbols.some((sym) => ((groups[String(sym).toUpperCase()] || []).length < needed));
            const hasMore = decisions.total > 0 && (decisions.rawLogs || []).length < decisions.total;
            if (!missing || !hasMore) break;
            safe += 1;
            await fetchDecisionHistoryPage((decisions.fetchPage || 1) + 1);
            decisions.itemsAll = (decisions.rawLogs || []).map((log) => {
              const trace = (decisions.traceMap || {})[log.trace_id];
              const card = normalizeDecisionCard(log, trace);
              card.primarySymbol = extractPrimarySymbol(log, trace);
              return card;
            });
          }
          decisions.flowPage = next;
          rebuildDecisionItems();
        } catch (e) {
          decisions.error = e.message;
          showToast(e.message, 'error');
        } finally {
          decisions.loading = false;
        }
      };

      const toggleSymbol = (sym) => {
        const idx = decisions.symbols.indexOf(sym);
        if (idx >= 0) {
          decisions.symbols.splice(idx, 1);
        } else {
          decisions.symbols.push(sym);
        }
        decisions.flowPage = 1;
        loadDecisions(true);
        loadProviderOutputs(true);
      };

      const clearSymbols = () => {
        decisions.symbols = [];
        decisions.flowPage = 1;
        loadDecisions(true);
        loadProviderOutputs(true);
      };

      const openDecision = (id) => {
        decisionDetail.id = id;
        view.value = 'decisionDetail';
        loadDecisionDetail(id);
      };

      const openStrategyConfig = (symbol) => {
        if (symbol) {
          decisions.symbols = [symbol];
        }
        view.value = 'decisions';
        loadDecisions(true);
        loadProviderOutputs(true);
      };

      const decisionHeadline = (log, trace) => formatAction(pickAction(log, trace));
      const decisionHeadlineAction = (log, trace) => pickAction(log, trace);

      const stopLossDistancePct = (pos) => {
        if (!pos) return null;
        const current = Number(pos.current_price);
        const stopLoss = Number(pos.stop_loss);
        if (!Number.isFinite(current) || current <= 0) return null;
        if (!Number.isFinite(stopLoss) || stopLoss <= 0) return null;
        const side = (pos.side || '').toString().toLowerCase();
        const dist = side.includes('short') ? (stopLoss - current) / current : (current - stopLoss) / current;
        if (!Number.isFinite(dist)) return null;
        return Math.abs(dist);
      };

      const nearestTriggerDistancePct = (pos) => {
        if (!pos) return null;
        const current = Number(pos.current_price);
        if (!Number.isFinite(current) || current <= 0) return null;
        const levels = [Number(pos.stop_loss), Number(pos.take_profit)].filter(
          (v) => Number.isFinite(v) && v > 0,
        );
        if (!levels.length) return null;
        let best = Infinity;
        levels.forEach((lvl) => {
          const dist = Math.abs(lvl - current) / current;
          if (Number.isFinite(dist) && dist < best) best = dist;
        });
        if (best === Infinity) return null;
        return best;
      };

      const cleanUserPrompt = (text) => {
        const compact = (input) => {
          let out = (input || '').toString().replace(/\r\n/g, '\n');
          out = out.replace(/[ \t]+\n/g, '\n');
          out = out.replace(/\n{3,}/g, '\n\n');
          return out.trim();
        };
        const cleaned = compact(stripCsvBlocks(text || ''));
        return cleaned.length ? cleaned : '—';
      };

      const mergedAgentPrompt = (step) => {
        if (!step) return '—';
        const system = (step.system_prompt || '').toString().trim();
        const user = cleanUserPrompt(step.user_prompt || '');
        const blocks = [];
        if (system) {
          blocks.push('SYSTEM PROMPT');
          blocks.push(system);
        }
        if ((user || '').toString().trim()) {
          blocks.push('USER PROMPT');
          blocks.push(user);
        }
        return blocks.length ? blocks.join('\n\n') : '—';
      };

      const csvBlockKey = (step, idx, tag) => `${step.stage || 'stage'}-${step.ts || idx}-${tag || idx}`;
      const sharedCsvBlockKey = (idx, tag) => `shared-${decisionDetail.id || '0'}-${idx}-${tag || idx}`;
      const isCsvExpanded = (key) => !!csvCollapse[key];
      const toggleCsvBlock = (key) => {
        csvCollapse[key] = !csvCollapse[key];
      };

      const loadDecisionDetail = async (id) => {
        decisionDetail.loading = true;
        decisionDetail.error = '';
        decisionDetail.data = null;
        expandMobileSteps.value = false;
        decisionFilters.stage = 'provider';
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
          const symbol = (positions.symbol || '').trim();
          const openParams = new URLSearchParams();
          openParams.set('page', positions.page);
          openParams.set('page_size', positions.pageSize);
          openParams.set('include_logs', '0');
          openParams.set('status', 'active');
          if (symbol) openParams.set('symbol', symbol);

          const closedParams = new URLSearchParams();
          closedParams.set('page', '1');
          closedParams.set('page_size', '24');
          closedParams.set('include_logs', '0');
          closedParams.set('status', 'closed');
          if (symbol) closedParams.set('symbol', symbol);

          const [openRes, closedRes] = await Promise.all([
            fetchJSON(`/api/live/freqtrade/positions?${openParams.toString()}`),
            fetchJSON(`/api/live/freqtrade/positions?${closedParams.toString()}`),
          ]);

          positions.items = openRes.positions || [];
          positions.total = openRes.total_count || 0;
          positions.closed = closedRes.positions || [];
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
        loadPositionDetail(tradeId).then(() => {
          refreshPositionPnL({ silent: true });
        });
      };

      const loadPositionDetail = async (tradeId) => {
        positionDetail.loading = true;
        positionDetail.error = '';
        positionDetail.data = null;
        positionDetail.planChanges = [];
        positionDetail.planInstances = [];
        positionDetail.planInstancesError = '';
        positionDetail.planInstancesLoading = true;
        positionDetail.tradeEvents = [];
        positionDetail.tradeEventsError = '';
        positionDetail.tradeEventsLoading = true;
        planAdjustForm.visible = false;
        planAdjustForm.submitting = false;
        try {
          const res = await fetchJSON(`/api/live/freqtrade/positions/${tradeId}?logs_limit=120`);
          positionDetail.data = normalizePositionDetail(res.position);
          const rawPlans = (res.position && (res.position.plans || res.position.Plans)) || [];
          const normalizedPlans = normalizePlanInstances(rawPlans);
          if (normalizedPlans.length) {
            positionDetail.planInstances = normalizedPlans;
            positionDetail.planInstancesLoading = false;
            syncTierEdits(positionDetail.planInstances);
            syncAtrEdits(positionDetail.planInstances);
          } else {
            loadPlanInstances(tradeId);
          }
          loadPlanChanges(tradeId);
          loadTradeEvents(tradeId);
          // Auto refresh PnL once to avoid stale values when entering detail.
          refreshPositionPnL({ silent: true });
          // Load TradingView chart after DOM update
          setTimeout(() => {
            if (positionDetail.data && positionDetail.data.symbol) {
              loadTradingViewWidget(positionDetail.data.symbol);
            }
          }, 100);
        } catch (e) {
          positionDetail.error = e.message;
          showToast(e.message, 'error');
          positionDetail.planInstancesLoading = false;
        } finally {
          positionDetail.loading = false;
        }
      };

      const refreshPositionPnL = async (opts = {}) => {
        const tradeId = positionDetail.tradeId;
        if (!tradeId) return;
        const silent = opts && opts.silent === true;
        positionDetail.pnlRefreshing = true;
        try {
          const res = await fetchJSON(`/api/live/freqtrade/positions/${tradeId}/refresh`, { method: 'POST' });
          const next = normalizePositionDetail(res.position);
          if (!next) return;
          if (!positionDetail.data) {
            positionDetail.data = next;
            return;
          }
          [
            'status',
            'opened_at',
            'closed_at',
            'holding_ms',
            'pnl_ratio',
            'pnl_usd',
            'realized_pnl_ratio',
            'realized_pnl_usd',
            'unrealized_pnl_ratio',
            'unrealized_pnl_usd',
            'current_price',
            'position_value',
            'remaining_ratio',
            'stop_loss',
            'take_profit',
          ].forEach((k) => {
            if (Object.prototype.hasOwnProperty.call(next, k)) {
              positionDetail.data[k] = next[k];
            }
          });
          if (!silent) showToast('盈亏已刷新');
        } catch (e) {
          if (!silent) showToast(`盈亏刷新失败: ${e.message}`, 'error');
        } finally {
          positionDetail.pnlRefreshing = false;
        }
      };

      const loadPlanChanges = async (tradeId) => {
        positionDetail.planChangesLoading = true;
        positionDetail.planChanges = [];
        try {
          const res = await fetchJSON(`/api/live/plans/changes?trade_id=${tradeId}&limit=80`);
          positionDetail.planChanges = normalizePlanChanges(res.changes || []);
        } catch (e) {
          showToast(`Plan log 加载失败: ${e.message}`, 'error');
        } finally {
          positionDetail.planChangesLoading = false;
        }
      };

      const loadPlanInstances = async (tradeId) => {
        if (!tradeId) {
          positionDetail.planInstancesLoading = false;
          return;
        }
        positionDetail.planInstancesLoading = true;
        positionDetail.planInstancesError = '';
        positionDetail.planInstances = [];
        try {
          const res = await fetchJSON(`/api/live/plans/instances?trade_id=${tradeId}`);
          positionDetail.planInstances = normalizePlanInstances(res.instances || []);
          syncTierEdits(positionDetail.planInstances);
          syncAtrEdits(positionDetail.planInstances);
        } catch (e) {
          positionDetail.planInstancesError = e.message;
          showToast(`Plan 状态加载失败: ${e.message}`, 'error');
        } finally {
          positionDetail.planInstancesLoading = false;
        }
      };

      const loadTradeEvents = async (tradeId) => {
        if (!tradeId) {
          positionDetail.tradeEventsLoading = false;
          return;
        }
        positionDetail.tradeEventsLoading = true;
        positionDetail.tradeEventsError = '';
        positionDetail.tradeEvents = [];
        try {
          const res = await fetchJSON(`/api/live/freqtrade/events?trade_id=${tradeId}&limit=120`);
          positionDetail.tradeEvents = res.events || [];
        } catch (e) {
          positionDetail.tradeEventsError = e.message;
          showToast(`事件流水加载失败: ${e.message}`, 'error');
        } finally {
          positionDetail.tradeEventsLoading = false;
        }
      };

      const tierGroupLabel = (alias) => {
        switch ((alias || '').toLowerCase()) {
          case 'tp_tiers':
            return '止盈分段 (tp_tiers)';
          case 'sl_tiers':
            return '止损分段 (sl_tiers)';
          case 'tp_single':
            return '止盈单段 (tp_single)';
          case 'sl_single':
            return '止损单段 (sl_single)';
          default:
            return alias ? `${alias}` : 'tiers';
        }
      };

      const resetTierEdits = () => {
        Object.keys(tierEdits).forEach((k) => {
          delete tierEdits[k];
        });
      };

      const syncTierEdits = (instances) => {
        resetTierEdits();
        (instances || []).forEach((inst) => {
          const comp = (inst.planComponent || '').trim();
          if (!comp || comp.indexOf('.tier') === -1) return;
          const state = inst.parsedState || {};
          const target = Number(state.target_price);
          const ratio = Number(state.ratio);
          if (!Number.isFinite(target) || !Number.isFinite(ratio)) return;
          tierEdits[comp] = { target_price: target, ratio };
        });
      };

      const resetAtrEdits = () => {
        Object.keys(atrEdits).forEach((k) => {
          delete atrEdits[k];
        });
      };

      const syncAtrEdits = (instances) => {
        resetAtrEdits();
        (instances || []).forEach((inst) => {
          const comp = (inst.planComponent || '').trim();
          if (!comp || comp.indexOf('.tier') !== -1) return;
          const state = inst.parsedState || {};
          const triggerPct = Number(pickStateValue(state, 'trigger_pct', 'TriggerPct'));
          const trailPct = Number(pickStateValue(state, 'trail_pct', 'TrailPct'));
          if (!Number.isFinite(triggerPct) || !Number.isFinite(trailPct) || triggerPct <= 0 || trailPct <= 0) return;
          const params = inst.parsedParams || {};
          const atr = Number(pickStateValue(params, 'atr_value', 'atrValue'));
          const entry = Number(pickStateValue(state, 'entry_price', 'EntryPrice'));
          if (!Number.isFinite(atr) || atr <= 0 || !Number.isFinite(entry) || entry <= 0) return;

          const triggerMul = (triggerPct * entry) / atr;
          const trailMul = (trailPct * entry) / atr;
          if (!Number.isFinite(triggerMul) || !Number.isFinite(trailMul) || triggerMul <= 0 || trailMul <= 0) return;
          atrEdits[comp] = { trigger_multiplier: triggerMul, trail_multiplier: trailMul };
        });
      };

      const planTierGroups = computed(() => {
        const out = [];
        const groups = new Map();
        (positionDetail.planInstances || []).forEach((inst) => {
          const comp = (inst.planComponent || '').trim();
          if (!comp || comp.indexOf('.tier') === -1) return;
          const alias = comp.split('.')[0] || '';
          const state = inst.parsedState || {};
          const targetPrice = Number(state.target_price);
          const ratio = Number(state.ratio);
          if (!Number.isFinite(targetPrice) || !Number.isFinite(ratio)) return;
          const rawStatus = (state.status || inst.status || '').toString().trim().toLowerCase();
          const editable = rawStatus !== 'done' && rawStatus !== 'triggered';
          const item = {
            planId: inst.planId,
            component: comp,
            alias,
            name: (state.name || comp).toString(),
            target_price: targetPrice,
            ratio,
            statusLabel: (state.status || inst.statusLabel || rawStatus || '—').toString(),
            editable,
          };
          const key = `${inst.planId}:${alias}`;
          if (!groups.has(key)) {
            groups.set(key, {
              key,
              planId: inst.planId,
              alias,
              label: tierGroupLabel(alias),
              tiers: [],
            });
          }
          groups.get(key).tiers.push(item);
        });
        groups.forEach((g) => {
          g.tiers.sort((a, b) => a.component.localeCompare(b.component));
          out.push(g);
        });
        out.sort((a, b) => a.key.localeCompare(b.key));
        return out;
      });

      const atrComponents = computed(() => {
        const out = [];
        (positionDetail.planInstances || []).forEach((inst) => {
          const comp = (inst.planComponent || '').trim();
          if (!comp || comp.indexOf('.tier') !== -1) return;
          const state = inst.parsedState || {};
          const triggerPct = Number(pickStateValue(state, 'trigger_pct', 'TriggerPct'));
          const trailPct = Number(pickStateValue(state, 'trail_pct', 'TrailPct'));
          if (!Number.isFinite(triggerPct) || !Number.isFinite(trailPct) || triggerPct <= 0 || trailPct <= 0) return;

          const params = inst.parsedParams || {};
          const atr = Number(pickStateValue(params, 'atr_value', 'atrValue'));
          const entry = Number(pickStateValue(state, 'entry_price', 'EntryPrice'));
          if (!Number.isFinite(atr) || atr <= 0 || !Number.isFinite(entry) || entry <= 0) return;

          const rawStatus = (inst.status || '').toString().trim().toLowerCase();
          const editable = rawStatus !== '2' && rawStatus !== 'done';

          const triggerMul = (triggerPct * entry) / atr;
          const trailMul = (trailPct * entry) / atr;
          if (!Number.isFinite(triggerMul) || !Number.isFinite(trailMul) || triggerMul <= 0 || trailMul <= 0) return;

          let label = comp;
          if (comp.toLowerCase().includes('tp')) label = `TP ATR (${comp})`;
          else if (comp.toLowerCase().includes('sl')) label = `SL ATR (${comp})`;

          out.push({
            planId: inst.planId,
            component: comp,
            label,
            atr_value: atr,
            entry_price: entry,
            trigger_pct: triggerPct,
            trail_pct: trailPct,
            trigger_multiplier: triggerMul,
            trail_multiplier: trailMul,
            editable,
          });
        });
        out.sort((a, b) => a.component.localeCompare(b.component));
        return out;
      });

      const planEditPreviewLines = computed(() => {
        const lines = [];
        planTierGroups.value.forEach((group) => {
          group.tiers.forEach((tier) => {
            const edit = tierEdits[tier.component];
            if (!edit) return;
            const nextTarget = Number(edit.target_price);
            const nextRatio = Number(edit.ratio);
            const changedTarget = Number.isFinite(nextTarget) && Math.abs(nextTarget - tier.target_price) > 1e-9;
            const changedRatio = Number.isFinite(nextRatio) && Math.abs(nextRatio - tier.ratio) > 1e-9;
            if (!changedTarget && !changedRatio) return;
            const parts = [];
            if (changedTarget) {
              parts.push(`target_price ${formatNumber(tier.target_price)} → ${formatNumber(nextTarget)}`);
            }
            if (changedRatio) {
              parts.push(`ratio ${formatPercent(tier.ratio)} → ${formatPercent(nextRatio)}`);
            }
            lines.push(`[${group.alias}] ${tier.name}: ${parts.join(' · ')}`);
          });
        });
        atrComponents.value.forEach((item) => {
          const edit = atrEdits[item.component];
          if (!edit) return;
          const nextTrigger = Number(edit.trigger_multiplier);
          const nextTrail = Number(edit.trail_multiplier);
          const changedTrigger =
            Number.isFinite(nextTrigger) && Math.abs(nextTrigger - item.trigger_multiplier) > 1e-9;
          const changedTrail = Number.isFinite(nextTrail) && Math.abs(nextTrail - item.trail_multiplier) > 1e-9;
          if (!changedTrigger && !changedTrail) return;
          const parts = [];
          if (changedTrigger) {
            const nextPct = (item.atr_value * nextTrigger) / item.entry_price;
            parts.push(
              `trigger_multiplier ${item.trigger_multiplier.toFixed(2)} → ${nextTrigger.toFixed(2)} (${formatPercent(item.trigger_pct)} → ${formatPercent(nextPct)})`,
            );
          }
          if (changedTrail) {
            const nextPct = (item.atr_value * nextTrail) / item.entry_price;
            parts.push(
              `trail_multiplier ${item.trail_multiplier.toFixed(2)} → ${nextTrail.toFixed(2)} (${formatPercent(item.trail_pct)} → ${formatPercent(nextPct)})`,
            );
          }
          lines.push(`[${item.component}] ${item.label}: ${parts.join(' · ')}`);
        });
        return lines;
      });

      const planEditError = computed(() => {
        const tol = 1e-6;
        for (const group of planTierGroups.value) {
          let oldSum = 0;
          let newSum = 0;
          for (const tier of group.tiers) {
            if (!tier.editable) continue;
            oldSum += Number(tier.ratio) || 0;
            const edit = tierEdits[tier.component];
            const ratio = Number(edit?.ratio);
            const target = Number(edit?.target_price);
            if (!Number.isFinite(target) || target <= 0) {
              return `${group.label} · ${tier.name} 目标价无效`;
            }
            if (!Number.isFinite(ratio) || ratio <= 0 || ratio > 1) {
              return `${group.label} · ${tier.name} 比例需在 (0,1]`;
            }
            newSum += ratio;
          }
          if (oldSum > 0 && Math.abs(oldSum - newSum) > tol) {
            return `${group.label} 比例合计需保持 ${oldSum.toFixed(4)}，当前=${newSum.toFixed(4)}`;
          }
        }
        for (const item of atrComponents.value) {
          if (!item.editable) continue;
          const edit = atrEdits[item.component];
          if (!edit) continue;
          const trig = Number(edit.trigger_multiplier);
          const trail = Number(edit.trail_multiplier);
          if (!Number.isFinite(trig) || trig <= 0) {
            return `${item.label} trigger_multiplier 无效`;
          }
          if (!Number.isFinite(trail) || trail <= 0) {
            return `${item.label} trail_multiplier 无效`;
          }
          if (trail >= trig) {
            return `${item.label} 要求 trail_multiplier < trigger_multiplier`;
          }
          if (trig < 1 || trig > 5) {
            return `${item.label} trigger_multiplier 需在 [1.0, 5.0]`;
          }
          if (trail < 0.5) {
            return `${item.label} trail_multiplier 需 >= 0.5`;
          }
          const nextTriggerPct = (item.atr_value * trig) / item.entry_price;
          const nextTrailPct = (item.atr_value * trail) / item.entry_price;
          if (nextTrailPct >= nextTriggerPct) {
            return `${item.label} 计算后 trail_pct 必须小于 trigger_pct`;
          }
          if (nextTriggerPct > 0.25) {
            return `${item.label} trigger_pct 不可超过 25%（当前=${formatPercent(nextTriggerPct)}）`;
          }
          if (nextTriggerPct < 0.005) {
            return `${item.label} trigger_pct 至少 0.5%（当前=${formatPercent(nextTriggerPct)}）`;
          }
          if (nextTrailPct < 0.002) {
            return `${item.label} trail_pct 至少 0.2%（当前=${formatPercent(nextTrailPct)}）`;
          }
        }
        return '';
      });

      const tierEditsSubmitting = ref(false);

      const submitTierEdits = async () => {
        if (!positionDetail.tradeId) {
          showToast('缺少 trade_id', 'error');
          return;
        }
        const err = (planEditError.value || '').trim();
        if (err) {
          showToast(err, 'error');
          return;
        }
        const changes = [];
        planTierGroups.value.forEach((group) => {
          group.tiers.forEach((tier) => {
            if (!tier.editable) return;
            const edit = tierEdits[tier.component];
            if (!edit) return;
            const nextTarget = Number(edit.target_price);
            const nextRatio = Number(edit.ratio);
            const params = {};
            if (Number.isFinite(nextTarget) && Math.abs(nextTarget - tier.target_price) > 1e-9) {
              params.target_price = nextTarget;
            }
            if (Number.isFinite(nextRatio) && Math.abs(nextRatio - tier.ratio) > 1e-9) {
              params.ratio = nextRatio;
            }
            if (Object.keys(params).length) {
              changes.push({ planId: tier.planId, component: tier.component, params });
            }
          });
        });
        atrComponents.value.forEach((item) => {
          if (!item.editable) return;
          const edit = atrEdits[item.component];
          if (!edit) return;
          const nextTrigger = Number(edit.trigger_multiplier);
          const nextTrail = Number(edit.trail_multiplier);
          const params = {};
          if (Number.isFinite(nextTrigger) && Math.abs(nextTrigger - item.trigger_multiplier) > 1e-9) {
            params.trigger_multiplier = nextTrigger;
          }
          if (Number.isFinite(nextTrail) && Math.abs(nextTrail - item.trail_multiplier) > 1e-9) {
            params.trail_multiplier = nextTrail;
          }
          if (Object.keys(params).length) {
            changes.push({ planId: item.planId, component: item.component, params });
          }
        });
        if (!changes.length) {
          showToast('暂无可提交的变更', 'warning');
          return;
        }
        tierEditsSubmitting.value = true;
        try {
          for (const item of changes) {
            await fetchJSON('/api/live/plans/adjust', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                trade_id: positionDetail.tradeId,
                plan_id: item.planId,
                plan_component: item.component,
                params: item.params,
              }),
            });
          }
          showToast('策略修改已提交');
          loadPositionDetail(positionDetail.tradeId);
        } catch (e) {
          showToast(`策略修改失败: ${e.message}`, 'error');
        } finally {
          tierEditsSubmitting.value = false;
        }
      };

      const openPlanAdjust = (inst) => {
        if (!inst) return;
        if (inst.planComponent) {
          showToast('分段计划的手动调整已下线，请直接从 exit_plan 更新完整策略', 'warning');
          return;
        }
        planAdjustForm.visible = true;
        planAdjustForm.submitting = false;
        planAdjustForm.planId = inst.planId;
        planAdjustForm.planComponent = '';
        const state = inst.parsedState || {};
        planAdjustForm.entryPrice =
          Number(pickStateValue(state, 'entry_price')) ||
          Number(positionDetail.data?.entry_price) ||
          0;
        planAdjustForm.side =
          (pickStateValue(state, 'side') || positionDetail.data?.side || '').toLowerCase();
        planAdjustForm.stop_loss_price = Number(
          pickStateValue(state, 'stop_loss', 'stop_loss_price'),
        );
        if (Number.isNaN(planAdjustForm.stop_loss_price)) {
          planAdjustForm.stop_loss_price = '';
        }
        planAdjustForm.final_take_profit = '';
        planAdjustForm.final_stop_loss = '';
      };

      const closePlanAdjust = () => {
        planAdjustForm.visible = false;
        planAdjustForm.submitting = false;
        planAdjustForm.planId = '';
        planAdjustForm.planComponent = '';
        planAdjustForm.entryPrice = 0;
        planAdjustForm.side = '';
        planAdjustForm.stop_loss_price = '';
        planAdjustForm.final_take_profit = '';
        planAdjustForm.final_stop_loss = '';
      };

      const submitPlanAdjust = async () => {
        if (!positionDetail.tradeId) {
          showToast('缺少 trade_id', 'error');
          return;
        }
        if (!planAdjustForm.planId) {
          showToast('请选择需要调整的 Plan', 'error');
          return;
        }
        const payload = {
          trade_id: positionDetail.tradeId,
          plan_id: planAdjustForm.planId,
          plan_component: '',
          params: {},
        };
        const sl = Number(planAdjustForm.stop_loss_price);
        const finalTP = Number(planAdjustForm.final_take_profit);
        const finalSL = Number(planAdjustForm.final_stop_loss);
        if (sl > 0) {
          const pct = priceToRelativePct(sl, planAdjustForm.entryPrice, planAdjustForm.side);
          if (pct === null) {
            showToast('缺少入场价，无法换算止损', 'error');
            return;
          }
          payload.params.stop_loss_pct = pct;
        }
        if (finalTP > 0) {
          const pct = priceToRelativePct(finalTP, planAdjustForm.entryPrice, planAdjustForm.side);
          if (pct === null) {
            showToast('缺少入场价，无法换算 Final TP', 'error');
            return;
          }
          payload.params.final_take_profit = pct;
        }
        if (finalSL > 0) {
          const pct = priceToRelativePct(finalSL, planAdjustForm.entryPrice, planAdjustForm.side);
          if (pct === null) {
            showToast('缺少入场价，无法换算 Final SL', 'error');
            return;
          }
          payload.params.final_stop_loss = pct;
        }
        if (Object.keys(payload.params).length === 0) {
          showToast('请填写至少一个调整字段', 'error');
          return;
        }
        planAdjustForm.submitting = true;
        try {
          await fetchJSON('/api/live/plans/adjust', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload),
          });
          showToast('Plan 调整已提交');
          planAdjustForm.visible = false;
          loadPlanInstances(positionDetail.tradeId);
          loadPlanChanges(positionDetail.tradeId);
        } catch (e) {
          showToast(`Plan 调整失败: ${e.message}`, 'error');
        } finally {
          planAdjustForm.submitting = false;
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
          if ((!manualOpen.entry_price || Number(manualOpen.entry_price) <= 0) && res.last > 0) {
            manualOpen.entry_price = res.last;
          }
        } catch (e) {
          showToast(e.message, 'error');
        } finally {
          manualOpen.loadingPrice = false;
        }
      };

      const planSummaryLines = (pos) => {
        if (!pos || !Array.isArray(pos.plan_summaries)) return [];
        return pos.plan_summaries.filter((line) => typeof line === 'string' && line.trim().length);
      };

      const planComponentLabel = (inst) => {
        if (!inst) return 'ROOT';
        const comp = inst.planComponent || inst.plan_component || '';
        if (!comp) return 'ROOT';
        return comp.toUpperCase();
      };

      const planInstanceStateLine = (inst) => {
        if (!inst) return '--';
        const state = inst.parsedState || {};
        if (!inst.planComponent) {
          const stop = pickStateValue(state, 'stop_loss', 'stop_loss_price');
          const take = pickStateValue(state, 'take_profit', 'take_profit_price');
          const remain = pickStateValue(state, 'remaining_ratio');
          const pending = pickStateValue(state, 'pending_event');
          const parts = [];
          if (stop) parts.push(`SL ${formatNumber(stop)}`);
          if (take) parts.push(`TP ${formatNumber(take)}`);
          if (remain) parts.push(`Remain ${(remain * 100).toFixed(1)}%`);
          if (pending) parts.push(`Pending ${pending}`);
          return parts.length ? parts.join(' · ') : '--';
        }
        const target = pickStateValue(state, 'target_price');
        const ratio = pickStateValue(state, 'ratio');
        const trig = pickStateValue(state, 'trigger_price');
        const last = pickStateValue(state, 'last_event');
        const parts = [];
        if (target) parts.push(`Target ${formatNumber(target)}`);
        if (ratio) parts.push(`Ratio ${(ratio * 100).toFixed(1)}%`);
        if (trig) parts.push(`Hit ${formatNumber(trig)}`);
        if (last) parts.push(`Last ${last}`);
        return parts.length ? parts.join(' · ') : '--';
      };

      const priceToRelativePct = (price, entry, side) => {
        const p = Number(price);
        const base = Number(entry);
        if (!p || !base || base <= 0) return null;
        const dir = (side || '').toLowerCase();
        if (dir === 'short') {
          return 1 - p / base;
        }
        return p / base - 1;
      };

      // Exit Levels parsing for visualization
      const exitLevels = computed(() => {
        const instances = positionDetail.planInstances || [];
        const data = positionDetail.data;
        if (!instances.length || !data) return [];
        const entry = Number(data.entry_price) || 0;
        const current = Number(data.current_price) || 0;
        if (entry <= 0) return [];

        const levels = [];
        let allPrices = [entry];
        if (current > 0) allPrices.push(current);

        // Parse each plan instance for target prices
        instances.forEach((inst) => {
          const state = inst.parsedState || {};
          const component = (inst.planComponent || '').toLowerCase();

          // Root level: final TP/SL
          if (!inst.planComponent || component === 'root') {
            const finalTP = Number(pickStateValue(state, 'final_take_profit')) || 0;
            const finalSL = Number(pickStateValue(state, 'final_stop_loss')) || 0;
            const tp = Number(pickStateValue(state, 'take_profit', 'take_profit_price')) || 0;
            const sl = Number(pickStateValue(state, 'stop_loss', 'stop_loss_price')) || 0;

            const tpPrice = finalTP > 0 ? finalTP : tp;
            const slPrice = finalSL > 0 ? finalSL : sl;
            const tpTriggered =
              finalTP > 0
                ? !!pickStateValue(state, 'final_take_profit_triggered', 'take_profit_triggered')
                : !!pickStateValue(state, 'take_profit_triggered');
            const slTriggered =
              finalSL > 0
                ? !!pickStateValue(state, 'final_stop_loss_triggered', 'stop_loss_triggered')
                : !!pickStateValue(state, 'stop_loss_triggered');

            if (tpPrice && tpPrice > 0) {
              levels.push({
                id: `${inst.planId}-root-tp`,
                type: finalTP > 0 ? 'tp-final' : 'tp',
                label: finalTP > 0 ? 'Final TP' : 'TP',
                price: tpPrice,
                ratio: null,
                hitPrice: null,
                status: tpTriggered ? 'triggered' : 'active',
              });
              allPrices.push(tpPrice);
            }
            if (slPrice && slPrice > 0) {
              levels.push({
                id: `${inst.planId}-root-sl`,
                type: finalSL > 0 ? 'sl-final' : 'sl',
                label: finalSL > 0 ? 'Final SL' : 'SL',
                price: slPrice,
                ratio: null,
                hitPrice: null,
                status: slTriggered ? 'triggered' : 'active',
              });
              allPrices.push(slPrice);
            }

            const trailActivation = Number(pickStateValue(state, 'trailing_activation_price')) || 0;
            const trailStop = Number(pickStateValue(state, 'trailing_stop_price')) || 0;
            if (trailActivation > 0) {
              levels.push({
                id: `${inst.planId}-trail-activation`,
                type: 'tier',
                label: 'Trail Activation',
                price: trailActivation,
                ratio: null,
                hitPrice: null,
                status: 'active',
              });
              allPrices.push(trailActivation);
            }
            if (trailStop > 0) {
              levels.push({
                id: `${inst.planId}-trail-stop`,
                type: 'sl-final',
                label: 'Trail Stop',
                price: trailStop,
                ratio: null,
                hitPrice: null,
                status: 'active',
              });
              allPrices.push(trailStop);
            }
          } else {
            // Tier components
            const target = pickStateValue(state, 'target_price');
            const ratio = pickStateValue(state, 'ratio');
            const triggered = pickStateValue(state, 'trigger_price');
            const statusStr = (pickStateValue(state, 'status') || '').toLowerCase();
            if (target && target > 0) {
              const isTp = component.includes('tp') || component.includes('take');
              const isSl = component.includes('sl') || component.includes('stop');
              const levelType = isTp ? 'tp' : isSl ? 'sl' : 'tier';
              const triggeredPrice = Number(triggered) || 0;
              const name = (pickStateValue(state, 'name') || '').toString().trim();
              let label = inst.planComponent.toUpperCase().replace(/[_.-]/g, ' ');
              if (name) {
                const shortName = name.toUpperCase().replace(/[_.-]/g, ' ');
                if (isTp) label = `TP ${shortName}`;
                else if (isSl) label = `SL ${shortName}`;
                else label = shortName;
              }
              levels.push({
                id: `${inst.planId}-${inst.planComponent}`,
                type: levelType,
                label,
                price: target,
                ratio: ratio || null,
                hitPrice: triggeredPrice > 0 ? triggeredPrice : null,
                status:
                  triggeredPrice > 0 || statusStr === 'triggered' || statusStr === 'done'
                    ? 'triggered'
                    : 'active',
              });
              allPrices.push(target);
              if (triggeredPrice > 0) allPrices.push(triggeredPrice);
            }
          }
        });

        // Calculate position percentage for each level
        if (levels.length === 0) return [];
        const minPrice = Math.min(...allPrices) * 0.98; // Add 2% padding
        const maxPrice = Math.max(...allPrices) * 1.02;
        const range = maxPrice - minPrice;
        if (range <= 0) return levels;

        levels.forEach((lv) => {
          lv.position = ((lv.price - minPrice) / range) * 100;
        });

        return levels;
      });

      const chartLevelChips = computed(() => {
        const data = positionDetail.data;
        if (!data) return [];
        const entry = Number(data.entry_price) || 0;
        if (entry <= 0) return [];
        const side = (data.side || '').toLowerCase();
        const current = Number(data.current_price) || 0;

        const chips = [];
        chips.push({
          id: 'entry',
          type: 'entry',
          label: 'Entry',
          price: entry,
          ratio: null,
          hitPrice: null,
          status: 'active',
          pct: null,
        });
        if (current > 0) {
          chips.push({
            id: 'current',
            type: 'current',
            label: 'Current',
            price: current,
            ratio: null,
            hitPrice: null,
            status: 'active',
            pct: priceToRelativePct(current, entry, side),
          });
        }

        (exitLevels.value || []).forEach((lv) => {
          chips.push({
            ...lv,
            pct: priceToRelativePct(lv.price, entry, side),
          });
        });

        const rank = (type) => {
          const t = (type || '').toString();
          if (t === 'entry') return 0;
          if (t === 'current') return 1;
          if (t.startsWith('tp')) return 10;
          if (t.startsWith('sl')) return 20;
          return 30;
        };

        return chips.slice().sort((a, b) => rank(a.type) - rank(b.type) || b.price - a.price);
      });

      // Entry and current price positions
      const entryLevelPosition = computed(() => {
        const data = positionDetail.data;
        if (!data || !exitLevels.value.length) return 50;
        const entry = Number(data.entry_price) || 0;
        if (entry <= 0) return 50;
        const allPrices = exitLevels.value
          .flatMap((l) => [l.price, l.hitPrice])
          .filter((p) => Number(p) > 0)
          .concat([entry, Number(data.current_price) || entry]);
        const minPrice = Math.min(...allPrices) * 0.98;
        const maxPrice = Math.max(...allPrices) * 1.02;
        const range = maxPrice - minPrice;
        if (range <= 0) return 50;
        return ((entry - minPrice) / range) * 100;
      });

      const currentPricePosition = computed(() => {
        const data = positionDetail.data;
        if (!data || !exitLevels.value.length) return 50;
        const current = Number(data.current_price) || 0;
        const entry = Number(data.entry_price) || 0;
        if (current <= 0) return entryLevelPosition.value;
        const allPrices = exitLevels.value
          .flatMap((l) => [l.price, l.hitPrice])
          .filter((p) => Number(p) > 0)
          .concat([entry, current]);
        const minPrice = Math.min(...allPrices) * 0.98;
        const maxPrice = Math.max(...allPrices) * 1.02;
        const range = maxPrice - minPrice;
        if (range <= 0) return 50;
        return ((current - minPrice) / range) * 100;
      });

      // TradingView Widget loader
      let tvWidgetLoaded = false;
      const loadTradingViewWidget = (symbol) => {
        if (!symbol) return;
        const containerId = `tv-widget-${positionDetail.tradeId}`;
        const container = document.getElementById(containerId);
        if (!container) return;
        // Convert symbol format: BTC/USDT -> BINANCE:BTCUSDT
        // Handle Freqtrade futures format: BTC/USDT:USDT -> BINANCE:BTCUSDT
        const rawSymbol = symbol.split(':')[0];
        const tvSymbol = 'BINANCE:' + rawSymbol.replace(/[/]/g, '').toUpperCase();
        container.innerHTML = '';
        const script = document.createElement('script');
        script.src = 'https://s3.tradingview.com/external-embedding/embed-widget-advanced-chart.js';
        script.async = true;
        script.innerHTML = JSON.stringify({
          autosize: true,
          symbol: tvSymbol,
          interval: '60',
          timezone: 'Asia/Shanghai',
          theme: 'light',
          style: '1',
          locale: 'zh_CN',
          enable_publishing: false,
          hide_top_toolbar: false,
          hide_legend: false,
          save_image: false,
          hide_volume: true,
          support_host: 'https://www.tradingview.com',
        });
        container.appendChild(script);
        tvWidgetLoaded = true;
      };

	      const changeFieldLabel = (field) => {

	        switch ((field || '').toLowerCase()) {
	          case 'state_json':
	            return 'State';
	          case 'status':
	            return '状态';
	          case 'plan_init':
	            return '初始化';
	          default:
	            return field;
	        }
	      };

	      const planChangeSourceLabel = (source) => {
	        const s = (source || '').toString().trim();
	        if (!s) return 'manual';
	        if (s.startsWith('llm:update_exit_plan:')) return 'LLM · update_exit_plan';
	        if (s === 'Manual/Admin') return 'Admin · 手动调整';
	        if (s === 'entry_fill') return '系统 · entry_fill';
	        if (s === 'manual_open') return '系统 · manual_open';
	        return s;
	      };

	      const shortTrace = (trace) => {
	        const t = (trace || '').toString().trim();
	        if (!t) return '';
	        if (t.length <= 10) return t;
	        return `${t.slice(0, 8)}…`;
	      };

	      const planChangeReasonLines = (reason) => {
	        const raw = (reason || '').toString();
	        if (!raw.trim()) return [];
	        const idx = raw.indexOf('变化详情:');
	        const head = (idx >= 0 ? raw.slice(0, idx) : raw).trim();
	        return head
	          .split('\n')
	          .map((l) => l.trim())
	          .filter((l) => l.length);
	      };

	      const planChangeGroups = computed(() => {
	        const list = positionDetail.planChanges || [];
	        if (!list.length) return [];
	        const groups = new Map();
	        list.forEach((log) => {
	          const src = (log.triggerSource || '').toString().trim() || 'manual';
	          const created = log.createdAt || '';
	          const ts = Date.parse(created) || 0;
	          const sec = ts ? Math.floor(ts / 1000) : 0;
	          const key = src.startsWith('llm:update_exit_plan:')
	            ? `src:${src}`
	            : `src:${src}:sec:${sec}`;
	          const cur = groups.get(key) || {
	            key,
	            createdAt: created,
	            triggerSource: src,
	            decisionTraceId: (log.decisionTraceId || '').toString().trim(),
	            logs: [],
	            lines: [],
	          };
	          cur.logs.push(log);
	          if (!cur.createdAt || ts > (Date.parse(cur.createdAt) || 0)) {
	            cur.createdAt = created;
	          }
	          planChangeReasonLines(log.reason).forEach((line) => cur.lines.push(line));
	          groups.set(key, cur);
	        });
	        const out = Array.from(groups.values()).map((g) => ({
	          ...g,
	          sourceLabel: planChangeSourceLabel(g.triggerSource),
	          traceShort: shortTrace(g.decisionTraceId),
	          lines: Array.from(new Set(g.lines)),
	        }));
	        out.sort((a, b) => (Date.parse(b.createdAt) || 0) - (Date.parse(a.createdAt) || 0));
	        return out;
	      });


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
        const preview = manualOpenPreview.value;
        if (preview.errors && preview.errors.length) {
          showToast(preview.errors[0], 'error');
          return;
        }
        manualOpen.submitting = true;
        try {
          const payload = {
            symbol,
            side: manualOpen.side,
            leverage: Number(manualOpen.leverage) || 0,
            position_size_usd: Number(manualOpen.position_size_usd) || 0,
            entry_price: Number(manualOpen.entry_price) || 0,
            stop_loss: Number(manualOpen.stop_loss) || 0,
            take_profit: Number(manualOpen.take_profit) || 0,
            exit_combo: manualOpen.exit_combo || '',
            tier1_target: Number(manualOpen.tier1_target) || 0,
            tier1_ratio: Number(manualOpen.tier1_ratio) || 0,
            tier2_target: Number(manualOpen.tier2_target) || 0,
            tier2_ratio: Number(manualOpen.tier2_ratio) || 0,
            tier3_target: Number(manualOpen.tier3_target) || 0,
            tier3_ratio: Number(manualOpen.tier3_ratio) || 0,
            sl_tier1_target: Number(manualOpen.sl_tier1_target) || 0,
            sl_tier1_ratio: Number(manualOpen.sl_tier1_ratio) || 0,
            sl_tier2_target: Number(manualOpen.sl_tier2_target) || 0,
            sl_tier2_ratio: Number(manualOpen.sl_tier2_ratio) || 0,
            sl_tier3_target: Number(manualOpen.sl_tier3_target) || 0,
            sl_tier3_ratio: Number(manualOpen.sl_tier3_ratio) || 0,
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

      const openManualConfirm = () => {
        const preview = manualOpenPreview.value;
        if (preview.errors && preview.errors.length) {
          showToast(preview.errors[0], 'error');
          return;
        }
        manualOpen.showConfirm = true;
      };

      const confirmManualOpen = async () => {
        if (manualOpen.submitting) return;
        manualOpen.showConfirm = false;
        await submitManualOpen();
      };

      const fillEntryPriceFromLast = () => {
        if (manualOpen.price && manualOpen.price.last > 0) {
          manualOpen.entry_price = manualOpen.price.last;
        } else {
          showToast('暂无最新价，请先刷新价格', 'error');
        }
      };

      const openImagePreview = (img) => {
        if (!img || !img.data_uri) return;
        imagePreview.src = img.data_uri;
        imagePreview.desc = (img.description || '').trim();
        imagePreview.visible = true;
      };

      const closeImagePreview = () => {
        imagePreview.visible = false;
        imagePreview.src = '';
        imagePreview.desc = '';
      };

      const handleResize = () => {
        if (typeof window === 'undefined') return;
        const mobile = window.innerWidth <= 768;
        const changed = mobile !== isMobile.value;
        isMobile.value = mobile;
        if (changed) {
          navCollapsed.value = mobile;
        }
      };

      onMounted(() => {
        handleResize();
        window.addEventListener('resize', handleResize);
        switchView(view.value);
      });

      onBeforeUnmount(() => {
        if (typeof window === 'undefined') return;
        window.removeEventListener('resize', handleResize);
      });

      // Watch for position data to load TradingView widget
      watch(
        () => positionDetail.data,
        (newData) => {
          if (newData && newData.symbol && view.value === 'positionDetail') {
            // Use nextTick to ensure DOM is rendered
            setTimeout(() => loadTradingViewWidget(newData.symbol), 100);
          }
        }
      );

      watch(
        () => [manualOpen.symbol, availableExitCombos.value.map((c) => normalizeComboKey(c.key)).join('|')],
        () => {
          const options = availableExitCombos.value || [];
          if (!options.length) {
            manualOpen.exit_combo = '';
            return;
          }
          const current = manualExitCombo.value;
          const normalized = options.map((c) => normalizeComboKey(c.key));
          if (!current || !normalized.includes(current)) {
            manualOpen.exit_combo = options[0].key;
          }
        },
        { immediate: true },
      );

      return {
        navCollapsed,
        isMobile,
        showSystemPrompt,
        showUserPrompt,
        expandMobileSteps,
        stepExpandAvailable,
        view,
        headline,
        desk,
        filteredDeskDecisions,
        filteredDeskPositions,
        strategyCards,
        enabledSymbols,
        decisions,
        positions,
        decisionDetail,
        decisionFilters,
        decisionProviders,
        filteredDecisionSteps,
        sharedPrompts,
        positionDetail,
        logs,
        manualOpen,
        availableExitCombos,
        manualOpenFlags,
        manualTierSum,
        manualSlTierSum,
        manualOpenPreview,
        defaultSymbols,
        toast,
        imagePreview,
        loadingAny,
        decisionFlowCanPage,
        decisionFlowHasPrev,
        decisionFlowHasNext,
        positionTotalPages,
        switchView,
        refreshCurrent,
        refreshPositionsPagePnL,
        openDecision,
        openStrategyConfig,
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
        stopLossDistancePct,
        nearestTriggerDistancePct,
        statusLabel,
        statusBadgeClass,
        decisionHeadline,
        decisionHeadlineAction,
        forceClosePosition,
        refreshPositionPnL,
        loadManualPrice,
        fillEntryPriceFromLast,
        submitManualOpen,
        openManualConfirm,
        confirmManualOpen,
        openImagePreview,
        closeImagePreview,
        formatDuration,
        stageLabel,
        stageDetail,
        stageBadgeClass,
        extractCsvBlocks,
        cleanUserPrompt,
        mergedAgentPrompt,
        csvBlockKey,
        sharedCsvBlockKey,
        isCsvExpanded,
        toggleCsvBlock,
        setStageFilter: (val) => {
          decisionFilters.stage = val;
          expandMobileSteps.value = false;
        },
        operationLabel,
        loadLogs,
        planSummaryLines,
        planComponentLabel,
	        planInstanceStateLine,
	        planStatusLabel,
	        changeFieldLabel,
	        planChangeGroups,
	        planAdjustForm,
        tierEdits,
        atrEdits,
        planTierGroups,
        atrComponents,
        planEditError,
        planEditPreviewLines,
        tierEditsSubmitting,
        submitTierEdits,
        openPlanAdjust,
        closePlanAdjust,
        submitPlanAdjust,
        exitLevels,
        entryLevelPosition,
        currentPricePosition,
        loadTradingViewWidget,
        chartLevelChips,
      };
    },
  });

  app.config.compilerOptions.delimiters = ['[[', ']]'];
  app.mount('#app');
})();

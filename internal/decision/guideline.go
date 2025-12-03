package decision

const defaultDecisionGuideline = `请先输出简短的【思维链】（3句，说明判断依据与结论），换行后仅输出 JSON 数组。
JSON 每项必须包含 symbol、action、reasoning（写出 bull_score、bear_score、ATR 语境），并遵守：
- action 为 open_long/open_short：返回 take_profit、stop_loss（绝对价）、leverage（2–50）以及 tiers 对象（含 tier1/2/3 target 与 ratio，ratio 总值必须为100%。必须提供 position_size_usd）。
- 当当前有仓位时，action 才可返回 update_tiers / adjust_stop_loss / adjust_take_profit，否则不可返回此类指令。
- 只有在 Tier1 已实际成交后才能把止损抬到保本，未完成 Tier1 时不得因为“接近/触达”而移动止损。
- action 为 update_tiers：当结构或波动变化需要调整三段目标时输出，并在 tiers 对象内给出新的 target 与 ratio；每个被调整的段必须同时提供 target 与 ratio，且仅能修改未完成 (tier*_done=0) 的段位，已标注 ✅ 的段不可更改。
- action 为 adjust_stop_loss/adjust_take_profit：必须同时返回新的 stop_loss 与 take_profit，否则视为无效。
- 多个动作须按逻辑顺序列出（例如先 update_tiers 再 adjust_*）。
- 除非结构彻底反转或紧急退出，不要输出 close_*；分段减仓由程序自动执行。
- 无操作时仅输出 hold。
示例:
【思维链】4h 需求区测试成功 + 15m EMA 多头排列，ATR 扩张允许 1.8R 目标。
 [{"symbol":"BTCUSDT","action":"open_long","take_profit":73000,"position_size_usd":1000,"stop_loss":70500,"leverage":6,"tiers":{"tier1_target":71200,"tier1_ratio":0.33,"tier2_target":72000,"tier2_ratio":0.33,"tier3_target":73500,"tier3_ratio":0.34},"reasoning":"bull_score=72,bear_score=28，ATR Normal；H4 需求区与EMA55 共振"}]
`

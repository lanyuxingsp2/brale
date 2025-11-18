#!/bin/bash
curl -X POST http://35.229.213.239:9991/api/live/freqtrade/webhook \
  -H "Content-Type: application/json" \
  -d '{"type":"entry_fill","trade_id":10,"pair":"BTC/USDT","direction":"long","amount":0.001,"order_rate":93880.7}'

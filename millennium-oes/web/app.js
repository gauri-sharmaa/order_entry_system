// -----------------------------------------------------------------------
// Millennium OES — Frontend (Full Dashboard)
// -----------------------------------------------------------------------

const API = '';
let orders = {};
let currentSide = 'buy';
let killSwitchActive = false;
let eventSource = null;

// -----------------------------------------------------------------------
// Init
// -----------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  loadOrders();
  loadDashboard();
  connectSSE();
  onOrderTypeChange();
  setInterval(loadDashboard, 5000);
});

// -----------------------------------------------------------------------
// Dashboard data (account, positions, risk, strategies, stats)
// -----------------------------------------------------------------------

async function loadDashboard() {
  // Account
  try {
    const res = await fetch(`${API}/api/account`);
    const d = await res.json();
    document.getElementById('hdrEquity').textContent = fmt$(parseFloat(d.equity) || 0);
    document.getElementById('hdrCash').textContent = fmt$(parseFloat(d.cash) || 0);
    document.getElementById('hdrBP').textContent = fmt$(parseFloat(d.buying_power) || 0);
  } catch (e) {}

  // Risk
  try {
    const res = await fetch(`${API}/api/risk`);
    const d = await res.json();
    document.getElementById('hdrPnl').textContent = fmtPnl(d.daily_pnl);
    document.getElementById('hdrPnl').style.color = d.daily_pnl >= 0 ? 'var(--green)' : 'var(--red)';
    document.getElementById('rSharpe').textContent = (d.sharpe_ratio || 0).toFixed(2);
    document.getElementById('rDD').textContent = (d.max_drawdown_pct || 0).toFixed(2) + '%';
    document.getElementById('rVaR').textContent = fmt$(d.daily_var_95 || 0);
    document.getElementById('rWin').textContent = (d.win_rate_pct || 0).toFixed(1) + '%';
    document.getElementById('rTrades').textContent = d.total_trades || 0;
    document.getElementById('rConc').textContent = (d.concentration_pct || 0).toFixed(1) + '%';
    killSwitchActive = d.kill_switch;
    document.getElementById('killSwitchBtn').classList.toggle('active', killSwitchActive);
  } catch (e) {}

  // Stats (latency)
  try {
    const res = await fetch(`${API}/api/stats`);
    const d = await res.json();
    const lat = d.avg_latency_us || 0;
    document.getElementById('hdrLatency').textContent = lat < 1000 ? lat.toFixed(0) + 'μs' : (lat/1000).toFixed(1) + 'ms';
  } catch (e) {}

  // Positions
  try {
    const res = await fetch(`${API}/api/portfolio`);
    const positions = await res.json();
    renderPositions(positions);
  } catch (e) {}

  // Strategies
  try {
    const res = await fetch(`${API}/api/strategies`);
    const strats = await res.json();
    renderStrategies(strats);
    updateStrategyDropdown(strats);
  } catch (e) {}
}

// -----------------------------------------------------------------------
// SSE
// -----------------------------------------------------------------------

function connectSSE() {
  if (eventSource) eventSource.close();
  eventSource = new EventSource(`${API}/api/stream`);

  eventSource.addEventListener('connected', () => {
    setConnectionStatus(true);
    log('Connected to engine stream', 'success');
  });

  eventSource.addEventListener('order', (e) => {
    const order = JSON.parse(e.data);
    updateOrderInBlotter(order);
    const stratTag = order.strategy ? ` [${order.strategy}]` : '';
    log(`Order #${order.id} → ${order.status.toUpperCase()} | ${order.symbol} ${order.side} ${order.qty}${stratTag}`,
        order.status === 'filled' ? 'success' : order.status === 'rejected' ? 'error' : 'info');
  });

  eventSource.onerror = () => {
    setConnectionStatus(false);
    setTimeout(connectSSE, 2000);
  };
}

function setConnectionStatus(connected) {
  const dot = document.getElementById('connectionDot');
  dot.classList.toggle('connected', connected);
}

// -----------------------------------------------------------------------
// Order Entry
// -----------------------------------------------------------------------

function setSide(side) {
  currentSide = side;
  document.getElementById('sideBuy').classList.toggle('active', side === 'buy');
  document.getElementById('sideSell').classList.toggle('active', side === 'sell');
  document.getElementById('sideShort').classList.toggle('active', side === 'sell_short');
  const btn = document.getElementById('submitBtn');
  btn.classList.remove('buy-mode', 'sell-mode');
  if (side === 'buy') btn.classList.add('buy-mode');
  else btn.classList.add('sell-mode');
}

function onOrderTypeChange() {
  const type = document.getElementById('orderType').value;
  hide('limitPriceRow'); hide('stopPriceRow'); hide('trailRow');
  hide('touchRow'); hide('bracketRow'); hide('icebergRow'); hide('algoRow');

  switch (type) {
    case 'LIMIT': show('limitPriceRow'); break;
    case 'STOP': show('stopPriceRow'); break;
    case 'STOP_LIMIT': show('limitPriceRow'); show('stopPriceRow'); break;
    case 'TRAILING_STOP': show('trailRow'); break;
    case 'MARKET_IF_TOUCHED': show('touchRow'); break;
    case 'LIMIT_IF_TOUCHED': show('touchRow'); show('limitPriceRow'); break;
    case 'BRACKET': show('limitPriceRow'); show('bracketRow'); break;
    case 'OCO': show('limitPriceRow'); show('stopPriceRow'); break;
    case 'TWAP': case 'VWAP': show('algoRow'); break;
    case 'ICEBERG': show('limitPriceRow'); show('icebergRow'); break;
    case 'LIMIT_ON_OPEN': case 'LIMIT_ON_CLOSE': case 'FUNARI': show('limitPriceRow'); break;
  }
  document.getElementById('submitBtn').textContent = `SUBMIT ${type.replace(/_/g,' ')}`;
}

async function submitOrder() {
  const symbol = document.getElementById('symbol').value.trim().toUpperCase();
  const type = document.getElementById('orderType').value;
  const tif = document.getElementById('tif').value;
  const qty = parseInt(document.getElementById('qty').value) || 0;
  const strategy = document.getElementById('strategySelect').value;

  if (!symbol) return showMessage('Symbol is required', 'error');
  if (qty <= 0) return showMessage('Quantity must be > 0', 'error');

  const body = { symbol, side: currentSide, type, qty, time_in_force: tif };
  if (strategy) body.strategy = strategy;

  const limitPrice = parseFloat(document.getElementById('limitPrice').value);
  const stopPrice = parseFloat(document.getElementById('stopPrice').value);
  if (!isNaN(limitPrice) && limitPrice > 0) body.price = limitPrice;
  if (!isNaN(stopPrice) && stopPrice > 0) body.stop_price = stopPrice;

  if (type === 'TRAILING_STOP') {
    body.trail_type = document.getElementById('trailType').value;
    body.trail_value = parseFloat(document.getElementById('trailValue').value) || 0;
  }

  try {
    const res = await fetch(`${API}/api/orders`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const data = await res.json();
    if (!res.ok) {
      showMessage(data.error || 'Rejected', 'error');
      log(`REJECTED: ${data.error}`, 'error');
      return;
    }
    showMessage(`Order #${data.id} submitted`, 'success');
    log(`SUBMITTED: ${symbol} ${currentSide.toUpperCase()} ${qty} @ ${type}${strategy ? ' ['+strategy+']' : ''}`, 'success');
    loadOrders();
  } catch (err) {
    showMessage('Network error', 'error');
  }
}

async function cancelOrder(orderID) {
  try {
    await fetch(`${API}/api/orders/${orderID}`, { method: 'DELETE' });
    log(`Cancel submitted for #${orderID}`, 'warn');
    loadOrders();
  } catch (e) {}
}

async function cancelAllOrders() {
  if (!confirm('Cancel all open orders?')) return;
  const active = Object.values(orders).filter(o =>
    ['new','pending_new','acknowledged','partially_filled','held'].includes(o.status));
  for (const o of active) await cancelOrder(o.id);
}

// -----------------------------------------------------------------------
// Data Loading
// -----------------------------------------------------------------------

async function loadOrders() {
  try {
    const res = await fetch(`${API}/api/orders`);
    const data = await res.json();
    orders = {};
    renderBlotter(data);
  } catch (e) {}
}

async function fetchQuote() {
  const symbol = document.getElementById('symbol').value.trim().toUpperCase();
  if (!symbol) return;
  try {
    const res = await fetch(`${API}/api/quote/${symbol}`);
    const data = await res.json();
    if (data.price) document.getElementById('quotePrice').textContent = '$' + data.price.toFixed(2);
  } catch (e) {
    document.getElementById('quotePrice').textContent = '—';
  }
}

// -----------------------------------------------------------------------
// Kill Switch
// -----------------------------------------------------------------------

async function toggleKillSwitch() {
  const newState = !killSwitchActive;
  if (newState && !confirm('ACTIVATE KILL SWITCH?')) return;
  try {
    const res = await fetch(`${API}/api/risk/killswitch`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ active: newState }),
    });
    const data = await res.json();
    killSwitchActive = data.active;
    document.getElementById('killSwitchBtn').classList.toggle('active', killSwitchActive);
    log(killSwitchActive ? '⚡ KILL SWITCH ACTIVATED' : 'Kill switch deactivated',
        killSwitchActive ? 'error' : 'success');
  } catch (e) {}
}

// -----------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------

function renderBlotter(orderList) {
  orders = {};
  orderList.forEach(o => { orders[o.id] = o; });
  const tbody = document.getElementById('ordersBody');
  if (orderList.length === 0) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="12">No orders yet</td></tr>';
    return;
  }
  tbody.innerHTML = orderList.map(renderOrderRow).join('');
}

function addOrderToBlotter(o) {
  orders[o.id] = o;
  const tbody = document.getElementById('ordersBody');
  const emptyRow = tbody.querySelector('.empty-row');
  if (emptyRow) emptyRow.remove();
  const existing = document.getElementById('row-' + o.id);
  if (existing) existing.outerHTML = renderOrderRow(o);
  else tbody.insertAdjacentHTML('afterbegin', renderOrderRow(o));
}

function updateOrderInBlotter(o) {
  orders[o.id] = o;
  const existing = document.getElementById('row-' + o.id);
  if (existing) {
    existing.outerHTML = renderOrderRow(o);
    const row = document.getElementById('row-' + o.id);
    if (row) {
      if (o.status === 'filled') row.classList.add('flash-fill');
      if (o.status === 'cancelled') row.classList.add('flash-cancel');
    }
  } else addOrderToBlotter(o);
}

function renderOrderRow(o) {
  const sideClass = o.side === 'buy' ? 'side-buy' : o.side === 'sell_short' ? 'side-short' : 'side-sell';
  const canCancel = ['new','pending_new','acknowledged','partially_filled','held'].includes(o.status);
  const cancelBtn = canCancel ? `<button class="btn-row-cancel" onclick="cancelOrder('${o.id}')">✕</button>` : '';
  const price = o.price > 0 ? '$' + o.price.toFixed(2) : '—';
  const stopPx = o.stop_price > 0 ? '$' + o.stop_price.toFixed(2) : '—';
  const avgPx = o.filled_avg_px > 0 ? '$' + o.filled_avg_px.toFixed(2) : '—';

  return `<tr id="row-${o.id}">
    <td>${fmtTime(o.created_at)}</td>
    <td><strong>${o.symbol}</strong></td>
    <td class="${sideClass}">${(o.side||'').toUpperCase()}</td>
    <td>${(o.type||'').replace(/_/g,' ')}</td>
    <td>${o.qty}</td>
    <td>${o.filled_qty || 0}</td>
    <td>${price}</td>
    <td>${stopPx}</td>
    <td>${avgPx}</td>
    <td>${(o.tif||'day').toUpperCase()}</td>
    <td><span class="badge badge-${o.status}">${(o.status||'').replace(/_/g,' ').toUpperCase()}</span></td>
    <td>${cancelBtn}</td>
  </tr>`;
}

function renderPositions(positions) {
  const tbody = document.getElementById('positionsBody');
  if (!positions || !Array.isArray(positions) || positions.length === 0) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="5">No positions</td></tr>';
    return;
  }
  tbody.innerHTML = positions.map(p => {
    const pnl = parseFloat(p.unrealized_pl) || 0;
    const pnlClass = pnl >= 0 ? 'pnl-positive' : 'pnl-negative';
    return `<tr>
      <td><strong>${p.symbol}</strong></td>
      <td class="${parseFloat(p.qty) >= 0 ? 'side-buy' : 'side-sell'}">${p.qty}</td>
      <td>$${parseFloat(p.avg_entry_price).toFixed(2)}</td>
      <td>$${parseFloat(p.current_price).toFixed(2)}</td>
      <td class="${pnlClass}">${fmtPnl(pnl)}</td>
    </tr>`;
  }).join('');
}

function renderStrategies(strats) {
  const tbody = document.getElementById('strategiesBody');
  if (!strats || strats.length === 0) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="4">No strategies</td></tr>';
    return;
  }
  tbody.innerHTML = strats.map(s => {
    const pnlClass = s.daily_pnl >= 0 ? 'pnl-positive' : 'pnl-negative';
    return `<tr>
      <td><strong>${s.id}</strong></td>
      <td>${s.order_count}</td>
      <td>${s.fill_count}</td>
      <td class="${pnlClass}">${fmtPnl(s.daily_pnl)}</td>
    </tr>`;
  }).join('');
}

function updateStrategyDropdown(strats) {
  const sel = document.getElementById('strategySelect');
  const current = sel.value;
  sel.innerHTML = '<option value="">None</option>';
  if (strats && strats.length > 0) {
    strats.forEach(s => {
      if (s.active) sel.innerHTML += `<option value="${s.id}">${s.id} — ${s.name}</option>`;
    });
  }
  sel.value = current;
}

function filterOrders() {
  const sym = document.getElementById('filterSymbol').value.toUpperCase();
  const status = document.getElementById('filterStatus').value;
  document.querySelectorAll('#ordersBody tr[id^="row-"]').forEach(row => {
    const o = orders[row.id.replace('row-', '')];
    if (!o) return;
    const matchSym = !sym || (o.symbol||'').includes(sym);
    const matchStatus = !status || o.status === status;
    row.style.display = matchSym && matchStatus ? '' : 'none';
  });
}

// -----------------------------------------------------------------------
// Activity Log
// -----------------------------------------------------------------------

function log(msg, level = 'info') {
  const container = document.getElementById('activityLog');
  const entry = document.createElement('div');
  entry.className = `log-entry ${level}`;
  entry.innerHTML = `<span class="log-time">${fmtTime(new Date().toISOString())}</span><span class="log-msg">${msg}</span>`;
  container.insertBefore(entry, container.firstChild);
  while (container.children.length > 200) container.removeChild(container.lastChild);
}

function clearLog() { document.getElementById('activityLog').innerHTML = ''; }

// -----------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------

function show(id) { const el = document.getElementById(id); if (el) el.style.display = ''; }
function hide(id) { const el = document.getElementById(id); if (el) el.style.display = 'none'; }

function showMessage(msg, type) {
  const el = document.getElementById('orderMessage');
  el.textContent = msg;
  el.className = 'order-message ' + type;
  setTimeout(() => { el.className = 'order-message'; }, 4000);
}

function fmt$(n) {
  if (n == null || isNaN(n)) return '—';
  return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 }).format(n);
}

function fmtPnl(n) {
  if (n == null || isNaN(n) || n === 0) return '$0';
  const sign = n >= 0 ? '+' : '';
  return sign + '$' + Math.abs(n).toFixed(2);
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

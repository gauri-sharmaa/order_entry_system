// -----------------------------------------------------------------------
// Millennium OES — Frontend Application (Bare-Metal Edition)
// -----------------------------------------------------------------------

const API = '';

// -----------------------------------------------------------------------
// State
// -----------------------------------------------------------------------

let orders = {};
let currentSide = 'buy';
let killSwitchActive = false;
let eventSource = null;

// -----------------------------------------------------------------------
// Init
// -----------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  loadOrders();
  loadStats();
  connectSSE();
  onOrderTypeChange();
  setInterval(loadStats, 5000);
});

// -----------------------------------------------------------------------
// SSE — real-time order updates
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
    log(`Order #${order.id} → ${order.status.toUpperCase()} | ${order.symbol} ${order.side} ${order.qty}`,
        order.status === 'filled' ? 'success' : order.status === 'rejected' ? 'error' : 'info');
  });

  eventSource.onerror = () => {
    setConnectionStatus(false);
    log('Stream disconnected — reconnecting...', 'warn');
    setTimeout(connectSSE, 2000);
  };
}

function setConnectionStatus(connected) {
  const dot = document.getElementById('connectionDot');
  dot.classList.toggle('connected', connected);
  dot.title = connected ? 'Connected' : 'Disconnected';
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
  if (side === 'sell' || side === 'sell_short') btn.classList.add('sell-mode');
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
    case 'LIMIT_ON_OPEN': case 'LIMIT_ON_CLOSE': show('limitPriceRow'); break;
    case 'FUNARI': show('limitPriceRow'); break;
  }

  document.getElementById('submitBtn').textContent = `SUBMIT ${type.replace(/_/g,' ')}`;
}

async function submitOrder() {
  const symbol = document.getElementById('symbol').value.trim().toUpperCase();
  const type   = document.getElementById('orderType').value;
  const tif    = document.getElementById('tif').value;
  const qty    = parseInt(document.getElementById('qty').value) || 0;

  if (!symbol) return showMessage('Symbol is required', 'error');
  if (qty <= 0) return showMessage('Quantity must be > 0', 'error');

  const body = { symbol, side: currentSide, type, qty, time_in_force: tif };

  const limitPrice = parseFloat(document.getElementById('limitPrice').value);
  const stopPrice  = parseFloat(document.getElementById('stopPrice').value);

  if (!isNaN(limitPrice) && limitPrice > 0) body.price = limitPrice;
  if (!isNaN(stopPrice)  && stopPrice  > 0) body.stop_price = stopPrice;

  if (type === 'TRAILING_STOP') {
    body.trail_type  = document.getElementById('trailType').value;
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
      showMessage(data.error || 'Order rejected', 'error');
      log(`REJECTED: ${data.error}`, 'error');
      return;
    }

    showMessage(`Order #${data.id} submitted`, 'success');
    log(`SUBMITTED: ${symbol} ${currentSide.toUpperCase()} ${qty} @ ${type}`, 'success');
    loadOrders(); // refresh blotter
  } catch (err) {
    showMessage('Network error: ' + err.message, 'error');
    log('Network error: ' + err.message, 'error');
  }
}

// -----------------------------------------------------------------------
// Order Management
// -----------------------------------------------------------------------

async function cancelOrder(orderID) {
  try {
    const res = await fetch(`${API}/api/orders/${orderID}`, { method: 'DELETE' });
    const data = await res.json();
    if (!res.ok) {
      log(`Cancel failed: ${data.error}`, 'error');
      return;
    }
    log(`Cancel submitted for #${orderID}`, 'warn');
    loadOrders();
  } catch (err) {
    log('Cancel error: ' + err.message, 'error');
  }
}

async function cancelAllOrders() {
  if (!confirm('Cancel all open orders?')) return;
  const active = Object.values(orders).filter(o =>
    ['new','pending_new','acknowledged','partially_filled','held'].includes(o.status));
  for (const o of active) {
    await cancelOrder(o.id);
  }
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
  } catch (err) {
    log('Failed to load orders: ' + err.message, 'error');
  }
}

async function loadStats() {
  try {
    const res = await fetch(`${API}/api/stats`);
    const data = await res.json();
    document.getElementById('equity').textContent = `${data.orders_processed || 0} orders`;
    document.getElementById('cash').textContent = `${(data.avg_latency_us || 0).toFixed(1)}μs avg`;
    document.getElementById('buyingPower').textContent = '—';
  } catch (err) {}

  try {
    const res = await fetch(`${API}/api/risk`);
    const data = await res.json();
    killSwitchActive = data.kill_switch;
    document.getElementById('killSwitchBtn').classList.toggle('active', killSwitchActive);
    const el = document.getElementById('dailyPnl');
    el.textContent = killSwitchActive ? 'HALTED' : 'ACTIVE';
    el.style.color = killSwitchActive ? 'var(--red)' : 'var(--green)';
  } catch (err) {}
}

function fetchQuote() {
  // No quote endpoint in bare-metal mode — prices come from FIX market data
  document.getElementById('quotePrice').textContent = '—';
}

// -----------------------------------------------------------------------
// Kill Switch
// -----------------------------------------------------------------------

async function toggleKillSwitch() {
  const newState = !killSwitchActive;
  if (newState && !confirm('ACTIVATE KILL SWITCH? This will halt all trading.')) return;

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
  } catch (err) {
    log('Kill switch error: ' + err.message, 'error');
  }
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
  if (existing) {
    existing.outerHTML = renderOrderRow(o);
  } else {
    tbody.insertAdjacentHTML('afterbegin', renderOrderRow(o));
  }
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
  } else {
    addOrderToBlotter(o);
  }
}

function renderOrderRow(o) {
  const sideClass = o.side === 'buy' ? 'side-buy' : o.side === 'sell_short' ? 'side-short' : 'side-sell';
  const canCancel = ['new','pending_new','acknowledged','partially_filled','held'].includes(o.status);
  const cancelBtn = canCancel
    ? `<button class="btn-row-cancel" onclick="cancelOrder('${o.id}')">✕</button>`
    : '';

  const price = o.price > 0 ? '$' + o.price.toFixed(2) : '—';
  const stopPx = o.stop_price > 0 ? '$' + o.stop_price.toFixed(2) : '—';
  const avgPx = o.filled_avg_px > 0 ? '$' + o.filled_avg_px.toFixed(2) : '—';

  return `<tr id="row-${o.id}">
    <td>${fmtTime(o.created_at)}</td>
    <td><strong>${o.symbol}</strong></td>
    <td class="${sideClass}">${(o.side || '').toUpperCase()}</td>
    <td>${(o.type || '').replace(/_/g,' ')}</td>
    <td>${o.qty}</td>
    <td>${o.filled_qty || 0}</td>
    <td>${price}</td>
    <td>${stopPx}</td>
    <td>${avgPx}</td>
    <td>${(o.tif || 'day').toUpperCase()}</td>
    <td><span class="badge badge-${o.status}">${(o.status || '').replace(/_/g,' ').toUpperCase()}</span></td>
    <td>${cancelBtn}</td>
  </tr>`;
}

function renderPositions(positions) {
  const tbody = document.getElementById('positionsBody');
  if (!positions || positions.length === 0) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="6">No positions</td></tr>';
    return;
  }
}

function filterOrders() {
  const sym    = document.getElementById('filterSymbol').value.toUpperCase();
  const status = document.getElementById('filterStatus').value;
  const rows   = document.querySelectorAll('#ordersBody tr[id^="row-"]');
  rows.forEach(row => {
    const o = orders[row.id.replace('row-', '')];
    if (!o) return;
    const matchSym    = !sym    || (o.symbol || '').includes(sym);
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

function setTIF(value) { document.getElementById('tif').value = value; }

function enableTIF(values) {
  Array.from(document.getElementById('tif').options).forEach(opt => {
    opt.disabled = !values.includes(opt.value);
  });
}

function fmt$(n) {
  if (n == null || isNaN(n)) return '—';
  return new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 }).format(n);
}

function fmtTime(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

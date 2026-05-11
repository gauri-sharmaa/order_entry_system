// -----------------------------------------------------------------------
// Millennium OES — Frontend Application
// -----------------------------------------------------------------------

const API = '';  // same origin; change to 'http://localhost:8080' for local dev

// -----------------------------------------------------------------------
// State
// -----------------------------------------------------------------------

let orders = {};          // orderID → order object
let currentSide = 'buy';
let killSwitchActive = false;
let eventSource = null;

// -----------------------------------------------------------------------
// Init
// -----------------------------------------------------------------------

document.addEventListener('DOMContentLoaded', () => {
  loadAccount();
  loadOrders();
  loadPositions();
  connectSSE();
  onOrderTypeChange();

  // Refresh account + positions every 10s
  setInterval(() => {
    loadAccount();
    loadPositions();
  }, 10_000);
});

// -----------------------------------------------------------------------
// Server-Sent Events — real-time order updates
// -----------------------------------------------------------------------

function connectSSE() {
  if (eventSource) eventSource.close();

  eventSource = new EventSource(`${API}/api/stream`);

  eventSource.addEventListener('connected', () => {
    setConnectionStatus(true);
    log('Connected to order stream', 'success');
  });

  eventSource.addEventListener('order_update', (e) => {
    const order = JSON.parse(e.data);
    updateOrderInBlotter(order);
    log(`Order ${order.id.slice(0,8)} → ${order.status.toUpperCase()} | ${order.symbol} ${order.side} ${order.qty}`, 
        order.status === 'filled' ? 'success' : order.status === 'rejected' ? 'error' : 'info');
  });

  eventSource.onerror = () => {
    setConnectionStatus(false);
    log('Stream disconnected — reconnecting in 3s...', 'warn');
    setTimeout(connectSSE, 3000);
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
  const tif = document.getElementById('tif');

  // Hide all dynamic rows
  hide('limitPriceRow');
  hide('stopPriceRow');
  hide('trailRow');
  hide('touchRow');
  hide('bracketRow');
  hide('icebergRow');
  hide('algoRow');

  // Reset TIF options
  enableTIF(['day','gtc','ioc','fok','gtd','ato','atc']);

  switch (type) {
    case 'LIMIT':
      show('limitPriceRow');
      break;
    case 'STOP':
      show('stopPriceRow');
      break;
    case 'STOP_LIMIT':
      show('limitPriceRow');
      show('stopPriceRow');
      break;
    case 'MARKET_ON_OPEN':
    case 'LIMIT_ON_OPEN':
      setTIF('ato');
      if (type === 'LIMIT_ON_OPEN') show('limitPriceRow');
      break;
    case 'MARKET_ON_CLOSE':
    case 'LIMIT_ON_CLOSE':
      setTIF('atc');
      if (type === 'LIMIT_ON_CLOSE') show('limitPriceRow');
      break;
    case 'TRAILING_STOP':
      show('trailRow');
      break;
    case 'MARKET_IF_TOUCHED':
      show('touchRow');
      break;
    case 'LIMIT_IF_TOUCHED':
      show('touchRow');
      show('limitPriceRow');
      break;
    case 'BRACKET':
      show('limitPriceRow');
      show('bracketRow');
      break;
    case 'OCO':
      show('limitPriceRow');
      show('stopPriceRow');
      break;
    case 'OTO':
      show('limitPriceRow');
      break;
    case 'TWAP':
    case 'VWAP':
      show('algoRow');
      break;
    case 'ICEBERG':
      show('limitPriceRow');
      show('icebergRow');
      break;
    case 'FUNARI':
      show('limitPriceRow');
      break;
  }

  // Update submit button label
  document.getElementById('submitBtn').textContent = `SUBMIT ${type.replace(/_/g,' ')}`;
}

async function submitOrder() {
  const symbol = document.getElementById('symbol').value.trim().toUpperCase();
  const type   = document.getElementById('orderType').value;
  const tif    = document.getElementById('tif').value;
  const qty    = parseFloat(document.getElementById('qty').value) || 0;

  if (!symbol) return showMessage('Symbol is required', 'error');
  if (qty <= 0) return showMessage('Quantity must be > 0', 'error');

  const body = {
    symbol,
    side: currentSide,
    type,
    qty,
    time_in_force: tif,
    extended_hours: document.getElementById('extendedHours').checked,
  };

  // Attach price fields based on type
  const limitPrice = parseFloat(document.getElementById('limitPrice').value);
  const stopPrice  = parseFloat(document.getElementById('stopPrice').value);
  const trailValue = parseFloat(document.getElementById('trailValue').value);
  const touchPrice = parseFloat(document.getElementById('touchPrice').value);

  if (!isNaN(limitPrice) && limitPrice > 0) body.limit_price = limitPrice;
  if (!isNaN(stopPrice)  && stopPrice  > 0) body.stop_price  = stopPrice;
  if (!isNaN(touchPrice) && touchPrice > 0) body.touch_price = touchPrice;

  if (type === 'TRAILING_STOP') {
    body.trail_type  = document.getElementById('trailType').value;
    body.trail_value = trailValue;
  }

  if (type === 'BRACKET') {
    const tp = parseFloat(document.getElementById('takeProfitPrice').value);
    const sl = parseFloat(document.getElementById('stopLossPrice').value);
    if (!isNaN(tp) && tp > 0) body.take_profit_price = tp;
    if (!isNaN(sl) && sl > 0) body.stop_loss_price   = sl;
  }

  if (type === 'OCO') {
    // Second leg: stop order at stop price
    body.oco_pair = {
      symbol,
      side: currentSide,
      type: 'STOP',
      qty,
      time_in_force: tif,
      stop_price: stopPrice,
    };
  }

  if (type === 'ICEBERG') {
    const vq = parseFloat(document.getElementById('visibleQty').value);
    if (!isNaN(vq) && vq > 0) body.visible_qty = vq;
  }

  if (type === 'TWAP' || type === 'VWAP') {
    const endTimeStr = document.getElementById('algoEndTime').value;
    const slices     = parseInt(document.getElementById('algoSlices').value) || 10;
    if (endTimeStr) {
      const [h, m] = endTimeStr.split(':').map(Number);
      const end = new Date();
      end.setHours(h, m, 0, 0);
      body.algo_params = { end_time: end.toISOString(), slice_count: slices };
    }
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

    showMessage(`Order submitted: ${data.id.slice(0,8)}`, 'success');
    log(`SUBMITTED: ${data.symbol} ${data.side.toUpperCase()} ${data.qty} @ ${type}`, 'success');
    addOrderToBlotter(data);

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
    log(`Cancelled order ${orderID.slice(0,8)}`, 'warn');
    updateOrderInBlotter(data);
  } catch (err) {
    log('Cancel error: ' + err.message, 'error');
  }
}

async function cancelAllOrders() {
  if (!confirm('Cancel all open orders?')) return;
  try {
    const res = await fetch(`${API}/api/orders`, { method: 'DELETE' });
    const data = await res.json();
    log(`Cancelled ${data.cancelled} orders`, 'warn');
    loadOrders();
  } catch (err) {
    log('Cancel all error: ' + err.message, 'error');
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

async function loadPositions() {
  try {
    const res = await fetch(`${API}/api/positions`);
    const data = await res.json();
    renderPositions(data);
  } catch (err) {
    // Silently fail — positions may not be available in paper mode
  }
}

async function loadAccount() {
  try {
    const res = await fetch(`${API}/api/account`);
    const data = await res.json();
    document.getElementById('equity').textContent      = fmt$(data.equity);
    document.getElementById('cash').textContent        = fmt$(data.cash);
    document.getElementById('buyingPower').textContent = fmt$(data.buying_power);
  } catch (err) {
    // Silently fail
  }

  // Also load daily P&L from risk endpoint
  try {
    const res = await fetch(`${API}/api/risk`);
    const data = await res.json();
    const pnl = data.daily_pnl || 0;
    const el = document.getElementById('dailyPnl');
    el.textContent = (pnl >= 0 ? '+' : '') + fmt$(pnl);
    el.style.color = pnl >= 0 ? 'var(--green)' : 'var(--red)';

    killSwitchActive = data.kill_switch_active;
    document.getElementById('killSwitchBtn').classList.toggle('active', killSwitchActive);
  } catch (err) {}
}

async function fetchQuote() {
  const symbol = document.getElementById('symbol').value.trim().toUpperCase();
  if (!symbol) return;
  try {
    const res = await fetch(`${API}/api/quote/${symbol}`);
    const data = await res.json();
    if (data.price) {
      document.getElementById('quotePrice').textContent = '$' + data.price.toFixed(2);
    }
  } catch (err) {}
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
    const newRow = document.createElement('tr');
    newRow.innerHTML = renderOrderRow(o);
    const tr = newRow.firstElementChild || newRow;
    existing.outerHTML = renderOrderRow(o);

    // Flash animation
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
    : '—';

  return `<tr id="row-${o.id}">
    <td>${fmtTime(o.created_at)}</td>
    <td><strong>${o.symbol}</strong></td>
    <td class="${sideClass}">${o.side.toUpperCase()}</td>
    <td>${o.type.replace(/_/g,' ')}</td>
    <td>${o.qty}</td>
    <td>${o.filled_qty || 0}</td>
    <td>${o.limit_price ? '$'+o.limit_price.toFixed(2) : '—'}</td>
    <td>${o.stop_price  ? '$'+o.stop_price.toFixed(2)  : '—'}</td>
    <td>${o.filled_avg_px ? '$'+o.filled_avg_px.toFixed(2) : '—'}</td>
    <td>${o.time_in_force.toUpperCase()}</td>
    <td><span class="badge badge-${o.status}">${o.status.replace(/_/g,' ').toUpperCase()}</span></td>
    <td>${cancelBtn}</td>
  </tr>`;
}

function renderPositions(positions) {
  const tbody = document.getElementById('positionsBody');
  if (!positions || positions.length === 0) {
    tbody.innerHTML = '<tr class="empty-row"><td colspan="6">No positions</td></tr>';
    return;
  }
  tbody.innerHTML = positions.map(p => {
    const pnlClass = p.unrealized_pl >= 0 ? 'pnl-positive' : 'pnl-negative';
    const pnlSign  = p.unrealized_pl >= 0 ? '+' : '';
    return `<tr>
      <td><strong>${p.symbol}</strong></td>
      <td class="${p.side === 'long' ? 'side-buy' : 'side-sell'}">${p.qty}</td>
      <td>$${parseFloat(p.avg_entry_price).toFixed(2)}</td>
      <td>$${parseFloat(p.current_price).toFixed(2)}</td>
      <td>${fmt$(p.market_value)}</td>
      <td class="${pnlClass}">${pnlSign}${fmt$(p.unrealized_pl)}</td>
    </tr>`;
  }).join('');
}

function filterOrders() {
  const sym    = document.getElementById('filterSymbol').value.toUpperCase();
  const status = document.getElementById('filterStatus').value;
  const rows   = document.querySelectorAll('#ordersBody tr[id^="row-"]');
  rows.forEach(row => {
    const o = orders[row.id.replace('row-', '')];
    if (!o) return;
    const matchSym    = !sym    || o.symbol.includes(sym);
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

  // Keep max 200 entries
  while (container.children.length > 200) {
    container.removeChild(container.lastChild);
  }
}

function clearLog() {
  document.getElementById('activityLog').innerHTML = '';
}

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

function setTIF(value) {
  document.getElementById('tif').value = value;
}

function enableTIF(values) {
  const sel = document.getElementById('tif');
  Array.from(sel.options).forEach(opt => {
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
  return d.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit', second: '2-digit' });
}

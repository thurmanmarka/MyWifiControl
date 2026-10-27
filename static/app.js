const base = window.BASE_PATH || '';
const $ = (id) => document.getElementById(id);

const CHANNELS = {
    '2.4': ['auto', 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13],
    '5': ['auto', 36, 40, 44, 48, 52, 56, 60, 64, 100, 104, 108, 112, 116, 120, 124, 128, 132, 136, 140, 144, 149, 153, 157, 161, 165],
};
const WIDTHS = { '2.4': [20, 40], '5': [20, 40, 80, 160] };

let aps = [];

async function api(path, options = {}) {
    const res = await fetch(base + path, {
        ...options,
        headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    });
    let body = {};
    try { body = await res.json(); } catch (_) { /* non-JSON error page */ }
    if (!res.ok) {
        const err = new Error(body.error || `HTTP ${res.status}`);
        err.body = body;
        throw err;
    }
    return body;
}

function currentAP() {
    return aps.find((a) => a.id === $('apSelect').value);
}

function setMsg(el, text, ok) {
    el.textContent = text;
    el.className = 'msg ' + (ok === undefined ? '' : ok ? 'ok' : 'bad');
}

function busy(buttons, on) {
    buttons.forEach((b) => { b.disabled = on; });
}

function option(value, label) {
    const o = document.createElement('option');
    o.value = value;
    o.textContent = label;
    return o;
}

function transcript(results) {
    return (results || [])
        .map((r) => `${r.failed ? '✗' : '›'} ${r.command}\n${r.output}`.trimEnd())
        .join('\n\n');
}

function buildRadioCards() {
    const ap = currentAP();
    const wrap = $('radioCards');
    wrap.replaceChildren();
    if (!ap) return;

    for (const band of ap.bands) {
        const card = document.createElement('div');
        card.className = 'radio-card';
        const h = document.createElement('h3');
        h.textContent = `${band} GHz radio`;

        const ch = document.createElement('select');
        ch.id = `ch-${band}`;
        ch.appendChild(option('', 'unchanged'));
        CHANNELS[band].forEach((c) => ch.appendChild(option(String(c), c === 'auto' ? 'Auto' : `Channel ${c}`)));

        const wd = document.createElement('select');
        wd.id = `wd-${band}`;
        wd.appendChild(option('', 'unchanged'));
        WIDTHS[band].forEach((w) => wd.appendChild(option(String(w), `${w} MHz`)));

        const grid = document.createElement('div');
        grid.className = 'grid';
        const l1 = document.createElement('label');
        l1.append('Channel', ch);
        const l2 = document.createElement('label');
        l2.append('Channel width', wd);
        grid.append(l1, l2);

        card.append(h, grid);
        wrap.appendChild(card);
    }
}

function changeRequest() {
    const ap = currentAP();
    return {
        wlan: Number($('wlan').value) || 0,
        ssid: $('ssid').value.trim(),
        passphrase: $('passphrase').value,
        band: $('band').value,
        radios: ap.bands
            .map((band) => ({ band, channel: $(`ch-${band}`).value, width: $(`wd-${band}`).value }))
            .filter((r) => r.channel || r.width),
        snmp: snmpChange(),
    };
}

function snmpChange() {
    const enabled = $('snmpEnabled').value;
    const s = {
        enabled: enabled === '' ? null : enabled === 'on',
        read_community: $('snmpRead').value,
        write_community: $('snmpWrite').value,
    };
    return s.enabled === null && !s.read_community && !s.write_community ? null : s;
}

function statCard(k, v) {
    const d = document.createElement('div');
    d.className = 'stat';
    const kk = document.createElement('div');
    kk.className = 'k';
    kk.textContent = k;
    const vv = document.createElement('div');
    vv.className = 'v';
    vv.textContent = v || '—';
    d.append(kk, vv);
    return d;
}

// Fill the WLAN picker, keeping the current choice. Until the AP has been read
// we only know the configured default.
function buildWlanSelect(wlans) {
    const ap = currentAP();
    const sel = $('wlan');
    const keep = sel.value || String(ap ? ap.wlan : 1);
    sel.replaceChildren();
    const list = wlans && wlans.length ? wlans : [{ index: ap ? ap.wlan : 1 }];
    for (const w of list) {
        sel.appendChild(option(String(w.index), w.ssid ? `WLAN ${w.index}: ${w.ssid}` : `WLAN ${w.index}`));
    }
    sel.value = list.some((w) => String(w.index) === keep) ? keep : String(list[0].index);
}

// secretCard shows a secret the AP only keeps encrypted (Wi-Fi password, SNMP
// community). s = { saved, set_by, set_at, configured }; path is the reveal API.
function secretCard(title, s, path) {
    const d = statCard(title, s.saved ? '••••••••' : 'not saved');
    const v = d.querySelector('.v');
    v.classList.add('secret');

    const note = document.createElement('div');
    note.className = 'note';
    if (s.configured === false) {
        v.textContent = 'not set';
        note.textContent = 'Not configured on the AP.';
        d.appendChild(note);
        return d;
    }
    if (!s.saved) {
        note.textContent = 'Set it through Wi-Fi Control to save it here.';
        d.appendChild(note);
        return d;
    }
    const when = s.set_at ? new Date(s.set_at).toLocaleString() : '';
    note.textContent = `Set by ${s.set_by || '?'} ${when}`;

    const btn = document.createElement('button');
    btn.className = 'btn secondary';
    btn.textContent = '🔑 Show';
    let shown = false;
    btn.addEventListener('click', async () => {
        if (shown) {
            v.textContent = '••••••••';
            btn.textContent = '🔑 Show';
            shown = false;
            return;
        }
        btn.disabled = true;
        try {
            const data = await api(path);
            v.textContent = data.secret;
            btn.textContent = 'Hide';
            shown = true;
            loadHistory(); // viewing a secret is logged
        } catch (e) {
            note.textContent = e.message;
            note.classList.add('warn');
        } finally {
            btn.disabled = false;
        }
    });
    d.append(note, btn);
    return d;
}

async function readStatus() {
    const ap = currentAP();
    if (!ap) return;
    const btn = $('refreshBtn');
    busy([btn], true);
    setMsg($('statusMsg'), `Connecting to ${ap.host} over SSH…`);
    try {
        const data = await api(`/api/aps/${encodeURIComponent(ap.id)}/status`);
        const cur = $('current');
        cur.replaceChildren();
        const row = (kind) => {
            const d = document.createElement('div');
            d.className = `current-row ${kind}`;
            cur.appendChild(d);
            return d;
        };
        const apPath = `/api/aps/${encodeURIComponent(ap.id)}`;
        for (const w of data.status.wlans || []) {
            row('wlan').append(
                statCard(`WLAN ${w.index} SSID${w.enabled ? '' : ' (off)'}`, w.ssid),
                statCard(`WLAN ${w.index} band`, w.band),
                secretCard(`WLAN ${w.index} password`,
                    { saved: w.password_saved, set_by: w.password_set_by, set_at: w.password_set_at },
                    `${apPath}/wlans/${w.index}/password`),
            );
        }
        buildWlanSelect(data.status.wlans);
        const radios = data.status.radios || [];
        if (radios.length) {
            const r = row('radios');
            for (const radio of radios) {
                const name = radio.band ? `${radio.band} GHz` : `Radio ${radio.index}`;
                r.append(
                    statCard(`${name} channel${radio.enabled ? '' : ' (off)'}`, radio.channel),
                    statCard(`${name} width`, radio.width ? `${radio.width} MHz` : ''),
                );
            }
        }
        const snmp = data.status.snmp;
        if (snmp) {
            row('snmp').append(
                statCard('SNMP agent', (snmp.enabled ? 'On' : 'Off') + (snmp.port ? ` (port ${snmp.port})` : '')),
                secretCard('SNMP read community', snmp.read_community, `${apPath}/snmp/read`),
                secretCard('SNMP write community', snmp.write_community, `${apPath}/snmp/write`),
            );
        }
        if (!cur.children.length) {
            const m = document.createElement('div');
            m.className = 'muted';
            m.textContent = 'Connected, but no wireless settings were recognised. Check the raw output and the profile in config.yaml.';
            cur.appendChild(m);
        }
        $('rawOut').textContent = data.raw || '(empty)';
        setMsg($('statusMsg'), `Read at ${new Date().toLocaleTimeString()}`, true);
    } catch (e) {
        $('rawOut').textContent = transcript(e.body && e.body.results) || '—';
        setMsg($('statusMsg'), e.message, false);
    } finally {
        busy([btn], false);
    }
}

async function previewOrApply(apply) {
    const ap = currentAP();
    if (!ap) return;
    const req = changeRequest();
    const out = $('changeOut');
    const btns = [$('previewBtn'), $('applyBtn')];

    if (apply) {
        const snmpWarn = req.snmp && (req.snmp.read_community || req.snmp.enabled === false)
            ? '\n\nSNMP monitors (like My-SNMP-App) will lose access to this AP until they use the new read community.'
            : '';
        const ok = confirm(
            `Apply these changes to ${ap.name}?\n\n` +
            'Devices on Wi-Fi will disconnect while the AP reconfigures. ' +
            'If you change the SSID or password you will need to reconnect.' + snmpWarn
        );
        if (!ok) return;
    }

    busy(btns, true);
    setMsg($('changeMsg'), apply ? 'Applying…' : '');
    try {
        const data = await api(`/api/aps/${encodeURIComponent(ap.id)}/${apply ? 'apply' : 'preview'}`, {
            method: 'POST',
            body: JSON.stringify(req),
        });
        out.classList.remove('hidden');
        if (apply) {
            out.textContent = transcript(data.results);
            if (data.warning) {
                setMsg($('changeMsg'), data.warning, false);
            } else {
                const saved = (data.saved || []).length ? ` Saved 🔐: ${data.saved.join(', ')}.` : '';
                setMsg($('changeMsg'), `Applied.${saved} Current settings are refreshing behind this window.`, true);
            }
            readStatus(); // refresh the dashboard behind the dialog
            for (const id of ['passphrase', 'snmpRead', 'snmpWrite']) $(id).value = '';
        } else {
            out.textContent = data.commands.join('\n');
            setMsg($('changeMsg'), 'These commands will be sent (nothing has changed yet).');
        }
    } catch (e) {
        if (e.body && e.body.results) {
            out.classList.remove('hidden');
            out.textContent = transcript(e.body.results);
        }
        setMsg($('changeMsg'), e.message, false);
    } finally {
        busy(btns, false);
        if (apply) loadHistory();
    }
}

async function runConsole() {
    const ap = currentAP();
    if (!ap) return;
    const commands = $('consoleIn').value.split('\n').map((s) => s.trim()).filter(Boolean);
    if (!commands.length) return;
    const btn = $('consoleBtn');
    const out = $('consoleOut');
    busy([btn], true);
    out.classList.remove('hidden');
    out.textContent = 'Running…';
    try {
        const data = await api(`/api/aps/${encodeURIComponent(ap.id)}/console`, {
            method: 'POST',
            body: JSON.stringify({ commands }),
        });
        out.textContent = transcript(data.results) + (data.error ? `\n\n[${data.error}]` : '');
    } catch (e) {
        out.textContent = e.message;
    } finally {
        busy([btn], false);
        loadHistory();
    }
}

// ---- Live clients -----------------------------------------------------------
// Polls the AP over SNMP while the page is visible. Throughput is the change in
// a byte counter between two polls; each client and radio keeps a few minutes
// of samples for its sparklines.

const LIVE_KEEP_MS = 5 * 60e3;
const WEAK_SNR_DB = 20; // below this a client's link is marginal
const SVG_NS = 'http://www.w3.org/2000/svg';

let liveTimer = null;
let liveBusy = false;
let livePrev = null; // last snapshot, for rates
let liveHist = new Map(); // mac or "radio-N" -> [{t, snr, mbps}]

function loadPref(key, fallback) {
    try { return localStorage.getItem(key) || fallback; } catch (_) { return fallback; }
}
function savePref(key, value) {
    try { localStorage.setItem(key, value); } catch (_) { /* private mode */ }
}

function scheduleLive(delay) {
    clearTimeout(liveTimer);
    const every = Number($('liveRate').value) * 1000;
    if (!every || document.hidden) return;
    liveTimer = setTimeout(pollLive, delay === undefined ? every : delay);
}

function resetLive() {
    livePrev = null;
    liveHist = new Map();
    $('liveRadios').replaceChildren();
    $('liveClients').replaceChildren();
    $('liveNote').textContent = '';
}

async function pollLive() {
    const ap = currentAP();
    if (!ap || liveBusy) return;
    liveBusy = true;
    try {
        const st = await api(`/api/aps/${encodeURIComponent(ap.id)}/live`);
        if (ap === currentAP()) {
            renderLive(ap, st);
            if ($('liveMsg').dataset.kind === 'poll') setMsg($('liveMsg'), '');
        }
    } catch (e) {
        setMsg($('liveMsg'), e.message, false);
        $('liveMsg').dataset.kind = 'poll';
    } finally {
        liveBusy = false;
        scheduleLive();
    }
}

// Mbps between two readings of a byte counter; null when there's no earlier
// reading or the counter went backwards (client reconnected, AP rebooted).
function mbps(prev, cur, seconds) {
    if (prev === undefined || cur < prev || seconds <= 0) return null;
    return (cur - prev) * 8 / seconds / 1e6;
}

function remember(key, t, sample) {
    const list = liveHist.get(key) || [];
    list.push({ t, ...sample });
    while (list.length && t - list[0].t > LIVE_KEEP_MS) list.shift();
    liveHist.set(key, list);
    return list;
}

function renderLive(ap, st) {
    const t = Date.parse(st.at);
    const secs = livePrev ? (t - livePrev.t) / 1000 : 0;
    const prevClients = livePrev ? livePrev.clients : new Map();
    const prevRadios = livePrev ? livePrev.radios : new Map();

    const radios = $('liveRadios');
    radios.replaceChildren();
    for (const r of st.radios) {
        const prev = prevRadios.get(r.index) || {};
        const tx = mbps(prev.tx, r.tx_bytes, secs), rx = mbps(prev.rx, r.rx_bytes, secs);
        const total = tx === null || rx === null ? null : tx + rx;
        const hist = remember(`radio-${r.index}`, t, { mbps: total });

        const card = statCard(`${r.band || `Radio ${r.index}`} · channel ${r.channel}, ${r.width_mhz} MHz`, '');
        const v = card.querySelector('.v');
        v.replaceChildren(
            Object.assign(document.createElement('span'), { textContent: total === null ? '…' : `${fmtMbps(total)} Mbps` }),
            sparkline(hist.map((h) => h.mbps), { min: 0 }),
        );
        card.appendChild(Object.assign(document.createElement('div'), {
            className: 'meta',
            textContent: `${r.clients} client${r.clients === 1 ? '' : 's'} · noise ${r.noise_dbm} dBm · airtime ${r.airtime_pct}% · ${r.tx_power_dbm} dBm tx`,
        }));
        radios.appendChild(card);
    }

    const body = $('liveClients');
    body.replaceChildren();
    const seen = new Set();
    const clients = st.clients.slice().sort((a, b) => (a.name || a.ip || a.mac).localeCompare(b.name || b.ip || b.mac));
    for (const c of clients) {
        seen.add(c.mac);
        const prev = prevClients.get(c.mac) || {};
        const tx = mbps(prev.tx, c.tx_bytes, secs), rx = mbps(prev.rx, c.rx_bytes, secs);
        const total = tx === null || rx === null ? null : tx + rx;
        const hist = remember(c.mac, t, { snr: c.snr_db, mbps: total });
        body.appendChild(clientRow(ap, c, total, hist));
    }
    if (!clients.length) {
        const tr = document.createElement('tr');
        const td = Object.assign(document.createElement('td'), { colSpan: 6, className: 'muted', textContent: 'No clients connected.' });
        tr.appendChild(td);
        body.appendChild(tr);
    }
    // A client that left starts a fresh history if it comes back
    for (const key of liveHist.keys()) {
        if (!key.startsWith('radio-') && !seen.has(key)) liveHist.delete(key);
    }

    livePrev = {
        t,
        clients: new Map(st.clients.map((c) => [c.mac, { tx: c.tx_bytes, rx: c.rx_bytes }])),
        radios: new Map(st.radios.map((r) => [r.index, { tx: r.tx_bytes, rx: r.rx_bytes }])),
    };
    $('liveNote').textContent = `${st.clients.length} connected · updated ${new Date(t).toLocaleTimeString()}`;
}

function clientRow(ap, c, total, hist) {
    const tr = document.createElement('tr');
    const cell = (...kids) => {
        const td = document.createElement('td');
        td.append(...kids);
        tr.appendChild(td);
        return td;
    };
    const line = (text, cls) => Object.assign(document.createElement('div'), { textContent: text, className: cls || '' });
    const metric = (num, spark, weak) => {
        const d = document.createElement('div');
        d.className = 'metric';
        d.append(Object.assign(document.createElement('span'), { className: 'num' + (weak ? ' weak' : ''), textContent: num }), spark);
        return d;
    };

    cell(line(c.name || c.mac), line([c.ip, c.name ? c.mac : '', c.vendor].filter(Boolean).join(' · '), 'sub'));
    cell(line(c.ssid), line(`${c.band} · ${c.mode}`, 'sub'));
    const weak = c.snr_db < WEAK_SNR_DB;
    cell(metric(`${c.snr_db} dB`, sparkline(hist.map((h) => h.snr), { min: 0, weakBelow: WEAK_SNR_DB }), weak));
    cell(metric(total === null ? '…' : `${fmtMbps(total)} Mbps`, sparkline(hist.map((h) => h.mbps), { min: 0 })));
    cell(`${Math.round(c.rate_mbps)} Mbps`);

    const btn = Object.assign(document.createElement('button'), { className: 'btn secondary', textContent: 'Disconnect' });
    btn.addEventListener('click', () => disconnectClient(ap, c, btn));
    cell(btn);
    return tr;
}

function fmtMbps(v) {
    return v < 10 ? v.toFixed(2) : v.toFixed(1);
}

// sparkline draws the last LIVE_KEEP_MS of samples, newest at the right. Nulls
// (no rate yet) break the line. weakBelow colours the line when the latest
// value is under it.
function sparkline(values, { min, max, weakBelow } = {}) {
    const W = 110, H = 26;
    const svg = document.createElementNS(SVG_NS, 'svg');
    svg.setAttribute('class', 'spark');
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    svg.setAttribute('aria-hidden', 'true');
    const nums = values.filter((v) => v !== null && v !== undefined);
    if (nums.length < 2) return svg;

    const lo = min !== undefined ? min : Math.min(...nums);
    let hi = max !== undefined ? max : Math.max(...nums);
    if (hi <= lo) hi = lo + 1;
    const step = W / Math.max(values.length - 1, 1);
    let d = '', pen = false;
    values.forEach((v, i) => {
        if (v === null || v === undefined) { pen = false; return; }
        const x = i * step, y = H - 1 - (Math.min(v, hi) - lo) / (hi - lo) * (H - 2);
        d += `${pen ? 'L' : 'M'}${x.toFixed(1)},${y.toFixed(1)}`;
        pen = true;
    });
    const path = document.createElementNS(SVG_NS, 'path');
    path.setAttribute('d', d);
    if (weakBelow !== undefined && nums[nums.length - 1] < weakBelow) path.setAttribute('class', 'weak');
    svg.appendChild(path);
    return svg;
}

async function disconnectClient(ap, c, btn) {
    const label = c.name || c.mac;
    const ok = confirm(
        `Disconnect ${label} from "${c.ssid}"?\n\n` +
        'This is a one-off kick, not a block: most devices reconnect within a few seconds.'
    );
    if (!ok) return;
    btn.disabled = true;
    const msg = $('liveMsg');
    msg.dataset.kind = 'action';
    setMsg(msg, `Disconnecting ${label}…`);
    try {
        await api(`/api/aps/${encodeURIComponent(ap.id)}/clients/${encodeURIComponent(c.mac)}/disconnect`, { method: 'POST', body: '{}' });
        setMsg(msg, `Disconnected ${label} at ${new Date().toLocaleTimeString()}.`, true);
    } catch (e) {
        setMsg(msg, `Could not disconnect ${label}: ${e.message}`, false);
    } finally {
        btn.disabled = false;
        loadHistory();
        scheduleLive(1000); // show it leaving (or coming back) soon
    }
}

const HISTORY_ACTIONS = {
    apply: 'Applied settings', console: 'Ran console commands', view_secret: 'Viewed a saved secret',
    disconnect: 'Disconnected a client',
};

// loadHistory shows the change log for the selected AP. The panel stays hidden
// when the app runs without a database.
async function loadHistory() {
    const ap = currentAP();
    if (!ap) return;
    let data;
    try {
        data = await api(`/api/aps/${encodeURIComponent(ap.id)}/history`);
    } catch (e) {
        $('historyPanel').classList.remove('hidden');
        $('historyList').replaceChildren(Object.assign(document.createElement('div'), { className: 'muted', textContent: e.message }));
        return;
    }
    $('historyPanel').classList.toggle('hidden', !data.enabled);
    const list = $('historyList');
    list.replaceChildren();
    if (!data.entries.length) {
        list.appendChild(Object.assign(document.createElement('div'), { className: 'muted', textContent: 'Nothing recorded yet.' }));
        return;
    }
    for (const e of data.entries) {
        const item = document.createElement('div');
        item.className = 'history-item';

        const head = document.createElement('div');
        head.className = 'history-head';
        const when = Object.assign(document.createElement('span'), {
            className: 'history-when',
            textContent: new Date(e.at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }),
        });
        const what = Object.assign(document.createElement('span'), {
            className: 'history-what', textContent: HISTORY_ACTIONS[e.action] || e.action,
        });
        const who = Object.assign(document.createElement('span'), { className: 'muted', textContent: `by ${e.user}` });
        const result = Object.assign(document.createElement('span'), {
            className: 'history-result ' + (e.ok ? 'ok' : 'bad'),
            textContent: e.ok ? '✓ OK' : '✕ Failed',
        });
        head.append(when, what, who, result);
        item.appendChild(head);

        if (e.detail) item.appendChild(Object.assign(document.createElement('div'), { className: 'history-detail', textContent: e.detail }));
        if (e.error) item.appendChild(Object.assign(document.createElement('div'), { className: 'history-error', textContent: e.error }));
        if (e.commands && e.commands.length) {
            const details = document.createElement('details');
            const summary = document.createElement('summary');
            summary.textContent = `${e.commands.length} command${e.commands.length > 1 ? 's' : ''}`;
            const pre = document.createElement('pre');
            pre.textContent = e.commands.join('\n');
            details.append(summary, pre);
            item.appendChild(details);
        }
        list.appendChild(item);
    }
}

async function init() {
    try {
        const list = await api('/api/aps');
        aps = list.access_points;
        const sel = $('apSelect');
        aps.forEach((a) => sel.appendChild(option(a.id, `${a.name} (${a.host})`)));
        if (!aps.length) {
            setMsg($('statusMsg'), 'No access points configured in config.yaml.', false);
            return;
        }
        buildRadioCards();
        buildWlanSelect();
        loadHistory();
        scheduleLive(0);
        sel.addEventListener('change', () => {
            buildRadioCards();
            $('wlan').value = '';
            buildWlanSelect();
            loadHistory();
            resetLive();
            scheduleLive(0);
        });
    } catch (e) {
        setMsg($('statusMsg'), e.message, false);
    }
}

const changeDialog = $('changeDialog');
$('openChangeBtn').addEventListener('click', () => changeDialog.showModal());
$('closeChangeBtn').addEventListener('click', () => changeDialog.close());
// Close on a backdrop click. The dialog's own padding also reports the dialog
// as the target, so check the click really landed outside its box.
changeDialog.addEventListener('click', (e) => {
    if (e.target !== changeDialog) return;
    const r = changeDialog.getBoundingClientRect();
    if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
        changeDialog.close();
    }
});

$('refreshBtn').addEventListener('click', readStatus);
$('previewBtn').addEventListener('click', () => previewOrApply(false));
$('applyBtn').addEventListener('click', () => previewOrApply(true));
$('consoleBtn').addEventListener('click', runConsole);

$('liveRate').value = loadPref('wifiLiveRate', '3');
$('liveRate').addEventListener('change', () => {
    savePref('wifiLiveRate', $('liveRate').value);
    livePrev = null; // a paused gap shouldn't count as one slow sample
    scheduleLive(0);
});
// Stop polling the AP while the tab is in the background
document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
        clearTimeout(liveTimer);
    } else {
        livePrev = null;
        scheduleLive(0);
    }
});
init();

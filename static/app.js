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
        sel.addEventListener('change', () => {
            buildRadioCards();
            $('wlan').value = '';
            buildWlanSelect();
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
init();

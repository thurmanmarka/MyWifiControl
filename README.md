# MyWifiControl 💦📶

### *W*ireless *A*ccess *P*oint. Obviously. What did you think it stood for?

A firmware update made the house WAP play hard to get. The web GUI won't let
anybody touch her settings anymore. But she still opens up for **SSH**, and
that's all we need.

MyWifiControl lets you change your Cambium WAP's SSID, Wi-Fi password,
broadcast band, channel and channel width over SSH, from a tasteful page inside
MyHomeServicesHub.

- She lives on port **8084** and receives visitors at **`https://<pi>/wifi`**, through the hub only
- **Read from AP**: get to know her first. Current SSID, band, channel and width.
- **Preview**: foreplay. See every command before anything actually goes in.
- **Apply**: the main event.
- **CLI console**: for admins who like to talk dirty to her in raw CLI. Also great for checking your firmware's exact syntax.
- **Clients**: who's over right now. Signal, throughput and link rate every few seconds, and a **Disconnect** button for anyone overstaying their welcome.
- **Every change is logged with the hub username.** She kisses and tells. (`journalctl -u wifi-control`)

## Consent 🔒

**Admin only.** Nobody touches the WAP without the right credentials.

- Every route except `/health` (the page, its assets, reading, preview, apply,
  console) gives `user` and `guest` a firm **403**.
- Anything that sneaks in without going through the hub gets a **401**.
- The hub enforces the same thing on its side, and only shows the tile to admins.

She trusts the hub's `X-Hub-User` / `X-Hub-Role` headers, which is only safe
because she binds to `127.0.0.1`. That means only the hub on the same Pi can
get anywhere near her. Keep it monogamous.

## She remembers 🔐

The WAP keeps her Wi-Fi passwords and SNMP communities to herself: salted `$crypt$1$` blobs that
come out different every single time she shows them, even when nothing changed.
(We checked. Several times.) So Wi-Fi Control keeps its own diary:

- Every password you set through the app is saved to `secrets.json`, AES-256-GCM
  encrypted with `secret.key` (generated on first run, never committed).
- Admins get a **🔑 Show** button on each network. Every peek is logged:
  `🔑 admin viewed the saved password for house WLAN 2`.
- **SNMP too.** Her read and write communities get the same treatment: turn
  the agent on/off, set either community, and **🔑 Show** the copy the app
  saved. Change the read community and anything polling her (My-SNMP-App)
  needs the new one, or it's left on read.
- **She can't be caught cheating.** Because she re-salts on every read, there's
  no way to tell if someone re-set the password behind the app's back
  (cnMaestro, a reset, the ISP). The saved copy is what *the app* last set.
- Passwords set before this existed show **not saved**. Set one through the app
  once and she'll remember it from then on.
- She never shows her `$crypt$` values to the browser, either. The AP accepts
  them back as-is, so they're as good as the real thing.

Back up `secrets.json` **and** `secret.key` together. Lose the key and the diary
is unreadable.

## She keeps receipts 🧾

Give her a MariaDB database and every apply, console session and **🔑 Show**
is written to a `change_log` table, and the page grows a **Change history**
panel: who, when, what changed in plain words (`SSID → "Home"; 5 GHz channel
36, 80 MHz`), whether it worked, and the commands she was sent.

- Secrets never make it in. Applied commands are masked the same way as the
  preview, and anything after `passphrase`, `password`, `community` and friends
  is starred out of console commands too.
- Without a database she still logs everything to the journal, she just
  doesn't show it on the page.

```yaml
database:
  enabled: true
  password: "..."   # host 127.0.0.1, port 3306, name wifi_db, user wifi_app by default
```

The nightly MariaDB backup picks up `wifi_db` on its own. `secrets.json` and
`secret.key` are files, not database rows, so back those up separately.

## Who's over? 📡

The **Clients** panel reads her radio and client tables over **SNMP** (two bulk
walks, under a second) every 2–10 seconds while the page is open and the tab is
in front. SSH would hold her CLI lock for every refresh; SNMP doesn't.

- Per radio: throughput, clients, channel, noise floor, airtime, transmit power.
- Per client: SNR and throughput with a 5-minute sparkline, link rate, SSID, band,
  Wi-Fi mode, IP, MAC, vendor. SNR under 20 dB shows red.
- Throughput is the change in her byte counters between two reads. The history
  lives in your browser only; My-SNMP-App keeps the long-term charts.
- The read community comes from `snmp_community` on the access point in
  `config.yaml`, or else the read community Wi-Fi Control saved when it last set one.

**Disconnect** sends one 802.11 deauth over SSH. The CLI has no command for it,
but `service radio` passes through to the Qualcomm driver tools:

```
service radio iwpriv wlan<(radio-1)*16 + (wlan-1)> kickmac aa:bb:cc:dd:ee:ff
```

The radio and WLAN come from her own client table, not the browser. It's a kick,
not a block: most devices are back within seconds. Every disconnect goes in the
journal and the change log with who did it.

## Getting her set up (Raspberry Pi)

```bash
cd MyWifiControl
cp config.example.yaml config.yaml
nano config.yaml          # her IP, plus your SSH username/password
./install.sh              # builds for ARM, installs to /home/myhomeservices/wifi-control, starts the service
```

Then redeploy the hub. It has new `/wifi` routes in `main.go` and `nginx.conf`,
plus role enforcement for every service:

```bash
cd ../MyHomeServicesHub
go build -o hub-gateway . && sudo cp hub-gateway /home/myhomeservices/hub/
sudo cp nginx.conf /etc/nginx/nginx.conf && sudo nginx -t && sudo systemctl reload nginx
sudo systemctl restart hub-gateway
```

## Your first time 🕯️

Go slow. Read the room.

1. **Make sure she's into it.** From the Pi, run `ssh admin@<ap-ip>` by hand
   and note her prompt (e.g. `E410-1A2B3C#`).
2. **Always use protection.** Open `/wifi` and click **Read from AP**. Grab her
   host key fingerprint from
   `sudo journalctl -u wifi-control | grep "host key"`, put it in
   `host_key_fingerprint:` in `config.yaml`, and restart the service. From then
   on, any imposter pretending to be your WAP gets refused. 🛡️
3. **Learn what she likes.** In the **CLI console**, run `show config`
   and check that:
   - the `wireless wlan N` block holding your SSID matches `wlan:` in config
   - `wireless radio 1/2` match the `radios:` band mapping
4. **Foreplay.** Click **Preview commands** for a small change and compare it
   against what she actually accepts in the console (`show wireless`, `show config`).
5. **Apply.** Wi-Fi clients will drop briefly while she reconfigures. Everyone
   needs a minute afterwards. Run the hub on a **wired** connection so you
   don't get kicked out of bed mid-change.

## Size matters 📏

| | 2.4 GHz | 5 GHz |
|---|---|---|
| Channels | 1–13, or `auto` | 36–165, or `auto` |
| Channel width | 20 / 40 MHz | 20 / 40 / 80 / 160 MHz |

Pick a wrong combination (say, 80 MHz on 2.4 GHz) and the app will gently but
firmly tell you no. We don't judge your frequency, but some things just don't fit.

## Her love language (when the defaults don't work)

We got to know her properly: an **XV2-2 on firmware 7.2-r1**. The built-in
`cnpilot` profile whispers exactly what she likes:

```
wireless wlan 1
ssid "SSID"
passphrase *******
band all
exit
wireless radio 2
channel 149
channel-width 80
exit
apply
save
```

Things we learned the hard way:

- **She skips the small talk.** SSH drops you straight into
  `WAP(config)#`, so there's no `configure terminal`.
- **`\r`, not `\n`.** Send her a bare newline and she echoes it back and does
  absolutely nothing. She only responds to a proper carriage return.
- **`show config`**, not `show running-config`.
- **`band all`**, not `band both`.
- **`apply`, then `save`.** `apply` makes it happen now. `save` makes her
  remember it in the morning (after a reboot).
- **Quotes only when needed.** Values with spaces get quoted, the way she
  writes them herself. And no `?` in an SSID or password: on her CLI, `?` pops
  up the help menu mid-sentence.

Different WAP, different kinks? Copy the commented `profiles:` block from
`config.example.yaml` into `config.yaml` and adjust it. No rebuild needed, just
a restart. Each line is a Go template, and lines that render empty are skipped.

**She's not responding?** If **Read from AP** times out, run a few commands in
the console and compare her prompt to `prompt:` in the profile.

**She keeps saying `--More--`?** That's the pager, and it's handled. She gets a
space every time she asks, until she's done. 😏

**Safe word:** `exit`.

## Development (in private)

```bash
go test ./...      # includes a fake Cambium SSH server, for practice
```

To play with her locally without the hub, set `server.allow_direct: true`
(every request is then treated as admin) and open http://127.0.0.1:8084/.
Turn it back off before she goes out in public.

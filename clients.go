package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// The live client view reads the AP over SNMP rather than SSH: two bulk walks
// take well under a second, so the page can refresh every few seconds, where
// an SSH login per refresh would hold the AP's CLI lock most of the time.
// Disconnecting a client is a change, so that goes over SSH like the others.

// CAMBIUM-MIB tables under cambiumStateGroup (1.3.6.1.4.1.17713.22.1)
const (
	oidRadioTable  = "1.3.6.1.4.1.17713.22.1.2.1"
	oidClientTable = "1.3.6.1.4.1.17713.22.1.3.1"
)

// Radio table columns
const (
	radioBand    = 3 // "2.4GHz", "5GHz"
	radioClients = 5
	radioChannel = 6
	radioWidth   = 7 // MHz
	radioTxPower = 8 // dBm
	radioTxBytes = 11
	radioRxBytes = 12
	radioNoise   = 16 // dBm, as text
	radioAirtime = 18 // "total/tx/rx/busy" %
)

// Client table columns
const (
	clientMAC     = 2 // "AA-BB-CC-DD-EE-FF"
	clientIP      = 3
	clientName    = 4
	clientSSID    = 5
	clientVendor  = 6
	clientMode    = 7  // "bgn", "ac", "axa"...
	clientRadio   = 8  // 1-based, the same numbering as "wireless radio N"
	clientWLAN    = 9  // "wireless wlan N"
	clientSNR     = 11 // dB
	clientRate    = 12 // Mbps, as text
	clientTxBytes = 14
	clientRxBytes = 16
)

// LiveStatus is a snapshot of the AP's radios and clients. Byte counters are
// raw; the page turns consecutive snapshots into rates.
type LiveStatus struct {
	At      time.Time    `json:"at"`
	Radios  []LiveRadio  `json:"radios"`
	Clients []LiveClient `json:"clients"`
}

type LiveRadio struct {
	Index      int    `json:"index"` // 1-based, as in "wireless radio N"
	Band       string `json:"band"`
	Channel    string `json:"channel"`
	WidthMHz   string `json:"width_mhz"`
	TxPowerDBm int    `json:"tx_power_dbm"`
	NoiseDBm   int    `json:"noise_dbm"`
	AirtimePct int    `json:"airtime_pct"`
	Clients    int    `json:"clients"`
	TxBytes    uint64 `json:"tx_bytes"`
	RxBytes    uint64 `json:"rx_bytes"`
}

type LiveClient struct {
	MAC      string  `json:"mac"` // aa:bb:cc:dd:ee:ff
	IP       string  `json:"ip"`
	Name     string  `json:"name"`
	SSID     string  `json:"ssid"`
	Vendor   string  `json:"vendor"`
	Mode     string  `json:"mode"`
	Band     string  `json:"band"`
	Radio    int     `json:"radio"`
	WLAN     int     `json:"wlan"`
	SNR      int     `json:"snr_db"`
	RateMbps float64 `json:"rate_mbps"`
	TxBytes  uint64  `json:"tx_bytes"`
	RxBytes  uint64  `json:"rx_bytes"`
}

var errNoCommunity = errors.New("no SNMP read community: set snmp_community for this AP in config.yaml, or set the read community through Wi-Fi Control")

// snmpCommunity prefers config.yaml, then the read community Wi-Fi Control
// saved when it last set one.
func snmpCommunity(ap *AccessPoint) (string, error) {
	if ap.SNMPCommunity != "" {
		return ap.SNMPCommunity, nil
	}
	_, c, err := secrets.Reveal(snmpKey(ap.ID, "read"))
	if errors.Is(err, os.ErrNotExist) {
		return "", errNoCommunity
	}
	return c, err
}

func readLive(ctx context.Context, ap *AccessPoint) (*LiveStatus, error) {
	community, err := snmpCommunity(ap)
	if err != nil {
		return nil, err
	}
	client := &gosnmp.GoSNMP{
		Context:   ctx,
		Target:    ap.Host,
		Port:      uint16(ap.SNMPPort),
		Community: community,
		Version:   gosnmp.Version2c,
		Timeout:   3 * time.Second,
		Retries:   1,
	}
	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("snmp connect: %w", err)
	}
	defer client.Conn.Close()

	radios, err := walkTable(client, oidRadioTable)
	if err != nil {
		return nil, fmt.Errorf("snmp: %w", err)
	}
	clients, err := walkTable(client, oidClientTable)
	if err != nil {
		return nil, fmt.Errorf("snmp: %w", err)
	}

	st := &LiveStatus{At: time.Now(), Radios: []LiveRadio{}, Clients: []LiveClient{}}
	bandOf := map[int]string{}
	for _, idx := range sortedRows(radios) {
		row := radios[idx]
		n, _ := strconv.Atoi(idx)
		r := LiveRadio{
			Index:      n + 1, // the radio table counts from 0
			Band:       pduString(row[radioBand]),
			Channel:    pduString(row[radioChannel]),
			WidthMHz:   pduString(row[radioWidth]),
			TxPowerDBm: pduInt(row[radioTxPower]),
			NoiseDBm:   textInt(row[radioNoise]),
			AirtimePct: textInt(strings.Split(pduString(row[radioAirtime]), "/")[0]),
			Clients:    pduInt(row[radioClients]),
			TxBytes:    pduUint(row[radioTxBytes]),
			RxBytes:    pduUint(row[radioRxBytes]),
		}
		bandOf[r.Index] = r.Band
		st.Radios = append(st.Radios, r)
	}
	for _, idx := range sortedRows(clients) {
		row := clients[idx]
		mac, ok := normalizeMAC(pduString(row[clientMAC]))
		if !ok {
			continue
		}
		c := LiveClient{
			MAC:     mac,
			IP:      pduString(row[clientIP]),
			Name:    strings.TrimSpace(pduString(row[clientName])),
			SSID:    pduString(row[clientSSID]),
			Vendor:  pduString(row[clientVendor]),
			Mode:    pduString(row[clientMode]),
			Radio:   pduInt(row[clientRadio]),
			WLAN:    pduInt(row[clientWLAN]),
			SNR:     pduInt(row[clientSNR]),
			TxBytes: pduUint(row[clientTxBytes]),
			RxBytes: pduUint(row[clientRxBytes]),
		}
		c.Band = bandOf[c.Radio]
		c.RateMbps, _ = strconv.ParseFloat(strings.TrimSpace(pduString(row[clientRate])), 64)
		st.Clients = append(st.Clients, c)
	}
	return st, nil
}

// walkTable bulk-walks an SNMP table into row index -> column -> value.
func walkTable(client *gosnmp.GoSNMP, entryOID string) (map[string]map[int]any, error) {
	pdus, err := client.BulkWalkAll(entryOID)
	if err != nil {
		return nil, err
	}
	prefix := entryOID + "."
	rows := map[string]map[int]any{}
	for _, p := range pdus {
		col, idx, ok := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(p.Name, "."), prefix), ".")
		n, err := strconv.Atoi(col)
		if !ok || err != nil {
			continue
		}
		if rows[idx] == nil {
			rows[idx] = map[int]any{}
		}
		rows[idx][n] = p.Value
	}
	return rows, nil
}

func sortedRows(rows map[string]map[int]any) []string {
	idxs := make([]string, 0, len(rows))
	for k := range rows {
		idxs = append(idxs, k)
	}
	// Rows are single numbers in both tables
	sort.Slice(idxs, func(i, j int) bool {
		a, _ := strconv.Atoi(idxs[i])
		b, _ := strconv.Atoi(idxs[j])
		return a < b
	})
	return idxs
}

func pduString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []byte:
		return string(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func pduInt(v any) int {
	if v == nil {
		return 0
	}
	return int(gosnmp.ToBigInt(v).Int64())
}

func pduUint(v any) uint64 {
	if v == nil {
		return 0
	}
	return gosnmp.ToBigInt(v).Uint64()
}

// textInt parses numbers the AP sends as text, such as a noise floor of "-107"
func textInt(v any) int {
	n, _ := strconv.Atoi(strings.TrimSpace(pduString(v)))
	return n
}

var macRe = regexp.MustCompile(`^[0-9a-fA-F]{2}([:-][0-9a-fA-F]{2}){5}$`)

// normalizeMAC accepts aa:bb:.. or AA-BB-.. and returns lower-case aa:bb:..,
// the form the radio driver takes.
func normalizeMAC(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !macRe.MatchString(s) {
		return "", false
	}
	return strings.ToLower(strings.ReplaceAll(s, "-", ":")), true
}

// vapIndex is the radio driver's interface number for a WLAN on a radio:
// cnPilot names them wlan<(radio-1)*16 + (wlan-1)> (wlan0 = radio 1 WLAN 1,
// wlan17 = radio 2 WLAN 2). Checked against "wlanconfig wlanN list sta" on an
// XV2-2 running 7.2-r1.
func vapIndex(radio, wlan int) int {
	return (radio-1)*16 + (wlan - 1)
}

func handleLive(w http.ResponseWriter, r *http.Request) {
	ap, _, ok := findAP(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	st, err := readLive(ctx, ap)
	if errors.Is(err, errNoCommunity) {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

// handleDisconnect kicks one client off the AP (an 802.11 deauth). It isn't a
// block: the client may reconnect straight away.
func handleDisconnect(w http.ResponseWriter, r *http.Request) {
	ap, p, ok := findAP(w, r)
	if !ok {
		return
	}
	mac, ok := normalizeMAC(r.PathValue("mac"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad MAC address")
		return
	}
	if len(p.Disconnect) == 0 {
		writeError(w, http.StatusNotImplemented, fmt.Sprintf("profile %q has no disconnect command", ap.Profile))
		return
	}

	// Don't stop when the browser goes away: the client being kicked may be
	// the one asking.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), requestBudget)
	defer cancel()

	// The radio and WLAN the client is on come from the AP itself, not the page
	st, err := readLive(ctx, ap)
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not look up the client: "+err.Error())
		return
	}
	var client *LiveClient
	for i := range st.Clients {
		if st.Clients[i].MAC == mac {
			client = &st.Clients[i]
		}
	}
	if client == nil {
		writeError(w, http.StatusNotFound, mac+" is not connected to "+ap.Name)
		return
	}
	if client.Radio < 1 || client.WLAN < 1 || client.WLAN > 16 {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("AP reported radio %d, WLAN %d for %s", client.Radio, client.WLAN, mac))
		return
	}

	cmds, err := renderLines(p.Disconnect, templateData{Radio: client.Radio, WLAN: client.WLAN, MAC: mac})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	user := r.Context().Value(ctxKey("user")).(string)
	label := clientLabel(client)
	log.Printf("🚪 %s disconnecting %s from %s: %q", user, label, ap.ID, cmds)
	entry := ChangeEntry{
		User: user, APID: ap.ID, Action: "disconnect", Commands: cmds,
		Detail: fmt.Sprintf("Disconnected %s from %q (%s)", label, client.SSID, client.Band),
	}

	results, err := RunCommands(ctx, ap, p, cmds)
	// The driver tools print nothing on success, so any output is an error
	// message (e.g. "Invalid argument").
	if err == nil {
		for _, res := range results {
			if strings.TrimSpace(res.Output) != "" {
				err = fmt.Errorf("AP said: %s", strings.TrimSpace(res.Output))
				break
			}
		}
	}
	if err != nil {
		log.Printf("❌ disconnect on %s failed: %v", ap.ID, err)
		entry.Error = err.Error()
		changes.Record(entry)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "results": results})
		return
	}
	entry.OK = true
	changes.Record(entry)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "client": label})
}

func clientLabel(c *LiveClient) string {
	if c.Name != "" {
		return fmt.Sprintf("%s (%s)", c.Name, c.MAC)
	}
	return c.MAC
}

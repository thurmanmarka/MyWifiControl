package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testAP() *AccessPoint {
	return &AccessPoint{ID: "t", Host: "127.0.0.1", Port: 22, Username: "admin", Password: "pw",
		Profile: "cnpilot", WLAN: 1, Radios: map[string]int{"2.4": 1, "5": 2}, TimeoutSeconds: 3}
}

func TestOnlyAdminsAllowed(t *testing.T) {
	cfg = &Config{AccessPoints: []AccessPoint{*testAP()}}
	mux := newMux()
	routes := []struct{ method, path string }{
		{"GET", "/"}, {"GET", "/static/app.js"}, {"GET", "/api/me"}, {"GET", "/api/aps"},
		{"GET", "/api/aps/t/status"}, {"POST", "/api/aps/t/preview"},
		{"POST", "/api/aps/t/apply"}, {"POST", "/api/aps/t/console"},
		{"GET", "/api/aps/t/wlans/1/password"}, {"GET", "/api/aps/t/snmp/read"},
	}
	for _, rt := range routes {
		for role, want := range map[string]int{"user": 403, "guest": 403, "": 403, "none": 401} {
			req := httptest.NewRequest(rt.method, rt.path, strings.NewReader("{}"))
			if role != "none" {
				req.Header.Set("X-Hub-Authenticated", "true")
				req.Header.Set("X-Hub-Role", role)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Errorf("%s %s as %q: got %d, want %d", rt.method, rt.path, role, rec.Code, want)
			}
		}
	}

	req := httptest.NewRequest("GET", "/api/aps", nil)
	req.Header.Set("X-Hub-Authenticated", "true")
	req.Header.Set("X-Hub-Role", "admin")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("admin GET /api/aps: got %d", rec.Code)
	}
	for _, p := range []string{"/health", "/wifi/health"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 200 {
			t.Errorf("%s should stay open, got %d", p, rec.Code)
		}
	}
}

func TestValidate(t *testing.T) {
	ap := testAP()
	cases := []struct {
		name string
		req  ChangeRequest
		ok   bool
	}{
		{"ssid", ChangeRequest{SSID: "Home Net"}, true},
		{"empty", ChangeRequest{}, false},
		{"quote injection", ChangeRequest{SSID: `x"` + "\nreboot"}, false},
		{"newline", ChangeRequest{SSID: "a\nb"}, false},
		{"long ssid", ChangeRequest{SSID: strings.Repeat("a", 33)}, false},
		{"short pass", ChangeRequest{Passphrase: "short"}, false},
		{"help char", ChangeRequest{SSID: "what?"}, false},
		{"edge space", ChangeRequest{Passphrase: " password1"}, false},
		{"band", ChangeRequest{Band: "5GHz"}, true},
		{"all bands", ChangeRequest{Band: "all"}, true},
		{"bad band", ChangeRequest{Band: "6GHz"}, false},
		{"5g channel", ChangeRequest{Radios: []RadioChange{{Band: "5", Channel: "149", Width: "80"}}}, true},
		{"auto", ChangeRequest{Radios: []RadioChange{{Band: "2.4", Channel: "Auto"}}}, true},
		{"5g chan on 2.4", ChangeRequest{Radios: []RadioChange{{Band: "2.4", Channel: "36"}}}, false},
		{"80 on 2.4", ChangeRequest{Radios: []RadioChange{{Band: "2.4", Width: "80"}}}, false},
		{"no 6g radio", ChangeRequest{Radios: []RadioChange{{Band: "6", Channel: "1"}}}, false},
	}
	for _, c := range cases {
		err := c.req.Validate(ap)
		if (err == nil) != c.ok {
			t.Errorf("%s: got err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestBuildChangeCommands(t *testing.T) {
	ap := testAP()
	req := ChangeRequest{SSID: "Home Net", Passphrase: "NoSpaces1", Band: "all",
		Radios: []RadioChange{{Band: "5", Channel: "36", Width: "80"}}}
	if err := req.Validate(ap); err != nil {
		t.Fatal(err)
	}
	cmds, err := BuildChangeCommands(builtinProfiles()["cnpilot"], ap, &req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"wireless wlan 1", `ssid "Home Net"`, "passphrase NoSpaces1", "band all", "exit",
		"wireless radio 2", "channel 36", "channel-width 80", "exit",
		"apply", "save",
	}
	if strings.Join(cmds, "|") != strings.Join(want, "|") {
		t.Errorf("got  %q\nwant %q", cmds, want)
	}
}

// Trimmed from a real XV2-2 "show config".
const sampleConfig = `wireless radio 1
 no shutdown
 frequency-band 2.4GHz
 channel auto
 channel-width 20
!
wireless radio 2
 frequency-band 5GHz
 channel 149
 channel-width 80
!
wireless wlan 1
 ssid "My Home"
 no shutdown
 security wpa2-psk
 band all
!
wireless wlan 2
 ssid Guest
 shutdown
!`

func TestParseCnPilotConfig(t *testing.T) {
	st := ParseCnPilotConfig(sampleConfig, testAP())
	if len(st.Radios) != 2 || len(st.WLANs) != 2 {
		t.Fatalf("got %+v", st)
	}
	if r := st.Radios[1]; r.Band != "5" || r.Channel != "149" || r.Width != "80" || !r.Enabled {
		t.Errorf("radio 2 = %+v", r)
	}
	if w := st.WLANs[0]; w.SSID != "My Home" || w.Band != "all" || w.Security != "wpa2-psk" || !w.Enabled {
		t.Errorf("wlan 1 = %+v", w)
	}
	if st.WLANs[1].Enabled {
		t.Errorf("wlan 2 should be shut down")
	}
}

// fakeCambium is a minimal SSH server whose shell mimics a real XV2-2: it starts
// in "(config)#", only executes a line on "\r", pages long output with --More--,
// and rejects unknown commands with "%Error".
func fakeCambium(t *testing.T) (port int, received *[]string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	srvCfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == "admin" && string(pw) == "pw" {
				return nil, nil
			}
			return nil, fmt.Errorf("denied")
		},
	}
	srvCfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var got []string
	crypt := "$crypt$1$original"
	fakeSNMP.on, fakeSNMP.read, fakeSNMP.write = true, "orig", "origw"

	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(nc, srvCfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nch := range chans {
					ch, chReqs, _ := nch.Accept()
					go func() {
						for req := range chReqs {
							req.Reply(req.Type == "pty-req" || req.Type == "shell", nil)
						}
					}()
					go serveCLI(ch, &got, &crypt)
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, &got
}

// fakeSNMP is the fake AP's SNMP state, shared across sessions (reset by fakeCambium).
var fakeSNMP struct {
	on          bool
	read, write string
}

// crypt is WLAN 2's stored passphrase; like the real AP, every set re-encrypts
// it with a fresh salt.
func serveCLI(ch ssh.Channel, got *[]string, crypt *string) {
	defer ch.Close()
	modes := []string{"MyAP(config)#"}
	prompt := func() string { return "\r" + modes[len(modes)-1] + " " }
	io.WriteString(ch, prompt())
	var line []byte
	b := make([]byte, 1)
	for {
		if _, err := ch.Read(b); err != nil {
			return
		}
		if b[0] == '\n' {
			io.WriteString(ch, "\n") // echoed, but not executed, like the real AP
			continue
		}
		if b[0] != '\r' {
			line = append(line, b[0])
			continue
		}
		cmd := strings.TrimSpace(string(line))
		line = line[:0]
		*got = append(*got, cmd)
		io.WriteString(ch, cmd+"\r\n") // echo
		switch {
		case cmd == "show config" && len(modes) == 1:
			io.WriteString(ch, "!\r\nwireless radio 1\r\n frequency-band 2.4GHz\r\n channel 6\r\n--More--")
			ch.Read(b) // wait for the space
			io.WriteString(ch, "\r\x1b[K channel-width 20\r\n!\r\n")
			io.WriteString(ch, "wireless wlan 2\r\n ssid Guest\r\n passphrase "+*crypt+"\r\n!\r\n")
			if fakeSNMP.read != "" {
				io.WriteString(ch, "snmp-server read-community $crypt$1$r"+fakeSNMP.read+"\r\n")
			}
			if fakeSNMP.write != "" {
				io.WriteString(ch, "snmp-server write-community $crypt$1$w"+fakeSNMP.write+"\r\n")
			}
			if fakeSNMP.on {
				io.WriteString(ch, "snmp-server\r\n")
			} else {
				io.WriteString(ch, "no snmp-server\r\n")
			}
		case cmd == "snmp-server" && len(modes) == 1:
			fakeSNMP.on = true
		case cmd == "no snmp-server" && len(modes) == 1:
			fakeSNMP.on = false
		case strings.HasPrefix(cmd, "snmp-server read-community ") && len(modes) == 1:
			fakeSNMP.read = strings.Fields(cmd)[2]
		case strings.HasPrefix(cmd, "snmp-server write-community ") && len(modes) == 1:
			fakeSNMP.write = strings.Fields(cmd)[2]
		case strings.HasPrefix(cmd, "wireless ") && len(modes) == 1:
			f := strings.Fields(cmd)
			modes = append(modes, "MyAP(config-"+f[1]+"-"+f[2]+")#")
		case strings.HasPrefix(cmd, "passphrase ") && modes[len(modes)-1] == "MyAP(config-wlan-2)#":
			salt := make([]byte, 4)
			rand.Read(salt)
			*crypt = fmt.Sprintf("$crypt$1$%x", salt)
		case (strings.HasPrefix(cmd, "ssid ") || strings.HasPrefix(cmd, "channel")) && len(modes) == 2:
		case (cmd == "apply" || cmd == "save") && len(modes) == 1:
		case cmd == "exit":
			if len(modes) == 1 {
				return
			}
			modes = modes[:len(modes)-1]
		default:
			io.WriteString(ch, "%Error processing cli command\r\n")
		}
		io.WriteString(ch, prompt())
	}
}

func TestRunCommandsAgainstFakeAP(t *testing.T) {
	port, got := fakeCambium(t)
	ap := testAP()
	ap.Port = port
	p := builtinProfiles()["cnpilot"]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := RunCommands(ctx, ap, p, p.Status)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	st := ParseCnPilotConfig(res[0].Output, ap)
	if len(st.Radios) != 1 || st.Radios[0].Channel != "6" || st.Radios[0].Width != "20" {
		t.Fatalf("parsed %+v from %q", st, res[0].Output)
	}

	req := ChangeRequest{SSID: "New", Radios: []RadioChange{{Band: "2.4", Channel: "11"}}}
	if err := req.Validate(ap); err != nil {
		t.Fatal(err)
	}
	cmds, _ := BuildChangeCommands(p, ap, &req)
	if _, err := RunCommands(ctx, ap, p, cmds); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(strings.Join(*got, "|"), `ssid New|exit|wireless radio 1|channel 11|exit|apply|save`) {
		t.Errorf("server received %q", *got)
	}

	// A rejected command stops the run and is reported.
	res, err = RunCommands(ctx, ap, p, []string{"bogus", "save"})
	if err == nil || len(res) != 1 || !res[0].Failed {
		t.Errorf("expected failure on bogus, got err=%v res=%+v", err, res)
	}

	// A pinned host key that doesn't match is refused.
	ap.HostKeyFingerprint = "SHA256:" + strconv.Itoa(12345)
	if _, err := RunCommands(ctx, ap, p, p.Status); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Errorf("expected host key mismatch, got %v", err)
	}
}

func TestSecretStore(t *testing.T) {
	dir := t.TempDir()
	path, key := dir+"/secrets.json", dir+"/secret.key"
	s, err := openStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(wlanKey("house", 2), "Guest", "hunter2-hunter2", "admin"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "hunter2") {
		t.Fatal("password stored in plain text")
	}

	// Reopening with the same key file decrypts it.
	s2, err := openStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	e, pw, err := s2.Reveal(wlanKey("house", 2))
	if err != nil || pw != "hunter2-hunter2" || e.Label != "Guest" || e.SetBy != "admin" {
		t.Fatalf("reveal = %q %+v %v", pw, e, err)
	}
	if _, _, err := s2.Reveal(wlanKey("house", 1)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing entry: got %v", err)
	}

	// An entry copied onto another WLAN's slot must not decrypt.
	s2.data[wlanKey("house", 1)] = s2.data[wlanKey("house", 2)]
	if _, _, err := s2.Reveal(wlanKey("house", 1)); err == nil {
		t.Error("swapped entry decrypted")
	}

	// A different key can't read it.
	os.Remove(key)
	s3, err := openStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s3.Reveal(wlanKey("house", 2)); err == nil {
		t.Error("decrypted with the wrong key")
	}
}

func TestMaskAPCrypt(t *testing.T) {
	in := "wireless wlan 1\n ssid Home\n passphrase $crypt$1$secret\n band all\nsnmp-server read-community $crypt$1$alsosecret"
	out := maskAPCrypt(in)
	if strings.Contains(out, "secret") || strings.Contains(out, "$crypt$") || !strings.Contains(out, " passphrase ********") || !strings.Contains(out, "ssid Home") {
		t.Errorf("got %q", out)
	}
}

// Apply a password through the HTTP API against the fake AP, then reveal it,
// then simulate someone re-setting it elsewhere.
func TestApplySavesPassword(t *testing.T) {
	port, _ := fakeCambium(t)
	ap := testAP()
	ap.Port = port
	cfg = &Config{AccessPoints: []AccessPoint{*ap}, Profiles: builtinProfiles()}
	cfg.Server.AllowDirect = true
	var err error
	dir := t.TempDir()
	if secrets, err = openStore(dir+"/s.json", dir+"/k"); err != nil {
		t.Fatal(err)
	}
	mux := newMux()
	do := func(method, path, body string) map[string]any {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 200 {
			t.Fatalf("%s %s: %d %v", method, path, rec.Code, out)
		}
		return out
	}
	wlan2 := func() map[string]any {
		st := do("GET", "/api/aps/t/status", "")
		if strings.Contains(fmt.Sprint(st), "$crypt$") {
			t.Fatal("AP's encrypted passphrase leaked into the status response")
		}
		for _, w := range st["status"].(map[string]any)["wlans"].([]any) {
			if w.(map[string]any)["index"].(float64) == 2 {
				return w.(map[string]any)
			}
		}
		t.Fatal("no wlan 2")
		return nil
	}

	if w := wlan2(); w["password_saved"] != false {
		t.Fatalf("before apply: %v", w)
	}
	if out := do("POST", "/api/aps/t/apply", `{"wlan":2,"passphrase":"GreenEggs-1234"}`); fmt.Sprint(out["saved"]) != "[Wi-Fi password]" {
		t.Fatalf("apply: %v", out)
	}
	if pw := do("GET", "/api/aps/t/wlans/2/password", "")["secret"]; pw != "GreenEggs-1234" {
		t.Fatalf("revealed %v", pw)
	}
	w := wlan2()
	if w["password_saved"] != true || w["password_set_by"] != "direct" {
		t.Fatalf("after apply: %v", w)
	}
	// The SSID wasn't part of the change, so it's read back from the AP.
	if e, _ := secrets.Meta(wlanKey("t", 2)); e.Label != "Guest" {
		t.Errorf("saved SSID = %q, want Guest", e.Label)
	}
}

func TestSNMPChange(t *testing.T) {
	ap := testAP()
	on, off := true, false
	cases := []struct {
		name string
		snmp SNMPChange
		ok   bool
	}{
		{"read", SNMPChange{ReadCommunity: "r3adOnly"}, true},
		{"enable", SNMPChange{Enabled: &on}, true},
		{"space", SNMPChange{ReadCommunity: "has space"}, false},
		{"help char", SNMPChange{WriteCommunity: "what?"}, false},
		{"too long", SNMPChange{ReadCommunity: strings.Repeat("a", 65)}, false},
		{"same", SNMPChange{ReadCommunity: "same1", WriteCommunity: "same1"}, false},
		{"empty", SNMPChange{}, false},
	}
	for _, c := range cases {
		s := c.snmp
		req := ChangeRequest{SNMP: &s}
		if err := req.Validate(ap); (err == nil) != c.ok {
			t.Errorf("%s: got err=%v, want ok=%v", c.name, err, c.ok)
		}
	}

	req := ChangeRequest{SNMP: &SNMPChange{Enabled: &off, ReadCommunity: "r3ad", WriteCommunity: "wr1te"}}
	if err := req.Validate(ap); err != nil {
		t.Fatal(err)
	}
	cmds, err := BuildChangeCommands(builtinProfiles()["cnpilot"], ap, &req)
	if err != nil {
		t.Fatal(err)
	}
	want := "no snmp-server|snmp-server read-community r3ad|snmp-server write-community wr1te|apply|save"
	if got := strings.Join(cmds, "|"); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if m := strings.Join(MaskSecrets(cmds, req.Secrets()...), "|"); strings.Contains(m, "r3ad") || strings.Contains(m, "wr1te") {
		t.Errorf("secrets not masked: %s", m)
	}

	st := ParseCnPilotConfig("snmp-server read-community $crypt$1$x\nsnmp-server port 161\nno snmp-server\n", ap)
	if st.SNMP == nil || st.SNMP.Enabled || !st.SNMP.Read.Configured || st.SNMP.Write.Configured || st.SNMP.Port != "161" {
		t.Errorf("parsed %+v", st.SNMP)
	}
}

// Change SNMP through the HTTP API against the fake AP, then reveal both communities.
func TestApplySavesSNMP(t *testing.T) {
	port, _ := fakeCambium(t)
	ap := testAP()
	ap.Port = port
	cfg = &Config{AccessPoints: []AccessPoint{*ap}, Profiles: builtinProfiles()}
	cfg.Server.AllowDirect = true
	var err error
	dir := t.TempDir()
	if secrets, err = openStore(dir+"/s.json", dir+"/k"); err != nil {
		t.Fatal(err)
	}
	mux := newMux()
	do := func(method, path, body string) map[string]any {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != 200 {
			t.Fatalf("%s %s: %d %v", method, path, rec.Code, out)
		}
		return out
	}
	snmp := func() map[string]any {
		return do("GET", "/api/aps/t/status", "")["status"].(map[string]any)["snmp"].(map[string]any)
	}

	if s := snmp(); s["enabled"] != true || s["read_community"].(map[string]any)["saved"] != false {
		t.Fatalf("before: %v", s)
	}
	out := do("POST", "/api/aps/t/apply", `{"snmp":{"read_community":"NewRead1","write_community":"NewWrite1"}}`)
	if fmt.Sprint(out["saved"]) != "[SNMP read community SNMP write community]" {
		t.Fatalf("apply: %v", out)
	}
	if strings.Contains(fmt.Sprint(out), "NewRead1") {
		t.Fatal("community leaked into the apply response")
	}
	if got := do("GET", "/api/aps/t/snmp/read", "")["secret"]; got != "NewRead1" {
		t.Errorf("read = %v", got)
	}
	if got := do("GET", "/api/aps/t/snmp/write", "")["secret"]; got != "NewWrite1" {
		t.Errorf("write = %v", got)
	}
	if fakeSNMP.read != "NewRead1" || fakeSNMP.write != "NewWrite1" {
		t.Errorf("AP got read=%q write=%q", fakeSNMP.read, fakeSNMP.write)
	}

	do("POST", "/api/aps/t/apply", `{"snmp":{"enabled":false}}`)
	if s := snmp(); s["enabled"] != false || s["read_community"].(map[string]any)["saved"] != true {
		t.Errorf("after disable: %v", s)
	}
}

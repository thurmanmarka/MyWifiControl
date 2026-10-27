package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The hub's proxy gives up after 30s, so finish (or fail) before that.
const requestBudget = 25 * time.Second

var (
	cfg     *Config
	secrets *secretStore
)

func main() {
	var err error
	cfg, err = LoadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	secrets, err = openStore(cfg.Server.SecretsFile, cfg.Server.SecretKeyFile)
	if err != nil {
		log.Fatalf("Failed to open password store: %v", err)
	}
	if cfg.Database.Enabled {
		changes, err = openChangeLog(cfg.Database)
		if err != nil {
			log.Fatalf("Failed to open change log database: %v", err)
		}
		log.Printf("🗄️  Change log in MariaDB database %s", cfg.Database.Name)
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port)
	log.Printf("📶 MyWifiControl starting on http://%s (%d access point(s))", addr, len(cfg.AccessPoints))
	if cfg.Server.AllowDirect {
		log.Printf("⚠️  allow_direct is on: requests that bypass the hub are treated as admin")
	}
	log.Fatal(http.ListenAndServe(addr, newMux()))
}

// Everything except the health checks is admin-only: reading the AP's config
// exposes the SSID, and every other route can change the AP.
func newMux() *http.ServeMux {
	admin := func(h http.HandlerFunc) http.HandlerFunc { return requireRole(h, "admin") }
	static := http.StripPrefix("/static/", http.FileServer(http.Dir("static")))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /wifi/health", handleHealth)

	mux.HandleFunc("GET /", admin(handleIndex))
	mux.HandleFunc("GET /static/", admin(static.ServeHTTP))
	mux.HandleFunc("GET /api/me", admin(handleMe))
	mux.HandleFunc("GET /api/aps", admin(handleListAPs))
	mux.HandleFunc("GET /api/aps/{id}/status", admin(handleStatus))
	mux.HandleFunc("POST /api/aps/{id}/preview", admin(handlePreview))
	mux.HandleFunc("POST /api/aps/{id}/apply", admin(handleApply))
	mux.HandleFunc("POST /api/aps/{id}/console", admin(handleConsole))
	mux.HandleFunc("GET /api/aps/{id}/wlans/{wlan}/password", admin(handleRevealPassword))
	mux.HandleFunc("GET /api/aps/{id}/snmp/{kind}", admin(handleRevealCommunity))
	mux.HandleFunc("GET /api/aps/{id}/history", admin(handleHistory))
	mux.HandleFunc("GET /api/aps/{id}/live", admin(handleLive))
	mux.HandleFunc("POST /api/aps/{id}/clients/{mac}/disconnect", admin(handleDisconnect))
	return mux
}

// handleHistory returns the AP's recent changes from the change log.
func handleHistory(w http.ResponseWriter, r *http.Request) {
	ap, _, ok := findAP(w, r)
	if !ok {
		return
	}
	if changes == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "entries": []ChangeEntry{}})
		return
	}
	entries, err := changes.Recent(ap.ID, 50)
	if err != nil {
		log.Printf("Change log query failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not read the change log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "entries": entries})
}

type ctxKey string

// requireRole trusts the X-Hub-* headers set by MyHomeServicesHub. That is only
// safe because this service binds to 127.0.0.1 by default, so only the hub can
// reach it.
func requireRole(next http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, role := r.Header.Get("X-Hub-User"), r.Header.Get("X-Hub-Role")
		if r.Header.Get("X-Hub-Authenticated") != "true" {
			if !cfg.Server.AllowDirect {
				writeError(w, http.StatusUnauthorized, "open this page through MyHomeServicesHub")
				return
			}
			user, role = "direct", "admin"
		}
		if !contains(roles, role) {
			writeError(w, http.StatusForbidden, "your role ("+role+") cannot do this")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey("user"), user)
		ctx = context.WithValue(ctx, ctxKey("role"), role)
		next(w, r.WithContext(ctx))
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	content, err := os.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "Failed to load page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"user": r.Context().Value(ctxKey("user")).(string),
		"role": r.Context().Value(ctxKey("role")).(string),
	})
}

func handleListAPs(w http.ResponseWriter, r *http.Request) {
	type apView struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Host    string   `json:"host"`
		Profile string   `json:"profile"`
		Bands   []string `json:"bands"`
		Pinned  bool     `json:"host_key_pinned"`
		WLAN    int      `json:"wlan"`
	}
	list := []apView{}
	for _, ap := range cfg.AccessPoints {
		var bands []string
		for _, b := range []string{"2.4", "5"} {
			if _, ok := ap.Radios[b]; ok {
				bands = append(bands, b)
			}
		}
		list = append(list, apView{ap.ID, ap.Name, ap.Host, ap.Profile, bands, ap.HostKeyFingerprint != "", ap.WLAN})
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_points": list})
}

func findAP(w http.ResponseWriter, r *http.Request) (*AccessPoint, Profile, bool) {
	id := r.PathValue("id")
	for i := range cfg.AccessPoints {
		if cfg.AccessPoints[i].ID == id {
			ap := &cfg.AccessPoints[i]
			return ap, cfg.Profiles[ap.Profile], true
		}
	}
	writeError(w, http.StatusNotFound, "unknown access point "+id)
	return nil, Profile{}, false
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	ap, p, ok := findAP(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestBudget)
	defer cancel()

	results, err := RunCommands(ctx, ap, p, p.Status)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "results": results})
		return
	}
	var raw strings.Builder
	for i := range results {
		raw.WriteString(results[i].Output)
		raw.WriteString("\n")
		results[i].Output = maskAPCrypt(results[i].Output)
	}

	st := ParseCnPilotConfig(raw.String(), ap)
	for i := range st.WLANs {
		wl := &st.WLANs[i]
		var s SecretStatus
		annotateSecret(&s, wlanKey(ap.ID, wl.Index))
		wl.PasswordSaved, wl.PasswordSetBy, wl.PasswordSetAt = s.Saved, s.SetBy, s.SetAt
	}
	if st.SNMP != nil {
		annotateSecret(&st.SNMP.Read, snmpKey(ap.ID, "read"))
		annotateSecret(&st.SNMP.Write, snmpKey(ap.ID, "write"))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  st,
		"raw":     maskAPCrypt(raw.String()),
		"results": results,
	})
}

var (
	passphraseLine = regexp.MustCompile(`(?m)^(\s*passphrase\s+)\S.*$`)
	cryptValue     = regexp.MustCompile(`\$crypt\$\S+`)
)

// maskAPCrypt hides the AP's secrets: passphrases plus every other $crypt$
// value (admin password hash, SNMP communities, LLDP keys). The AP accepts
// encrypted values back as-is, so they're as good as the real thing.
func maskAPCrypt(s string) string {
	s = passphraseLine.ReplaceAllString(s, "${1}********")
	return cryptValue.ReplaceAllString(s, "********")
}

// annotateSecret marks whether Wi-Fi Control holds a copy of a secret. There's
// no way to check the copy against the AP: it re-encrypts secrets with a fresh
// salt every time it prints its config.
func annotateSecret(s *SecretStatus, key string) {
	saved, ok := secrets.Meta(key)
	if !ok {
		return
	}
	s.Saved, s.SetBy, s.SetAt = true, saved.SetBy, &saved.SetAt
}

func handleRevealPassword(w http.ResponseWriter, r *http.Request) {
	ap, _, ok := findAP(w, r)
	if !ok {
		return
	}
	wlan, err := strconv.Atoi(r.PathValue("wlan"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad WLAN index")
		return
	}
	revealSecret(w, r, ap.ID, wlanKey(ap.ID, wlan), fmt.Sprintf("password for %s WLAN %d", ap.ID, wlan))
}

func handleRevealCommunity(w http.ResponseWriter, r *http.Request) {
	ap, _, ok := findAP(w, r)
	if !ok {
		return
	}
	kind := r.PathValue("kind")
	if kind != "read" && kind != "write" {
		writeError(w, http.StatusBadRequest, "kind must be read or write")
		return
	}
	revealSecret(w, r, ap.ID, snmpKey(ap.ID, kind), fmt.Sprintf("SNMP %s community for %s", kind, ap.ID))
}

func revealSecret(w http.ResponseWriter, r *http.Request, apID, key, what string) {
	saved, secret, err := secrets.Reveal(key)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "nothing saved yet; set it through Wi-Fi Control first")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	user := r.Context().Value(ctxKey("user")).(string)
	log.Printf("🔑 %s viewed the saved %s", user, what)
	changes.Record(ChangeEntry{User: user, APID: apID, Action: "view_secret", Detail: "Viewed the saved " + what, OK: true})

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"label":  saved.Label,
		"secret": secret,
		"set_by": saved.SetBy,
		"set_at": saved.SetAt,
	})
}

func decodeChange(w http.ResponseWriter, r *http.Request, ap *AccessPoint, p Profile) (*ChangeRequest, []string, bool) {
	var req ChangeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return nil, nil, false
	}
	if err := req.Validate(ap); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, nil, false
	}
	cmds, err := BuildChangeCommands(p, ap, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	return &req, cmds, true
}

func handlePreview(w http.ResponseWriter, r *http.Request) {
	ap, p, ok := findAP(w, r)
	if !ok {
		return
	}
	req, cmds, ok := decodeChange(w, r, ap, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": MaskSecrets(cmds, req.Secrets()...)})
}

func handleApply(w http.ResponseWriter, r *http.Request) {
	ap, p, ok := findAP(w, r)
	if !ok {
		return
	}
	req, cmds, ok := decodeChange(w, r, ap, p)
	if !ok {
		return
	}
	user := r.Context().Value(ctxKey("user")).(string)
	maskedCmds := MaskSecrets(cmds, req.Secrets()...)
	log.Printf("✏️  %s applying to %s: %q", user, ap.ID, maskedCmds)
	entry := ChangeEntry{User: user, APID: ap.ID, Action: "apply", Commands: maskedCmds}

	// Don't stop when the browser disconnects: changing the Wi-Fi you're
	// browsing over drops you mid-request, and stopping between "apply" and
	// "save" would leave a live change that's lost on reboot and never saved.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), requestBudget)
	defer cancel()
	results, err := RunCommands(ctx, ap, p, cmds)
	for i := range results {
		results[i].Command = MaskSecrets([]string{results[i].Command}, req.Secrets()...)[0]
	}
	if err != nil {
		log.Printf("❌ apply to %s failed: %v", ap.ID, err)
		entry.Error = err.Error()
		changes.Record(entry)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "results": results})
		return
	}
	log.Printf("✅ apply to %s succeeded", ap.ID)

	resp := map[string]any{"ok": true, "results": results}
	var saved, failed []string
	note := func(what string, err error) {
		if err != nil {
			log.Printf("⚠️  could not save %s for %s: %v", what, ap.ID, err)
			failed = append(failed, what)
		} else {
			saved = append(saved, what)
		}
	}
	if req.Passphrase != "" {
		note("Wi-Fi password", rememberPassword(ctx, ap, p, req, user))
	}
	if s := req.SNMP; s != nil {
		if s.ReadCommunity != "" {
			note("SNMP read community", secrets.Save(snmpKey(ap.ID, "read"), "SNMP read community", s.ReadCommunity, user))
		}
		if s.WriteCommunity != "" {
			note("SNMP write community", secrets.Save(snmpKey(ap.ID, "write"), "SNMP write community", s.WriteCommunity, user))
		}
	}
	resp["saved"] = saved
	if len(failed) > 0 {
		resp["warning"] = "Applied, but could not save: " + strings.Join(failed, ", ")
	}
	entry.OK = true
	entry.Detail = req.Summary()
	if len(failed) > 0 {
		entry.Error = resp["warning"].(string)
	}
	changes.Record(entry)
	writeJSON(w, http.StatusOK, resp)
}

// rememberPassword saves a password the app just set. If the SSID wasn't part
// of the change, it's read back from the AP in a separate session, so a failure
// there can't make a successful apply look failed.
func rememberPassword(ctx context.Context, ap *AccessPoint, p Profile, req *ChangeRequest, user string) error {
	ssid := req.SSID
	if ssid == "" {
		results, err := RunCommands(ctx, ap, p, p.Status)
		if err != nil {
			log.Printf("⚠️  SSID read-back after apply failed (password still saved): %v", err)
		}
		var raw strings.Builder
		for _, res := range results {
			raw.WriteString(res.Output + "\n")
		}
		for _, wl := range ParseCnPilotConfig(raw.String(), ap).WLANs {
			if wl.Index == req.WLAN {
				ssid = wl.SSID
			}
		}
	}
	if err := secrets.Save(wlanKey(ap.ID, req.WLAN), ssid, req.Passphrase, user); err != nil {
		return err
	}
	log.Printf("🔐 saved password for %s WLAN %d (%s), set by %s", ap.ID, req.WLAN, ssid, user)
	return nil
}

// handleConsole runs raw CLI commands. Useful for finding the exact syntax your
// firmware wants (e.g. "show config", "show wireless") before trusting the profile.
func handleConsole(w http.ResponseWriter, r *http.Request) {
	ap, p, ok := findAP(w, r)
	if !ok {
		return
	}
	var req struct {
		Commands []string `json:"commands"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	var cmds []string
	for _, c := range req.Commands {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if strings.ContainsAny(c, "\r\n") {
			writeError(w, http.StatusBadRequest, "one command per line")
			return
		}
		cmds = append(cmds, c)
	}
	if len(cmds) == 0 || len(cmds) > 25 {
		writeError(w, http.StatusBadRequest, "send between 1 and 25 commands")
		return
	}

	user := r.Context().Value(ctxKey("user")).(string)
	maskedCmds := MaskConsoleSecrets(cmds)
	log.Printf("⌨️  %s console on %s: %q", user, ap.ID, maskedCmds)

	// Console output is shown as-is; don't stop on error lines here.
	p.ErrorRegex = ""
	ctx, cancel := context.WithTimeout(r.Context(), requestBudget)
	defer cancel()
	results, err := RunCommands(ctx, ap, p, cmds)
	resp := map[string]any{"results": results}
	entry := ChangeEntry{User: user, APID: ap.ID, Action: "console", Commands: maskedCmds, OK: err == nil}
	if err != nil {
		resp["error"] = err.Error()
		entry.Error = err.Error()
	}
	changes.Record(entry)
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

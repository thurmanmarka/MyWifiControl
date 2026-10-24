package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Profile describes the CLI dialect of an access point. Every command list is a
// list of text/template lines; lines that render empty are skipped.
type Profile struct {
	Description string   `yaml:"description"`
	Prompt      string   `yaml:"prompt"`        // regex matched against the last line of output
	Pager       string   `yaml:"pager"`         // regex for "--More--" style pagers (answered with a space)
	ErrorRegex  string   `yaml:"error_pattern"` // output matching this marks a command as failed
	Status      []string `yaml:"status"`
	EnterConfig []string `yaml:"enter_config"`
	WLAN        []string `yaml:"wlan"`
	Radio       []string `yaml:"radio"`
	SNMP        []string `yaml:"snmp"`
	Commit      []string `yaml:"commit"`
}

func builtinProfiles() map[string]Profile {
	return map[string]Profile{
		// Cambium cnPilot / XV-series CLI. Verified against an XV2-2 on 7.2-r1:
		// login lands in "(config)#", "show config" dumps the config, and
		// "apply" activates changes while "save" persists them.
		"cnpilot": {
			Description: "Cambium cnPilot / XV-series CLI",
			Prompt:      `^[^\s].{0,80}[#>$]\s*$`,
			Pager:       `(?i)-+\s*more\s*-+`,
			ErrorRegex:  `(?i)(%\s*error|%\s*invalid|error processing|error parsing|unknown command|incomplete command|syntax error)`,
			Status:      []string{"show config"},
			WLAN: []string{
				"wireless wlan {{.WLAN}}",
				"{{if .SSID}}ssid {{q .SSID}}{{end}}",
				"{{if .Passphrase}}passphrase {{q .Passphrase}}{{end}}",
				"{{if .Band}}band {{.Band}}{{end}}",
				"exit",
			},
			Radio: []string{
				"wireless radio {{.Radio}}",
				"{{if .Channel}}channel {{.Channel}}{{end}}",
				"{{if .Width}}channel-width {{.Width}}{{end}}",
				"exit",
			},
			SNMP: []string{
				"{{if .SNMPOn}}snmp-server{{end}}",
				"{{if .SNMPOff}}no snmp-server{{end}}",
				"{{if .ReadCommunity}}snmp-server read-community {{.ReadCommunity}}{{end}}",
				"{{if .WriteCommunity}}snmp-server write-community {{.WriteCommunity}}{{end}}",
			},
			Commit: []string{"apply", "save"},
		},
	}
}

// cliQuote quotes a value only when it contains spaces, matching how the AP
// itself prints values in "show config" (ssid "My Net" vs ssid MyNet).
func cliQuote(s string) string {
	if strings.Contains(s, " ") {
		return `"` + s + `"`
	}
	return s
}

type ChangeRequest struct {
	WLAN       int           `json:"wlan"` // WLAN index to change; 0 = the AP's configured default
	SSID       string        `json:"ssid"`
	Passphrase string        `json:"passphrase"`
	Band       string        `json:"band"` // which bands the SSID broadcasts on: all, 2.4GHz, 5GHz
	Radios     []RadioChange `json:"radios"`
	SNMP       *SNMPChange   `json:"snmp"`
}

type SNMPChange struct {
	Enabled        *bool  `json:"enabled"` // nil = unchanged
	ReadCommunity  string `json:"read_community"`
	WriteCommunity string `json:"write_community"`
}

func (s *SNMPChange) empty() bool {
	return s == nil || (s.Enabled == nil && s.ReadCommunity == "" && s.WriteCommunity == "")
}

type RadioChange struct {
	Band    string `json:"band"` // "2.4" or "5"
	Channel string `json:"channel"`
	Width   string `json:"width"`
}

type templateData struct {
	WLAN       int
	SSID       string
	Passphrase string
	Band       string
	Radio      int
	Channel    string
	Width      string

	SNMPOn, SNMPOff               bool
	ReadCommunity, WriteCommunity string
}

var (
	channels24 = map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true, 8: true, 9: true, 10: true, 11: true, 12: true, 13: true}
	channels5  = map[int]bool{
		36: true, 40: true, 44: true, 48: true, 52: true, 56: true, 60: true, 64: true,
		100: true, 104: true, 108: true, 112: true, 116: true, 120: true, 124: true, 128: true,
		132: true, 136: true, 140: true, 144: true, 149: true, 153: true, 157: true, 161: true, 165: true,
	}
	widths   = map[string][]string{"2.4": {"20", "40"}, "5": {"20", "40", "80", "160"}}
	ssidBand = []string{"all", "2.4GHz", "5GHz"}
)

// checkCLIText rejects characters that could break out of a quoted CLI argument,
// start a new command line, or trigger the CLI's inline "?" help.
func checkCLIText(field, v string) error {
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("%s may not start or end with a space", field)
	}
	for _, r := range v {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("%s may only contain printable ASCII characters", field)
		}
		if r == '"' || r == '\\' || r == '?' {
			return fmt.Errorf(`%s may not contain ", \ or ?`, field)
		}
	}
	return nil
}

func (c *ChangeRequest) Validate(ap *AccessPoint) error {
	c.SSID = strings.TrimSpace(c.SSID)
	c.Band = strings.TrimSpace(c.Band)
	if c.WLAN == 0 {
		c.WLAN = ap.WLAN
	}
	if c.WLAN < 1 || c.WLAN > 16 {
		return fmt.Errorf("WLAN must be between 1 and 16")
	}

	if c.SSID != "" {
		if len(c.SSID) > 32 {
			return fmt.Errorf("SSID must be 1-32 characters")
		}
		if err := checkCLIText("SSID", c.SSID); err != nil {
			return err
		}
	}
	if c.Passphrase != "" {
		if len(c.Passphrase) < 8 || len(c.Passphrase) > 63 {
			return fmt.Errorf("passphrase must be 8-63 characters")
		}
		if err := checkCLIText("passphrase", c.Passphrase); err != nil {
			return err
		}
	}
	if c.Band != "" && !contains(ssidBand, c.Band) {
		return fmt.Errorf("band must be one of %s", strings.Join(ssidBand, ", "))
	}

	seen := map[string]bool{}
	for i := range c.Radios {
		r := &c.Radios[i]
		r.Channel = strings.ToLower(strings.TrimSpace(r.Channel))
		r.Width = strings.TrimSpace(r.Width)
		if _, ok := ap.Radios[r.Band]; !ok {
			return fmt.Errorf("this access point has no %s GHz radio configured", r.Band)
		}
		if seen[r.Band] {
			return fmt.Errorf("%s GHz radio listed twice", r.Band)
		}
		seen[r.Band] = true

		if r.Channel != "" && r.Channel != "auto" {
			n, err := strconv.Atoi(r.Channel)
			valid := channels24
			if r.Band == "5" {
				valid = channels5
			}
			if err != nil || !valid[n] {
				return fmt.Errorf("channel %q is not valid for %s GHz", r.Channel, r.Band)
			}
		}
		if r.Width != "" && !contains(widths[r.Band], r.Width) {
			return fmt.Errorf("width %s MHz is not valid for %s GHz (allowed: %s)", r.Width, r.Band, strings.Join(widths[r.Band], ", "))
		}
	}

	if s := c.SNMP; !s.empty() {
		for _, f := range []struct{ name, v string }{
			{"read community", s.ReadCommunity}, {"write community", s.WriteCommunity},
		} {
			if f.v == "" {
				continue
			}
			if len(f.v) > 64 {
				return fmt.Errorf("%s must be at most 64 characters", f.name)
			}
			// No spaces: we haven't verified how the AP handles quoted communities.
			if strings.Contains(f.v, " ") {
				return fmt.Errorf("%s may not contain spaces", f.name)
			}
			if err := checkCLIText(f.name, f.v); err != nil {
				return err
			}
		}
		if s.ReadCommunity != "" && s.ReadCommunity == s.WriteCommunity {
			return fmt.Errorf("read and write communities should be different")
		}
	}

	if c.SSID == "" && c.Passphrase == "" && c.Band == "" && !c.hasRadioChanges() && c.SNMP.empty() {
		return fmt.Errorf("nothing to change")
	}
	return nil
}

// hasWLANChanges reports whether the request touches the selected WLAN.
func (c *ChangeRequest) hasWLANChanges() bool {
	return c.SSID != "" || c.Passphrase != "" || c.Band != ""
}

// Secrets returns every secret in the request, for masking.
func (c *ChangeRequest) Secrets() []string {
	s := []string{c.Passphrase}
	if c.SNMP != nil {
		s = append(s, c.SNMP.ReadCommunity, c.SNMP.WriteCommunity)
	}
	return s
}

func (c *ChangeRequest) hasRadioChanges() bool {
	for _, r := range c.Radios {
		if r.Channel != "" || r.Width != "" {
			return true
		}
	}
	return false
}

// BuildChangeCommands renders the full command sequence for a validated request.
func BuildChangeCommands(p Profile, ap *AccessPoint, c *ChangeRequest) ([]string, error) {
	var cmds []string
	add := func(lines []string, data templateData) error {
		out, err := renderLines(lines, data)
		cmds = append(cmds, out...)
		return err
	}

	if err := add(p.EnterConfig, templateData{}); err != nil {
		return nil, err
	}
	if c.hasWLANChanges() {
		data := templateData{WLAN: c.WLAN, SSID: c.SSID, Passphrase: c.Passphrase, Band: c.Band}
		if err := add(p.WLAN, data); err != nil {
			return nil, err
		}
	}
	for _, r := range c.Radios {
		if r.Channel == "" && r.Width == "" {
			continue
		}
		data := templateData{Radio: ap.Radios[r.Band], Channel: r.Channel, Width: r.Width}
		if err := add(p.Radio, data); err != nil {
			return nil, err
		}
	}
	if s := c.SNMP; !s.empty() {
		if len(p.SNMP) == 0 {
			return nil, fmt.Errorf("profile %q has no snmp commands", ap.Profile)
		}
		data := templateData{ReadCommunity: s.ReadCommunity, WriteCommunity: s.WriteCommunity}
		if s.Enabled != nil {
			data.SNMPOn, data.SNMPOff = *s.Enabled, !*s.Enabled
		}
		if err := add(p.SNMP, data); err != nil {
			return nil, err
		}
	}
	if err := add(p.Commit, templateData{}); err != nil {
		return nil, err
	}
	return cmds, nil
}

func renderLines(lines []string, data templateData) ([]string, error) {
	var out []string
	for _, line := range lines {
		t, err := template.New("cmd").Funcs(template.FuncMap{"q": cliQuote}).Parse(line)
		if err != nil {
			return nil, fmt.Errorf("bad template %q: %w", line, err)
		}
		var b bytes.Buffer
		if err := t.Execute(&b, data); err != nil {
			return nil, err
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// MaskSecrets hides secrets in commands before they are shown or logged.
func MaskSecrets(cmds []string, secrets ...string) []string {
	masked := make([]string, len(cmds))
	for i, c := range cmds {
		for _, s := range secrets {
			if s != "" {
				c = strings.ReplaceAll(c, s, "********")
			}
		}
		masked[i] = c
	}
	return masked
}

// APStatus is the best-effort parsed view of the AP's running configuration.
type APStatus struct {
	WLANs  []WLANStatus  `json:"wlans"`
	Radios []RadioStatus `json:"radios"`
	SNMP   *SNMPStatus   `json:"snmp,omitempty"`
}

type SNMPStatus struct {
	Enabled bool         `json:"enabled"`
	Port    string       `json:"port,omitempty"`
	Read    SecretStatus `json:"read_community"`
	Write   SecretStatus `json:"write_community"`
}

// SecretStatus describes a secret the AP only shows encrypted.
type SecretStatus struct {
	Configured bool       `json:"configured"` // the AP has a value set
	Saved      bool       `json:"saved"`      // Wi-Fi Control has a copy
	SetBy      string     `json:"set_by,omitempty"`
	SetAt      *time.Time `json:"set_at,omitempty"`
}

type WLANStatus struct {
	Index    int    `json:"index"`
	SSID     string `json:"ssid"`
	Band     string `json:"band"`
	Security string `json:"security"`
	Enabled  bool   `json:"enabled"`

	PasswordSaved bool       `json:"password_saved"`
	PasswordSetBy string     `json:"password_set_by,omitempty"`
	PasswordSetAt *time.Time `json:"password_set_at,omitempty"`
}

type RadioStatus struct {
	Index   int    `json:"index"`
	Band    string `json:"band"`
	Channel string `json:"channel"`
	Width   string `json:"width"`
	Enabled bool   `json:"enabled"`
}

var blockStart = regexp.MustCompile(`^wireless (radio|wlan) (\d+)\s*$`)

// ParseCnPilotConfig extracts radio and WLAN settings from "show config" output:
//
//	wireless radio 1
//	 frequency-band 2.4GHz
//	 channel auto
//	 channel-width 20
//	!
//	wireless wlan 1
//	 ssid "MyNet"
//	 band all
func ParseCnPilotConfig(out string, ap *AccessPoint) APStatus {
	bandOf := map[int]string{}
	for band, idx := range ap.Radios {
		bandOf[idx] = band
	}

	var st APStatus
	var radio *RadioStatus
	var wlan *WLANStatus
	flush := func() {
		if radio != nil {
			st.Radios = append(st.Radios, *radio)
		}
		if wlan != nil {
			st.WLANs = append(st.WLANs, *wlan)
		}
		radio, wlan = nil, nil
	}

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r ")
		if m := blockStart.FindStringSubmatch(line); m != nil {
			flush()
			idx, _ := strconv.Atoi(m[2])
			if m[1] == "radio" {
				radio = &RadioStatus{Index: idx, Band: bandOf[idx], Enabled: true}
			} else {
				wlan = &WLANStatus{Index: idx, Enabled: true}
			}
			continue
		}
		if line == "" || !strings.HasPrefix(line, " ") {
			flush()
			parseTopLevel(&st, line)
			continue
		}

		key, val, _ := strings.Cut(strings.TrimSpace(line), " ")
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch {
		case radio != nil:
			switch key {
			case "channel":
				radio.Channel = val
			case "channel-width":
				radio.Width = val
			case "frequency-band":
				radio.Band = strings.TrimSuffix(val, "GHz")
			case "shutdown":
				radio.Enabled = false
			}
		case wlan != nil:
			switch key {
			case "ssid":
				wlan.SSID = val
			case "band":
				wlan.Band = val
			case "security":
				wlan.Security = val
			case "shutdown":
				wlan.Enabled = false
			}
		}
	}
	flush()
	return st
}

// parseTopLevel picks up the global SNMP lines:
//
//	snmp-server read-community $crypt$1$...
//	snmp-server write-community $crypt$1$...
//	snmp-server              (or "no snmp-server" when the agent is off)
func parseTopLevel(st *APStatus, line string) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return
	}
	no := f[0] == "no"
	if no {
		f = f[1:]
	}
	if len(f) == 0 || f[0] != "snmp-server" {
		return
	}
	if st.SNMP == nil {
		st.SNMP = &SNMPStatus{}
	}
	switch {
	case len(f) == 1:
		st.SNMP.Enabled = !no
	case f[1] == "read-community":
		st.SNMP.Read.Configured = !no
	case f[1] == "write-community":
		st.SNMP.Write.Configured = !no
	case f[1] == "port" && len(f) > 2:
		st.SNMP.Port = f[2]
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

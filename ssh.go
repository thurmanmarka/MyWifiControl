package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// enterKey is what a terminal sends for Enter. Cambium CLIs echo a bare "\n"
// but never execute the line, so commands must end in "\r".
const enterKey = "\r"

type CommandResult struct {
	Command string `json:"command"`
	Output  string `json:"output"`
	Failed  bool   `json:"failed"`
}

// apLocks serialises sessions per AP so two changes never interleave on one CLI.
var (
	apLocks   = map[string]*sync.Mutex{}
	apLocksMu sync.Mutex
)

func lockAP(id string) func() {
	apLocksMu.Lock()
	m, ok := apLocks[id]
	if !ok {
		m = &sync.Mutex{}
		apLocks[id] = m
	}
	apLocksMu.Unlock()
	m.Lock()
	return m.Unlock
}

// RunCommands opens an interactive CLI shell on the AP and runs cmds in order.
// Cambium CLIs generally do not support "exec" channels, so this drives a PTY
// like a person typing. It stops at the first command whose output matches the
// profile's error pattern.
func RunCommands(ctx context.Context, ap *AccessPoint, p Profile, cmds []string) ([]CommandResult, error) {
	defer lockAP(ap.ID)()

	prompt, err := regexp.Compile(p.Prompt)
	if err != nil {
		return nil, fmt.Errorf("bad prompt pattern: %w", err)
	}
	var pager, errPat *regexp.Regexp
	if p.Pager != "" {
		if pager, err = regexp.Compile(p.Pager); err != nil {
			return nil, fmt.Errorf("bad pager pattern: %w", err)
		}
	}
	if p.ErrorRegex != "" {
		if errPat, err = regexp.Compile("(?m)" + p.ErrorRegex); err != nil {
			return nil, fmt.Errorf("bad error pattern: %w", err)
		}
	}

	client, err := dial(ctx, ap)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	defer session.Close()

	buf := newShellBuffer()
	session.Stdout = buf
	session.Stderr = buf
	stdin, err := session.StdinPipe()
	if err != nil {
		return nil, err
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}
	if err := session.RequestPty("vt100", 500, 200, modes); err != nil {
		return nil, fmt.Errorf("request pty: %w", err)
	}
	if err := session.Shell(); err != nil {
		return nil, fmt.Errorf("start shell: %w", err)
	}

	sh := &shell{buf: buf, in: stdin, prompt: prompt, pager: pager, timeout: ap.Timeout()}

	// Wait for the login banner to finish; nudge with Enter if the prompt is shy.
	if _, err := sh.waitPrompt(ctx); err != nil {
		io.WriteString(stdin, enterKey)
		if _, err := sh.waitPrompt(ctx); err != nil {
			return nil, fmt.Errorf("no CLI prompt from %s (check the profile prompt pattern): %w", ap.Host, err)
		}
	}

	var results []CommandResult
	for _, cmd := range cmds {
		if _, err := io.WriteString(stdin, cmd+enterKey); err != nil {
			return results, fmt.Errorf("send %q: %w", cmd, err)
		}
		out, err := sh.waitPrompt(ctx)
		res := CommandResult{Command: cmd, Output: cleanOutput(out, cmd)}
		if err != nil {
			res.Failed = true
			results = append(results, res)
			return results, fmt.Errorf("waiting for %q: %w", cmd, err)
		}
		if errPat != nil && errPat.MatchString(res.Output) {
			res.Failed = true
			results = append(results, res)
			return results, fmt.Errorf("AP rejected command %q", cmd)
		}
		results = append(results, res)
	}

	io.WriteString(stdin, "exit"+enterKey)
	return results, nil
}

func dial(ctx context.Context, ap *AccessPoint) (*ssh.Client, error) {
	var auths []ssh.AuthMethod
	if ap.KeyFile != "" {
		key, err := os.ReadFile(ap.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("read key file: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse key file: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if ap.Password != "" {
		pw := ap.Password
		auths = append(auths,
			ssh.Password(pw),
			// Some Cambium firmware only offers keyboard-interactive.
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range ans {
					ans[i] = pw
				}
				return ans, nil
			}),
		)
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("access point %q has no password or key_file", ap.ID)
	}

	// Embedded APs often only speak older algorithms, so offer the insecure
	// ones too. They are only used if the AP supports nothing better.
	supported, insecure := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
	cfg := &ssh.ClientConfig{
		User:              ap.Username,
		Auth:              auths,
		HostKeyCallback:   hostKeyCheck(ap),
		HostKeyAlgorithms: append(supported.HostKeys, insecure.HostKeys...),
		Timeout:           ap.Timeout(),
		Config: ssh.Config{
			KeyExchanges: append(supported.KeyExchanges, insecure.KeyExchanges...),
			Ciphers:      append(supported.Ciphers, insecure.Ciphers...),
			MACs:         append(supported.MACs, insecure.MACs...),
		},
	}

	addr := net.JoinHostPort(ap.Host, strconv.Itoa(ap.Port))
	d := net.Dialer{Timeout: ap.Timeout()}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake with %s: %w", addr, err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

func hostKeyCheck(ap *AccessPoint) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		if ap.HostKeyFingerprint == "" {
			log.Printf("⚠️  %s host key %s (%s) is not pinned; add host_key_fingerprint: %q to config.yaml",
				ap.ID, fp, key.Type(), fp)
			return nil
		}
		want := strings.TrimSpace(ap.HostKeyFingerprint)
		if !strings.HasPrefix(want, "SHA256:") {
			want = "SHA256:" + strings.TrimRight(want, "=")
		}
		if fp != want {
			return fmt.Errorf("host key mismatch for %s: got %s, expected %s", ap.Host, fp, want)
		}
		return nil
	}
}

// shellBuffer collects PTY output and signals when new data arrives.
type shellBuffer struct {
	mu     sync.Mutex
	b      bytes.Buffer
	notify chan struct{}
}

func newShellBuffer() *shellBuffer {
	return &shellBuffer{notify: make(chan struct{}, 1)}
}

func (s *shellBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.b.Write(p)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
	return len(p), nil
}

// take returns everything buffered so far and clears it.
func (s *shellBuffer) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.b.String()
	s.b.Reset()
	return out
}

func (s *shellBuffer) peek() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type shell struct {
	buf     *shellBuffer
	in      io.Writer
	prompt  *regexp.Regexp
	pager   *regexp.Regexp
	timeout time.Duration
}

// waitPrompt reads until the last output line looks like a CLI prompt, answering
// any pager prompts on the way. It returns the output collected.
func (sh *shell) waitPrompt(ctx context.Context) (string, error) {
	var collected strings.Builder
	timer := time.NewTimer(sh.timeout)
	defer timer.Stop()

	for {
		cur := stripANSI(sh.buf.peek())
		last := cur[strings.LastIndex(cur, "\n")+1:]
		// Cambium redraws prompts with a leading "\r" ("\n\rName(config)# "), so
		// only the text after the last carriage return is what's on screen.
		visible := strings.Trim(last, "\r")
		visible = visible[strings.LastIndex(visible, "\r")+1:]

		if sh.pager != nil && sh.pager.MatchString(last) {
			sh.buf.take()
			collected.WriteString(strings.TrimSuffix(cur, last))
			io.WriteString(sh.in, " ")
			resetTimer(timer, sh.timeout)
			continue
		}
		if sh.prompt.MatchString(visible) {
			// Let any trailing bytes land before deciding we're done.
			select {
			case <-sh.buf.notify:
				continue
			case <-time.After(150 * time.Millisecond):
			}
			sh.buf.take()
			collected.WriteString(cur)
			return collected.String(), nil
		}

		select {
		case <-sh.buf.notify:
			resetTimer(timer, sh.timeout)
		case <-timer.C:
			collected.WriteString(stripANSI(sh.buf.take()))
			return collected.String(), fmt.Errorf("timed out after %s", sh.timeout)
		case <-ctx.Done():
			collected.WriteString(stripANSI(sh.buf.take()))
			return collected.String(), ctx.Err()
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Z0-9]|\x08`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

// cleanOutput drops the echoed command and the trailing prompt line.
func cleanOutput(out, cmd string) string {
	out = strings.ReplaceAll(out, "\r", "")
	lines := strings.Split(out, "\n")
	if len(lines) > 0 && strings.Contains(lines[0], cmd) {
		lines = lines[1:]
	}
	if len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n ")
}

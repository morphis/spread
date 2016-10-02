package spread

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/terminal"
	"net"
	"regexp"
	"strconv"
	"syscall"
)

type Client struct {
	server Server
	sshc   *ssh.Client
	config *ssh.ClientConfig
	addr   string

	warnTimeout time.Duration
	killTimeout time.Duration
}

func Dial(server Server, username, password string) (*Client, error) {
	config := &ssh.ClientConfig{
		User:    username,
		Auth:    []ssh.AuthMethod{ssh.Password(password)},
		Timeout: 10 * time.Second,
	}
	addr := server.Address()
	if !strings.Contains(addr, ":") {
		addr += ":22"
	}
	sshc, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s: %v", server, err)
	}
	client := &Client{
		server: server,
		sshc:   sshc,
		config: config,
		addr:   addr,
	}
	client.SetWarnTimeout(0)
	client.SetKillTimeout(0)
	return client, nil
}

func (c *Client) dialOnReboot() error {
	// First wait until SSH isn't working anymore.
	timeout := time.After(c.killTimeout)
	relog := time.NewTicker(c.warnTimeout)
	defer relog.Stop()
	retry := time.NewTicker(1 * time.Second)
	defer retry.Stop()

	waitConfig := *c.config
	waitConfig.Timeout = 5 * time.Second
	for {
		before := time.Now()
		sshc, err := ssh.Dial("tcp", c.addr, &waitConfig)
		if err != nil {
			// It's gone.
			break
		}
		sshc.Close()
		// Dial was observed not respecting the timeout by a long shot. Enforce it.
		if time.Now().After(before.Add(waitConfig.Timeout)) {
			break
		}

		select {
		case <-retry.C:
		case <-relog.C:
			printf("Reboot of %s is taking a while...", c.server)
		case <-timeout:
			return fmt.Errorf("kill-timeout reached, %s did not reboot after request", c.server)
		}
	}

	// Then wait for it to come back up.
	for {
		sshc, err := ssh.Dial("tcp", c.addr, c.config)
		if err == nil {
			c.sshc.Close()
			c.sshc = sshc
			return nil
		}
		select {
		case <-retry.C:
		case <-relog.C:
			printf("Reboot of %s is taking a while...", c.server)
		case <-timeout:
			return fmt.Errorf("kill-timeout reached, cannot reconnect to %s after reboot: %v", c.server, err)
		}
	}
}

func (c *Client) Close() error {
	return c.sshc.Close()
}

func (c *Client) Server() Server {
	return c.server
}

func (c *Client) SetWarnTimeout(timeout time.Duration) {
	if timeout == 0 {
		timeout = defaultWarnTimeout
	} else if timeout == -1 {
		timeout = maxTimeout
	}
	c.warnTimeout = timeout

	if c.killTimeout%c.warnTimeout == 0 {
		// So message from kill won't race with warning.
		c.killTimeout -= 1 * time.Second
	}
}

func (c *Client) SetKillTimeout(timeout time.Duration) {
	if timeout == 0 {
		timeout = defaultKillTimeout
	} else if timeout == -1 {
		timeout = maxTimeout
	}
	c.killTimeout = timeout

	if c.killTimeout%c.warnTimeout == 0 {
		// So message from kill won't race with warning.
		c.killTimeout -= 1 * time.Second
	}
}

func (c *Client) WriteFile(path string, data []byte) error {
	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	errch := make(chan error, 2)
	go func() {
		_, err := stdin.Write(data)
		if err != nil {
			errch <- err
		}
		errch <- stdin.Close()
	}()

	debugf("Writing to %s at %s:\n-----\n%# v\n-----", c.server, path, string(data))

	var stderr safeBuffer
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`%s/bin/bash -c "cat >'%s'"`, c.sudo(), path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		return fmt.Errorf("cannot write to %s at %s: %v", c.server, path, err)
	}

	if err := <-errch; err != nil {
		printf("Error writing to %s at %s: %v", c.server, path, err)
	}
	return nil
}

func (c *Client) ReadFile(path string) ([]byte, error) {
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	debugf("Reading from %s at %s...", c.server, path)

	var stdout, stderr safeBuffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	cmd := fmt.Sprintf(`%scat "%s"`, c.sudo(), path)
	err = c.runCommand(session, cmd, nil, &stderr)
	if err != nil {
		err = outputErr(stderr.Bytes(), err)
		logf("Cannot read from %s at %s: %v", c.server, path, err)
		return nil, fmt.Errorf("cannot read from %s at %s: %v", c.server, path, err)
	}
	output := stdout.Bytes()
	debugf("Got data from %s at %s:\n-----\n%# v\n-----", c.server, path, string(output))
	return output, nil
}

const (
	traceOutput = iota
	combinedOutput
	splitOutput
	shellOutput
)

func (c *Client) Run(script string, dir string, env *Environment) error {
	_, err := c.run(script, dir, env, combinedOutput)
	return err
}

func (c *Client) Output(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, splitOutput)
}

func (c *Client) CombinedOutput(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, combinedOutput)
}

func (c *Client) Trace(script string, dir string, env *Environment) (output []byte, err error) {
	return c.run(script, dir, env, traceOutput)
}

func (c *Client) Shell(script string, dir string, env *Environment) error {
	_, err := c.run(script, dir, env, shellOutput)
	return err
}

type rebootError struct {
	Key string
}

func (e *rebootError) Error() string { return "reboot requested" }

const maxReboots = 10

func (c *Client) run(script string, dir string, env *Environment, mode int) (output []byte, err error) {
	if env == nil {
		env = NewEnvironment()
	}
	rebootKey := ""
	for reboot := 0; ; reboot++ {
		if rebootKey == "" {
			rebootKey = strconv.Itoa(reboot)
		}
		env.Set("SPREAD_REBOOT", rebootKey)
		output, err = c.runPart(script, dir, env, mode, output)
		rerr, ok := err.(*rebootError)
		if !ok {
			return output, err
		}
		if reboot > maxReboots {
			return nil, fmt.Errorf("%s rebooted more than %d times", c.server)
		}

		printf("Rebooting %s as requested...", c.server)

		rebootKey = rerr.Key
		output = append(output, '\n')

		timedout := time.After(c.killTimeout)
		err := c.Run(fmt.Sprintf("reboot &\nsleep %.0f", c.killTimeout.Seconds()), "", nil)
		if err != nil {
			err = c.Run("echo should-have-disconnected", "", nil)
		}
		if err == nil {
			select {
			case <-timedout:
				return nil, fmt.Errorf("kill-timeout reached while waiting for %s to reboot", c.server)
			default:
			}
			return nil, fmt.Errorf("reboot request on %s failed", c.server)
		}
		if err := c.dialOnReboot(); err != nil {
			return nil, err
		}
	}
	panic("unreachable")
}

var rebootExp = regexp.MustCompile("^<REBOOT(?: (.*))?>$")

func (c *Client) runPart(script string, dir string, env *Environment, mode int, previous []byte) (output []byte, err error) {
	script = strings.TrimSpace(script)
	if len(script) == 0 {
		return nil, nil
	}
	script += "\n"
	session, err := c.sshc.NewSession()
	if err != nil {
		return nil, err
	}
	defer session.Close()

	var buf bytes.Buffer
	if dir != "" {
		buf.WriteString(fmt.Sprintf("cd \"%s\"\n", dir))
	}
	buf.WriteString("REBOOT() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<REBOOT>' || echo \"<REBOOT $1>\"; exit 213; }\n")
	buf.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	buf.WriteString("export DEBIAN_PRIORITY=critical\n")

	for _, k := range env.Keys() {
		v := env.Get(k)
		if len(v) == 0 || v[0] == '"' || v[0] == '\'' {
			fmt.Fprintf(&buf, "export %s=%s\n", k, v)
		} else {
			fmt.Fprintf(&buf, "export %s=\"%s\"\n", k, v)
		}
	}
	if mode == shellOutput && env.Get("PS1") != "" {
		fmt.Fprintf(&buf, "echo PS1=\\''%s'\\' > /root/.bashrc\n", env.Get("PS1"))
	}
	if mode == traceOutput {
		// Don't trace environment variables so secrets don't leak.
		fmt.Fprintf(&buf, "set -x\n")
	}

	if mode == shellOutput {
		fmt.Fprintf(&buf, "\n%s\n", script)
	} else {
		// Prevent any commands attempting to read from stdin to consume
		// the shell script itself being sent to bash via its stdin.
		fmt.Fprintf(&buf, "\n(\n%s\n) < /dev/null\n", script)
	}

	errch := make(chan error, 2)
	if mode == shellOutput {
		session.Stdin = os.Stdin
		errch <- nil
	} else {
		stdin, err := session.StdinPipe()
		if err != nil {
			return nil, err
		}
		defer stdin.Close()

		go func() {
			_, err := stdin.Write(buf.Bytes())
			if err != nil {
				errch <- err
			}
			errch <- stdin.Close()
		}()
	}

	debugf("Sending script to %s:\n-----\n%s\n------", c.server, buf.Bytes())

	var stdout, stderr safeBuffer
	var cmd string
	switch mode {
	case traceOutput, combinedOutput:
		cmd = c.sudo() + "/bin/bash -eu - 2>&1"
		session.Stdout = &stdout
	case splitOutput:
		cmd = c.sudo() + "/bin/bash -eu -"
		session.Stdout = &stdout
		session.Stderr = &stderr
	case shellOutput:
		cmd = fmt.Sprintf("{\nf=$(mktemp)\ntrap 'rm '$f EXIT\ncat > $f <<SCRIPT_END\n%s\nSCRIPT_END\n%s/bin/bash $f\n}", buf.String(), c.sudo())
		session.Stdout = os.Stdout
		session.Stderr = os.Stderr
		w, h, err := terminal.GetSize(0)
		if err != nil {
			return nil, fmt.Errorf("cannot get local terminal size: %v", err)
		}
		if err := session.RequestPty(getenv("TERM", "vt100"), h, w, nil); err != nil {
			return nil, fmt.Errorf("cannot get remote pseudo terminal: %v", err)
		}
	default:
		panic("internal error: invalid output mode")
	}

	if mode == shellOutput {
		termLock()
		tstate, terr := terminal.MakeRaw(0)
		if terr != nil {
			return nil, fmt.Errorf("cannot put local terminal in raw mode: %v", terr)
		}
		err = session.Run(cmd)
		terminal.Restore(0, tstate)
		termUnlock()
	} else {
		err = c.runCommand(session, cmd, &stdout, &stderr)
	}

	if stdout.Len() > 0 {
		debugf("Output from running script on %s:\n-----\n%s\n-----", c.server, stdout.Bytes())
	}
	if stderr.Len() > 0 {
		debugf("Error output from running script on %s:\n-----\n%s\n-----", c.server, stderr.Bytes())
	}

	if e, ok := err.(*ssh.ExitError); ok && e.ExitStatus() == 213 {
		lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'})
		if match := rebootExp.FindSubmatch(lines[len(lines)-1]); len(match) > 0 {
			return append(previous, stdout.Bytes()...), &rebootError{string(match[1])}
		}
	}

	if err == nil || mode != splitOutput {
		output = stdout.Bytes()
	} else if mode == splitOutput {
		output = stderr.Bytes()
	}

	// When running scripts under Go's non-shell ssh session, this fails:
	// # echo echo ok | /bin/bash -c "/bin/bash --login" 2>&1 | cat
	const errmsg = "mesg: ttyname failed: Inappropriate ioctl for device"
	output = bytes.TrimSpace(bytes.TrimPrefix(output, []byte(errmsg)))

	output = append(previous, output...)
	if err != nil {
		return nil, outputErr(output, err)
	}
	if err := <-errch; err != nil {
		printf("Error writing script to %s: %v", c.server, err)
	}
	return output, nil
}

func (c *Client) sudo() string {
	if c.config.User == "root" {
		return ""
	}
	return "sudo -i "
}

func getenv(name, defaultValue string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return defaultValue
}

func (c *Client) RemoveAll(path string) error {
	_, err := c.CombinedOutput(fmt.Sprintf(`rm -rf "%s"`, path), "", nil)
	return err
}

func (c *Client) SetupRootAccess(password string) error {
	var script string
	if c.config.User == "root" {
		script = fmt.Sprintf(`echo root:'%s' | chpasswd`, password)
	} else {
		script = strings.Join([]string{
			`sudo sed -i 's/\(PermitRootLogin\|PasswordAuthentication\)\>.*/\1 yes/' /etc/ssh/sshd_config`,
			`echo root:'` + password + `' | sudo chpasswd`,
			`sudo pkill -o -HUP sshd || true`,
		}, "\n")
	}
	_, err := c.CombinedOutput(script, "", nil)
	if err != nil {
		return fmt.Errorf("cannot setup root access: %s", err)
	}
	if c.config.User == "root" {
		c.config.Auth = []ssh.AuthMethod{ssh.Password(password)}
	}
	return nil
}

func (c *Client) MissingOrEmpty(dir string) (bool, error) {
	output, err := c.Output(fmt.Sprintf(`! test -e "%s" || ls -a "%s"`, dir, dir), "", nil)
	if err != nil {
		return false, fmt.Errorf("cannot check if %s on %s is empty: %v", dir, c.server, err)
	}
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		for _, s := range strings.Split(string(output), "\n") {
			if s != "." && s != ".." {
				debugf("Found %q inside %q, considering non-empty.", s, dir)
				return false, nil
			}
		}
	}
	return true, nil
}

func (c *Client) Send(from, to string, include, exclude []string) error {
	empty, err := c.MissingOrEmpty(to)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("remote directory %s is not empty", to)
	}

	session, err := c.sshc.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	defer stdin.Close()

	args := []string{
		"-cz",
		"--exclude=.spread-reuse*",
	}
	for _, pattern := range exclude {
		args = append(args, "--exclude="+pattern)
	}
	for _, pattern := range include {
		args = append(args, pattern)
	}

	var stderr bytes.Buffer

	cmd := exec.Command("tar", args...)
	cmd.Dir = from
	cmd.Stdout = stdin
	cmd.Stderr = &stderr
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("cannot start local tar command: %v", err)
	}

	errch := make(chan error, 1)
	go func() {
		errch <- cmd.Wait()
		stdin.Close()
	}()

	var stdout safeBuffer
	session.Stdout = &stdout
	rcmd := fmt.Sprintf(`%s/bin/bash -c "mkdir -p '%s' && cd '%s' && /bin/tar -xz 2>&1"`, c.sudo(), to, to)
	err = c.runCommand(session, rcmd, &stdout, nil)
	if err != nil {
		return outputErr(stdout.Bytes(), err)
	}

	if err := <-errch; err != nil {
		return fmt.Errorf("local tar command returned error: %v", outputErr(stderr.Bytes(), err))
	}
	return nil
}

const (
	defaultWarnTimeout = 5 * time.Minute
	defaultKillTimeout = 15 * time.Minute
	maxTimeout         = 365 * 24 * time.Hour
)

func (c *Client) runCommand(session *ssh.Session, cmd string, stdout, stderr io.Writer) error {
	err := session.Start(cmd)
	if err != nil {
		return fmt.Errorf("cannot start remote command: %v", err)
	}

	done := make(chan error)
	go func() {
		done <- session.Wait()
	}()

	var lastOut, lastErr int

	kill := time.After(c.killTimeout)
	warn := time.NewTicker(c.warnTimeout)
	defer warn.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-kill:
			session.Signal(ssh.SIGKILL)
			out := stdout
			if out == nil {
				out = stderr
			}
			if out != nil {
				out.Write([]byte("\n<kill-timeout reached>"))
			}
			return fmt.Errorf("kill-timeout reached")
		case <-warn.C:
			var output, errput []byte
			if buf, ok := stdout.(*safeBuffer); ok {
				output, lastOut = buf.Since(lastOut)
			}
			if buf, ok := stderr.(*safeBuffer); ok {
				errput, lastErr = buf.Since(lastErr)
				if len(output) == 0 || bytes.HasPrefix(errput, output) {
					// Also avoids double (... same ...) message.
					output = errput
				} else if len(errput) > 0 {
					output = append(output, '\n', '\n')
					output = append(output, errput...)
				}
			}
			if bytes.Equal(output, unchangedMarker) {
				printf("WARNING: %s running late. Output unchanged.", c.server)
			} else if len(output) == 0 {
				printf("WARNING: %s running late. Output still empty.", c.server)
			} else {
				printf("WARNING: %s running late. Current output:\n-----\n%s\n-----", c.server, output)
			}
		}
	}
	panic("unreachable")
}

var commandExp = regexp.MustCompile("^<([A-Z_]+)(?: (.*))?>$")

// runScript runs a local script in a polished manner.
//
// It's not used by the SSH client, but mimics the Client.runPart+runCommand closely.
func runScript(mode int, script, dir string, env *Environment, warnTimeout, killTimeout time.Duration) (stdout, stderr []byte, err error) {
	script = strings.TrimSpace(script)
	if len(script) == 0 {
		return nil, nil, nil
	}
	script += "\n"

	var buf bytes.Buffer
	buf.WriteString("ADDRESS() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<ADDRESS>' || echo \"<ADDRESS $1>\"; }\n")
	buf.WriteString("FATAL() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<FATAL>' || echo \"<FATAL $@>\"; exit 213; }\n")
	buf.WriteString("ERROR() { { set +xu; } 2> /dev/null; [ -z \"$1\" ] && echo '<ERROR>' || echo \"<ERROR $@>\"; exit 213; }\n")
	buf.WriteString("export DEBIAN_FRONTEND=noninteractive\n")
	buf.WriteString("export DEBIAN_PRIORITY=critical\n")

	for _, k := range env.Keys() {
		v := env.Get(k)
		if len(v) == 0 || v[0] == '"' || v[0] == '\'' {
			fmt.Fprintf(&buf, "export %s=%s\n", k, v)
		} else {
			fmt.Fprintf(&buf, "export %s=\"%s\"\n", k, v)
		}
	}

	if mode == traceOutput {
		// Don't trace environment variables so secrets don't leak.
		fmt.Fprintf(&buf, "set -x\n")
	}

	// Prevent any commands attempting to read from stdin to consume
	// the shell script itself being sent to bash via its stdin.
	fmt.Fprintf(&buf, "\n(\n%s\n) < /dev/null\n", script)

	debugf("Running local script:\n-----\n%s\n------", buf.Bytes())

	var outbuf, errbuf safeBuffer
	cmd := exec.Command("/bin/bash", "-eu", "-")
	cmd.Stdin = &buf
	if dir != "" {
		cmd.Dir = dir
	}
	switch mode {
	case traceOutput, combinedOutput:
		cmd.Stdout = &outbuf
		cmd.Stderr = &outbuf
	case splitOutput:
		cmd.Stdout = &outbuf
		cmd.Stderr = &errbuf
	case shellOutput:
		panic("internal error: runScript does not support shell mode")
	default:
		panic("internal error: invalid output mode")
	}

	err = cmd.Start()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot start local command: %v", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	if warnTimeout == 0 {
		warnTimeout = defaultWarnTimeout
	} else if warnTimeout == -1 {
		warnTimeout = maxTimeout
	}
	if killTimeout == 0 {
		killTimeout = defaultKillTimeout
	} else if killTimeout == -1 {
		killTimeout = maxTimeout
	}

	if killTimeout%warnTimeout == 0 {
		// So message from kill won't race with warning.
		killTimeout -= 1 * time.Second
	}

	var lastOut, lastErr int

	kill := time.After(killTimeout)
	warn := time.NewTicker(warnTimeout)
	defer warn.Stop()
Loop:
	for {
		select {
		case err = <-done:
			break Loop
		case <-kill:
			cmd.Process.Kill()
			buf := &outbuf
			if errbuf.Len() > 0 {
				buf = &errbuf
			}
			buf.Write([]byte("\n<kill-timeout reached>"))
			err = fmt.Errorf("kill-timeout reached")
		case <-warn.C:
			var output, errput []byte
			output, lastOut = outbuf.Since(lastOut)
			errput, lastErr = errbuf.Since(lastErr)
			if len(output) == 0 || bytes.HasPrefix(errput, output) {
				// Also avoids double (... same ...) message.
				output = errput
			} else if len(errput) > 0 {
				output = append(output, '\n', '\n')
				output = append(output, errput...)
			}
			if bytes.Equal(output, unchangedMarker) {
				printf("WARNING: local script running late. Output unchanged.")
			} else if len(output) == 0 {
				printf("WARNING: local script running late. Output still empty.")
			} else {
				printf("WARNING: local script running late. Current output:\n-----\n%s\n-----", output)
			}
		}
	}

	if outbuf.Len() > 0 {
		debugf("Output from running local script:\n-----\n%s\n-----", outbuf.Bytes())
	}
	if errbuf.Len() > 0 {
		debugf("Error output from running script:\n-----\n%s\n-----", errbuf.Bytes())
	}

	if exitStatus(err) == 213 {
		lines := bytes.Split(bytes.TrimSpace(outbuf.Bytes()), []byte{'\n'})
		m := commandExp.FindSubmatch(lines[len(lines)-1])
		if len(m) > 0 && string(m[1]) == "ERROR" {
			return nil, nil, fmt.Errorf("%s", m[2])
		}
		if len(m) > 0 && string(m[1]) == "FATAL" {
			return nil, nil, &FatalError{fmt.Errorf("%s", m[2])}
		}
	}

	if err != nil {
		if errbuf.Len() > 0 {
			err = outputErr(errbuf.Bytes(), err)
		} else if outbuf.Len() > 0 {
			err = outputErr(outbuf.Bytes(), err)
		}
		return nil, nil, err
	}
	return outbuf.Bytes(), errbuf.Bytes(), nil
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		return 1
	}
	wait, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		return 1
	}
	return wait.ExitStatus()
}

type safeBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (sbuf *safeBuffer) Write(data []byte) (int, error) {
	sbuf.mu.Lock()
	n, err := sbuf.buf.Write(data)
	sbuf.mu.Unlock()
	return n, err
}

func (sbuf *safeBuffer) Bytes() []byte {
	sbuf.mu.Lock()
	data := sbuf.buf.Bytes()
	sbuf.mu.Unlock()
	return data
}

var unchangedMarker = []byte("(...)")

func (sbuf *safeBuffer) Since(offset int) (data []byte, len int) {
	sbuf.mu.Lock()
	defer sbuf.mu.Unlock()

	data = sbuf.buf.Bytes()
	copy := true
	for i := offset - 1; i > 1; i-- {
		if data[i] == '\n' {
			data = append(unchangedMarker, data[i:]...)
			copy = false
			break
		}
	}
	if copy {
		data = append([]byte(nil), data...)
	}
	return bytes.TrimSpace(data), sbuf.buf.Len()
}

func (sbuf *safeBuffer) Len() int {
	sbuf.mu.Lock()
	l := sbuf.buf.Len()
	sbuf.mu.Unlock()
	return l
}

func outputErr(output []byte, err error) error {
	output = bytes.TrimSpace(output)
	if len(output) > 0 {
		if bytes.Contains(output, []byte{'\n'}) {
			err = fmt.Errorf("\n-----\n%s\n-----", output)
		} else {
			err = fmt.Errorf("%s", output)
		}
	}
	return err
}

func waitPortUp(what fmt.Stringer, address string) error {
	if !strings.Contains(address, ":") {
		address += ":22"
	}

	var timeout = time.After(5 * time.Minute)
	var relog = time.NewTicker(15 * time.Second)
	defer relog.Stop()
	var retry = time.NewTicker(1 * time.Second)
	defer retry.Stop()

	for {
		conn, err := net.Dial("tcp", address)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case <-retry.C:
		case <-relog.C:
			printf("Cannot connect to %s: %v", what, err)
		case <-timeout:
			return fmt.Errorf("cannot connect to %s: %v", what, err)
		}
	}
	return nil
}

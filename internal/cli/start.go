package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dboss/internal/config"
	"dboss/internal/ctl"
	"dboss/internal/daemon"
	"dboss/internal/devtls"
	"dboss/internal/ops"
	"dboss/internal/supervisor"

	"golang.org/x/term"
)

// start runs the session in the foreground. Under systemd this is the service process; on a
// terminal every app's output is echoed with an app/proc prefix, and the apps wait for ENTER
// after the banner so its addresses can be opened first. A dev session then restarts its app on r.
// A dev start in a folder whose session already runs restarts that session's app instead.
func (c CLI) start(args []string) error {
	set := flag.NewFlagSet("start", flag.ContinueOnError)
	set.SetOutput(c.Err)
	configPath := configFlag(set)
	login := set.Bool("login", false, "print a one-time console sign-in link")
	yes := set.Bool("y", false, "start the apps without waiting for ENTER")
	https := set.Bool("https", false, "dev session: listen on :80 and :443 with the local certificate authority instead of a free http port")
	if err := set.Parse(args); err != nil {
		return err
	}
	if set.NArg() != 0 {
		return errors.New("usage: dboss start [-c path] [--login] [-y] [--https]")
	}
	cfg, err := loadHostConfig(*configPath)
	if err != nil {
		return err
	}
	if *https && !cfg.Dev() {
		return errors.New("--https is for a dev session; a host serves HTTPS through proxy.tls")
	}
	info, statErr := os.Stdout.Stat()
	terminal := statErr == nil && info.Mode()&os.ModeCharDevice != 0
	if cfg.Dev() {
		if handed, err := c.restartLive(cfg, *https, terminal); handed {
			return err
		}
	}
	var echo *supervisor.Echo
	if terminal {
		echo = supervisor.NewEcho(c.Out)
		if cfg.Dev() {
			echo.Solo()
		}
		warnUnignoredRuntime(c.Err, cfg)
		if *https {
			c.offerTrust()
		}
	}
	session, err := daemon.Build(cfg, echo, daemon.Options{HTTPS: *https})
	if err != nil {
		return err
	}
	defer session.Close()
	if *login {
		// stdout only: the token must never reach the daemon log
		local, _, err := session.LoginURL()
		if err != nil {
			return err
		}
		fmt.Fprintf(c.Out, "login: %s (one-time, 3 minutes)\n", local)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := session.Serve(ctx); err != nil {
		return err
	}
	if echo == nil {
		session.Boot()
		<-ctx.Done()
		return nil
	}
	keys, restore := c.terminalKeys()
	defer restore()
	if !*yes && !c.waitForEnter(ctx, keys) {
		return nil
	}
	session.Boot()
	if keys == nil || !cfg.Dev() {
		restore()
		<-ctx.Done()
		return nil
	}
	app := filepath.Base(cfg.Dir)
	say := func(text string) { echo.Print(echo.Key("dboss", "dboss") + text) }
	say("press r to restart " + app + ", Ctrl-C to stop")
	restartOnKeys(ctx, keys, app, say, session.RestartApp)
	return nil
}

// restartLive hands a dev start to the session already serving this folder: rescan, restart the
// app, print that session's banner and return, so `dboss start -y` can run again and again (an
// agent harness does) and always leaves the app restarted. It reports false when no session
// answers, or when only a fresh session applies the change (--https differs, or the rescan names
// a key that needs a restart); the start then takes the earlier session over.
func (c CLI) restartLive(cfg config.Config, https, terminal bool) (bool, error) {
	probe := ctl.Client{Socket: cfg.Socket, Timeout: 5 * time.Second}
	var live daemon.Banner
	if err := probe.Call(ctl.Request{Method: ctl.BannerMethod}, &live); err != nil || live.HTTPS != https {
		return false, nil
	}
	var rescan ops.RescanResult
	if err := probe.Call(ctl.Request{Method: ops.ActionRescan}, &rescan); err != nil || len(rescan.RestartRequired) > 0 {
		return false, nil
	}
	app := filepath.Base(cfg.Dir)
	restartErr := ctl.Client{Socket: cfg.Socket}.Call(ctl.Request{Method: ops.ActionRestart, App: app}, nil)
	if err := probe.Call(ctl.Request{Method: ctl.BannerMethod}, &live); err != nil {
		return true, errors.Join(restartErr, err)
	}
	echo := supervisor.NewEcho(c.Out)
	echo.Solo()
	if !terminal {
		echo.Plain()
	}
	for _, line := range live.Lines(echo) {
		echo.Print(line)
	}
	if restartErr != nil {
		return true, fmt.Errorf("restart %s: %w", app, restartErr)
	}
	echo.Print(echo.Key("dboss", "dboss") + "restarted " + app + " in the running session; `dboss logs -f` follows its output")
	return true, nil
}

// waitForEnter holds the apps until ENTER and reports false when the session is interrupted
// first. Without a terminal on stdin nobody can press it, so it does not wait.
func (c CLI) waitForEnter(ctx context.Context, keys <-chan byte) bool {
	if keys == nil {
		return true
	}
	fmt.Fprint(c.Out, "Press ENTER to start the apps (dboss start -y skips this) ")
	// Single-key input does not echo, so the ENTER that ends the prompt line is printed here.
	defer fmt.Fprintln(c.Out)
	for {
		select {
		case key, open := <-keys:
			if !open || key == '\n' || key == '\r' {
				return true
			}
		case <-ctx.Done():
			return false
		}
	}
}

// trust adds the dev certificate authority's root to the system trust store, so the browser
// accepts the HTTPS a dev session serves. It creates the root first when there is none yet.
func (c CLI) trust(args []string) error {
	if len(args) != 0 {
		return errors.New("usage: dboss trust")
	}
	dir, err := devtls.DefaultDir()
	if err != nil {
		return err
	}
	authority, err := devtls.Open(dir)
	if err != nil {
		return err
	}
	if authority.Trusted() {
		fmt.Fprintf(c.Out, "already trusted: %s\n", authority.RootPath())
		return nil
	}
	return c.installTrust(authority)
}

func (c CLI) installTrust(authority *devtls.Authority) error {
	fmt.Fprintf(c.Out, "adding %s to the system trust store\n", authority.RootPath())
	if err := authority.Install(c.Out); err != nil {
		return err
	}
	if !authority.Trusted() {
		return fmt.Errorf("the root was added but the system still does not trust it; import %s by hand", authority.RootPath())
	}
	fmt.Fprintln(c.Out, "trusted: restart the browser if it was open; Firefox may need the root imported in its own settings")
	return nil
}

// offerTrust asks, before a hand-run dev session starts serving, whether to trust the local
// certificate authority its HTTPS uses. Declining or failing never stops the start; the banner
// keeps saying the certificate is not trusted.
func (c CLI) offerTrust() {
	in, ok := c.In.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return
	}
	dir, err := devtls.DefaultDir()
	if err != nil {
		return
	}
	authority, err := devtls.Open(dir)
	if err != nil || authority.Trusted() {
		return
	}
	fmt.Fprint(c.Out, "HTTPS in this dev session uses a local certificate authority this machine does not trust yet,\nso browsers will warn. Trust it now? It may ask for your password. [Y/n] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "" && answer != "y" && answer != "yes" {
		fmt.Fprintln(c.Out, "starting without it; run `dboss trust` any time")
		return
	}
	if err := c.installTrust(authority); err != nil {
		fmt.Fprintf(c.Err, "dboss: trust: %v; starting anyway\n", err)
	}
}

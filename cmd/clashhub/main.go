package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "on":
		cmdOn(false)
	case "auto-on":
		cmdOn(true)
	case "off":
		cmdOff()
	case "status":
		cmdStatus()
	case "serve":
		cmdServe(false)
	case "serve-auto":
		cmdServe(true)
	case "logs":
		cmdLogs()
	case "retry":
		cmdRetry()
	case "sysproxy":
		cmdSysProxy()
	case "update":
		cmdUpdate()
	case "doctor":
		cmdDoctor()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`clashhub - Shadowrocket (1082) -> FlClash (33210) hot-failover proxy for macOS

Usage:
  clashhub on       enable: save and configure system proxy (VPNs are not changed)
  clashhub off      disable: stop retries and restore the original system proxy
  clashhub status   show mode, upstream, canary latency, VPN and proxy state
  clashhub retry    ask a running daemon to probe the engine again now
  clashhub update   regenerate the node config and reload the engine
  clashhub logs     show the last log lines
  clashhub doctor   one-shot diagnostics
  clashhub serve    run the daemon in the foreground (internal)

Config: ~/Library/Application Support/ClashHub/config.json
State:  ~/Library/Application Support/ClashHub/state.json
Log:    ~/Library/Logs/ClashHub.log
`)
}

func cmdServe(auto bool) {
	cfg, err := loadConfig()
	if err != nil {
		fatal("load config: %v", err)
	}
	app, err := newApp(cfg)
	if err != nil {
		fatal("init: %v", err)
	}
	app.autoStart = auto
	if err := app.run(); err != nil {
		fmt.Fprintf(os.Stderr, "clashhub: %v\n", err)
		os.Exit(1)
	}
}

func lockOperation() *os.File {
	if err := os.MkdirAll(appSupportDir(), 0o755); err != nil {
		fatal("create state directory: %v", err)
	}
	f, err := os.OpenFile(lockPath()+".operation", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fatal("open operation lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		fatal("lock operation: %v", err)
	}
	return f
}

func cmdOn(auto bool) {
	operation := lockOperation()
	defer operation.Close()
	cfg, err := loadConfig()
	if err != nil {
		fatal("load config: %v", err)
	}
	st, err := readState()
	if err != nil {
		fatal("read state: %v", err)
	}
	if st != nil && st.Active && daemonMatches(st) {
		fmt.Printf("clashhub is already on (pid %d)\n", st.PID)
		printStatus(st, cfg)
		return
	}
	if auto {
		// Check before recovery as well as afterwards: recovery must never
		// re-enable a saved third-party proxy or PAC on unattended login.
		if st != nil && len(st.Services) > 0 {
			if err := autoProxyAllowed(st.Services, cfg); err != nil {
				fatal("automatic start refused: saved recovery snapshot: %v", err)
			}
		}
		if _, err := autoSnapshotWithRetry(cfg, 6, 5*time.Second); err != nil {
			fatal("automatic start refused: %v", err)
		}
	}
	if st != nil && (st.Active || len(st.Services) > 0) {
		if st.PID > 0 && pidAlive(st.PID) {
			fatal("state PID is alive but identity is unverified; refusing cleanup")
		}
		// Turning on takes over unconditionally: stale state is cleared and
		// the CURRENT system proxy is snapshotted fresh as the restore point.
		// Restoring a stale snapshot here would touch settings that are no
		// longer ours (for example the fallback client rewrote them) and
		// could disable a working proxy.
		if err := clearStaleState(st); err != nil {
			fatal("clear stale state: %v", err)
		}
	}

	if err := runGeneratorExclusive(); err != nil {
		fatal("generate node config: %v", err)
	}
	services, err := snapshotAllServices()
	if auto {
		services, err = autoSnapshotWithRetry(cfg, 6, 5*time.Second)
	}
	if err != nil {
		fatal("snapshot system proxy: %v", err)
	}
	st = &State{
		Active:       true,
		StartedAt:    time.Now(),
		Services:     services,
		AppliedHTTP:  cfg.HTTPListen,
		AppliedSOCKS: cfg.SOCKSListen,
		Mode:         "starting",
	}
	if err := writeState(st); err != nil {
		fatal("write state: %v", err)
	}

	exe, err := os.Executable()
	if err != nil {
		_ = cleanupFromState(st, cfg)
		fatal("locate executable: %v", err)
	}
	lf, err := os.OpenFile(logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		_ = cleanupFromState(st, cfg)
		fatal("open log: %v", err)
	}
	serveArg := "serve"
	if auto {
		serveArg = "serve-auto"
	}
	cmd := exec.Command(exe, serveArg)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = lf.Close()
		_ = cleanupFromState(st, cfg)
		fatal("start daemon: %v", err)
	}
	_ = cmd.Process.Release()
	_ = lf.Close()

	fmt.Println("clashhub starting (this can take a few seconds while the engine starts)...")
	deadline := time.Now().Add(50 * time.Second)
	var last *State
	for time.Now().Before(deadline) {
		cur, _ := readState()
		last = cur
		if cur != nil && cur.PID > 0 {
			if !pidAlive(cur.PID) {
				fmt.Fprintln(os.Stderr, "daemon exited during startup; last log lines:")
				printLogTail(15)
				cleanupFromState(cur, cfg)
				os.Exit(1)
			}
			if cur.Ready && (cur.Mode == string(ModePrimary) || cur.Mode == string(ModeFallback)) {
				printStatus(cur, cfg)
				return
			}
		}
		time.Sleep(400 * time.Millisecond)
	}

	fmt.Println("still starting; check with: clashhub status / clashhub logs")
	if last != nil {
		printStatus(last, cfg)
	}
}

func cmdOff() {
	operation := lockOperation()
	defer operation.Close()
	cfg, err := loadConfig()
	if err != nil {
		fatal("load config: %v", err)
	}
	st, err := readState()
	if err != nil {
		fatal("read state: %v", err)
	}
	if st == nil {
		fmt.Println("clashhub is already off")
		return
	}
	wasRunning := st.Active && daemonMatches(st)
	if wasRunning {
		p, err := os.FindProcess(st.PID)
		if err != nil {
			fatal("find daemon: %v", err)
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			fatal("signal daemon: %v", err)
		}
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) && daemonMatches(st) {
			time.Sleep(300 * time.Millisecond)
		}
		if daemonMatches(st) {
			fatal("daemon did not exit; refusing to force-kill")
		}
	} else if st.PID > 0 && pidAlive(st.PID) {
		fatal("state PID is alive but identity is unverified; refusing to signal or restore")
	}
	cur, err := readState()
	if err != nil {
		fatal("read state: %v", err)
	}
	if cur != nil && (cur.Active || len(cur.Services) > 0) {
		if wasRunning {
			fatal("safe proxy handoff incomplete; recovery snapshot retained (see clashhub logs); retry clashhub off")
		}
		// Off only restores settings clashhub owns; anything changed externally
		// (for example by the fallback client) is left untouched. Restore
		// failures must not block turning off: report and clear bookkeeping.
		if err := cleanupFromState(cur, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: restore system proxy incomplete; non-clashhub settings left untouched: %v\n", err)
			if e := clearStaleState(cur); e != nil {
				fatal("clear stale state: %v", e)
			}
		}
	}
	if id, e := exitSessionID(st.Services, st.AppliedHTTP, st.AppliedSOCKS); e == nil {
		if journal, e := loadExitJournal(id); e == nil {
			for _, intent := range journal.Fields {
				if intent.Done && !intent.After.Enabled {
					fmt.Fprintln(os.Stderr, "WARNING: no verified proxy was available; proxy disabled and traffic may connect directly without proxy protection")
					break
				}
			}
		}
	}
	fmt.Println("clashhub off")
}

// cmdSysProxy re-applies clashhub's own endpoints to the system proxy without
// touching the engine or the routing state. A third-party client (or a failed
// partial write) can leave the system pointing elsewhere or with a disabled
// HTTP entry, which looks like "the proxy is on but nothing works".
func cmdSysProxy() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("load config: %v", err)
	}
	st, err := readState()
	if err != nil {
		fatal("read state: %v", err)
	}
	if st == nil || !st.Active || !daemonMatches(st) {
		fatal("clashhub is not running; start it with: clashhub on")
	}
	if len(st.Services) == 0 {
		fatal("no proxy snapshot recorded; restart with: clashhub off && clashhub on")
	}
	if err := applyOurProxy(st.Services, cfg.HTTPListen, cfg.SOCKSListen); err != nil {
		fatal("apply system proxy: %v", err)
	}
	fmt.Printf("system proxy -> %s (http/https) / %s (socks)\n", cfg.HTTPListen, cfg.SOCKSListen)
	for _, svc := range st.Services {
		cur, e := snapshotService(svc.Name)
		if e != nil {
			fmt.Printf("  %s: unreadable (%v)\n", svc.Name, e)
			continue
		}
		fmt.Printf("  %s: web=%s:%d on=%v secure=%s:%d on=%v socks=%s:%d on=%v\n",
			svc.Name, cur.Web.Server, cur.Web.Port, cur.Web.Enabled,
			cur.Secure.Server, cur.Secure.Port, cur.Secure.Enabled,
			cur.Socks.Server, cur.Socks.Port, cur.Socks.Enabled)
	}
}

// clearStaleState resets stale bookkeeping without touching the system
// proxy: the caller snapshots current settings right after taking over.
func clearStaleState(st *State) error {
	if st == nil {
		return nil
	}
	st.Active = false
	st.Ready = false
	st.PID = 0
	st.ProcessIdentity = ""
	st.EnginePID = 0
	st.Services = nil
	st.AppliedHTTP = ""
	st.AppliedSOCKS = ""
	st.Mode = "off"
	st.Upstream = ""
	return writeState(st)
}

func cleanupFromState(st *State, cfg Config) error {
	if st == nil {
		return nil
	}
	// A recorded engine PID alone is not proof of ownership; never signal it here.
	if len(st.Services) > 0 {
		if st.AppliedHTTP == "" || st.AppliedSOCKS == "" {
			return fmt.Errorf("snapshot has no recorded applied proxy endpoints; retained for manual recovery")
		}
		if err := restoreAllProxy(st.Services, st.AppliedHTTP, st.AppliedSOCKS); err != nil {
			return err
		}
	}
	st.Active = false
	st.Ready = false
	st.PID = 0
	st.ProcessIdentity = ""
	st.EnginePID = 0
	st.Services = nil
	st.AppliedHTTP = ""
	st.AppliedSOCKS = ""
	st.Mode = "off"
	st.Upstream = ""
	return writeState(st)
}

func cmdStatus() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("load config: %v", err)
	}
	st, err := readState()
	if err != nil {
		fatal("read state: %v", err)
	}
	if st == nil {
		fmt.Println("clashhub: OFF (no state)")
		return
	}
	printStatus(st, cfg)
}

func printStatus(st *State, cfg Config) {
	running := st.Active && daemonMatches(st)
	if !running {
		fmt.Println("clashhub: OFF")
		if st.Mode != "" && st.Mode != "off" {
			fmt.Printf("last mode: %s (%s)\n", st.Mode, st.LastReason)
		}
		if st.Active {
			fmt.Println("state says active but the daemon is not running; run: clashhub off")
		}
		return
	}

	fmt.Printf("clashhub: ON (pid %d)\n", st.PID)
	fmt.Printf("mode: %s (%s)\n", st.Mode, st.Upstream)
	fmt.Printf("engine %s (pid %d): %s\n", cfg.PrimaryAddr, st.EnginePID, openWord(portOpen(cfg.PrimaryAddr, 500*time.Millisecond)))
	fmt.Printf("fallback %s: %s\n", cfg.FallbackAddr, openWord(portOpen(cfg.FallbackAddr, 500*time.Millisecond)))
	if st.ProbeMS > 0 {
		fmt.Printf("gstatic: %d ms\n", st.ProbeMS)
	}
	if st.AICode != 0 {
		fmt.Printf("openai: HTTP %d, %d ms\n", st.AICode, st.AIMs)
	}
	if st.Egress != "" {
		state := "not JP"
		if st.EgressOK {
			state = "JP ok"
		}
		fmt.Printf("egress: %s (%s)\n", st.Egress, state)
	}

	_, wantHTTP, _ := splitListen(cfg.HTTPListen)
	_, wantSocks, _ := splitListen(cfg.SOCKSListen)
	gotHTTP, gotSocks := currentSystemProxyPorts()
	if gotHTTP == wantHTTP && gotSocks == wantSocks {
		fmt.Printf("system proxy: http=%d socks=%d (ok)\n", gotHTTP, gotSocks)
	} else {
		fmt.Printf("system proxy: http=%d socks=%d (expected %d/%d)\n", gotHTTP, gotSocks, wantHTTP, wantSocks)
	}
	if !st.LastSwitch.IsZero() {
		fmt.Printf("last switch: %s (%s)\n", st.LastSwitch.Format("2006-01-02 15:04:05"), st.LastReason)
	}
	if !st.NextRetry.IsZero() && st.Mode == string(ModeFallback) {
		fmt.Printf("next engine retry: %s\n", st.NextRetry.Format("15:04:05"))
	}
	fmt.Printf("log: %s\n", logPath())
}

func openWord(ok bool) string {
	if ok {
		return "open"
	}
	return "closed"
}

func cmdUpdate() {
	if err := runGeneratorExclusive(); err != nil {
		fatal("generate node config: %v", err)
	}
	st, _ := readState()
	if st != nil && st.Active && daemonMatches(st) {
		p, err := os.FindProcess(st.PID)
		if err == nil {
			_ = p.Signal(syscall.SIGUSR2)
		}
		fmt.Println("node config regenerated; engine reload requested")
		return
	}
	fmt.Println("node config regenerated")
}

func cmdLogs() {
	printLogTail(80)
}

func printLogTail(n int) {
	b, err := os.ReadFile(logPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "read log: %v\n", err)
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	fmt.Println(strings.Join(lines, "\n"))
}

func cmdRetry() {
	st, err := readState()
	if err != nil {
		fatal("read state: %v", err)
	}
	if st == nil || !st.Active || !daemonMatches(st) {
		fatal("clashhub is not running; start it with: clashhub on")
	}
	p, err := os.FindProcess(st.PID)
	if err != nil {
		fatal("find daemon: %v", err)
	}
	if err := p.Signal(syscall.SIGUSR1); err != nil {
		fatal("signal daemon: %v", err)
	}
	fmt.Printf("asked clashhub (pid %d) to test 7890 again; watch: clashhub status\n", st.PID)
}

func cmdDoctor() {
	cfg, err := loadConfig()
	if err != nil {
		fatal("load config: %v", err)
	}
	fmt.Printf("config: %s\n", configPath())
	fmt.Printf("%s: %s\n", cfg.PrimaryAddr, openWord(portOpen(cfg.PrimaryAddr, 800*time.Millisecond)))
	fmt.Printf("%s: %s\n", cfg.FallbackAddr, openWord(portOpen(cfg.FallbackAddr, 800*time.Millisecond)))
	hc, sc := currentSystemProxyPorts()
	fmt.Printf("system proxy: http=%d socks=%d\n", hc, sc)

	for _, addr := range []string{cfg.PrimaryAddr, cfg.FallbackAddr} {
		fmt.Printf("%s:\n", addr)
		if !portOpen(addr, 800*time.Millisecond) {
			fmt.Println("  port closed")
			continue
		}
		code, ms, err := probeStatus(addr, cfg.ProbeURL, probeExpectedCode(cfg.ProbeURL), 6*time.Second)
		fmt.Printf("  probe:   HTTP %d, %dms, err=%v\n", code, ms, err)
		acode, ams, aerr := probeStatus(addr, cfg.AIProbeURL, 401, 8*time.Second)
		fmt.Printf("  openai:  HTTP %d, %dms, err=%v\n", acode, ams, aerr)
		ip, loc, eerr := egressThrough(addr, 8*time.Second)
		fmt.Printf("  egress:  %s %s, err=%v\n", loc, ip, eerr)
	}
}

func existsWord(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "found"
	}
	return "missing"
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

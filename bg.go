package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Background mode: "warren tcp 25565 -b" starts a detached copy of warren
// with the same arguments, waits until it's online, prints the address and
// returns. Each background process leaves a small JSON file in bgDir so
// "warren ps" and "warren stop" can find it again.

type bgProc struct {
	PID     int       `json:"pid"`
	Args    []string  `json:"args"`
	Log     string    `json:"log"`
	Started time.Time `json:"started"`
}

// takeBackgroundFlag removes -b/--background from args and reports whether
// it was there.
func takeBackgroundFlag(args []string) ([]string, bool) {
	var rest []string
	found := false
	for _, a := range args {
		switch a {
		case "-b", "--b", "-background", "--background":
			found = true
		default:
			rest = append(rest, a)
		}
	}
	return rest, found
}

func bgDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir = filepath.Join(dir, "warren", "background")
	return dir, os.MkdirAll(dir, 0o700)
}

// startBackground runs "warren ARGS..." detached and waits for it to come up.
func startBackground(args []string) error {
	dir, err := bgDir()
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	name := time.Now().Format("20060102-150405") + "-" + args[0]
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		name += "-" + args[1]
	}
	logPath := filepath.Join(dir, sanitize(name)+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachedAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	p := bgProc{PID: cmd.Process.Pid, Args: args, Log: logPath, Started: time.Now()}
	statePath := filepath.Join(dir, strconv.Itoa(p.PID)+".json")
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		cmd.Process.Kill()
		return err
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	// Tunnels print "warren is online"; the server logs its control port.
	ready := "warren is online"
	if args[0] == "server" {
		ready = "control port"
	}
	deadline := time.After(20 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
			os.Remove(statePath)
			out, _ := os.ReadFile(logPath)
			os.Stderr.Write(out)
			return errors.New("warren stopped right after starting (see above)")
		case <-deadline:
			fmt.Printf("warren is running in the background (pid %d) but isn't online yet.\n", p.PID)
			out, _ := os.ReadFile(logPath)
			if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); lines[len(lines)-1] != "" {
				fmt.Printf("Last message: %s\n", lines[len(lines)-1])
			}
			fmt.Printf("Log: %s\nStop it with: warren stop %d\n", logPath, p.PID)
			return nil
		case <-tick.C:
			out, _ := os.ReadFile(logPath)
			if strings.Contains(string(out), ready) {
				os.Stdout.Write(out)
				if !strings.HasSuffix(string(out), "\n\n") {
					fmt.Println()
				}
				fmt.Printf("  running in the background (pid %d)\n", p.PID)
				fmt.Printf("  log:   %s\n", logPath)
				fmt.Printf("  stop:  warren stop %d    list: warren ps\n\n", p.PID)
				return nil
			}
		}
	}
}

// shownArgs is p.Args for printing, with any --token value hidden.
func (p bgProc) shownArgs() string {
	out := make([]string, len(p.Args))
	hideNext := false
	for i, a := range p.Args {
		switch {
		case hideNext:
			out[i] = "***"
			hideNext = false
		case a == "-token" || a == "--token":
			out[i] = a
			hideNext = true
		case strings.HasPrefix(a, "-token=") || strings.HasPrefix(a, "--token="):
			out[i] = a[:strings.Index(a, "=")+1] + "***"
		default:
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '_'
	}, s)
}

// loadBgProcs returns the background processes that are still running and
// forgets the ones that aren't.
func loadBgProcs() ([]bgProc, string, error) {
	dir, err := bgDir()
	if err != nil {
		return nil, "", err
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var procs []bgProc
	for _, f := range files {
		var p bgProc
		b, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(b, &p) != nil || p.PID <= 0 || !processAlive(p.PID) {
			os.Remove(f)
			continue
		}
		procs = append(procs, p)
	}
	sort.Slice(procs, func(i, j int) bool { return procs[i].Started.Before(procs[j].Started) })
	return procs, dir, nil
}

// address finds the "url  ->  local" line a tunnel printed when it came online.
func (p bgProc) address() string {
	b, err := os.ReadFile(p.Log)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "  ->  ") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func cmdPs(args []string) error {
	procs, _, err := loadBgProcs()
	if err != nil {
		return err
	}
	if len(procs) == 0 {
		fmt.Println("No warren processes running in the background.")
		return nil
	}
	fmt.Printf("%-8s %-17s %-28s %s\n", "PID", "STARTED", "COMMAND", "ADDRESS")
	for _, p := range procs {
		fmt.Printf("%-8d %-17s %-28s %s\n", p.PID, p.Started.Format("2006-01-02 15:04"),
			p.shownArgs(), p.address())
	}
	return nil
}

func cmdStop(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: warren stop PID|all   (see \"warren ps\")")
	}
	procs, dir, err := loadBgProcs()
	if err != nil {
		return err
	}
	var targets []bgProc
	for _, p := range procs {
		if args[0] == "all" || args[0] == strconv.Itoa(p.PID) {
			targets = append(targets, p)
		}
	}
	if len(targets) == 0 {
		if args[0] == "all" {
			fmt.Println("No warren processes running in the background.")
			return nil
		}
		return fmt.Errorf("no background warren with pid %s (see \"warren ps\")", args[0])
	}
	for _, p := range targets {
		if err := stopProcess(p.PID); err != nil {
			return fmt.Errorf("stopping %d: %w", p.PID, err)
		}
		os.Remove(filepath.Join(dir, strconv.Itoa(p.PID)+".json"))
		fmt.Printf("Stopped %d (warren %s)\n", p.PID, p.shownArgs())
	}
	return nil
}

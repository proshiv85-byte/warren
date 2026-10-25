package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// GitHub repository that "warren update" downloads releases from.
const updateRepo = "proshiv85-byte/warren"

type ghRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only check whether a newer version exists")
	force := fs.Bool("force", false, "reinstall even if already up to date")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	rel, err := latestRelease()
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	newer := versionLess(version, latest)
	if !newer && !*force {
		fmt.Printf("warren %s is up to date.\n", version)
		return nil
	}
	if *check {
		fmt.Printf("warren %s is available (you have %s).\nRun \"warren update\" to install it. Changes: %s\n", latest, version, rel.HTMLURL)
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// Update the real file if warren is a symlink. Resolving can fail on
	// some Windows drives (network, OneDrive, Wine); the plain path is fine then.
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	// Fail early, before downloading, if we can't write next to the binary.
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".warren-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			if runtime.GOOS == "windows" {
				return fmt.Errorf("no permission to replace %s: run the terminal as administrator and try again", exe)
			}
			return fmt.Errorf("no permission to replace %s: run \"sudo warren update\"", exe)
		}
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	asset := "warren-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	var binURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case asset:
			binURL = a.URL
		case "SHA256SUMS":
			sumsURL = a.URL
		}
	}
	if binURL == "" {
		tmp.Close()
		return fmt.Errorf("release %s has no %s download for this system", rel.TagName, asset)
	}
	if sumsURL == "" {
		tmp.Close()
		return fmt.Errorf("release %s has no SHA256SUMS file, so the download can't be verified", rel.TagName)
	}

	fmt.Printf("Downloading warren %s (%s)...\n", latest, asset)
	want, err := expectedSum(sumsURL, asset)
	if err != nil {
		tmp.Close()
		return err
	}
	h := sha256.New()
	err = download(binURL, io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s (got %s, want %s): not installing it", asset, got, want)
	}

	if st, err := os.Stat(exe); err == nil {
		os.Chmod(tmpPath, st.Mode().Perm())
		copyOwner(st, tmpPath)
	} else {
		os.Chmod(tmpPath, 0o755)
	}
	out, err := exec.Command(tmpPath, "version").Output()
	if err != nil || !strings.Contains(string(out), latest) {
		return fmt.Errorf("the downloaded warren doesn't run on this system (%v): not installing it", err)
	}

	if err := replaceExecutable(exe, tmpPath); err != nil {
		return fmt.Errorf("installing: %w", err)
	}
	fmt.Printf("Updated warren %s -> %s (%s)\n", version, latest, exe)
	fmt.Printf("Changes: %s\n", rel.HTMLURL)
	afterUpdate(exe)
	return nil
}

func latestRelease() (*ghRelease, error) {
	req, err := http.NewRequest("GET", "https://api.github.com/repos/"+updateRepo+"/releases/latest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.TagName == "" {
		return nil, errors.New("GitHub returned a release without a version")
	}
	return &rel, nil
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

func download(url string, w io.Writer) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "warren/"+version)
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, io.LimitReader(resp.Body, 200<<20))
	return err
}

// expectedSum reads the checksum for name from a sha256sum-style file.
func expectedSum(url, name string) (string, error) {
	var b strings.Builder
	if err := download(url, &b); err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(b.String()))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS has no entry for %s", name)
}

// versionLess reports whether a < b for dotted versions like "0.2.0".
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// replaceExecutable puts newPath in place of exe. A running program can't
// be overwritten on Windows, but it can be renamed, so the old one is moved
// aside first and deleted the next time warren starts.
func replaceExecutable(exe, newPath string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(newPath, exe)
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return err
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(old, exe)
		return err
	}
	return nil
}

// removeOldExecutable cleans up after an update on Windows.
func removeOldExecutable() {
	if runtime.GOOS != "windows" {
		return
	}
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}

// afterUpdate restarts the systemd service if it runs this binary, and
// points out background tunnels that are still on the old version.
func afterUpdate(exe string) {
	if runtime.GOOS == "linux" && os.Geteuid() == 0 &&
		exec.Command("systemctl", "is-active", "--quiet", "warren").Run() == nil {
		unit, _ := exec.Command("systemctl", "show", "-p", "ExecStart", "--value", "warren").Output()
		if strings.Contains(string(unit), exe) {
			if err := exec.Command("systemctl", "restart", "warren").Run(); err != nil {
				fmt.Printf("Could not restart the warren service (%v). Run: systemctl restart warren\n", err)
			} else {
				fmt.Println("Restarted the warren service. Clients reconnect by themselves.")
			}
		}
	}

	procs, _, err := loadBgProcs()
	if err == nil && len(procs) > 0 {
		fmt.Printf("\n%d warren process(es) started with -b still run the old version until restarted:\n", len(procs))
		for _, p := range procs {
			fmt.Printf("  %d  warren %s\n", p.PID, p.shownArgs())
		}
		fmt.Println("Restart them with \"warren stop PID\" and start them again with -b.")
	}
}

// Package update: self-update via CI-built GitHub Releases (GO-PORT §3).
// Hourly: compare the latest release tag to the embedded version; on
// difference, download this platform's asset + SHA256SUMS, verify,
// atomically replace the running binary, and signal exit — the
// supervisor restarts on the new code (launchd KeepAlive / systemd
// Restart=always / run-forever.sh). ANY failure logs and skips the
// cycle: an unreachable GitHub never stops collection. A locally built
// binary (version "dev") NEVER auto-updates — the compiled equivalent
// of the dirty-checkout rule.
package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const repo = "jthingelstad/elixir-mcp-collector"

func platformAsset() string {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	return fmt.Sprintf("collector_%s_%s", runtime.GOOS, arch)
}

type release struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

var client = &http.Client{Timeout: 60 * time.Second}

func get(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "elixir-mcp-collector-updater")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("HTTP %d from %s", res.StatusCode, url)
	}
	return res, nil
}

func fetchRelease(pin string) (*release, error) {
	url := "https://api.github.com/repos/" + repo + "/releases/latest"
	if pin != "" {
		url = "https://api.github.com/repos/" + repo + "/releases/tags/" + pin
	}
	res, err := get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var r release
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Check returns true when the binary was replaced and the process
// should exit so the supervisor restarts it. version is the embedded
// build version; pin (COLLECTOR_PIN_VERSION) freezes a machine to one
// tag. Every failure path returns false with a logged reason.
func Check(version, pin string, log func(level, msg string)) bool {
	if version == "dev" || version == "" {
		return false
	}
	if pin != "" && pin == version {
		return false
	}
	rel, err := fetchRelease(pin)
	if err != nil {
		log("warn", "update check skipped: "+err.Error())
		return false
	}
	if rel.TagName == "" || rel.TagName == version {
		return false
	}
	asset := platformAsset()
	var binURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case asset:
			binURL = a.BrowserDownloadURL
		case "SHA256SUMS":
			sumsURL = a.BrowserDownloadURL
		}
	}
	if binURL == "" || sumsURL == "" {
		log("warn", fmt.Sprintf("update skipped: release %s missing %s or SHA256SUMS", rel.TagName, asset))
		return false
	}

	sumsRes, err := get(sumsURL)
	if err != nil {
		log("warn", "update skipped: "+err.Error())
		return false
	}
	sums, err := io.ReadAll(sumsRes.Body)
	sumsRes.Body.Close()
	if err != nil {
		log("warn", "update skipped: "+err.Error())
		return false
	}
	var want string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.HasSuffix(fields[1], asset) {
			want = fields[0]
		}
	}
	if want == "" {
		log("warn", "update skipped: no checksum for "+asset)
		return false
	}

	binRes, err := get(binURL)
	if err != nil {
		log("warn", "update skipped: "+err.Error())
		return false
	}
	data, err := io.ReadAll(binRes.Body)
	binRes.Body.Close()
	if err != nil {
		log("warn", "update skipped: "+err.Error())
		return false
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		log("error", "update REFUSED: checksum mismatch for "+rel.TagName)
		return false
	}

	self, err := os.Executable()
	if err != nil {
		log("warn", "update skipped: "+err.Error())
		return false
	}
	tmp := filepath.Join(filepath.Dir(self), ".collector.next")
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		log("warn", "update skipped: "+err.Error())
		return false
	}
	if err := os.Rename(tmp, self); err != nil {
		os.Remove(tmp)
		log("warn", "update skipped: "+err.Error())
		return false
	}
	log("info", fmt.Sprintf("updated %s -> %s; exiting for restart", version, rel.TagName))
	return true
}

package tuna

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const releaseRepo = "darylcecile/tuna"

func update(ctx context.Context, p Paths, from, current string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var binary []byte
	version := current
	if from != "" {
		var err error
		binary, err = os.ReadFile(from)
		if err != nil {
			return "", err
		}
	} else {
		b, err := download(ctx, "https://api.github.com/repos/"+releaseRepo+"/releases/latest", 1024*1024)
		if err != nil {
			return "", err
		}
		var release struct {
			Tag    string `json:"tag_name"`
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"browser_download_url"`
			} `json:"assets"`
		}
		if err := json.Unmarshal(b, &release); err != nil {
			return "", err
		}
		version = release.Tag
		if strings.TrimPrefix(current, "v") == strings.TrimPrefix(version, "v") {
			return version, nil
		}
		name := "tuna_" + runtime.GOOS + "_" + runtime.GOARCH
		urls := map[string]string{}
		for _, a := range release.Assets {
			urls[a.Name] = a.URL
		}
		if urls[name] == "" || urls["checksums.txt"] == "" {
			return "", fmt.Errorf("release %s has no binary/checksum for %s", version, name)
		}
		binary, err = download(ctx, urls[name], 100*1024*1024)
		if err != nil {
			return "", err
		}
		sums, err := download(ctx, urls["checksums.txt"], 1024*1024)
		if err != nil {
			return "", err
		}
		if err := verifyChecksum(binary, string(sums), name); err != nil {
			return "", err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".tuna-update-*")
	if err != nil {
		return "", fmt.Errorf("cannot update %s: %w", exe, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0755); err != nil {
		tmp.Close()
		return "", err
	}
	_, err = tmp.Write(binary)
	closeErr := tmp.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	cmd := exec.CommandContext(ctx, tmp.Name(), "--version")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("replacement binary cannot run: %w", err)
	}
	if !strings.HasPrefix(string(out), "tuna version ") {
		return "", fmt.Errorf("replacement is not a tuna binary")
	}
	version = strings.TrimSpace(strings.TrimPrefix(string(out), "tuna version "))
	wasRunning := running(p)
	if wasRunning {
		if err := stop(p); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp.Name(), exe); err != nil {
		if wasRunning {
			_ = start(p)
		}
		return "", err
	}
	// Reinstall embedded integrations using the new executable, not this old process.
	refresh := exec.CommandContext(ctx, exe, "--home", p.Dir, "setup")
	if out, err := refresh.CombinedOutput(); err != nil {
		return version, fmt.Errorf("binary updated, but setup failed: %w: %s", err, out)
	}
	if wasRunning {
		if err := start(p); err != nil {
			return version, err
		}
	}
	return version, nil
}

func download(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tuna")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %s; for unpublished builds use 'tuna update --from /path/to/tuna'", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download exceeds size limit")
	}
	return b, nil
}

func verifyChecksum(binary []byte, sums, name string) error {
	sum := sha256.Sum256(binary)
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name && fields[0] == hex.EncodeToString(sum[:]) {
			return nil
		}
	}
	return fmt.Errorf("checksum verification failed for %s", name)
}

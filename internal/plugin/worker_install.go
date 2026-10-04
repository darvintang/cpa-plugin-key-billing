// Created by Darvin.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
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

// CPA's store installs only the library. Fetch its version-matched companion
// separately and verify the release checksum before making it executable.
func ensureTaskWorker(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("task worker is not a regular file")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	asset := fmt.Sprintf("cpa-key-billing-plus-worker_%s_%s_%s", Version, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		asset += ".exe"
	}
	base := GitHubRepository + "/releases/download/v" + Version + "/"
	return installTaskWorker(path, asset, base, &http.Client{Timeout: 15 * time.Second}, signTaskWorker)
}

func installTaskWorker(path, asset, base string, client *http.Client, sign func(string) error) error {
	checksums, err := downloadWorkerAsset(client, base+"checksums.txt", 1<<20)
	if err != nil {
		return fmt.Errorf("download task worker checksums: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			want = strings.ToLower(fields[0])
			break
		}
	}
	decoded, err := hex.DecodeString(want)
	if err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("task worker checksum is missing or invalid for %s", asset)
	}
	data, err := downloadWorkerAsset(client, base+asset, 32<<20)
	if err != nil {
		return fmt.Errorf("download task worker: %w", err)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("task worker checksum mismatch")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".task-worker-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Chmod(0700)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := sign(file.Name()); err != nil {
		return err
	}
	// A fresh inode avoids stale kernel signature state from in-place updates.
	return os.Rename(file.Name(), path)
}

func downloadWorkerAsset(client *http.Client, endpoint string, limit int64) ([]byte, error) {
	response, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("task worker asset exceeds size limit")
	}
	return data, nil
}

func signTaskWorker(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if err := exec.Command("/usr/bin/codesign", "--force", "--sign", "-", path).Run(); err != nil {
		return fmt.Errorf("sign task worker: %w", err)
	}
	return exec.Command("/usr/bin/codesign", "--verify", "--strict", path).Run()
}

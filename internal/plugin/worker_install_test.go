// Created by Darvin.
package plugin

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallTaskWorkerVerifiesAndInstallsAtomically(t *testing.T) {
	asset := "cpa-key-billing-plus-worker_test_darwin_arm64"
	data := []byte("worker-binary")
	sum := sha256.Sum256(data)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			fmt.Fprintf(w, "%x  %s\n", sum, asset)
		case "/" + asset:
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "plugins", "worker")
	signed := false
	if err := installTaskWorker(path, asset, server.URL+"/", server.Client(), func(name string) error {
		signed = true
		if name == path {
			t.Fatal("worker must be signed before atomic installation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(path)
	if err != nil || string(installed) != string(data) || !signed {
		t.Fatalf("installed=%q signed=%t err=%v", installed, signed, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0700 {
		t.Fatalf("mode=%v", info.Mode())
	}
}

func TestInstallTaskWorkerRejectsChecksumMismatch(t *testing.T) {
	asset := "worker_test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/checksums.txt" {
			fmt.Fprintf(w, "%064x  %s\n", 0, asset)
			return
		}
		_, _ = w.Write([]byte("tampered"))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "plugins", "worker")
	if err := installTaskWorker(path, asset, server.URL+"/", server.Client(), func(string) error { return nil }); err == nil {
		t.Fatal("checksum mismatch was accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("mismatched worker was installed")
	}
}

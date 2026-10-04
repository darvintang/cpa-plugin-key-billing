// Created by Darvin.
package tasksettings

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Only ciphertext is persisted; modification and the wrong local key must fail authentication.
func TestEncryptedSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	want := Session{Origin: "http://127.0.0.1:8317", Key: "dummy-management-secret"}
	if err := SaveSession(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSession(path)
	if err != nil || got != want {
		t.Fatalf("round trip failed: %v", err)
	}
	directory := path + ".task-session"
	cipherPath := filepath.Join(directory, "session.enc")
	raw, err := os.ReadFile(cipherPath)
	if err != nil || bytes.Contains(raw, []byte(want.Key)) {
		t.Fatal("session contains plaintext or is unreadable")
	}
	for name, mode := range map[string]os.FileMode{directory: 0700, cipherPath: 0600, filepath.Join(directory, "master.key"): 0600} {
		info, err := os.Stat(name)
		if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != mode {
			t.Fatalf("incorrect session permissions: %v", err)
		}
	}
	original := append([]byte(nil), raw...)
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(cipherPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("tampered session accepted")
	}
	if err := os.WriteFile(cipherPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "master.key"), bytes.Repeat([]byte{1}, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err == nil {
		t.Fatal("session accepted with a different key")
	}
}

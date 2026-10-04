// Created by Darvin.
package tasksettings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Session contains only the loopback origin and management credential needed by the external worker.
type Session struct {
	Origin string `json:"origin"`
	Key    string `json:"key"`
}

const sessionAAD = "cpa-key-billing-plus:task-session:v1"

func sessionCipher(path string, create bool) (cipher.AEAD, error) {
	directory := path + ".task-session"
	if create {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
		if err := os.Chmod(directory, 0700); err != nil {
			return nil, err
		}
	}
	keyPath := filepath.Join(directory, "master.key")
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) && create {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		// Exclusive creation prevents replacing the key for an existing encrypted session.
		var file *os.File
		file, err = os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, err = file.Write(key)
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid task encryption key")
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func SaveSession(path string, session Session) error {
	aead, err := sessionCipher(path, true)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(session)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	// Random nonces and authenticated encryption reject edited or truncated session files.
	encrypted := aead.Seal(nonce, nonce, plain, []byte(sessionAAD))
	directory := path + ".task-session"
	file, err := os.CreateTemp(directory, ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(encrypted)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), filepath.Join(directory, "session.enc"))
	}
	return err
}

func LoadSession(path string) (Session, error) {
	var session Session
	raw, err := os.ReadFile(filepath.Join(path+".task-session", "session.enc"))
	if err != nil {
		return session, err
	}
	aead, err := sessionCipher(path, false)
	if err != nil {
		return session, err
	}
	if len(raw) < aead.NonceSize() {
		return session, fmt.Errorf("invalid encrypted task session")
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(sessionAAD))
	if err != nil {
		return session, fmt.Errorf("task session authentication failed")
	}
	if err := json.Unmarshal(plain, &session); err != nil {
		return Session{}, fmt.Errorf("invalid task session")
	}
	return session, nil
}

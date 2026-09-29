package jfin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Account is one logged-in server+user pair.
type Account struct {
	Server      string `json:"server"`
	Username    string `json:"username"`
	User        string `json:"user"` // display name
	AccessToken string `json:"access_token"`
	UserID      string `json:"user_id"`
	DeviceID    string `json:"device_id"`
}

// CredFile is the on-disk credential store (cred.json, mode 0600).
type CredFile struct {
	Active   int       `json:"active"`
	Accounts []Account `json:"accounts"`
}

func CredPath(configDir string) string { return filepath.Join(configDir, "cred.json") }

// Load reads the store; a missing file is not an error (first run).
func (cf *CredFile) Load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(b, cf); err != nil {
		return errors.New("jfin: cred.json corrupted: " + err.Error())
	}
	return nil
}

func (cf *CredFile) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Upsert adds or replaces the account for (server, username).
func (cf *CredFile) Upsert(acc Account) {
	for i := range cf.Accounts {
		if cf.Accounts[i].Server == acc.Server && cf.Accounts[i].Username == acc.Username {
			cf.Accounts[i] = acc
			return
		}
	}
	cf.Accounts = append(cf.Accounts, acc)
	if cf.Active >= len(cf.Accounts) {
		cf.Active = len(cf.Accounts) - 1
	}
}

// Remove deletes the account; returns false if not present.
func (cf *CredFile) Remove(server, username string) bool {
	for i := range cf.Accounts {
		if cf.Accounts[i].Server == server && cf.Accounts[i].Username == username {
			cf.Accounts = append(cf.Accounts[:i], cf.Accounts[i+1:]...)
			if cf.Active >= len(cf.Accounts) {
				cf.Active = len(cf.Accounts) - 1
			}
			if cf.Active < 0 {
				cf.Active = 0
			}
			return true
		}
	}
	return false
}

func (cf *CredFile) Get(server, username string) (Account, bool) {
	for _, a := range cf.Accounts {
		if a.Server == server && a.Username == username {
			return a, true
		}
	}
	return Account{}, false
}

// ActiveAccount is the currently selected account.
func (cf *CredFile) ActiveAccount() (Account, bool) {
	if len(cf.Accounts) == 0 {
		return Account{}, false
	}
	if cf.Active < 0 || cf.Active >= len(cf.Accounts) {
		cf.Active = 0
	}
	return cf.Accounts[cf.Active], true
}

package tw

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// authDoc 是磁盘凭据的 auth 段。
type authDoc struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	ApiHost      string `json:"apiHost"`
	MachineID    string `json:"machineId"`
	DeviceID     string `json:"deviceId"`
}

// accountDoc 是磁盘凭据的 account 段。
type accountDoc struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// authFileDoc 是固定嵌套落盘形。
type authFileDoc struct {
	Auth    authDoc    `json:"auth"`
	Account accountDoc `json:"account"`
}

// AuthFileID 返回凭据文件名（去 .json），也是 core 的 account_uid。
func AuthFileID(uid string) string {
	return "trae-" + strings.TrimSpace(uid)
}

// authsDir 返回 <data>/auths。
func authsDir(dataDir string) string { return filepath.Join(dataDir, "auths") }

// AuthPathFor 返回 uid 对应的凭据绝对路径。
func AuthPathFor(dataDir, uid string) string {
	return filepath.Join(authsDir(dataDir), AuthFileID(uid)+".json")
}

// AuthFileNameOf 从凭据路径取出 auth file id（去目录与 .json）。
func AuthFileNameOf(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".json")
}

// SaveAuth 原子落盘凭据（嵌套形，0600 文件 / 0700 目录），返回 auth file id。
func SaveAuth(dataDir string, a *Auth) (string, error) {
	if a == nil {
		return "", fmt.Errorf("traework: nil auth")
	}
	uid := a.UID
	if strings.TrimSpace(uid) == "" && a.FilePath != "" {
		uid = strings.TrimSuffix(filepath.Base(a.FilePath), ".json")
		uid = strings.TrimPrefix(uid, "trae-")
	}
	if strings.TrimSpace(uid) == "" {
		return "", fmt.Errorf("traework: cannot save auth without uid")
	}
	dir := authsDir(dataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)

	doc := authFileDoc{
		Auth: authDoc{
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
			ExpiresAt:    a.ExpiresAt,
			Domain:       a.Domain,
			ApiHost:      a.ApiHost,
			MachineID:    a.MachineID,
			DeviceID:     a.DeviceID,
		},
		Account: accountDoc{
			UID:          uid,
			EnterpriseID: a.EnterpriseID,
			Nickname:     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, AuthFileID(uid)+".json")
	if err := atomicWrite(path, raw, 0o600); err != nil {
		return "", err
	}
	a.UID = uid
	a.FilePath = path
	a.Kind = "traework"
	return AuthFileID(uid), nil
}

// atomicWrite 写临时文件后 rename，保证原子性。
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// LoadAuth 按 auth file id（trae-<uid> 或裸 uid）读取凭据。
func LoadAuth(dataDir, name string) (*Auth, error) {
	name = strings.TrimSpace(strings.TrimSuffix(filepath.Base(name), ".json"))
	if name == "" {
		return nil, fmt.Errorf("traework: empty auth name")
	}
	path := filepath.Join(authsDir(dataDir), name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !strings.HasPrefix(name, "trae-") {
			path = filepath.Join(authsDir(dataDir), "trae-"+name+".json")
			raw, err = os.ReadFile(path)
		}
		if err != nil {
			return nil, err
		}
	}
	a, err := parseAuthFile(raw)
	if err != nil {
		return nil, err
	}
	a.FilePath = path
	if a.UID == "" {
		a.UID = strings.TrimPrefix(name, "trae-")
	}
	return a, nil
}

// parseAuthFile 解析嵌套形或扁平形凭据。
func parseAuthFile(raw []byte) (*Auth, error) {
	var doc authFileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("traework: decode auth file: %w", err)
	}
	a := &Auth{
		Kind:         "traework",
		AccessToken:  doc.Auth.AccessToken,
		RefreshToken: doc.Auth.RefreshToken,
		ExpiresAt:    normalizeExpiresAt(doc.Auth.ExpiresAt),
		Domain:       doc.Auth.Domain,
		ApiHost:      doc.Auth.ApiHost,
		MachineID:    doc.Auth.MachineID,
		DeviceID:     doc.Auth.DeviceID,
		UID:          doc.Account.UID,
		EnterpriseID: doc.Account.EnterpriseID,
		Nickname:     doc.Account.Nickname,
	}
	// 兼容扁平形：token/字段直接挂在顶层。
	if a.AccessToken == "" && a.RefreshToken == "" {
		var flat map[string]json.RawMessage
		if json.Unmarshal(raw, &flat) == nil {
			a.AccessToken = firstNonEmptyStr(a.AccessToken, strField(flat, "accessToken", "access_token", "AccessToken"))
			a.RefreshToken = firstNonEmptyStr(a.RefreshToken, strField(flat, "refreshToken", "refresh_token", "RefreshToken"))
			a.Domain = firstNonEmptyStr(a.Domain, strField(flat, "domain", "Domain"))
			a.ApiHost = firstNonEmptyStr(a.ApiHost, strField(flat, "apiHost", "ApiHost"))
			a.MachineID = firstNonEmptyStr(a.MachineID, strField(flat, "machineId", "MachineID"))
			a.DeviceID = firstNonEmptyStr(a.DeviceID, strField(flat, "deviceId", "DeviceID"))
			a.UID = firstNonEmptyStr(a.UID, strField(flat, "uid", "UID"))
			a.Nickname = firstNonEmptyStr(a.Nickname, strField(flat, "nickname", "Nickname"))
			a.EnterpriseID = firstNonEmptyStr(a.EnterpriseID, strField(flat, "enterpriseId", "EnterpriseID"))
			if a.ExpiresAt == 0 {
				a.ExpiresAt = normalizeExpiresAt(intField(flat, "expiresAt", "expires_at", "ExpiresAt"))
			}
		}
	}
	if a.Domain == "" {
		a.Domain = "trae.cn"
	}
	if a.AccessToken == "" && a.RefreshToken == "" {
		return nil, fmt.Errorf("traework: auth file has no token")
	}
	return a, nil
}

// strField 从原始 JSON map 中取首个非空字符串字段。
func strField(m map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return ""
}

// intField 从原始 JSON map 中取首个非零整数（或字符串数字）字段。
func intField(m map[string]json.RawMessage, keys ...string) int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		var n int64
		if json.Unmarshal(v, &n) == nil && n != 0 {
			return n
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			if f, ok := parseRate(s); ok && f != 0 {
				return int64(f)
			}
		}
	}
	return 0
}

// ListAuths 列出 <data>/auths 下所有凭据（按文件名排序）。
func ListAuths(dataDir string) ([]*Auth, error) {
	entries, err := os.ReadDir(authsDir(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		a, err := LoadAuth(dataDir, strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

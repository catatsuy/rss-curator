package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type State struct {
	Processed   map[string]time.Time `json:"processed"`
	Initialized map[string]bool      `json:"initialized"`
	// 発見済み識別子と記事識別子の対応。URLや採否は保存しない。
	Aliases             map[string]string     `json:"aliases"`
	Fetch               map[string]FetchState `json:"fetch,omitempty"`
	HostRetryAfter      map[string]time.Time  `json:"host_retry_after,omitempty"`
	CrawlbaseRetryAfter time.Time             `json:"crawlbase_retry_after,omitzero"`
}

func hash(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func loadState(path string) (*State, error) {
	s := &State{Processed: map[string]time.Time{}, Initialized: map[string]bool{}, Aliases: map[string]string{}, Fetch: map[string]FetchState{}, HostRetryAfter: map[string]time.Time{}}
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(b, s); e != nil {
		return nil, fmt.Errorf("状態ファイルが不正です: %w", e)
	}
	if s.Processed == nil || s.Initialized == nil || s.Aliases == nil {
		return nil, fmt.Errorf("状態の必須項目がありません")
	}
	// 旧形式で取得失敗中だった記事は、それまでの試行回数を引き継ぐ。
	for id, f := range s.Fetch {
		if f.Failures == 0 && f.LastFailure != "" {
			f.Failures = f.Attempts
			s.Fetch[id] = f
		}
	}
	return s, nil
}
func writeJSON(path string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(path, append(b, '\n'))
}
func atomicWrite(path string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if e != nil {
		return e
	}
	n := f.Name()
	defer os.Remove(n)
	if e = f.Chmod(0600); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return os.Rename(n, path)
}
func validID(id string) bool { b, e := hex.DecodeString(id); return e == nil && len(b) == 32 }
func ack(path, id string) error {
	if !validID(id) {
		return fmt.Errorf("記事IDが不正です")
	}
	s, e := loadState(path)
	if e != nil {
		return e
	}
	s.Processed[id] = time.Now().UTC()
	return writeJSON(path, s)
}

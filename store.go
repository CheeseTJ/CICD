package main

import (
	"encoding/json"
	"os"
	"sync"
)

// RepoState 保存某个 GitHub 仓库在本工具中的状态。
type RepoState struct {
	Marked     bool   `json:"marked"`     // 手动标记是否为 app（覆盖自动检测）
	MarkedSet  bool   `json:"marked_set"` // 是否存在手动覆盖
	Identifier string `json:"identifier"` // 蒲公英 buildIdentifier（Android 包名）
	AppKey     string `json:"app_key"`    // 蒲公英 appKey（同一 app 各版本相同）
	BuildKey   string `json:"build_key"`  // 最近一次同步得到的 buildKey

	LastSyncedVersion string `json:"last_synced_version"`
	LastSyncedTag     string `json:"last_synced_tag"`
	LastSyncedAt      string `json:"last_synced_at"`
	LastSyncedAtUnix  int64  `json:"last_synced_at_unix"`

	AutoSync         bool   `json:"auto_sync"`
	PgyerShortcutUrl string `json:"pgyer_shortcut_url"` // 蒲公英下载页地址
}

// Store 负责把仓库状态持久化到本地 JSON 文件。
type Store struct {
	mu    sync.RWMutex
	path  string
	Repos map[string]*RepoState `json:"repos"`
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, Repos: map[string]*RepoState{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.Repos); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Get(fullName string) *RepoState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Repos[fullName]
}

// Update 对指定仓库的状态进行修改，若不存在则创建。
func (s *Store) Update(fullName string, fn func(*RepoState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.Repos[fullName]
	if st == nil {
		st = &RepoState{}
		s.Repos[fullName] = st
	}
	fn(st)
}

func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := json.MarshalIndent(s.Repos, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o644)
}

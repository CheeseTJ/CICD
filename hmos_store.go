// 鸿蒙应用清单：记录「哪些 App 用哪套证书签、签出来传蒲公英」。
//
// 与 RepoState 的关系
// -------------------
// RepoState 描述的是「GitHub 仓库 → 蒲公英 APK」这条链路（Android）。
// HmosApp 描述的是「本地 HAP → 重签 → 蒲公英 HAP」这条链路（鸿蒙），
// 两者没有交集：鸿蒙包不进 GitHub Release，也不需要 GitHub token。
//
// 为什么把证书信息存在 App 上而不是全局
// ------------------------------------
// 不同 App 可以用不同证书（不同开发者账号、或想用不同 bundleName），
// 也可能共用同一套（个人项目常见）。所以证书字段留在 App 里，但允许多个 App
// 指向同一组文件路径 —— 不额外做「证书库」抽象，避免多一层间接。
package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// HmosApp 一个鸿蒙应用。
type HmosApp struct {
	// ID 稳定标识，用 bundleName（鸿蒙的包名全局唯一，天然不重复）
	ID string `json:"id"`
	// Name 展示名（可随意改，不影响签名）
	Name string `json:"name"`
	// BundleName 包名，必须与 HAP 里的 module.json 一致
	BundleName string `json:"bundle_name"`

	// 签名材料
	CertFile    string `json:"cert_file"`    // .cer / .pem，证书链
	ProfileFile string `json:"profile_file"` // .p7b
	Keystore    string `json:"keystore"`     // .p12 / .jks
	KeyAlias    string `json:"key_alias"`

	// HapFile 当前用来重签的源包（上一次签好的、或你手动放的未签名包）。
	// 重签是幂等的：用新证书再签一次同一份包即可，不会越签越大。
	HapFile string `json:"hap_file"`

	// 蒲公英
	PgyerShortcutUrl string `json:"pgyer_shortcut_url"` // 下载页
	PgyerBuildKey    string `json:"pgyer_build_key"`
	LastUploadAt     int64  `json:"last_upload_at"`
	LastUploadVer    string `json:"last_upload_version"`

	// 自动重签：证书到期前 N 天自动用现有 HapFile 重签并上传
	AutoResign bool `json:"auto_resign"`
	// LastResignAt 上次成功重签的时间，用于避免同一天重复触发
	LastResignAt int64 `json:"last_resign_at"`
	// LastError 最近一次失败原因，前端直接展示
	LastError string `json:"last_error"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// HmosStore 鸿蒙应用清单的持久化。
//
// 单独一个文件（默认 data/hmos.json）而不是塞进 RepoState 的 data.json：
// 两者生命周期不同，混在一起会让 data.json 的迁移变复杂；而且鸿蒙清单里
// 不含任何密钥内容（只有路径），单独存更便于备份与排查。
type HmosStore struct {
	mu   sync.RWMutex
	path string
	Apps map[string]*HmosApp `json:"apps"` // key = HmosApp.ID
}

// NewHmosStore 加载清单；文件不存在时返回空清单（首次运行）。
func NewHmosStore(path string) (*HmosStore, error) {
	s := &HmosStore{path: path, Apps: map[string]*HmosApp{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 首次运行：目录可能还不存在，建出来，免得后续 Save 失败。
			if dir := filepath.Dir(path); dir != "" && dir != "." {
				_ = os.MkdirAll(dir, 0o755)
			}
			return s, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	var wrapper struct {
		Apps map[string]*HmosApp `json:"apps"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return nil, err
	}
	if wrapper.Apps != nil {
		s.Apps = wrapper.Apps
	}
	return s, nil
}

// List 返回按名称排序的副本，避免调用方直接改内部 map。
func (s *HmosStore) List() []*HmosApp {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*HmosApp, 0, len(s.Apps))
	for _, a := range s.Apps {
		cp := *a
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Get 取单个应用副本。
func (s *HmosStore) Get(id string) (*HmosApp, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.Apps[id]
	if !ok {
		return nil, false
	}
	cp := *a
	return &cp, true
}

// Upsert 新增或更新。返回错误说明字段不合法。
func (s *HmosStore) Upsert(a *HmosApp) error {
	if a == nil {
		return errors.New("空的应用配置")
	}
	if a.BundleName == "" {
		return errors.New("bundle_name 不能为空")
	}
	if a.ID == "" {
		a.ID = a.BundleName
	}
	if a.Name == "" {
		a.Name = a.BundleName
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().Unix()
	if old, ok := s.Apps[a.ID]; ok {
		a.CreatedAt = old.CreatedAt
	} else if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	s.Apps[a.ID] = a
	return nil
}

// Patch 局部更新，只覆盖非 nil 字段。
//
// 单独提供 Patch 而不是让调用方先 Get 再 Upsert：并发场景下后者会丢掉别人的
// 修改（比如后台重签任务写 LastResignAt 时覆盖掉用户刚改的名字）。
func (s *HmosStore) Patch(id string, fn func(*HmosApp)) (*HmosApp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.Apps[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	fn(a)
	a.UpdatedAt = time.Now().Unix()
	cp := *a
	return &cp, nil
}

// Delete 移除应用（不影响磁盘上的证书与包文件）。
func (s *HmosStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Apps[id]; !ok {
		return os.ErrNotExist
	}
	delete(s.Apps, id)
	return nil
}

// Save 落盘。写临时文件再 rename，避免写到一半进程退出留下半截 JSON。
func (s *HmosStore) Save() error {
	s.mu.RLock()
	payload, err := json.MarshalIndent(map[string]interface{}{"apps": s.Apps}, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

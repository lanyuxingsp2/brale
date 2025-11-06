package prompt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Manager 负责从目录中加载 .txt 提示词模板
type Manager struct {
	dir   string
	cache map[string]string // name -> content
}

func NewManager(dir string) *Manager { return &Manager{dir: dir, cache: make(map[string]string)} }

// Load 扫描目录并读取所有 .txt 文件
func (m *Manager) Load() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return fmt.Errorf("读取提示词目录失败: %w", err)
	}
	m.cache = make(map[string]string)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".txt") {
			continue
		}
		path := filepath.Join(m.dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("读取模板失败 %s: %w", path, err)
		}
		name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		m.cache[name] = string(b)
	}
	return nil
}

// Reload 重新加载
func (m *Manager) Reload() error { return m.Load() }

// Get 获取模板内容
func (m *Manager) Get(name string) (string, bool) { v, ok := m.cache[name]; return v, ok }

// MustWriteSample 写入一个 default.txt（用于测试/示例）
func MustWriteSample(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	sample := "你是专业的加密货币交易AI。请根据市场数据与风险控制做出决策。\n"
	return os.WriteFile(filepath.Join(dir, "default.txt"), []byte(sample), 0644)
}

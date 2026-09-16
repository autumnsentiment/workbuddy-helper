package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProxyConfFileName 代理配置文件名（明文，仅含代理 URL，不含凭据以外敏感数据）。
// 独立于加密 state.bin：容器重建/数据迁移后仍可恢复代理配置。
const ProxyConfFileName = "proxy.conf"

// LoadProxyConf 从数据目录读取代理 URL；文件不存在返回 ("", nil)。
// 内容格式：第一行非空文本即代理 URL（如 socks5://192.168.5.1:1070）；
// 空文件或 "direct" 表示直连。
func LoadProxyConf(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ProxyConfFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read proxy config: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	line = strings.TrimRight(line, "\r")
	if line == "" || strings.EqualFold(line, "direct") {
		return "", nil
	}
	return line, nil
}

// SaveProxyConf 将代理 URL 写入数据目录（原子写）；proxyURL 为空时写 "direct"。
func SaveProxyConf(dir, proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		proxyURL = "direct"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dir, ProxyConfFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(proxyURL+"\n"), 0o600); err != nil {
		return fmt.Errorf("write proxy config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace proxy config: %w", err)
	}
	return nil
}

// LoadProxyConf 是 Store 的便捷包装。
func (s *Store) LoadProxyConf() (string, error) { return LoadProxyConf(s.dir) }

// SaveProxyConf 是 Store 的便捷包装。
func (s *Store) SaveProxyConf(proxyURL string) error { return SaveProxyConf(s.dir, proxyURL) }

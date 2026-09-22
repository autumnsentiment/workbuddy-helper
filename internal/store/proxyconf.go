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
	value, _, err := LoadProxyConfWithPresence(dir)
	return value, err
}

// LoadProxyConfWithPresence 与 LoadProxyConf 相同，但额外返回配置文件是否存在。
// 这样可以区分“文件不存在，需要兼容 state.bin”与“明确写入 direct，应覆盖旧代理”。
func LoadProxyConfWithPresence(dir string) (value string, found bool, err error) {
	raw, err := readConfigFile(filepath.Join(dir, ProxyConfFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read proxy config: %w", err)
	}
	for _, rawLine := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(strings.TrimRight(rawLine, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.EqualFold(line, "direct") {
			return "", true, nil
		}
		return line, true, nil
	}
	return "", true, nil
}

// SaveProxyConf 将代理 URL 写入数据目录（原子写）；proxyURL 为空时写 "direct"。
func SaveProxyConf(dir, proxyURL string) error {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		proxyURL = "direct"
	}
	path := filepath.Join(dir, ProxyConfFileName)
	return writeConfigFile(path, []byte(proxyURL+"\n"))
}

// LoadProxyConf 是 Store 的便捷包装。
func (s *Store) LoadProxyConf() (string, error) { return LoadProxyConf(s.dir) }

// LoadProxyConfWithPresence 是 Store 的便捷包装。
func (s *Store) LoadProxyConfWithPresence() (string, bool, error) {
	return LoadProxyConfWithPresence(s.dir)
}

// SaveProxyConf 是 Store 的便捷包装。
func (s *Store) SaveProxyConf(proxyURL string) error { return SaveProxyConf(s.dir, proxyURL) }

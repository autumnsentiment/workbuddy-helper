package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DNSConfFileName 固定 DNS 配置文件（明文，每行一个 server:port）。
// 用于国内版链路（以及解析兜底）：容器内 Docker DNS 故障时不依赖系统解析。
// 文件不存在时使用代码内默认公共 DNS；"system" 单行表示回退系统解析。
const DNSConfFileName = "dns.conf"

// DefaultPublicDNS 代码内默认公共 DNS
var DefaultPublicDNS = []string{"223.5.5.5:53", "119.29.29.29:53", "1.1.1.1:53"}

// LoadDNSConf 读取固定 DNS 列表；文件不存在返回 (nil, nil) 表示用默认。
func LoadDNSConf(dir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, DNSConfFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read dns config: %w", err)
	}
	var servers []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.EqualFold(line, "system") {
			return nil, nil
		}
		if !strings.Contains(line, ":") {
			line += ":53"
		}
		servers = append(servers, line)
	}
	return servers, nil
}

// SaveDNSConf 写入固定 DNS 列表（每行一个）；空列表写 "system"（回退系统解析）。
func SaveDNSConf(dir string, servers []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	body := "# WorkBuddy Helper 固定 DNS（每行一个 server[:port]，system=回退系统解析）\n"
	if len(servers) == 0 {
		body += "system\n"
	} else {
		for _, srv := range servers {
			body += srv + "\n"
		}
	}
	path := filepath.Join(dir, DNSConfFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write dns config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace dns config: %w", err)
	}
	return nil
}

// LoadDNSConf / SaveDNSConf 是 Store 的便捷包装。
func (s *Store) LoadDNSConf() ([]string, error)     { return LoadDNSConf(s.dir) }
func (s *Store) SaveDNSConf(servers []string) error { return SaveDNSConf(s.dir, servers) }

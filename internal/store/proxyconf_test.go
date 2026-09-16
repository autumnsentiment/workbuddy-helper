package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProxyConfRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// 不存在 -> 空且无错误
	got, err := LoadProxyConf(dir)
	if err != nil || got != "" {
		t.Fatalf("empty dir: got=%q err=%v", got, err)
	}
	// 保存 socks5
	if err := SaveProxyConf(dir, "socks5://192.168.5.1:1070"); err != nil {
		t.Fatal(err)
	}
	got, err = LoadProxyConf(dir)
	if err != nil || got != "socks5://192.168.5.1:1070" {
		t.Fatalf("roundtrip: got=%q err=%v", got, err)
	}
	// 空值 -> direct
	if err := SaveProxyConf(dir, ""); err != nil {
		t.Fatal(err)
	}
	got, err = LoadProxyConf(dir)
	if err != nil || got != "" {
		t.Fatalf("direct: got=%q err=%v", got, err)
	}
	// 文件确实存在且内容为 direct
	raw, _ := os.ReadFile(filepath.Join(dir, ProxyConfFileName))
	if string(raw) != "direct\n" {
		t.Fatalf("file content: %q", raw)
	}
}

func TestProxyConfMalformed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProxyConfFileName), []byte("  socks5h://gw:1080\n# comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProxyConf(dir)
	if err != nil || got != "socks5h://gw:1080" {
		t.Fatalf("malformed parse: got=%q err=%v", got, err)
	}
}

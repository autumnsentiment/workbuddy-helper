package store

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDNSConfDistinguishesMissingAndSystem(t *testing.T) {
	dir := t.TempDir()
	servers, found, err := LoadDNSConfWithPresence(dir)
	if err != nil || found || servers != nil {
		t.Fatalf("missing: servers=%v found=%v err=%v", servers, found, err)
	}

	if err := SaveDNSConf(dir, []string{}); err != nil {
		t.Fatal(err)
	}
	servers, found, err = LoadDNSConfWithPresence(dir)
	if err != nil || !found || servers == nil || len(servers) != 0 {
		t.Fatalf("system: servers=%v found=%v err=%v", servers, found, err)
	}

	if err := os.WriteFile(filepath.Join(dir, DNSConfFileName), []byte("# custom\n223.5.5.5\n1.1.1.1:53\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	servers, found, err = LoadDNSConfWithPresence(dir)
	want := []string{"223.5.5.5:53", "1.1.1.1:53"}
	if err != nil || !found || !reflect.DeepEqual(servers, want) {
		t.Fatalf("custom: servers=%v found=%v err=%v", servers, found, err)
	}
}

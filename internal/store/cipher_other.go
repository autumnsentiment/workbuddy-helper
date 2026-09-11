//go:build !windows

package store

// platformSeal / platformMagic are used for new writes on portable platforms
// (Linux/macOS/freebsd): the key-file AES-GCM cipher.
func platformSeal() func([]byte, string) ([]byte, error) {
	return sealPortable
}

func platformMagic() []byte { return fileMagicPort }

// openDPAPI is unreachable on non-Windows (a WBH1 file cannot be decrypted
// without Windows DPAPI) but exists so decodeState compiles everywhere.
func openDPAPI([]byte) ([]byte, error) {
	return nil, errDPAPIUnavailable
}

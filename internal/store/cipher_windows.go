//go:build windows

package store

// platformSeal / platformMagic are used for new writes on Windows: DPAPI.
func platformSeal() func([]byte, string) ([]byte, error) {
	return func(plain []byte, _ string) ([]byte, error) {
		return protectDPAPI(plain)
	}
}

func platformMagic() []byte { return fileMagicLegacy }

// openDPAPI decrypts a DPAPI blob produced on the current Windows user/machine.
func openDPAPI(sealed []byte) ([]byte, error) {
	return unprotectDPAPI(sealed)
}

//go:build !windows

package store

import "errors"

var errDPAPIUnavailable = errors.New("this data uses Windows DPAPI and can only be read on the originating Windows user; re-add the account or export a portable copy from the Windows helper")

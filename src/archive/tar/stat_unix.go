// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build linux darwin dragonfly freebsd openbsd netbsd solaris

package tar

import (
	"os"
	"syscall"
)

func init() {
	sysStat = statUnix // ��ʼ����sysStat��ֵΪstatUnix
}

func statUnix(fi os.FileInfo, h *Header) error {
	sys, ok := fi.Sys().(*syscall.Stat_t) // ���Stat_t�ṹ
	if !ok {
		return nil
	}
	h.Uid = int(sys.Uid) // ���Uid
	h.Gid = int(sys.Gid) // ���Gid
	// TODO(bradfitz): populate username & group.  os/user
	// doesn't cache LookupId lookups, and lacks group
	// lookup functions.
	h.AccessTime = statAtime(sys) // ��÷���ʱ��
	h.ChangeTime = statCtime(sys)
	// TODO(bradfitz): major/minor device numbers?
	return nil
}

// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// On AMD64, virtual addresses are 48-bit numbers sign extended to 64.
// We shift the address left 16 to eliminate the sign extended part and make
// room in the bottom for the count.
// In addition to the 16 bits taken from the top, we can take 3 from the
// bottom, because node must be pointer-aligned, giving a total of 19 bits
// of count.

func lfstackPack(node *lfnode, cnt uintptr) uint64 { // 将节点信息压缩为一个uint64
	return uint64(uintptr(unsafe.Pointer(node)))<<16 | uint64(cnt&(1<<19-1))
}

func lfstackUnpack(val uint64) (node *lfnode, cnt uintptr) { // 解开一个uint64为节点信息
	node = (*lfnode)(unsafe.Pointer(uintptr(int64(val) >> 19 << 3)))
	cnt = uintptr(val & (1<<19 - 1))
	return
}

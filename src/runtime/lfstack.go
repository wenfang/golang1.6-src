// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// 无锁的栈
// Lock-free stack.
// The following code runs only on g0 stack. 下面的代码只能在g0栈上运行

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

func lfstackpush(head *uint64, node *lfnode) { // 将节点压入栈中
	node.pushcnt++                                     // 增加push的序列号
	new := lfstackPack(node, node.pushcnt)             // 将数据压缩到new中
	if node1, _ := lfstackUnpack(new); node1 != node { // 解压缩后数据不一样了，抛出异常
		print("runtime: lfstackpush invalid packing: node=", node, " cnt=", hex(node.pushcnt), " packed=", hex(new), " -> node=", node1, "\n")
		throw("lfstackpush")
	}
	for { // 通过无锁队列，加入到栈中
		old := atomic.Load64(head) // 获取head中的值
		node.next = old
		if atomic.Cas64(head, old, new) { // 将head加入头部
			break
		}
	}
}

func lfstackpop(head *uint64) unsafe.Pointer { // 从栈中返回节点指针
	for {
		old := atomic.Load64(head) // 从头部中返回一个值
		if old == 0 {
			return nil
		}
		node, _ := lfstackUnpack(old)
		next := atomic.Load64(&node.next)
		if atomic.Cas64(head, old, next) {
			return unsafe.Pointer(node)
		}
	}
}

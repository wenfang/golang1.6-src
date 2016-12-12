// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object allocator.  Returned memory is not zeroed.
//
// See malloc.go for overview.

package runtime

import "unsafe"

// FixAlloc用作固定大小的对象的分配,malloc使用FixAlloc管理MCache和MSpan结构对象
// FixAlloc is a simple free-list allocator for fixed size objects.
// Malloc uses a FixAlloc wrapped around sysAlloc to manages its
// MCache and MSpan objects.
// 由Fixalloc_Alloc返回的内存没有清0，调用者负责锁定
// Memory returned by FixAlloc_Alloc is not zeroed.
// The caller is responsible for locking around FixAlloc calls.
// Callers can keep state in the object but the first word is
// smashed by freeing and reallocating.
type fixalloc struct { // fixalloc结构
	size   uintptr                     //  用来分配多大的对象
	first  func(arg, p unsafe.Pointer) // called first time p is returned 当p返回时第一次调用
	arg    unsafe.Pointer              // first调用参数
	list   *mlink                      // 当前结构的连接列表
	chunk  unsafe.Pointer              // chunk可用空间的地址
	nchunk uint32                      // chunk中可用空间的大小
	inuse  uintptr                     // in-use bytes now 当前由该fixalloc分配的处于使用状态的字节数
	stat   *uint64
}

// 通用的块的连接列表，块的大小一般都比mlink大，由于给mlink.next的赋值会导致写分界，因而
// 其不能被一些内部的GC结构使用
// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being preformed
// this can not be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
type mlink struct { // mlink连接结构
	next *mlink
}

// 初始化一个f，用来分配固定size大小的对象，内部使用分配器获得成块的内存
// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *uint64) {
	f.size = size   // 用来分配多大的对象
	f.first = first // first函数
	f.arg = arg     // first函数的参数
	f.list = nil
	f.chunk = nil
	f.nchunk = 0
	f.inuse = 0
	f.stat = stat
}

func (f *fixalloc) alloc() unsafe.Pointer {
	if f.size == 0 { // 如果该fixalloc可分配的固定size为0，抛出异常
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	if f.list != nil { // 如果列表中有数据，从列表中取出
		v := unsafe.Pointer(f.list)
		f.list = f.list.next // 从列表中取出一个
		f.inuse += f.size    // 当前已经分配的大小增加
		return v             // 返回已分配数据的地址
	}
	// 到这里，列表中已经没有空闲对象了
	if uintptr(f.nchunk) < f.size { // 如果chunk中的空间也不够了，重新分配chunk
		f.chunk = persistentalloc(_FixAllocChunk, 0, f.stat)
		f.nchunk = _FixAllocChunk // 设定当前chunk的大小为16K
	}

	v := f.chunk
	if f.first != nil { // 如果first函数非空
		f.first(f.arg, v) // 执行first函数
	}
	f.chunk = add(f.chunk, f.size) // 将chunk数据向后移动一部分
	f.nchunk -= uint32(f.size)     // 当前chunk仍保留的内存的大小
	f.inuse += f.size
	return v
}

func (f *fixalloc) free(p unsafe.Pointer) { // 释放p指向的地址，加入列表中
	f.inuse -= f.size // 已用空间减少
	v := (*mlink)(p)  // 加入到列表中
	v.next = f.list
	f.list = v
}

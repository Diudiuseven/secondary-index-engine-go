package engine

import "sync/atomic"

// atomicState 是已提交状态的原子句柄。
type atomicState = atomic.Pointer[state]

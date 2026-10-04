package engine

import "hash/maphash"

// treap 是不可变（持久化）随机平衡二叉搜索树。
//
// 所有修改都通过路径复制返回新根；旧根及其引用到的节点保持不变，
// 因此事务开始时拿到的快照可以与并发提交的新版本同时存在，无需读锁。
//
// 键按字节序排序。优先级由键哈希派生，使同一逻辑键在任意快照中都有确定的
// 优先级，重建与增量维护得到的树形状一致；不同键的优先级冲突概率极低，
// 冲突时以键序兜底（不影响正确性，只影响平衡性）。
type treap struct {
	root *tnode
	size int
}

type tnode struct {
	key, val []byte
	prio     uint64
	left     *tnode
	right    *tnode
}

var prioSeed = maphash.MakeSeed()

func keyPriority(key []byte) uint64 {
	h := maphash.Bytes(prioSeed, key)
	// 避开 0（仅用于内部约定）。
	if h == 0 {
		return 1
	}
	return h
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func newNode(key, val []byte) *tnode {
	return &tnode{key: cloneBytes(key), val: cloneBytes(val), prio: keyPriority(key)}
}

func (n *tnode) withLeft(l *tnode) *tnode {
	return &tnode{key: n.key, val: n.val, prio: n.prio, left: l, right: n.right}
}

func (n *tnode) withRight(r *tnode) *tnode {
	return &tnode{key: n.key, val: n.val, prio: n.prio, left: n.left, right: r}
}

func (n *tnode) withVal(val []byte) *tnode {
	return &tnode{key: n.key, val: cloneBytes(val), prio: n.prio, left: n.left, right: n.right}
}

// rotateRight 右旋：输入 n 已是路径上的新复制节点，但其左孩子 l 可能来自旧树，
// 因此新根也必须重新构造，不能直接复用 l，避免把旧节点的子树关系带进新版本。
//
//	   n                 l
//	  / \               / \
//	 l   r    =>      ll   n
//	/ \                  / \
//
// ll  lr              lr   r
func rotateRight(n *tnode) *tnode {
	l := n.left
	newN := &tnode{key: n.key, val: n.val, prio: n.prio, left: l.right, right: n.right}
	return &tnode{key: l.key, val: l.val, prio: l.prio, left: l.left, right: newN}
}

func rotateLeft(n *tnode) *tnode {
	r := n.right
	newN := &tnode{key: n.key, val: n.val, prio: n.prio, left: n.left, right: r.left}
	return &tnode{key: r.key, val: r.val, prio: r.prio, left: newN, right: r.right}
}

// prioLess 堆比较：优先级大者为根；相等时按键序保证确定性。
func prioLess(aPrio uint64, aKey []byte, bPrio uint64, bKey []byte) bool {
	if aPrio != bPrio {
		return aPrio < bPrio
	}
	return compareBytes(aKey, bKey) > 0
}

func insertNode(root *tnode, node *tnode) *tnode {
	if root == nil {
		return node
	}
	c := compareBytes(node.key, root.key)
	if c == 0 {
		// 同键：替换值，保留原节点形状与优先级。
		return root.withVal(node.val)
	}
	if c < 0 {
		newLeft := insertNode(root.left, node)
		n := root.withLeft(newLeft)
		if prioLess(n.prio, n.key, newLeft.prio, newLeft.key) {
			return rotateRight(n)
		}
		return n
	}
	newRight := insertNode(root.right, node)
	n := root.withRight(newRight)
	if prioLess(n.prio, n.key, newRight.prio, newRight.key) {
		return rotateLeft(n)
	}
	return n
}

func mergeTreap(a, b *tnode) *tnode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if !prioLess(a.prio, a.key, b.prio, b.key) {
		return a.withRight(mergeTreap(a.right, b))
	}
	return b.withLeft(mergeTreap(a, b.left))
}

func deleteNode(root *tnode, key []byte) *tnode {
	if root == nil {
		return nil
	}
	c := compareBytes(key, root.key)
	if c < 0 {
		return root.withLeft(deleteNode(root.left, key))
	}
	if c > 0 {
		return root.withRight(deleteNode(root.right, key))
	}
	return mergeTreap(root.left, root.right)
}

func getNode(root *tnode, key []byte) *tnode {
	for root != nil {
		switch compareBytes(key, root.key) {
		case 0:
			return root
		case -1:
			root = root.left
		default:
			root = root.right
		}
	}
	return nil
}

func countNodes(root *tnode) int {
	if root == nil {
		return 0
	}
	return 1 + countNodes(root.left) + countNodes(root.right)
}

func newTreap() *treap { return &treap{} }

func (t *treap) get(key []byte) ([]byte, bool) {
	n := getNode(t.root, key)
	if n == nil {
		return nil, false
	}
	return n.val, true
}

func (t *treap) upsert(key, val []byte) *treap {
	return &treap{root: insertNode(t.root, newNode(key, val)), size: 0}
}

func (t *treap) delete(key []byte) *treap {
	newRoot := deleteNode(t.root, key)
	return &treap{root: newRoot, size: 0}
}

func (t *treap) len() int {
	if t.size != 0 {
		return t.size
	}
	return countNodes(t.root)
}

// ascIter 从 >= start（start 为 nil 表示最小键）开始升序遍历；
// stop 返回 true 立即终止。
//
// 算法：维护一条“当前最小未输出节点所在的左脊”。
// 栈顶即下一个待输出节点；输出后丢弃栈顶并把其右子树的左脊压入。
// 每个节点恰好入栈一次，不会重复访问。
func (t *treap) ascIter(start []byte, stop func(k, v []byte) bool) {
	var stack []*tnode
	pushLeft := func(node *tnode) {
		for node != nil {
			if start != nil && compareBytes(node.key, start) < 0 {
				node = node.right
				continue
			}
			stack = append(stack, node)
			node = node.left
		}
	}
	pushLeft(t.root)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if stop(n.key, n.val) {
			return
		}
		pushLeft(n.right)
	}
}

package watch

// folderTree holds the folders a watch marks one by one, by the key the kernel names each with: a file handle
// for fanotify, a watch descriptor for inotify. A folder's path is its parents' names, so a folder renamed
// moves everything below it at once. It does no I/O.
type folderTree[K comparable] struct {
	nodes map[K]*folderNode[K]
	root  *folderNode[K]
}

type folderNode[K comparable] struct {
	key      K
	parent   *folderNode[K]
	name     string
	children map[string]*folderNode[K]
	// detached says that the folder left its parent, by a rename whose other end is not known yet.
	detached bool
}

func newFolderTree[K comparable](rootKey K) *folderTree[K] {
	root := &folderNode[K]{key: rootKey, children: map[string]*folderNode[K]{}}
	return &folderTree[K]{nodes: map[K]*folderNode[K]{rootKey: root}, root: root}
}

// add records a folder below another. A key already known is that folder, which moves there.
func (t *folderTree[K]) add(parent *folderNode[K], name string, key K) *folderNode[K] {
	if n := t.nodes[key]; n != nil {
		t.attach(n, parent, name)
		return n
	}
	n := &folderNode[K]{key: key, children: map[string]*folderNode[K]{}}
	t.nodes[key] = n
	t.attach(n, parent, name)
	return n
}

// detach takes the folder of that name out of its parent, and gives it, or nil when none is known.
func (t *folderTree[K]) detach(parent *folderNode[K], name string) *folderNode[K] {
	n := parent.children[name]
	if n == nil {
		return nil
	}
	delete(parent.children, name)
	n.parent, n.detached = nil, true
	return n
}

// attach puts a folder below another, under a name, in place of any other folder of that name.
func (t *folderTree[K]) attach(n, parent *folderNode[K], name string) {
	if n.parent != nil && n.parent.children[n.name] == n {
		delete(n.parent.children, n.name)
	}
	if old := parent.children[name]; old != nil && old != n {
		t.remove(old)
	}
	n.parent, n.name, n.detached = parent, name, false
	parent.children[name] = n
}

// remove forgets a folder and everything below it, and gives their keys.
func (t *folderTree[K]) remove(n *folderNode[K]) []K {
	if n.parent != nil && n.parent.children[n.name] == n {
		delete(n.parent.children, n.name)
	}
	var keys []K
	var walk func(*folderNode[K])
	walk = func(m *folderNode[K]) {
		for _, c := range m.children {
			walk(c)
		}
		if t.nodes[m.key] == m {
			delete(t.nodes, m.key)
		}
		keys = append(keys, m.key)
	}
	walk(n)
	return keys
}

// path gives a folder's path below the root, and false when it, or a folder above it, left the tree.
func (n *folderNode[K]) path() (string, bool) {
	var parts []string
	for m := n; m.parent != nil; m = m.parent {
		parts = append(parts, m.name)
		if m.parent.detached {
			return "", false
		}
	}
	if n.detached {
		return "", false
	}
	out := ""
	for i := len(parts) - 1; i >= 0; i-- {
		if out != "" {
			out += "/"
		}
		out += parts[i]
	}
	return out, true
}

// join gives the path of a name in a folder whose path is dir.
func join(dir, name string) string {
	switch {
	case name == "" || name == ".":
		return dir
	case dir == "":
		return name
	default:
		return dir + "/" + name
	}
}

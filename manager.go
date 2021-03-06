package parcello

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	zipexe "github.com/daaku/go.zipexe"
	"github.com/kardianos/osext"
)

var (
	// ErrReadOnly is returned if the file is read-only and write operations are disabled.
	ErrReadOnly = errors.New("File is read-only")
	// ErrWriteOnly is returned if the file is write-only and read operations are disabled.
	ErrWriteOnly = errors.New("File is write-only")
	// ErrIsDirectory is returned if the file under operation is not a regular file but a directory.
	ErrIsDirectory = errors.New("Is directory")
)

var _ FileSystemManager = &ResourceManager{}

// ResourceManager represents a virtual in memory file system
type ResourceManager struct {
	rw   sync.RWMutex
	root *Node
}

// NewResourceManager creates a new manager
func NewResourceManager() (*ResourceManager, error) {
	manager := &ResourceManager{
		root: &Node{Name: "/", IsDir: true},
	}

	path, err := osext.Executable()
	if err != nil {
		return manager, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	reader, err := zipexe.NewReader(file, info.Size())
	if err != nil {
		return manager, nil
	}

	if err = manager.uncompress(reader); err != nil {
		return nil, err
	}

	return manager, nil
}

// Add adds resource to the manager
func (m *ResourceManager) Add(binary Binary) error {
	m.rw.Lock()
	defer m.rw.Unlock()

	if m.root == nil {
		m.root = &Node{Name: "/", IsDir: true}
	}

	reader, err := zip.NewReader(bytes.NewReader(binary), int64(len(binary)))
	if err != nil {
		return err
	}

	return m.uncompress(reader)
}

func (m *ResourceManager) uncompress(reader *zip.Reader) error {
	for _, header := range reader.File {
		path := split(header.Name)
		node := add(path, m.root)

		if node == m.root || node == nil {
			return fmt.Errorf("Invalid path: '%s'", header.Name)
		}

		file, err := header.Open()
		if err != nil {
			return err
		}
		defer file.Close()

		content, err := ioutil.ReadAll(file)
		if err != nil {
			return err
		}

		node.IsDir = false
		node.Content = &content
	}

	return nil
}

// Root returns a sub-manager for given path
func (m *ResourceManager) Root(name string) (FileSystemManager, error) {
	if _, node := find(split(name), nil, m.root); node != nil {
		if node.IsDir {
			return &ResourceManager{root: node}, nil
		}
	}

	return nil, fmt.Errorf("Resource '%s' not found", name)
}

// Open opens an embedded resource for read
func (m *ResourceManager) Open(name string) (ReadOnlyFile, error) {
	return m.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile is the generalized open call; most users will use Open
func (m *ResourceManager) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	parent, node, err := m.open(name)
	if err != nil {
		return nil, err
	}

	if isWritable(flag) && node != nil && node.IsDir {
		return nil, &os.PathError{Op: "open", Path: name, Err: ErrIsDirectory}
	}

	if hasFlag(os.O_CREATE, flag) {
		if node != nil && !hasFlag(os.O_TRUNC, flag) {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
		}

		node = newNode(filepath.Base(name), parent)
	}

	if node == nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}

	return newFile(node, flag)
}

func (m *ResourceManager) open(name string) (*Node, *Node, error) {
	parent, node := find(split(name), nil, m.root)
	if node != m.root && parent == nil {
		return nil, nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}

	return parent, node, nil
}

// Walk walks the file tree rooted at root, calling walkFn for each file or
// directory in the tree, including root.
func (m *ResourceManager) Walk(dir string, fn filepath.WalkFunc) error {
	if _, node := find(split(dir), nil, m.root); node != nil {
		return walk(dir, node, fn)
	}

	return fmt.Errorf("Directory '%s' not found", dir)
}

func add(path []string, node *Node) *Node {
	if !node.IsDir || node.Content != nil {
		return nil
	}

	if len(path) == 0 {
		return node
	}

	name := path[0]

	for _, child := range node.Children {
		if child.Name == name {
			return add(path[1:], child)
		}
	}

	child := &Node{
		Mutex:   &sync.RWMutex{},
		Name:    name,
		IsDir:   true,
		ModTime: time.Now(),
	}

	node.Children = append(node.Children, child)
	return add(path[1:], child)
}

func split(path string) []string {
	parts := []string{}

	for _, part := range strings.Split(path, string(os.PathSeparator)) {
		if part != "" && part != "/" {
			parts = append(parts, part)
		}
	}

	return parts
}

func find(path []string, parent, node *Node) (*Node, *Node) {
	if len(path) == 0 || node == nil {
		return parent, node
	}

	for _, child := range node.Children {
		if path[0] == child.Name {
			if len(path) == 1 {
				return node, child
			}
			return find(path[1:], node, child)
		}
	}

	return parent, nil
}

func walk(path string, node *Node, fn filepath.WalkFunc) error {
	if err := fn(path, &ResourceFileInfo{Node: node}, nil); err != nil {
		return err
	}

	for _, child := range node.Children {
		if err := walk(filepath.Join(path, child.Name), child, fn); err != nil {
			return err
		}
	}

	return nil
}

func newNode(name string, parent *Node) *Node {
	node := &Node{
		Name:    name,
		IsDir:   false,
		ModTime: time.Now(),
	}

	parent.Children = append(parent.Children, node)
	return node
}

func newFile(node *Node, flag int) (File, error) {
	if isWritable(flag) {
		node.ModTime = time.Now()
	}

	if node.Content == nil || hasFlag(os.O_TRUNC, flag) {
		buf := make([]byte, 0)
		node.Content = &buf
		node.Mutex = &sync.RWMutex{}
	}

	f := NewResourceFile(node)

	if hasFlag(os.O_APPEND, flag) {
		_, _ = f.Seek(0, io.SeekEnd)
	}

	if hasFlag(os.O_RDWR, flag) {
		return f, nil
	}
	if hasFlag(os.O_WRONLY, flag) {
		return &woFile{f}, nil
	}

	return &roFile{f}, nil
}

func hasFlag(flag int, flags int) bool {
	return flags&flag == flag
}

func isWritable(flag int) bool {
	return hasFlag(os.O_WRONLY, flag) || hasFlag(os.O_RDWR, flag) || hasFlag(os.O_APPEND, flag)
}

type roFile struct {
	*ResourceFile
}

// Write is disabled and returns ErrorReadOnly
func (f *roFile) Write(p []byte) (n int, err error) {
	return 0, ErrReadOnly
}

// woFile wraps the given file and disables Read(..) operation.
type woFile struct {
	*ResourceFile
}

// Read is disabled and returns ErrorWroteOnly
func (f *woFile) Read(p []byte) (n int, err error) {
	return 0, ErrWriteOnly
}

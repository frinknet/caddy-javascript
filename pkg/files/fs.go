package files

import (
	"os"
	"path"
)

type FS struct {
	root *os.Root
	dir  string
}

func NewFS(dir string) (*FS, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &FS{root: root, dir: dir}, nil
}

func (f *FS) Dir() string {
	return f.dir
}

func (f *FS) Close() error {
	return f.root.Close()
}

func (f *FS) ReadFile(name string) ([]byte, error) {
	return f.root.ReadFile(relative(name))
}

func (f *FS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return f.root.WriteFile(relative(name), data, perm)
}

func relative(name string) string {
	cleaned := path.Clean("/" + name)
	return cleaned[1:]
}

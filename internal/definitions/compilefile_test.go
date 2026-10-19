package definitions

import (
	"os"
	"path/filepath"
)

// compileFile compiles the bundle in the directory holding the journeys.yaml
// at path, the way production does: ReadBundle then CompileBundle.
func compileFile(path string) (*Set, error) {
	b, err := ReadBundle(os.DirFS(filepath.Dir(path)))
	if err != nil {
		return nil, err
	}
	return CompileBundle(b)
}

package catalog

import (
	"os"
	"path/filepath"

	"github.com/swiftdiaries/az-agent-platform-go/internal/definitions"
)

// compileFile compiles the bundle in the directory holding the journeys.yaml
// at path: ReadBundle then CompileBundle.
func compileFile(path string) (*definitions.Set, error) {
	b, err := definitions.ReadBundle(os.DirFS(filepath.Dir(path)))
	if err != nil {
		return nil, err
	}
	return definitions.CompileBundle(b)
}

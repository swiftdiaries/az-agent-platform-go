package runtime

import (
	"os"
	"testing"

	"github.com/swiftdiaries/az-agent-platform-go/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

package skillrt

import (
	"os"
	"testing"

	"sokratos/logger"
)

func TestMain(m *testing.M) {
	_ = logger.Init(os.TempDir())
	os.Exit(m.Run())
}

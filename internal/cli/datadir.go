package cli

import (
	"errors"
	"os"
	"path/filepath"
)

// dataDirEnv overrides where perfwatch keeps its database, logs and config.
const dataDirEnv = "PERFWATCH_DATA_DIR"

// dataDir returns $PERFWATCH_DATA_DIR, or %LOCALAPPDATA%\perfwatch.
func dataDir() (string, error) {
	if dir := os.Getenv(dataDirEnv); dir != "" {
		return dir, nil
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", errors.New("LOCALAPPDATA is not set; set " + dataDirEnv + " to choose the data directory")
	}
	return filepath.Join(base, "perfwatch"), nil
}

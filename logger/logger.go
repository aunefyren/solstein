// Package logger holds the process-wide logrus logger. Everything logs through
// logger.Log, which writes to stdout and to solstein.log in the config directory.
package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	easy "github.com/t-tomalak/logrus-easy-formatter"
)

const logFileName = "solstein.log"

// Log starts out writing to stdout at info level, so anything logged before
// Init (or in tests that never call it) still goes somewhere sensible.
var Log = newLogger(os.Stdout, logrus.InfoLevel)

// Init points Log at stdout plus the log file in configDir and sets the level.
// The returned file should be closed on shutdown.
func Init(configDir string, level string) (io.Closer, error) {
	parsedLevel, err := logrus.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", level, err)
	}

	path := filepath.Join(configDir, logFileName)
	logFile, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open log file %s: %w", path, err)
	}

	Log = newLogger(io.MultiWriter(os.Stdout, logFile), parsedLevel)
	return logFile, nil
}

func newLogger(output io.Writer, level logrus.Level) *logrus.Logger {
	log := logrus.New()
	log.SetOutput(output)
	log.SetLevel(level)
	log.Formatter = &easy.Formatter{
		TimestampFormat: "2006-01-02 15:04:05",
		LogFormat:       "[%lvl%]: %time% - %msg%\n",
	}
	return log
}

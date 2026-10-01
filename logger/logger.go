// Package logger holds the process-wide logrus logger. Everything logs through
// logger.Log, which writes to stdout and to solstein.log in the config directory.
package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

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
	log.Formatter = escapingFormatter{inner: &easy.Formatter{
		TimestampFormat: "2006-01-02 15:04:05",
		LogFormat:       "[%lvl%]: %time% - %msg%\n",
	}}
	return log
}

// escapingFormatter keeps every entry on one line of plain text. Messages
// carry text from outside Solstein — episode and feed titles from source
// feeds, errors quoting what a host sent — and a newline in one would let a
// feed write lines of its own into the log (a forged "[INFO]: …"), and an
// escape sequence would reach the terminal of whoever reads it. Doing it
// here covers every log line, rather than each call site having to remember.
type escapingFormatter struct {
	inner logrus.Formatter
}

func (formatter escapingFormatter) Format(entry *logrus.Entry) ([]byte, error) {
	if strings.IndexFunc(entry.Message, needsEscaping) >= 0 {
		escaped := *entry
		escaped.Message = escapeControl(entry.Message)
		entry = &escaped
	}
	return formatter.inner.Format(entry)
}

// needsEscaping is true for control characters (C0, DEL and C1) but tab,
// and for the Unicode line and paragraph separators, which some viewers
// break lines at.
func needsEscaping(r rune) bool {
	return r == ' ' || r == ' ' || (unicode.IsControl(r) && r != '\t')
}

// escapeControl writes the control characters in text as visible escapes
// (`\n`, `\r`, `\x1b`, ` ` …), so what was there still shows. Tabs are
// left as they are.
func escapeControl(text string) string {
	var escaped strings.Builder
	for _, r := range text {
		switch {
		case r == '\n':
			escaped.WriteString(`\n`)
		case r == '\r':
			escaped.WriteString(`\r`)
		case !needsEscaping(r):
			escaped.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&escaped, `\x%02x`, r)
		default:
			fmt.Fprintf(&escaped, `\u%04x`, r)
		}
	}
	return escaped.String()
}

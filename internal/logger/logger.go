package logger

import (
	"os"

	"github.com/sirupsen/logrus"
)

// New creates a logrus logger configured with JSON output and the given log
// level. Falls back to InfoLevel for unrecognised level strings.
func New(level string) *logrus.Logger {
	log := logrus.New()
	log.SetOutput(os.Stdout)
	log.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
	})

	parsed, err := logrus.ParseLevel(level)
	if err != nil {
		parsed = logrus.InfoLevel
	}
	log.SetLevel(parsed)

	return log
}

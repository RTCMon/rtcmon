package logger_test

import (
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/logger"
)

func TestNew_InfoLevel(t *testing.T) {
	log := logger.New("info")
	if log == nil {
		t.Fatal("New returned nil")
	}
	if log.GetLevel() != logrus.InfoLevel {
		t.Errorf("level: got %v, want %v", log.GetLevel(), logrus.InfoLevel)
	}
}

func TestNew_DebugLevel(t *testing.T) {
	log := logger.New("debug")
	if log.GetLevel() != logrus.DebugLevel {
		t.Errorf("level: got %v, want %v", log.GetLevel(), logrus.DebugLevel)
	}
}

func TestNew_UnknownLevel(t *testing.T) {
	// Unknown level must not panic and must fall back to InfoLevel.
	log := logger.New("foobar")
	if log == nil {
		t.Fatal("New returned nil")
	}
	if log.GetLevel() != logrus.InfoLevel {
		t.Errorf("level: got %v, want %v (fallback)", log.GetLevel(), logrus.InfoLevel)
	}
}

func TestNew_EmptyLevel(t *testing.T) {
	log := logger.New("")
	if log.GetLevel() != logrus.InfoLevel {
		t.Errorf("level: got %v, want %v (fallback)", log.GetLevel(), logrus.InfoLevel)
	}
}

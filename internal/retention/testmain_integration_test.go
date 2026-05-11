//go:build integration

package retention_test

import (
	"context"
	"os"
	"testing"

	"github.com/RTCMon/rtcmon/internal/testutil"
)

func TestMain(m *testing.M) {
	connStr, teardown := testutil.StartPostgresForMain(context.Background())
	os.Setenv("TEST_DB_URL", connStr) //nolint:errcheck
	code := m.Run()
	teardown()
	os.Exit(code)
}

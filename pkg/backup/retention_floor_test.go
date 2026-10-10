package backup

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
)

func TestApplyRetention_NeverDeletesNewestMatchingObject(t *testing.T) {
	policies := map[string]*dbtether.RetentionPolicy{
		"keepDaily":   {KeepDaily: intPtr(7)},
		"keepWeekly":  {KeepWeekly: intPtr(2)},
		"keepMonthly": {KeepMonthly: intPtr(1)},
	}
	for name, policy := range policies {
		t.Run(name, func(t *testing.T) {
			rm := &RetentionManager{Log: zap.NewNop().Sugar()}
			mc := storage.NewMockClient()
			mc.AddObject("c/app/20260101-120000.sql.gz", []byte("old"), time.Time{})
			mc.AddObject("c/app/20260102-120000.sql.gz", []byte("newest"), time.Time{})

			toDelete, err := rm.ApplyRetention(context.Background(), mc, "c/app/", policy, nil)

			require.NoError(t, err)
			assert.NotContains(t, toDelete, "c/app/20260102-120000.sql.gz")
		})
	}
}

func TestApplyRetention_TimeWindowsAnchorAtNewestObject(t *testing.T) {
	rm := &RetentionManager{Log: zap.NewNop().Sugar()}
	mc := storage.NewMockClient()
	for _, day := range []string{"10", "09", "08", "07", "06"} {
		mc.AddObject("c/app/202601"+day+"-120000.sql.gz", []byte(day), time.Time{})
	}

	toDelete, err := rm.ApplyRetention(context.Background(), mc, "c/app/", &dbtether.RetentionPolicy{KeepDaily: intPtr(2)}, nil)

	require.NoError(t, err)
	assert.NotContains(t, toDelete, "c/app/20260110-120000.sql.gz")
	assert.NotContains(t, toDelete, "c/app/20260109-120000.sql.gz")
	assert.Contains(t, toDelete, "c/app/20260106-120000.sql.gz")
}

package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
)

func TestParseTimestampFromKey(t *testing.T) {
	tests := []struct {
		name        string
		key         string
		wantTime    time.Time
		shouldError bool
	}{
		{
			name:     "simple filename",
			key:      "20260120-143022.sql.gz",
			wantTime: time.Date(2026, 1, 20, 14, 30, 22, 0, time.UTC),
		},
		{
			name:     "with runID",
			key:      "20260120-143022-a1b2c3d4.sql.gz",
			wantTime: time.Date(2026, 1, 20, 14, 30, 22, 0, time.UTC),
		},
		{
			name:     "with path prefix",
			key:      "cluster/database/20260120-143022.sql.gz",
			wantTime: time.Date(2026, 1, 20, 14, 30, 22, 0, time.UTC),
		},
		{
			name:     "with nested path",
			key:      "backups/2026/01/cluster/database/20260120-143022-xyz123.sql.gz",
			wantTime: time.Date(2026, 1, 20, 14, 30, 22, 0, time.UTC),
		},
		{
			name:        "no timestamp",
			key:         "backup.sql.gz",
			shouldError: true,
		},
		{
			name:        "invalid timestamp format",
			key:         "2026-01-20.sql.gz",
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parseTimestampFromKey(tt.key)
			if tt.shouldError {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantTime, parsed)
			}
		})
	}
}

func TestCalculateKeepSet(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())

	// Create test files spanning several days
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	files := []BackupFile{
		{Key: "backup-20260120-100000.sql.gz", Timestamp: now.Add(-2 * time.Hour)},       // today 10:00
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-10 * time.Hour)},      // today 02:00
		{Key: "backup-20260119-020000.sql.gz", Timestamp: now.Add(-34 * time.Hour)},      // yesterday 02:00
		{Key: "backup-20260118-020000.sql.gz", Timestamp: now.Add(-58 * time.Hour)},      // 2 days ago
		{Key: "backup-20260117-020000.sql.gz", Timestamp: now.Add(-82 * time.Hour)},      // 3 days ago
		{Key: "backup-20260116-020000.sql.gz", Timestamp: now.Add(-106 * time.Hour)},     // 4 days ago
		{Key: "backup-20260115-020000.sql.gz", Timestamp: now.Add(-130 * time.Hour)},     // 5 days ago
		{Key: "backup-20260114-020000.sql.gz", Timestamp: now.Add(-154 * time.Hour)},     // 6 days ago
		{Key: "backup-20260113-020000.sql.gz", Timestamp: now.Add(-178 * time.Hour)},     // 7 days ago
		{Key: "backup-20260112-020000.sql.gz", Timestamp: now.Add(-202 * time.Hour)},     // 8 days ago
		{Key: "backup-20260101-020000.sql.gz", Timestamp: now.Add(-19 * 24 * time.Hour)}, // 19 days ago
		{Key: "backup-20251220-020000.sql.gz", Timestamp: now.Add(-31 * 24 * time.Hour)}, // 31 days ago
		{Key: "backup-20251201-020000.sql.gz", Timestamp: now.Add(-50 * 24 * time.Hour)}, // 50 days ago
	}

	t.Run("keepLast=3", func(t *testing.T) {
		policy := &dbtether.RetentionPolicy{KeepLast: intPtr(3)}
		keep := rm.calculateKeepSet(files, policy)

		// Should keep the 3 most recent
		assert.True(t, keep["backup-20260120-100000.sql.gz"])
		assert.True(t, keep["backup-20260120-020000.sql.gz"])
		assert.True(t, keep["backup-20260119-020000.sql.gz"])
		assert.False(t, keep["backup-20260118-020000.sql.gz"])
	})

	t.Run("keepDaily=5", func(t *testing.T) {
		policy := &dbtether.RetentionPolicy{KeepDaily: intPtr(5)}
		keep := rm.calculateKeepSet(files, policy)

		// Files are sorted newest-first, so "first seen" per day = newest of that day
		assert.True(t, keep["backup-20260120-100000.sql.gz"])  // newest of today - kept
		assert.False(t, keep["backup-20260120-020000.sql.gz"]) // older of today - not kept
		assert.True(t, keep["backup-20260119-020000.sql.gz"])
		assert.True(t, keep["backup-20260118-020000.sql.gz"])
		assert.True(t, keep["backup-20260117-020000.sql.gz"])
		assert.True(t, keep["backup-20260116-020000.sql.gz"])
		assert.False(t, keep["backup-20260115-020000.sql.gz"]) // beyond 5 days
	})

	t.Run("combined keepLast=2 keepDaily=7", func(t *testing.T) {
		policy := &dbtether.RetentionPolicy{
			KeepLast:  intPtr(2),
			KeepDaily: intPtr(7),
		}
		keep := rm.calculateKeepSet(files, policy)

		// keepLast=2: most recent 2
		assert.True(t, keep["backup-20260120-100000.sql.gz"])
		assert.True(t, keep["backup-20260120-020000.sql.gz"])

		// keepDaily=7: one per day in last 7 days (newest per day since sorted newest-first)
		// Note: cutoff is calculated from current time (now = 2026-01-20 12:00),
		// so 7 days ago = 2026-01-13 12:00, meaning 2026-01-13 02:00 is BEFORE cutoff
		assert.True(t, keep["backup-20260119-020000.sql.gz"])
		assert.True(t, keep["backup-20260118-020000.sql.gz"])
		assert.True(t, keep["backup-20260117-020000.sql.gz"])
		assert.True(t, keep["backup-20260116-020000.sql.gz"])
		assert.True(t, keep["backup-20260115-020000.sql.gz"])
		assert.True(t, keep["backup-20260114-020000.sql.gz"])
		// 20260113-020000 is 02:00 UTC, but cutoff is 20260113-120000, so it's before cutoff
		assert.False(t, keep["backup-20260113-020000.sql.gz"])

		// Beyond 7 days
		assert.False(t, keep["backup-20260112-020000.sql.gz"])
	})
}

func TestRetentionPolicy_AllNil(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())

	files := []BackupFile{
		{Key: "backup-20260120-100000.sql.gz", Timestamp: time.Now()},
	}

	keep := rm.calculateKeepSet(files, &dbtether.RetentionPolicy{})
	assert.Empty(t, keep) // No files kept if no policy fields set
}

func TestCalculateKeepSet_Weekly(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())

	// Create test files spanning several weeks (each Monday at 2am)
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC) // Monday Jan 20
	files := []BackupFile{
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-10 * time.Hour)},                 // This week (Mon Jan 20)
		{Key: "backup-20260113-020000.sql.gz", Timestamp: now.Add(-7*24*time.Hour - 10*time.Hour)},  // Week -1 (Mon Jan 13)
		{Key: "backup-20260106-020000.sql.gz", Timestamp: now.Add(-14*24*time.Hour - 10*time.Hour)}, // Week -2 (Mon Jan 6)
		{Key: "backup-20251230-020000.sql.gz", Timestamp: now.Add(-21*24*time.Hour - 10*time.Hour)}, // Week -3 (Mon Dec 30)
		{Key: "backup-20251223-020000.sql.gz", Timestamp: now.Add(-28*24*time.Hour - 10*time.Hour)}, // Week -4 (Mon Dec 23)
	}

	policy := &dbtether.RetentionPolicy{KeepWeekly: intPtr(3)}
	keep := rm.calculateKeepSet(files, policy)

	// Should keep 3 weeks worth (newest first from each week)
	assert.True(t, keep["backup-20260120-020000.sql.gz"], "week 0 should be kept")
	assert.True(t, keep["backup-20260113-020000.sql.gz"], "week 1 should be kept")
	assert.True(t, keep["backup-20260106-020000.sql.gz"], "week 2 should be kept")
	assert.False(t, keep["backup-20251230-020000.sql.gz"], "week 3 should not be kept")
	assert.False(t, keep["backup-20251223-020000.sql.gz"], "week 4 should not be kept")
}

func TestCalculateKeepSet_Monthly(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())

	// Create test files spanning several months (now = Jan 20, 2026)
	files := []BackupFile{
		{Key: "backup-20260115-020000.sql.gz", Timestamp: time.Date(2026, 1, 15, 2, 0, 0, 0, time.UTC)},  // Jan 2026
		{Key: "backup-20251215-020000.sql.gz", Timestamp: time.Date(2025, 12, 15, 2, 0, 0, 0, time.UTC)}, // Dec 2025
		{Key: "backup-20251115-020000.sql.gz", Timestamp: time.Date(2025, 11, 15, 2, 0, 0, 0, time.UTC)}, // Nov 2025
		{Key: "backup-20251015-020000.sql.gz", Timestamp: time.Date(2025, 10, 15, 2, 0, 0, 0, time.UTC)}, // Oct 2025
		{Key: "backup-20250915-020000.sql.gz", Timestamp: time.Date(2025, 9, 15, 2, 0, 0, 0, time.UTC)},  // Sep 2025
		{Key: "backup-20250815-020000.sql.gz", Timestamp: time.Date(2025, 8, 15, 2, 0, 0, 0, time.UTC)},  // Aug 2025 - beyond cutoff
	}

	policy := &dbtether.RetentionPolicy{KeepMonthly: intPtr(4)} // 4 months
	keep := rm.calculateKeepSet(files, policy)

	// Should keep 4 months (cutoff = now - 4 months = Sep 20, 2025)
	assert.True(t, keep["backup-20260115-020000.sql.gz"], "Jan should be kept")
	assert.True(t, keep["backup-20251215-020000.sql.gz"], "Dec should be kept")
	assert.True(t, keep["backup-20251115-020000.sql.gz"], "Nov should be kept")
	assert.True(t, keep["backup-20251015-020000.sql.gz"], "Oct should be kept")
	// Sep 15 is before cutoff (Sep 20)
	assert.False(t, keep["backup-20250915-020000.sql.gz"], "Sep should not be kept (before cutoff)")
	assert.False(t, keep["backup-20250815-020000.sql.gz"], "Aug should not be kept")
}

func TestCalculateKeepSet_MultiplePerDay(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())

	// Multiple backups per day - only first (newest) should be kept for daily
	now := time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC)
	files := []BackupFile{
		{Key: "backup-20260120-160000.sql.gz", Timestamp: now.Add(-2 * time.Hour)},  // 16:00
		{Key: "backup-20260120-140000.sql.gz", Timestamp: now.Add(-4 * time.Hour)},  // 14:00
		{Key: "backup-20260120-120000.sql.gz", Timestamp: now.Add(-6 * time.Hour)},  // 12:00
		{Key: "backup-20260120-100000.sql.gz", Timestamp: now.Add(-8 * time.Hour)},  // 10:00
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-16 * time.Hour)}, // 02:00
	}

	policy := &dbtether.RetentionPolicy{KeepDaily: intPtr(1)}
	keep := rm.calculateKeepSet(files, policy)

	// Only the newest backup of the day should be kept
	assert.True(t, keep["backup-20260120-160000.sql.gz"], "newest should be kept")
	assert.False(t, keep["backup-20260120-140000.sql.gz"])
	assert.False(t, keep["backup-20260120-120000.sql.gz"])
	assert.False(t, keep["backup-20260120-100000.sql.gz"])
	assert.False(t, keep["backup-20260120-020000.sql.gz"])
}

func TestCalculateKeepSet_EmptyFiles(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())

	files := []BackupFile{}
	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(10)}

	keep := rm.calculateKeepSet(files, policy)
	assert.Empty(t, keep)
}

func TestCalculateKeepSet_AllPoliciesCombined(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())

	// Complex scenario with all policies
	// now = 2026-01-20 12:00 UTC
	// ISO weeks:
	//   Jan 20 = week 2026-W04
	//   Jan 19 = week 2026-W04
	//   Jan 18 = week 2026-W03
	//   Jan 13 = week 2026-W03 (same as Jan 18!)
	//   Jan 7  = week 2026-W02
	//   Dec 15 = week 2025-W51
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	files := []BackupFile{
		// Recent files (keepLast territory)
		{Key: "backup-20260120-100000.sql.gz", Timestamp: now.Add(-2 * time.Hour)},
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-10 * time.Hour)},
		// Daily territory (within 3 days)
		{Key: "backup-20260119-020000.sql.gz", Timestamp: now.Add(-34 * time.Hour)},
		{Key: "backup-20260118-020000.sql.gz", Timestamp: now.Add(-58 * time.Hour)},
		// Weekly territory - different ISO weeks
		{Key: "backup-20260113-020000.sql.gz", Timestamp: now.Add(-7*24*time.Hour - 10*time.Hour)}, // W03, but Jan 18 also W03
		{Key: "backup-20260107-120000.sql.gz", Timestamp: now.Add(-13 * 24 * time.Hour)},           // W02
		// Monthly territory
		{Key: "backup-20251215-020000.sql.gz", Timestamp: time.Date(2025, 12, 15, 2, 0, 0, 0, time.UTC)},
	}

	policy := &dbtether.RetentionPolicy{
		KeepLast:    intPtr(2),
		KeepDaily:   intPtr(3),
		KeepWeekly:  intPtr(4), // 4 weeks = 28 days
		KeepMonthly: intPtr(2),
	}
	keep := rm.calculateKeepSet(files, policy)

	// keepLast=2 should keep first 2 (after sorting newest-first)
	assert.True(t, keep["backup-20260120-100000.sql.gz"], "keepLast: newest should be kept")
	assert.True(t, keep["backup-20260120-020000.sql.gz"], "keepLast: second newest should be kept")

	// keepDaily=3 adds more days (one per day, newest-first)
	assert.True(t, keep["backup-20260119-020000.sql.gz"], "keepDaily: Jan 19 should be kept")
	assert.True(t, keep["backup-20260118-020000.sql.gz"], "keepDaily: Jan 18 should be kept")

	// keepWeekly=4: Jan 20 (W04), Jan 18 (W03), Jan 7 (W02) - one per week
	// Note: Jan 13 is SAME ISO week as Jan 18, so Jan 18 wins (it comes first in sorted list)
	assert.True(t, keep["backup-20260107-120000.sql.gz"], "keepWeekly: Jan 7 (W02) should be kept")
	// Jan 13 skipped because Jan 18 already represents W03
	assert.False(t, keep["backup-20260113-020000.sql.gz"], "keepWeekly: Jan 13 skipped (same week as Jan 18)")

	// keepMonthly=2: Dec 15 is within 2 months from Jan 20
	assert.True(t, keep["backup-20251215-020000.sql.gz"], "keepMonthly: Dec 15 should be kept")
}

func TestParseTimestampFromKey_Fallback(t *testing.T) {
	// Test that files without timestamp in filename can use S3 LastModified fallback
	// (The actual fallback logic is in listBackupFiles, here we test parseTimestampFromKey behavior)

	tests := []struct {
		name        string
		key         string
		shouldError bool
	}{
		{
			name:        "standard timestamp format works",
			key:         "cluster/db/20260120-143022.sql.gz",
			shouldError: false,
		},
		{
			name:        "no timestamp - needs fallback",
			key:         "cluster/db/my_database-abc12345.sql.gz",
			shouldError: true, // parseTimestampFromKey fails, but listBackupFiles uses LastModified
		},
		{
			name:        "only runID - needs fallback",
			key:         "cluster/db/backup-runid123.sql.gz",
			shouldError: true,
		},
		{
			name:        "random filename - needs fallback",
			key:         "cluster/db/latest.sql.gz",
			shouldError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTimestampFromKey(tt.key)
			if tt.shouldError {
				assert.Error(t, err, "should need fallback for key: %s", tt.key)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func intPtr(i int) *int {
	return &i
}

// Integration tests using MockClient

func TestApplyRetention_WithMockClient(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	// Create mock storage with test data
	mockClient := storage.NewMockClient()

	// Add files with timestamps in keys
	now := time.Now()
	mockClient.AddObject("cluster/db/20260120-140000.sql.gz", []byte("backup1"), now.Add(-1*time.Hour))
	mockClient.AddObject("cluster/db/20260120-130000.sql.gz", []byte("backup2"), now.Add(-2*time.Hour))
	mockClient.AddObject("cluster/db/20260120-120000.sql.gz", []byte("backup3"), now.Add(-3*time.Hour))
	mockClient.AddObject("cluster/db/20260120-110000.sql.gz", []byte("backup4"), now.Add(-4*time.Hour))
	mockClient.AddObject("cluster/db/20260120-100000.sql.gz", []byte("backup5"), now.Add(-5*time.Hour))

	// Apply retention: keep last 2
	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(2)}
	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", policy)

	require.NoError(t, err)
	assert.Len(t, toDelete, 3, "should mark 3 files for deletion")

	// Delete files
	err = rm.DeleteFiles(ctx, mockClient, toDelete)
	require.NoError(t, err)

	// Verify only 2 files remain
	assert.Equal(t, 2, mockClient.Count(), "should have 2 files remaining")

	// Verify the kept files are the newest ones
	_, ok1 := mockClient.GetObject("cluster/db/20260120-140000.sql.gz")
	_, ok2 := mockClient.GetObject("cluster/db/20260120-130000.sql.gz")
	assert.True(t, ok1, "newest file should remain")
	assert.True(t, ok2, "second newest file should remain")
}

func TestApplyRetention_EmptyStorage(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()
	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(5)}

	toDelete, err := rm.ApplyRetention(ctx, mockClient, "prefix/", policy)

	require.NoError(t, err)
	assert.Empty(t, toDelete)
}

func TestApplyRetention_NilPolicy(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()
	mockClient.AddObject("cluster/db/20260120-140000.sql.gz", []byte("data"), time.Now())

	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", nil)

	require.NoError(t, err)
	assert.Empty(t, toDelete)
	assert.Equal(t, 1, mockClient.Count(), "file should remain untouched")
}

func TestApplyRetention_ListError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()
	mockClient.ListError = fmt.Errorf("S3 unavailable")

	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(2)}
	_, err := rm.ApplyRetention(ctx, mockClient, "prefix/", policy)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to list backup files")
}

func TestDeleteFiles_WithErrors(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()
	mockClient.AddObject("file1.sql.gz", []byte("data"), time.Now())
	mockClient.AddObject("file2.sql.gz", []byte("data"), time.Now())
	mockClient.DeleteError = fmt.Errorf("delete failed")

	// Should continue deleting despite errors
	err := rm.DeleteFiles(ctx, mockClient, []string{"file1.sql.gz", "file2.sql.gz"})

	// DeleteFiles logs warnings but returns nil (best-effort deletion)
	require.NoError(t, err)
}

func TestApplyRetention_FallbackToLastModified(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()

	// Files WITHOUT timestamp in name - should use LastModified
	now := time.Now()
	mockClient.AddObject("cluster/db/latest.sql.gz", []byte("newest"), now.Add(-1*time.Hour))
	mockClient.AddObject("cluster/db/backup.sql.gz", []byte("older"), now.Add(-2*time.Hour))
	mockClient.AddObject("cluster/db/old.sql.gz", []byte("oldest"), now.Add(-3*time.Hour))

	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(2)}
	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", policy)

	require.NoError(t, err)
	assert.Len(t, toDelete, 1, "should mark 1 file for deletion using LastModified fallback")
}

func TestMockClient_UploadWithTags(t *testing.T) {
	mockClient := storage.NewMockClient()
	ctx := context.Background()

	tags := &storage.ObjectTags{
		Database:  "mydb",
		Cluster:   "mycluster",
		Namespace: "default",
		Timestamp: "20260120-140000",
		CreatedBy: "dbtether",
	}

	err := mockClient.UploadWithTags(ctx, "backup.sql.gz", bytes.NewReader([]byte("data")), tags)
	require.NoError(t, err)

	// Verify tags were stored
	storedTags, ok := mockClient.GetTags("backup.sql.gz")
	require.True(t, ok)
	assert.Equal(t, "mydb", storedTags.Database)
	assert.Equal(t, "mycluster", storedTags.Cluster)
}

func TestMockClient_Download(t *testing.T) {
	mockClient := storage.NewMockClient()
	ctx := context.Background()

	// Upload data
	err := mockClient.Upload(ctx, "test.sql.gz", bytes.NewReader([]byte("hello world")))
	require.NoError(t, err)

	// Download and verify
	reader, err := mockClient.Download(ctx, "test.sql.gz")
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(data))
}

func TestMockClient_NotFound(t *testing.T) {
	mockClient := storage.NewMockClient()
	ctx := context.Background()

	_, err := mockClient.Download(ctx, "nonexistent.sql.gz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestMockClient_Exists(t *testing.T) {
	mockClient := storage.NewMockClient()
	ctx := context.Background()

	mockClient.AddObject("exists.sql.gz", []byte("data"), time.Now())

	exists, err := mockClient.Exists(ctx, "exists.sql.gz")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = mockClient.Exists(ctx, "not-exists.sql.gz")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestMockClient_List(t *testing.T) {
	mockClient := storage.NewMockClient()
	ctx := context.Background()

	mockClient.AddObject("prefix/file1.sql.gz", []byte("data"), time.Now())
	mockClient.AddObject("prefix/file2.sql.gz", []byte("data"), time.Now())
	mockClient.AddObject("other/file3.sql.gz", []byte("data"), time.Now())

	objects, err := mockClient.List(ctx, "prefix/")
	require.NoError(t, err)
	assert.Len(t, objects, 2)

	// List all
	objects, err = mockClient.List(ctx, "")
	require.NoError(t, err)
	assert.Len(t, objects, 3)
}

func TestMockClient_Clear(t *testing.T) {
	mockClient := storage.NewMockClient()
	mockClient.AddObject("file1.sql.gz", []byte("data"), time.Now())
	mockClient.AddObject("file2.sql.gz", []byte("data"), time.Now())

	assert.Equal(t, 2, mockClient.Count())

	mockClient.Clear()

	assert.Equal(t, 0, mockClient.Count())
}

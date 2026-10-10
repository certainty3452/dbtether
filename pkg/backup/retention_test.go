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

	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	files := []BackupFile{
		{Key: "backup-20260120-100000.sql.gz", Timestamp: now.Add(-2 * time.Hour)},
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-10 * time.Hour)},
		{Key: "backup-20260119-020000.sql.gz", Timestamp: now.Add(-34 * time.Hour)},
		{Key: "backup-20260118-020000.sql.gz", Timestamp: now.Add(-58 * time.Hour)},
		{Key: "backup-20260117-020000.sql.gz", Timestamp: now.Add(-82 * time.Hour)},
		{Key: "backup-20260116-020000.sql.gz", Timestamp: now.Add(-106 * time.Hour)},
		{Key: "backup-20260115-020000.sql.gz", Timestamp: now.Add(-130 * time.Hour)},
		{Key: "backup-20260114-020000.sql.gz", Timestamp: now.Add(-154 * time.Hour)},
		{Key: "backup-20260113-020000.sql.gz", Timestamp: now.Add(-178 * time.Hour)},
		{Key: "backup-20260112-020000.sql.gz", Timestamp: now.Add(-202 * time.Hour)},
		{Key: "backup-20260101-020000.sql.gz", Timestamp: now.Add(-19 * 24 * time.Hour)},
		{Key: "backup-20251220-020000.sql.gz", Timestamp: now.Add(-31 * 24 * time.Hour)},
		{Key: "backup-20251201-020000.sql.gz", Timestamp: now.Add(-50 * 24 * time.Hour)},
	}

	t.Run("keepLast=3", func(t *testing.T) {
		policy := &dbtether.RetentionPolicy{KeepLast: intPtr(3)}
		keep := KeepSet(files, policy)

		assert.True(t, keep["backup-20260120-100000.sql.gz"])
		assert.True(t, keep["backup-20260120-020000.sql.gz"])
		assert.True(t, keep["backup-20260119-020000.sql.gz"])
		assert.False(t, keep["backup-20260118-020000.sql.gz"])
	})

	t.Run("keepDaily=5", func(t *testing.T) {
		policy := &dbtether.RetentionPolicy{KeepDaily: intPtr(5)}
		keep := KeepSet(files, policy)

		assert.True(t, keep["backup-20260120-100000.sql.gz"])
		assert.False(t, keep["backup-20260120-020000.sql.gz"])
		assert.True(t, keep["backup-20260119-020000.sql.gz"])
		assert.True(t, keep["backup-20260118-020000.sql.gz"])
		assert.True(t, keep["backup-20260117-020000.sql.gz"])
		assert.True(t, keep["backup-20260116-020000.sql.gz"])
		assert.False(t, keep["backup-20260115-020000.sql.gz"])
	})

	t.Run("combined keepLast=2 keepDaily=7", func(t *testing.T) {
		policy := &dbtether.RetentionPolicy{
			KeepLast:  intPtr(2),
			KeepDaily: intPtr(7),
		}
		keep := KeepSet(files, policy)

		assert.True(t, keep["backup-20260120-100000.sql.gz"])
		assert.True(t, keep["backup-20260120-020000.sql.gz"])

		// Seven days back from the newest file (Jan 20 10:00): 20260113-020000 falls outside
		assert.True(t, keep["backup-20260119-020000.sql.gz"])
		assert.True(t, keep["backup-20260118-020000.sql.gz"])
		assert.True(t, keep["backup-20260117-020000.sql.gz"])
		assert.True(t, keep["backup-20260116-020000.sql.gz"])
		assert.True(t, keep["backup-20260115-020000.sql.gz"])
		assert.True(t, keep["backup-20260114-020000.sql.gz"])
		assert.False(t, keep["backup-20260113-020000.sql.gz"])

		assert.False(t, keep["backup-20260112-020000.sql.gz"])
	})
}

func TestRetentionPolicy_AllNil(t *testing.T) {

	files := []BackupFile{
		{Key: "backup-20260120-100000.sql.gz", Timestamp: time.Now()},
	}

	keep := KeepSet(files, &dbtether.RetentionPolicy{})
	assert.Empty(t, keep)
}

func TestCalculateKeepSet_Weekly(t *testing.T) {

	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	files := []BackupFile{
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-10 * time.Hour)},
		{Key: "backup-20260113-020000.sql.gz", Timestamp: now.Add(-7*24*time.Hour - 10*time.Hour)},
		{Key: "backup-20260106-020000.sql.gz", Timestamp: now.Add(-14*24*time.Hour - 10*time.Hour)},
		{Key: "backup-20251230-020000.sql.gz", Timestamp: now.Add(-21*24*time.Hour - 10*time.Hour)},
		{Key: "backup-20251223-020000.sql.gz", Timestamp: now.Add(-28*24*time.Hour - 10*time.Hour)},
	}

	policy := &dbtether.RetentionPolicy{KeepWeekly: intPtr(3)}
	keep := KeepSet(files, policy)

	assert.True(t, keep["backup-20260120-020000.sql.gz"], "week 0 should be kept")
	assert.True(t, keep["backup-20260113-020000.sql.gz"], "week 1 should be kept")
	assert.True(t, keep["backup-20260106-020000.sql.gz"], "week 2 should be kept")
	assert.False(t, keep["backup-20251230-020000.sql.gz"], "week 3 should not be kept")
	assert.False(t, keep["backup-20251223-020000.sql.gz"], "week 4 should not be kept")
}

func TestCalculateKeepSet_Monthly(t *testing.T) {

	files := []BackupFile{
		{Key: "backup-20260115-020000.sql.gz", Timestamp: time.Date(2026, 1, 15, 2, 0, 0, 0, time.UTC)},
		{Key: "backup-20251215-020000.sql.gz", Timestamp: time.Date(2025, 12, 15, 2, 0, 0, 0, time.UTC)},
		{Key: "backup-20251115-020000.sql.gz", Timestamp: time.Date(2025, 11, 15, 2, 0, 0, 0, time.UTC)},
		{Key: "backup-20251015-020000.sql.gz", Timestamp: time.Date(2025, 10, 15, 2, 0, 0, 0, time.UTC)},
		{Key: "backup-20250915-020000.sql.gz", Timestamp: time.Date(2025, 9, 15, 2, 0, 0, 0, time.UTC)},
		{Key: "backup-20250815-020000.sql.gz", Timestamp: time.Date(2025, 8, 15, 2, 0, 0, 0, time.UTC)},
	}

	policy := &dbtether.RetentionPolicy{KeepMonthly: intPtr(4)}
	keep := KeepSet(files, policy)

	// Four months back from the newest file (Jan 15 02:00): Sep 15 02:00 sits on the excluded boundary
	assert.True(t, keep["backup-20260115-020000.sql.gz"], "Jan should be kept")
	assert.True(t, keep["backup-20251215-020000.sql.gz"], "Dec should be kept")
	assert.True(t, keep["backup-20251115-020000.sql.gz"], "Nov should be kept")
	assert.True(t, keep["backup-20251015-020000.sql.gz"], "Oct should be kept")
	assert.False(t, keep["backup-20250915-020000.sql.gz"], "Sep should not be kept")
	assert.False(t, keep["backup-20250815-020000.sql.gz"], "Aug should not be kept")
}

func TestCalculateKeepSet_MultiplePerDay(t *testing.T) {

	now := time.Date(2026, 1, 20, 18, 0, 0, 0, time.UTC)
	files := []BackupFile{
		{Key: "backup-20260120-160000.sql.gz", Timestamp: now.Add(-2 * time.Hour)},
		{Key: "backup-20260120-140000.sql.gz", Timestamp: now.Add(-4 * time.Hour)},
		{Key: "backup-20260120-120000.sql.gz", Timestamp: now.Add(-6 * time.Hour)},
		{Key: "backup-20260120-100000.sql.gz", Timestamp: now.Add(-8 * time.Hour)},
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-16 * time.Hour)},
	}

	policy := &dbtether.RetentionPolicy{KeepDaily: intPtr(1)}
	keep := KeepSet(files, policy)

	assert.True(t, keep["backup-20260120-160000.sql.gz"], "newest should be kept")
	assert.False(t, keep["backup-20260120-140000.sql.gz"])
	assert.False(t, keep["backup-20260120-120000.sql.gz"])
	assert.False(t, keep["backup-20260120-100000.sql.gz"])
	assert.False(t, keep["backup-20260120-020000.sql.gz"])
}

func TestCalculateKeepSet_EmptyFiles(t *testing.T) {

	files := []BackupFile{}
	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(10)}

	keep := KeepSet(files, policy)
	assert.Empty(t, keep)
}

func TestCalculateKeepSet_AllPoliciesCombined(t *testing.T) {

	// Jan 13 and Jan 18 share ISO week 2026-W03; the newer Jan 18 represents it.
	now := time.Date(2026, 1, 20, 12, 0, 0, 0, time.UTC)
	files := []BackupFile{
		{Key: "backup-20260120-100000.sql.gz", Timestamp: now.Add(-2 * time.Hour)},
		{Key: "backup-20260120-020000.sql.gz", Timestamp: now.Add(-10 * time.Hour)},
		{Key: "backup-20260119-020000.sql.gz", Timestamp: now.Add(-34 * time.Hour)},
		{Key: "backup-20260118-020000.sql.gz", Timestamp: now.Add(-58 * time.Hour)},
		{Key: "backup-20260113-020000.sql.gz", Timestamp: now.Add(-7*24*time.Hour - 10*time.Hour)}, // W03, but Jan 18 also W03
		{Key: "backup-20260107-120000.sql.gz", Timestamp: now.Add(-13 * 24 * time.Hour)},
		{Key: "backup-20251215-020000.sql.gz", Timestamp: time.Date(2025, 12, 15, 2, 0, 0, 0, time.UTC)},
	}

	policy := &dbtether.RetentionPolicy{
		KeepLast:    intPtr(2),
		KeepDaily:   intPtr(3),
		KeepWeekly:  intPtr(4),
		KeepMonthly: intPtr(2),
	}
	keep := KeepSet(files, policy)

	assert.True(t, keep["backup-20260120-100000.sql.gz"], "keepLast: newest should be kept")
	assert.True(t, keep["backup-20260120-020000.sql.gz"], "keepLast: second newest should be kept")

	assert.True(t, keep["backup-20260119-020000.sql.gz"], "keepDaily: Jan 19 should be kept")
	assert.True(t, keep["backup-20260118-020000.sql.gz"], "keepDaily: Jan 18 should be kept")

	// Jan 13 is skipped: Jan 18 already represents W03.
	assert.True(t, keep["backup-20260107-120000.sql.gz"], "keepWeekly: Jan 7 (W02) should be kept")
	assert.False(t, keep["backup-20260113-020000.sql.gz"], "keepWeekly: Jan 13 skipped (same week as Jan 18)")

	assert.True(t, keep["backup-20251215-020000.sql.gz"], "keepMonthly: Dec 15 should be kept")
}

func TestParseTimestampFromKey_Fallback(t *testing.T) {

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

func TestApplyRetention_WithMockClient(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()

	now := time.Now()
	mockClient.AddObject("cluster/db/20260120-140000.sql.gz", []byte("backup1"), now.Add(-1*time.Hour))
	mockClient.AddObject("cluster/db/20260120-130000.sql.gz", []byte("backup2"), now.Add(-2*time.Hour))
	mockClient.AddObject("cluster/db/20260120-120000.sql.gz", []byte("backup3"), now.Add(-3*time.Hour))
	mockClient.AddObject("cluster/db/20260120-110000.sql.gz", []byte("backup4"), now.Add(-4*time.Hour))
	mockClient.AddObject("cluster/db/20260120-100000.sql.gz", []byte("backup5"), now.Add(-5*time.Hour))

	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(2)}
	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", policy, nil)

	require.NoError(t, err)
	assert.Len(t, toDelete, 3, "should mark 3 files for deletion")

	err = rm.DeleteFiles(ctx, mockClient, toDelete)
	require.NoError(t, err)

	assert.Equal(t, 2, mockClient.Count(), "should have 2 files remaining")

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

	toDelete, err := rm.ApplyRetention(ctx, mockClient, "prefix/", policy, nil)

	require.NoError(t, err)
	assert.Empty(t, toDelete)
}

func TestApplyRetention_NilPolicy(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()
	mockClient.AddObject("cluster/db/20260120-140000.sql.gz", []byte("data"), time.Now())

	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", nil, nil)

	require.NoError(t, err)
	assert.Empty(t, toDelete)
	assert.Equal(t, 1, mockClient.Count(), "file should remain untouched")
}

func TestApplyRetention_NoActiveRule(t *testing.T) {
	tests := []struct {
		name       string
		policy     *dbtether.RetentionPolicy
		wantDelete int
	}{
		{
			name:       "empty policy deletes nothing",
			policy:     &dbtether.RetentionPolicy{},
			wantDelete: 0,
		},
		{
			name: "all-zero policy deletes nothing",
			policy: &dbtether.RetentionPolicy{
				KeepLast:    intPtr(0),
				KeepDaily:   intPtr(0),
				KeepWeekly:  intPtr(0),
				KeepMonthly: intPtr(0),
			},
			wantDelete: 0,
		},
		{
			name:       "one positive rule still works",
			policy:     &dbtether.RetentionPolicy{KeepLast: intPtr(2)},
			wantDelete: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, _ := zap.NewDevelopment()
			rm := NewRetentionManager(logger.Sugar())
			ctx := context.Background()

			mockClient := storage.NewMockClient()
			now := time.Now()
			mockClient.AddObject("cluster/db/20260120-140000.sql.gz", []byte("backup1"), now.Add(-1*time.Hour))
			mockClient.AddObject("cluster/db/20260120-130000.sql.gz", []byte("backup2"), now.Add(-2*time.Hour))
			mockClient.AddObject("cluster/db/20260120-120000.sql.gz", []byte("backup3"), now.Add(-3*time.Hour))
			mockClient.AddObject("cluster/db/20260120-110000.sql.gz", []byte("backup4"), now.Add(-4*time.Hour))
			mockClient.AddObject("cluster/db/20260120-100000.sql.gz", []byte("backup5"), now.Add(-5*time.Hour))

			toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", tt.policy, nil)

			require.NoError(t, err)
			assert.Len(t, toDelete, tt.wantDelete)
		})
	}
}

func TestApplyRetention_ListError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()
	mockClient.ListError = fmt.Errorf("S3 unavailable")

	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(2)}
	_, err := rm.ApplyRetention(ctx, mockClient, "prefix/", policy, nil)

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

	err := rm.DeleteFiles(ctx, mockClient, []string{"file1.sql.gz", "file2.sql.gz"})

	// DeleteFiles logs warnings but returns nil (best-effort deletion)
	require.NoError(t, err)
}

func TestApplyRetention_FallbackToLastModified(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()

	now := time.Now()
	mockClient.AddObject("cluster/db/latest.sql.gz", []byte("newest"), now.Add(-1*time.Hour))
	mockClient.AddObject("cluster/db/backup.sql.gz", []byte("older"), now.Add(-2*time.Hour))
	mockClient.AddObject("cluster/db/old.sql.gz", []byte("oldest"), now.Add(-3*time.Hour))

	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(2)}
	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", policy, nil)

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

	storedTags, ok := mockClient.GetTags("backup.sql.gz")
	require.True(t, ok)
	assert.Equal(t, "mydb", storedTags.Database)
	assert.Equal(t, "mycluster", storedTags.Cluster)
}

func TestMockClient_Download(t *testing.T) {
	mockClient := storage.NewMockClient()
	ctx := context.Background()

	err := mockClient.Upload(ctx, "test.sql.gz", bytes.NewReader([]byte("hello world")))
	require.NoError(t, err)

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

func TestFilenameTemplateToRegex(t *testing.T) {
	tests := []struct {
		name     string
		template string
		match    []string
		noMatch  []string
	}{
		{
			name:     "default template",
			template: "{{ .Timestamp }}.sql.gz",
			match: []string{
				"cluster/db/20260424-030101.sql.gz",
				"microservices/ingestion_dev/20260331-030202.sql.gz",
			},
			noMatch: []string{
				"cluster/db/pre-migration-uploaded-by-20260422-120000-abc12345.sql.gz",
				"cluster/db/manual-backup.sql.gz",
			},
		},
		{
			name:     "template with runID",
			template: "{{ .Timestamp }}-{{ .RunID }}.sql.gz",
			match: []string{
				"cluster/db/20260424-030101-abc12345.sql.gz",
			},
			noMatch: []string{
				"cluster/db/20260424-030101.sql.gz",
				"cluster/db/pre-migration-20260424-030101-abc12345.sql.gz",
			},
		},
		{
			name:     "custom prefix template",
			template: "pre-migration-uploaded-by-{{ .Timestamp }}-{{ .RunID }}.sql.gz",
			match: []string{
				"cluster/db/pre-migration-uploaded-by-20260422-120000-abc12345.sql.gz",
			},
			noMatch: []string{
				"cluster/db/20260424-030101.sql.gz",
			},
		},
		{
			name:     "template with database name",
			template: "{{ .DatabaseName }}-{{ .Timestamp }}.sql.gz",
			match: []string{
				"cluster/db/ingestion_dev-20260424-030101.sql.gz",
			},
			noMatch: []string{
				"cluster/db/20260424-030101.sql.gz",
			},
		},
		{
			name:     "empty template returns nil",
			template: "",
			match:    nil,
			noMatch:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re, err := FilenameTemplateToRegex(tt.template)
			if tt.template == "" {
				assert.NoError(t, err)
				assert.Nil(t, re)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, re)

			for _, s := range tt.match {
				assert.True(t, re.MatchString(s), "expected %q to match pattern %q", s, re.String())
			}
			for _, s := range tt.noMatch {
				assert.False(t, re.MatchString(s), "expected %q NOT to match pattern %q", s, re.String())
			}
		})
	}
}

func TestApplyRetention_FilenameFilter(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()

	now := time.Now()
	mockClient.AddObject("cluster/db/20260424-030101.sql.gz", []byte("newest"), now.Add(-1*time.Hour))
	mockClient.AddObject("cluster/db/20260419-030050.sql.gz", []byte("older"), now.Add(-5*24*time.Hour))
	mockClient.AddObject("cluster/db/20260331-030202.sql.gz", []byte("oldest"), now.Add(-24*24*time.Hour))
	mockClient.AddObject("cluster/db/pre-migration-uploaded-by-20260422-120000-abc12345.sql.gz", []byte("manual"), now.Add(-2*24*time.Hour))

	filter, err := FilenameTemplateToRegex("{{ .Timestamp }}.sql.gz")
	require.NoError(t, err)

	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(1)}
	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", policy, filter)

	require.NoError(t, err)
	assert.Len(t, toDelete, 2, "should delete 2 old scheduled backups")

	for _, key := range toDelete {
		assert.NotContains(t, key, "pre-migration", "manual backup must not be deleted")
	}

	assert.NotContains(t, toDelete, "cluster/db/20260424-030101.sql.gz")
}

func TestApplyRetention_NilFilterDeletesAll(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	rm := NewRetentionManager(logger.Sugar())
	ctx := context.Background()

	mockClient := storage.NewMockClient()

	now := time.Now()
	mockClient.AddObject("cluster/db/20260424-030101.sql.gz", []byte("scheduled"), now.Add(-1*time.Hour))
	mockClient.AddObject("cluster/db/pre-migration-20260422-120000-abc12345.sql.gz", []byte("manual"), now.Add(-2*24*time.Hour))

	policy := &dbtether.RetentionPolicy{KeepLast: intPtr(1)}
	toDelete, err := rm.ApplyRetention(ctx, mockClient, "cluster/db/", policy, nil)

	require.NoError(t, err)
	assert.Len(t, toDelete, 1, "without filter, manual backup is also subject to retention")
}

func TestApplyRetention_OnlyDirectChildrenOfPrefix(t *testing.T) {
	rm := NewRetentionManager(zap.NewNop().Sugar())
	mockClient := storage.NewMockClient()
	mockClient.AddObject("cluster/db/20260424-030101.sql.gz", []byte("newest"), time.Time{})
	mockClient.AddObject("cluster/db/20260419-030050.sql.gz", []byte("older"), time.Time{})
	mockClient.AddObject("cluster/db/archive/20260101-030000.sql.gz", []byte("nested"), time.Time{})

	toDelete, err := rm.ApplyRetention(context.Background(), mockClient, "cluster/db/", &dbtether.RetentionPolicy{KeepLast: intPtr(1)}, nil)

	require.NoError(t, err)
	assert.Equal(t, []string{"cluster/db/20260419-030050.sql.gz"}, toDelete)
}

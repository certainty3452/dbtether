package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"

	"go.uber.org/zap"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
)

// BackupFile represents a backup file in storage
type BackupFile struct {
	Key       string    // Full S3 key
	Timestamp time.Time // Parsed from filename
	Size      int64
}

// RetentionManager handles backup retention policy
type RetentionManager struct {
	Log *zap.SugaredLogger
}

// NewRetentionManager creates a new RetentionManager
func NewRetentionManager(log *zap.SugaredLogger) *RetentionManager {
	return &RetentionManager{Log: log}
}

// ApplyRetention applies retention policy and returns files to delete
func (m *RetentionManager) ApplyRetention(
	ctx context.Context,
	storageClient storage.StorageClient,
	prefix string,
	policy *dbtether.RetentionPolicy,
) ([]string, error) {
	if policy == nil {
		return nil, nil
	}

	// List all backup files
	files, err := m.listBackupFiles(ctx, storageClient, prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list backup files: %w", err)
	}

	if len(files) == 0 {
		return nil, nil
	}

	// Sort by timestamp descending (newest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].Timestamp.After(files[j].Timestamp)
	})

	// Calculate which files to keep
	keepSet := m.calculateKeepSet(files, policy)

	// Find files to delete
	var toDelete []string
	for _, f := range files {
		if !keepSet[f.Key] {
			toDelete = append(toDelete, f.Key)
		}
	}

	m.Log.Infow("retention policy applied",
		"totalFiles", len(files),
		"keeping", len(keepSet),
		"deleting", len(toDelete),
	)

	return toDelete, nil
}

// DeleteFiles deletes the specified files from storage
func (m *RetentionManager) DeleteFiles(ctx context.Context, storageClient storage.StorageClient, keys []string) error {
	for _, key := range keys {
		if err := storageClient.Delete(ctx, key); err != nil {
			m.Log.Warnw("failed to delete backup file", "key", key, "error", err)
			// Continue with other deletions
		} else {
			m.Log.Infow("deleted backup file", "key", key)
		}
	}
	return nil
}

func (m *RetentionManager) listBackupFiles(ctx context.Context, storageClient storage.StorageClient, prefix string) ([]BackupFile, error) {
	objects, err := storageClient.List(ctx, prefix)
	if err != nil {
		return nil, err
	}

	var files []BackupFile
	for _, obj := range objects {
		// Try to parse timestamp from filename first
		timestamp, err := parseTimestampFromKey(obj.Key)
		if err != nil {
			// Fallback to storage LastModified if filename doesn't contain timestamp
			if obj.LastModified.IsZero() {
				m.Log.Debugw("skipping file without timestamp", "key", obj.Key)
				continue
			}
			m.Log.Debugw("using S3 LastModified as timestamp fallback", "key", obj.Key)
			timestamp = obj.LastModified
		}

		files = append(files, BackupFile{
			Key:       obj.Key,
			Timestamp: timestamp,
			Size:      obj.Size,
		})
	}

	return files, nil
}

func (m *RetentionManager) calculateKeepSet(files []BackupFile, policy *dbtether.RetentionPolicy) map[string]bool {
	keep := make(map[string]bool)
	now := time.Now()

	m.applyKeepLast(files, policy, keep)
	m.applyKeepDaily(files, policy, keep, now)
	m.applyKeepWeekly(files, policy, keep, now)
	m.applyKeepMonthly(files, policy, keep, now)

	return keep
}

// applyKeepLast keeps the N most recent backups
func (m *RetentionManager) applyKeepLast(files []BackupFile, policy *dbtether.RetentionPolicy, keep map[string]bool) {
	if policy.KeepLast == nil || *policy.KeepLast <= 0 {
		return
	}
	for i := 0; i < *policy.KeepLast && i < len(files); i++ {
		keep[files[i].Key] = true
	}
}

// applyKeepDaily keeps first backup of each day for N days
func (m *RetentionManager) applyKeepDaily(files []BackupFile, policy *dbtether.RetentionPolicy, keep map[string]bool, now time.Time) {
	if policy.KeepDaily == nil || *policy.KeepDaily <= 0 {
		return
	}
	cutoff := now.AddDate(0, 0, -*policy.KeepDaily)
	seenDays := make(map[string]bool)

	for _, f := range files {
		if f.Timestamp.Before(cutoff) {
			continue
		}
		dayKey := f.Timestamp.Format("2006-01-02")
		if !seenDays[dayKey] {
			seenDays[dayKey] = true
			keep[f.Key] = true
		}
	}
}

// applyKeepWeekly keeps first backup of each week for N weeks
func (m *RetentionManager) applyKeepWeekly(files []BackupFile, policy *dbtether.RetentionPolicy, keep map[string]bool, now time.Time) {
	if policy.KeepWeekly == nil || *policy.KeepWeekly <= 0 {
		return
	}
	cutoff := now.AddDate(0, 0, -*policy.KeepWeekly*7)
	seenWeeks := make(map[string]bool)

	for _, f := range files {
		if f.Timestamp.Before(cutoff) {
			continue
		}
		year, week := f.Timestamp.ISOWeek()
		weekKey := fmt.Sprintf("%d-W%02d", year, week)
		if !seenWeeks[weekKey] {
			seenWeeks[weekKey] = true
			keep[f.Key] = true
		}
	}
}

// applyKeepMonthly keeps first backup of each month for N months
func (m *RetentionManager) applyKeepMonthly(files []BackupFile, policy *dbtether.RetentionPolicy, keep map[string]bool, now time.Time) {
	if policy.KeepMonthly == nil || *policy.KeepMonthly <= 0 {
		return
	}
	cutoff := now.AddDate(0, -*policy.KeepMonthly, 0)
	seenMonths := make(map[string]bool)

	for _, f := range files {
		if f.Timestamp.Before(cutoff) {
			continue
		}
		monthKey := f.Timestamp.Format("2006-01")
		if !seenMonths[monthKey] {
			seenMonths[monthKey] = true
			keep[f.Key] = true
		}
	}
}

// parseTimestampFromKey extracts timestamp from backup filename
// Expected formats:
// - YYYYMMDD-HHMMSS.sql.gz
// - YYYYMMDD-HHMMSS-runid.sql.gz
// - prefix/path/YYYYMMDD-HHMMSS.sql.gz
var timestampRegex = regexp.MustCompile(`(\d{8}-\d{6})`)

func parseTimestampFromKey(key string) (time.Time, error) {
	matches := timestampRegex.FindStringSubmatch(key)
	if len(matches) < 2 {
		return time.Time{}, fmt.Errorf("no timestamp found in key: %s", key)
	}

	return time.Parse("20060102-150405", matches[1])
}

package backup

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
)

type BackupFile struct {
	Key       string
	Timestamp time.Time
	Size      int64
}

type RetentionManager struct {
	Log *zap.SugaredLogger
}

func NewRetentionManager(log *zap.SugaredLogger) *RetentionManager {
	return &RetentionManager{Log: log}
}

// Objects that are not direct children of prefix or fail filenameFilter are never deleted.
func (m *RetentionManager) ApplyRetention(
	ctx context.Context,
	storageClient storage.StorageClient,
	prefix string,
	policy *dbtether.RetentionPolicy,
	filenameFilter *regexp.Regexp,
) ([]string, error) {
	if policy == nil {
		return nil, nil
	}

	// No active rule would mark every backup for deletion; old objects can predate the CEL rule.
	if !HasActiveRule(policy) {
		m.Log.Warnw("retention policy has no active rule; nothing will be deleted",
			"prefix", prefix,
		)
		return nil, nil
	}

	files, err := m.listBackupFiles(ctx, storageClient, prefix, filenameFilter)
	if err != nil {
		return nil, fmt.Errorf("failed to list backup files: %w", err)
	}

	if len(files) == 0 {
		return nil, nil
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].Timestamp.After(files[j].Timestamp)
	})

	keepSet := KeepSet(files, policy)

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

func (m *RetentionManager) DeleteFiles(ctx context.Context, storageClient storage.StorageClient, keys []string) error {
	for _, key := range keys {
		if err := storageClient.Delete(ctx, key); err != nil {
			m.Log.Warnw("failed to delete backup file", "key", key, "error", err)
		} else {
			m.Log.Infow("deleted backup file", "key", key)
		}
	}
	return nil
}

func (m *RetentionManager) listBackupFiles(ctx context.Context, storageClient storage.StorageClient, prefix string, filenameFilter *regexp.Regexp) ([]BackupFile, error) {
	objects, err := storageClient.List(ctx, prefix)
	if err != nil {
		return nil, err
	}

	files := make([]BackupFile, 0, len(objects))
	for _, obj := range objects {
		name, ok := strings.CutPrefix(obj.Key, prefix)
		if !ok || name == "" || strings.Contains(name, "/") {
			continue
		}
		if filenameFilter != nil && !filenameFilter.MatchString(name) {
			m.Log.Debugw("skipping file not matching schedule pattern", "key", obj.Key)
			continue
		}

		timestamp, err := parseTimestampFromKey(name)
		if err != nil {
			if obj.LastModified.IsZero() {
				m.Log.Debugw("skipping file without timestamp", "key", obj.Key)
				continue
			}
			m.Log.Debugw("using S3 LastModified as timestamp fallback", "key", obj.Key)
			timestamp = obj.LastModified
		}

		files = append(files, BackupFile{
			Key:       obj.Key,
			Timestamp: timestamp.UTC(),
			Size:      obj.Size,
		})
	}

	return files, nil
}

// Windows end at the newest file, not the wall clock, so a stalled schedule keeps its last backups.
func KeepSet(files []BackupFile, policy *dbtether.RetentionPolicy) map[string]bool {
	keep := make(map[string]bool)
	if len(files) == 0 || !HasActiveRule(policy) {
		return keep
	}
	newest := files[0].Timestamp
	keep[files[0].Key] = true

	if policy.KeepLast != nil {
		for i := 0; i < *policy.KeepLast && i < len(files); i++ {
			keep[files[i].Key] = true
		}
	}
	keepNewestPerPeriod(files, policy.KeepDaily, keep, func(n int) time.Time { return newest.AddDate(0, 0, -n) }, dayKey)
	keepNewestPerPeriod(files, policy.KeepWeekly, keep, func(n int) time.Time { return newest.AddDate(0, 0, -7*n) }, isoWeekKey)
	keepNewestPerPeriod(files, policy.KeepMonthly, keep, func(n int) time.Time { return newest.AddDate(0, -n, 0) }, monthKey)

	return keep
}

func keepNewestPerPeriod(
	files []BackupFile,
	count *int,
	keep map[string]bool,
	cutoffFor func(int) time.Time,
	periodKey func(time.Time) string,
) {
	if count == nil || *count <= 0 {
		return
	}
	cutoff := cutoffFor(*count)
	seen := make(map[string]bool)
	for _, f := range files {
		if !f.Timestamp.After(cutoff) {
			continue
		}
		key := periodKey(f.Timestamp)
		if !seen[key] {
			seen[key] = true
			keep[f.Key] = true
		}
	}
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func isoWeekKey(t time.Time) string {
	year, week := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

func monthKey(t time.Time) string { return t.Format("2006-01") }

// Non-matching files are excluded from retention, protecting manually named backups.
func FilenameTemplateToRegex(tmpl string) (*regexp.Regexp, error) {
	if tmpl == "" {
		return nil, nil
	}

	replacements := map[string]string{
		"{{ .Timestamp }}":    `\d{8}-\d{6}`,
		"{{.Timestamp}}":      `\d{8}-\d{6}`,
		"{{ .RunID }}":        `[a-z0-9]{8}`,
		"{{.RunID}}":          `[a-z0-9]{8}`,
		"{{ .DatabaseName }}": `[^/]+`,
		"{{.DatabaseName}}":   `[^/]+`,
		"{{ .ClusterName }}":  `[^/]+`,
		"{{.ClusterName}}":    `[^/]+`,
		"{{ .Year }}":         `\d{4}`,
		"{{.Year}}":           `\d{4}`,
		"{{ .Month }}":        `\d{2}`,
		"{{.Month}}":          `\d{2}`,
		"{{ .Day }}":          `\d{2}`,
		"{{.Day}}":            `\d{2}`,
	}

	pattern := regexp.QuoteMeta(tmpl)
	for tmplVar, regex := range replacements {
		pattern = strings.ReplaceAll(pattern, regexp.QuoteMeta(tmplVar), regex)
	}

	return regexp.Compile("(?:^|/)" + pattern + "$")
}

var timestampRegex = regexp.MustCompile(`(\d{8}-\d{6})`)

func parseTimestampFromKey(key string) (time.Time, error) {
	matches := timestampRegex.FindStringSubmatch(key)
	if len(matches) < 2 {
		return time.Time{}, fmt.Errorf("no timestamp found in key: %s", key)
	}

	return time.Parse("20060102-150405", matches[1])
}

// Without an active rule retention must delete nothing rather than everything.
func HasActiveRule(policy *dbtether.RetentionPolicy) bool {
	return (policy.KeepLast != nil && *policy.KeepLast > 0) ||
		(policy.KeepDaily != nil && *policy.KeepDaily > 0) ||
		(policy.KeepWeekly != nil && *policy.KeepWeekly > 0) ||
		(policy.KeepMonthly != nil && *policy.KeepMonthly > 0)
}

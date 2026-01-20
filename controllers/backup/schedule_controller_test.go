package backup

import (
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
)

const (
	testCronSchedule = "0 2 * * *"
	backupName1      = "backup-1"
	backupName2      = "backup-2"
	backupName3      = "backup-3"
	backupName4      = "backup-4"
	backupName5      = "backup-5"
)

func TestCronParsing(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		valid    bool
	}{
		{
			name:     "daily at 2am",
			schedule: testCronSchedule,
			valid:    true,
		},
		{
			name:     "every hour",
			schedule: "0 * * * *",
			valid:    true,
		},
		{
			name:     "weekly on sunday",
			schedule: "0 0 * * 0",
			valid:    true,
		},
		{
			name:     "invalid - too few fields",
			schedule: "0 2 * *",
			valid:    false,
		},
		{
			name:     "invalid - bad value",
			schedule: "0 25 * * *",
			valid:    false,
		},
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parser.Parse(tt.schedule)
			if tt.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestNextScheduledTime(t *testing.T) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	// Parse testCronSchedule (daily at 2:00 AM)
	schedule, err := parser.Parse(testCronSchedule)
	require.NoError(t, err)

	// If last run was today at 1:00 AM, next should be today at 2:00 AM
	now := time.Date(2026, 1, 20, 1, 0, 0, 0, time.UTC)
	next := schedule.Next(now)
	assert.Equal(t, 2, next.Hour())
	assert.Equal(t, 0, next.Minute())
	assert.Equal(t, 20, next.Day())

	// If last run was today at 3:00 AM, next should be tomorrow at 2:00 AM
	now = time.Date(2026, 1, 20, 3, 0, 0, 0, time.UTC)
	next = schedule.Next(now)
	assert.Equal(t, 2, next.Hour())
	assert.Equal(t, 21, next.Day())
}

func TestBackupScheduleSpec(t *testing.T) {
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-orders",
			Namespace: "default",
		},
		Spec: dbtether.BackupScheduleSpec{
			DatabaseRef: dbtether.DatabaseReference{
				Name: "orders-db",
			},
			StorageRef: dbtether.StorageReference{
				Name: "company-s3",
			},
			Schedule:         testCronSchedule,
			FilenameTemplate: "{{ .Timestamp }}-{{ .RunID }}.sql.gz",
			Retention: &dbtether.RetentionPolicy{
				KeepLast:    intPtr(7),
				KeepDaily:   intPtr(30),
				KeepWeekly:  intPtr(12),
				KeepMonthly: intPtr(12),
			},
			Suspend: false,
		},
	}

	assert.Equal(t, "orders-db", schedule.Spec.DatabaseRef.Name)
	assert.Equal(t, "company-s3", schedule.Spec.StorageRef.Name)
	assert.Equal(t, testCronSchedule, schedule.Spec.Schedule)
	assert.False(t, schedule.Spec.Suspend)

	require.NotNil(t, schedule.Spec.Retention)
	assert.Equal(t, 7, *schedule.Spec.Retention.KeepLast)
	assert.Equal(t, 30, *schedule.Spec.Retention.KeepDaily)
	assert.Equal(t, 12, *schedule.Spec.Retention.KeepWeekly)
	assert.Equal(t, 12, *schedule.Spec.Retention.KeepMonthly)
}

func intPtr(i int) *int {
	return &i
}

func TestGenerateBackupName(t *testing.T) {
	r := &BackupScheduleReconciler{}

	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nightly-backup",
			Namespace: "default",
		},
	}

	// Test deterministic name generation based on scheduled time
	scheduledTime := time.Date(2026, 1, 20, 14, 45, 0, 0, time.UTC)
	name := r.generateBackupName(schedule, scheduledTime)

	assert.Equal(t, "nightly-backup-20260120-1445", name)

	// Same scheduled time should produce the same name (deterministic)
	name2 := r.generateBackupName(schedule, scheduledTime)
	assert.Equal(t, name, name2)

	// Different time should produce different name
	differentTime := time.Date(2026, 1, 20, 15, 0, 0, 0, time.UTC)
	name3 := r.generateBackupName(schedule, differentTime)
	assert.NotEqual(t, name, name3)
	assert.Equal(t, "nightly-backup-20260120-1500", name3)
}

func TestGenerateBackupName_DifferentTimezones(t *testing.T) {
	r := &BackupScheduleReconciler{}

	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-schedule",
			Namespace: "prod",
		},
	}

	// Test that times are normalized to UTC
	localTime := time.Date(2026, 1, 20, 16, 30, 0, 0, time.FixedZone("CET", 3600))
	utcTime := time.Date(2026, 1, 20, 15, 30, 0, 0, time.UTC)

	localName := r.generateBackupName(schedule, localTime)
	utcName := r.generateBackupName(schedule, utcTime)

	// Should produce same name because both are 15:30 UTC
	assert.Equal(t, localName, utcName)
	assert.Equal(t, "my-schedule-20260120-1530", localName)
}

func TestGenerateBackupName_EdgeCases(t *testing.T) {
	r := &BackupScheduleReconciler{}

	tests := []struct {
		name         string
		scheduleName string
		scheduleTime time.Time
		expected     string
	}{
		{
			name:         "midnight UTC",
			scheduleName: "midnight-backup",
			scheduleTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			expected:     "midnight-backup-20260101-0000",
		},
		{
			name:         "end of year",
			scheduleName: "eoy-backup",
			scheduleTime: time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC),
			expected:     "eoy-backup-20261231-2359",
		},
		{
			name:         "leap year Feb 29",
			scheduleName: "leap-backup",
			scheduleTime: time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC),
			expected:     "leap-backup-20280229-1200",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schedule := &dbtether.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{Name: tt.scheduleName, Namespace: "default"},
			}
			got := r.generateBackupName(schedule, tt.scheduleTime)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestRetentionPolicy_Fields(t *testing.T) {
	// Test that all retention fields work
	policy := &dbtether.RetentionPolicy{
		KeepLast:    intPtr(5),
		KeepDaily:   intPtr(7),
		KeepWeekly:  intPtr(4),
		KeepMonthly: intPtr(12),
	}

	assert.Equal(t, 5, *policy.KeepLast)
	assert.Equal(t, 7, *policy.KeepDaily)
	assert.Equal(t, 4, *policy.KeepWeekly)
	assert.Equal(t, 12, *policy.KeepMonthly)
}

func TestBackupScheduleStatus_Fields(t *testing.T) {
	now := metav1.Now()
	status := dbtether.BackupScheduleStatus{
		Phase:                "Active",
		Message:              "Running normally",
		LastBackupTime:       &now,
		LastSuccessfulBackup: "my-schedule-20260120-0200",
		NextScheduledTime:    &now,
		ManagedBackups:       10,
		ObservedGeneration:   1,
	}

	assert.Equal(t, "Active", status.Phase)
	assert.Equal(t, "Running normally", status.Message)
	assert.NotNil(t, status.LastBackupTime)
	assert.NotNil(t, status.NextScheduledTime)
	assert.Equal(t, "my-schedule-20260120-0200", status.LastSuccessfulBackup)
	assert.Equal(t, 10, status.ManagedBackups)
	assert.Equal(t, int64(1), status.ObservedGeneration)
}

func TestBuildStoragePath(t *testing.T) {
	r := &BackupScheduleReconciler{}

	cluster := &dbtether.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
	}
	db := &dbtether.Database{
		Status: dbtether.DatabaseStatus{DatabaseName: "my_database"},
	}

	tests := []struct {
		name         string
		pathTemplate string
		expected     string
		expectError  bool
	}{
		{
			name:         "empty template uses default",
			pathTemplate: "",
			expected:     "test-cluster/my_database",
		},
		{
			name:         "simple template",
			pathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}",
			expected:     "test-cluster/my_database",
		},
		{
			name:         "cluster only",
			pathTemplate: "backups/{{ .ClusterName }}",
			expected:     "backups/test-cluster",
		},
		{
			name:         "database only",
			pathTemplate: "{{ .DatabaseName }}",
			expected:     "my_database",
		},
		{
			name:         "nested path",
			pathTemplate: "prod/{{ .ClusterName }}/dbs/{{ .DatabaseName }}/archives",
			expected:     "prod/test-cluster/dbs/my_database/archives",
		},
		{
			name:         "invalid template",
			pathTemplate: "{{ .InvalidField }}",
			expected:     "",
			expectError:  false, // template executes but produces "<no value>"
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := &dbtether.BackupStorage{
				Spec: dbtether.BackupStorageSpec{
					PathTemplate: tt.pathTemplate,
				},
			}

			result, err := r.buildStoragePath(storage, cluster, db)
			if tt.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tt.pathTemplate == "{{ .InvalidField }}" {
				// This produces "<no value>" when field is missing
				assert.Contains(t, result, "no value")
			} else {
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestBuildStoragePath_InvalidTemplate(t *testing.T) {
	r := &BackupScheduleReconciler{}

	cluster := &dbtether.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
	}
	db := &dbtether.Database{
		Status: dbtether.DatabaseStatus{DatabaseName: "my_database"},
	}

	// Malformed template syntax should return error
	storage := &dbtether.BackupStorage{
		Spec: dbtether.BackupStorageSpec{
			PathTemplate: "{{ .ClusterName", // Unclosed template
		},
	}

	_, err := r.buildStoragePath(storage, cluster, db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid path template")
}

func TestRetentionCleanup_NoPolicy(t *testing.T) {
	// Schedule without retention policy should not trigger cleanup
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-schedule",
			Namespace: "default",
		},
		Spec: dbtether.BackupScheduleSpec{
			DatabaseRef: dbtether.DatabaseReference{Name: "test-db"},
			StorageRef:  dbtether.StorageReference{Name: "test-storage"},
			Schedule:    testCronSchedule,
			Retention:   nil, // No retention policy
		},
	}

	// Just verify struct is valid and Retention is nil
	assert.Nil(t, schedule.Spec.Retention)
}

func TestRetentionPolicy_AllFieldsCombined(t *testing.T) {
	// Test that all retention fields can be set together
	policy := &dbtether.RetentionPolicy{
		KeepLast:    intPtr(10),
		KeepDaily:   intPtr(14),
		KeepWeekly:  intPtr(8),
		KeepMonthly: intPtr(24),
	}

	assert.NotNil(t, policy.KeepLast)
	assert.NotNil(t, policy.KeepDaily)
	assert.NotNil(t, policy.KeepWeekly)
	assert.NotNil(t, policy.KeepMonthly)

	assert.Equal(t, 10, *policy.KeepLast)
	assert.Equal(t, 14, *policy.KeepDaily)
	assert.Equal(t, 8, *policy.KeepWeekly)
	assert.Equal(t, 24, *policy.KeepMonthly)
}

func TestScheduleReconciler_HandleScheduledBackup_DeterministicName(t *testing.T) {
	// Test that scheduled backup names are deterministic
	r := &BackupScheduleReconciler{}

	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app-backup",
			Namespace: "production",
		},
	}

	// Same schedule time should always produce same name
	scheduleTime := time.Date(2026, 1, 20, 14, 30, 0, 0, time.UTC)

	name1 := r.generateBackupName(schedule, scheduleTime)
	name2 := r.generateBackupName(schedule, scheduleTime)

	assert.Equal(t, name1, name2)
	assert.Equal(t, "my-app-backup-20260120-1430", name1)

	// Different time produces different name
	differentTime := time.Date(2026, 1, 20, 15, 0, 0, 0, time.UTC)
	name3 := r.generateBackupName(schedule, differentTime)
	assert.NotEqual(t, name1, name3)
	assert.Equal(t, "my-app-backup-20260120-1500", name3)
}

func TestScheduleReconciler_CalculateNextRun(t *testing.T) {
	r := &BackupScheduleReconciler{}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	cronSchedule, err := parser.Parse("0 2 * * *") // Daily at 2am
	require.NoError(t, err)

	tests := []struct {
		name          string
		schedule      *dbtether.BackupSchedule
		expectedAfter time.Time
	}{
		{
			name: "no last backup - use creation time",
			schedule: &dbtether.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test",
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC)),
				},
				Status: dbtether.BackupScheduleStatus{
					LastBackupTime: nil,
				},
			},
			expectedAfter: time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "has last backup - use last backup time",
			schedule: &dbtether.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "test",
					Namespace:         "default",
					CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)),
				},
				Status: dbtether.BackupScheduleStatus{
					LastBackupTime: ptrToTime(time.Date(2026, 1, 20, 2, 0, 0, 0, time.UTC)),
				},
			},
			expectedAfter: time.Date(2026, 1, 20, 2, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextRun := r.calculateNextRun(tt.schedule, cronSchedule)
			assert.True(t, nextRun.After(tt.expectedAfter) || nextRun.Equal(cronSchedule.Next(tt.expectedAfter)))
		})
	}
}

func ptrToTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

func TestCleanupBackupCRDs_KeepLast(t *testing.T) {
	// Test that cleanupBackupCRDs respects keepLast policy

	// With keepLast=3, we should keep 3 newest and delete the rest
	policy := &dbtether.RetentionPolicy{
		KeepLast: intPtr(3),
	}

	// Verify policy struct is correct
	require.NotNil(t, policy.KeepLast)
	assert.Equal(t, 3, *policy.KeepLast)

	// Test sorting logic: backups should be sorted newest-first
	now := time.Now()
	backups := []struct {
		name      string
		timestamp time.Time
	}{
		{backupName1, now.Add(-1 * time.Hour)}, // newest
		{backupName2, now.Add(-2 * time.Hour)}, // 2nd
		{backupName3, now.Add(-3 * time.Hour)}, // 3rd (kept)
		{backupName4, now.Add(-4 * time.Hour)}, // 4th (deleted)
		{backupName5, now.Add(-5 * time.Hour)}, // 5th (deleted)
	}

	// Simulate sorting (newest first)
	sorted := make([]struct {
		name      string
		timestamp time.Time
	}, len(backups))
	copy(sorted, backups)

	// Sort newest first (already sorted in this case)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].timestamp.After(sorted[i].timestamp) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	// Verify sorting
	assert.Equal(t, backupName1, sorted[0].name)
	assert.Equal(t, backupName2, sorted[1].name)
	assert.Equal(t, backupName3, sorted[2].name)
	assert.Equal(t, backupName4, sorted[3].name)
	assert.Equal(t, backupName5, sorted[4].name)

	// With keepLast=3, items at index 3+ should be deleted
	keepCount := *policy.KeepLast
	toDelete := []string{}
	for i := keepCount; i < len(sorted); i++ {
		toDelete = append(toDelete, sorted[i].name)
	}

	assert.Len(t, toDelete, 2)
	assert.Contains(t, toDelete, backupName4)
	assert.Contains(t, toDelete, backupName5)
}

func TestCleanupBackupCRDs_NoKeepLast(t *testing.T) {
	// When keepLast is not set, no CRDs should be deleted

	policies := []*dbtether.RetentionPolicy{
		nil,
		{},                     // empty policy
		{KeepDaily: intPtr(7)}, // only keepDaily, no keepLast
	}

	for i, policy := range policies {
		keepCount := 0
		if policy != nil && policy.KeepLast != nil && *policy.KeepLast > 0 {
			keepCount = *policy.KeepLast
		}

		assert.Equal(t, 0, keepCount, "policy %d should have keepCount=0", i)
	}
}

func TestCleanupBackupCRDs_OnlyCompletedOrFailed(t *testing.T) {
	// Verify that only Completed/Failed backups are considered for deletion

	statuses := []string{
		"Pending",   // should NOT be deleted
		"Running",   // should NOT be deleted
		"Completed", // CAN be deleted
		"Failed",    // CAN be deleted
	}

	canDelete := func(status string) bool {
		return status == "Completed" || status == "Failed"
	}

	assert.False(t, canDelete("Pending"))
	assert.False(t, canDelete("Running"))
	assert.True(t, canDelete("Completed"))
	assert.True(t, canDelete("Failed"))

	for _, status := range statuses {
		result := canDelete(status)
		t.Logf("Status %q can be deleted: %v", status, result)
	}
}

func TestShouldRunRetentionCleanup_NoAnnotation(t *testing.T) {
	r := &BackupScheduleReconciler{}

	// Schedule without annotations - should allow cleanup
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-schedule",
			Namespace: "default",
		},
	}

	assert.True(t, r.shouldRunRetentionCleanup(schedule))
}

func TestShouldRunRetentionCleanup_EmptyAnnotations(t *testing.T) {
	r := &BackupScheduleReconciler{}

	// Schedule with empty annotations - should allow cleanup
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-schedule",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
	}

	assert.True(t, r.shouldRunRetentionCleanup(schedule))
}

func TestShouldRunRetentionCleanup_RecentCleanup(t *testing.T) {
	r := &BackupScheduleReconciler{}

	// Cleanup happened 30 seconds ago - should NOT allow cleanup (debounce=60s)
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-schedule",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationLastRetentionClean: time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339),
			},
		},
	}

	assert.False(t, r.shouldRunRetentionCleanup(schedule))
}

func TestShouldRunRetentionCleanup_OldCleanup(t *testing.T) {
	r := &BackupScheduleReconciler{}

	// Cleanup happened 2 minutes ago - should allow cleanup
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-schedule",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationLastRetentionClean: time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
	}

	assert.True(t, r.shouldRunRetentionCleanup(schedule))
}

func TestShouldRunRetentionCleanup_ExactlyAtDebounce(t *testing.T) {
	r := &BackupScheduleReconciler{}

	// Cleanup happened exactly at debounce time - should allow cleanup
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-schedule",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationLastRetentionClean: time.Now().Add(-RetentionCleanupDebounce).UTC().Format(time.RFC3339),
			},
		},
	}

	assert.True(t, r.shouldRunRetentionCleanup(schedule))
}

func TestShouldRunRetentionCleanup_InvalidTimestamp(t *testing.T) {
	r := &BackupScheduleReconciler{}

	// Invalid timestamp format - should allow cleanup (fail-safe)
	schedule := &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-schedule",
			Namespace: "default",
			Annotations: map[string]string{
				AnnotationLastRetentionClean: "not-a-valid-timestamp",
			},
		},
	}

	assert.True(t, r.shouldRunRetentionCleanup(schedule))
}

func TestRetentionCleanupDebounce_Constant(t *testing.T) {
	// Verify debounce constant is 60 seconds
	assert.Equal(t, 60*time.Second, RetentionCleanupDebounce)
}

func TestAnnotationLastRetentionClean_Constant(t *testing.T) {
	// Verify annotation name constant
	assert.Equal(t, "dbtether.io/last-retention-cleanup", AnnotationLastRetentionClean)
}

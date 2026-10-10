package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	backuppkg "github.com/certainty3452/dbtether/pkg/backup"
	"github.com/certainty3452/dbtether/pkg/storage"
)

func TestWriteTerminationMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "termination-log")
	cause := errors.New("restore failed: psql failed: exit status 3: psql:<stdin>:12: ERROR: syntax error")

	writeTerminationMessage(path, cause)

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read the termination message: %v", err)
	}
	if string(written) != cause.Error() {
		t.Errorf("termination message is %q, want %q", written, cause.Error())
	}
}

func TestWriteTerminationMessageTolerantOfUnwritablePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-directory", "termination-log")

	writeTerminationMessage(path, errors.New("restore failed"))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to stay absent, stat returned %v", path, err)
	}
}

func TestRunBackupJobFailsWhenResultCannotBeRecorded(t *testing.T) {
	recordErr := errors.New("backups.dbtether.io is forbidden")
	run := func(context.Context, *backuppkg.BackupConfig) (*backuppkg.BackupResult, error) {
		return &backuppkg.BackupResult{Path: "c/app/20260301-000000.sql.gz", Size: 1, UncompressedSize: 2}, nil
	}
	record := func(context.Context, *backuppkg.BackupConfig, *backuppkg.BackupResult) error { return recordErr }

	err := runBackupJob(context.Background(), &backuppkg.BackupConfig{BackupName: "b", Namespace: "ns"}, noStartRecord, run, record)

	if !errors.Is(err, recordErr) {
		t.Fatalf("runBackupJob() = %v, want an error wrapping %v", err, recordErr)
	}
}

func TestRunBackupJobDoesNotRecordFailedBackup(t *testing.T) {
	runErr := errors.New("pg_dump failed")
	run := func(context.Context, *backuppkg.BackupConfig) (*backuppkg.BackupResult, error) { return nil, runErr }
	record := func(context.Context, *backuppkg.BackupConfig, *backuppkg.BackupResult) error {
		t.Fatal("a failed backup must not be recorded")
		return nil
	}

	if err := runBackupJob(context.Background(), &backuppkg.BackupConfig{}, noStartRecord, run, record); !errors.Is(err, runErr) {
		t.Fatalf("runBackupJob() = %v, want %v", err, runErr)
	}
}

func TestBackupConfigFromEnvBuildsProviderConfig(t *testing.T) {
	for _, env := range []string{"DB_HOST", "DB_NAME", "DB_USER", "DB_PASSWORD", "CLUSTER_NAME", "DATABASE_NAME", "BACKUP_NAME", "BACKUP_NAMESPACE", "RUN_ID"} {
		t.Setenv(env, "x")
	}
	cases := map[string]struct {
		env   map[string]string
		check func(*testing.T, *backuppkg.BackupConfig)
	}{
		"gcs": {
			env: map[string]string{"STORAGE_TYPE": "gcs", "GCS_BUCKET": "gb", "GCS_PROJECT": "gp"},
			check: func(t *testing.T, cfg *backuppkg.BackupConfig) {
				if cfg.GCSConfig != (storage.GCSConfig{Bucket: "gb", Project: "gp"}) {
					t.Errorf("GCSConfig = %+v", cfg.GCSConfig)
				}
			},
		},
		"azure": {
			env: map[string]string{"STORAGE_TYPE": "azure", "AZURE_CONTAINER": "ac", "AZURE_ACCOUNT": "aa"},
			check: func(t *testing.T, cfg *backuppkg.BackupConfig) {
				if cfg.AzureConfig != (storage.AzureConfig{Container: "ac", StorageAccount: "aa"}) {
					t.Errorf("AzureConfig = %+v", cfg.AzureConfig)
				}
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			cfg := backupConfigFromEnv()
			if cfg.StorageType != name {
				t.Fatalf("StorageType = %q, want %q", cfg.StorageType, name)
			}
			tc.check(t, cfg)
		})
	}
}

func noRestoreStartRecord(context.Context, restoreTarget) error { return nil }

func TestRunRestoreJobFailsWhenResultCannotBeRecorded(t *testing.T) {
	recordErr := errors.New("restores.dbtether.io is forbidden")
	run := func(context.Context, *backuppkg.RestoreConfig) error { return nil }
	record := func(context.Context, restoreTarget) error { return recordErr }
	target := restoreTarget{Key: types.NamespacedName{Name: "r", Namespace: "ns"}, RunID: "run00001"}

	err := runRestoreJob(context.Background(), &backuppkg.RestoreConfig{}, target, noRestoreStartRecord, run, record)

	if !errors.Is(err, recordErr) {
		t.Fatalf("runRestoreJob() = %v, want an error wrapping %v", err, recordErr)
	}
}

func TestRunRestoreJobDoesNotRecordFailedRestore(t *testing.T) {
	runErr := errors.New("pg_restore failed")
	run := func(context.Context, *backuppkg.RestoreConfig) error { return runErr }
	record := func(context.Context, restoreTarget) error {
		t.Fatal("a failed restore must not be recorded")
		return nil
	}

	if err := runRestoreJob(context.Background(), &backuppkg.RestoreConfig{}, restoreTarget{}, noRestoreStartRecord, run, record); !errors.Is(err, runErr) {
		t.Fatalf("runRestoreJob() = %v, want %v", err, runErr)
	}
}

func TestRunRestoreJobDoesNotTouchDatabaseWhenStartCannotBeRecorded(t *testing.T) {
	run := func(context.Context, *backuppkg.RestoreConfig) error {
		t.Fatal("a run whose start is not recorded must not touch the target database")
		return nil
	}
	recordStart := func(context.Context, restoreTarget) error { return errRunSuperseded }
	record := func(context.Context, restoreTarget) error {
		t.Fatal("a run that never started must not record a result")
		return nil
	}
	target := restoreTarget{Key: types.NamespacedName{Name: "r", Namespace: "ns"}, RunID: "stale001"}

	err := runRestoreJob(context.Background(), &backuppkg.RestoreConfig{}, target, recordStart, run, record)

	if !errors.Is(err, errRunSuperseded) {
		t.Fatalf("runRestoreJob() = %v, want an error wrapping %v", err, errRunSuperseded)
	}
}

func TestOperatorServiceAccountFromEnvRequiresValue(t *testing.T) {
	t.Setenv("OPERATOR_SERVICE_ACCOUNT", "")
	if _, err := operatorServiceAccountFromEnv(); err == nil {
		t.Fatal("expected an error when OPERATOR_SERVICE_ACCOUNT is empty")
	}

	t.Setenv("OPERATOR_SERVICE_ACCOUNT", "release-dbtether")
	got, err := operatorServiceAccountFromEnv()
	if err != nil || got != "release-dbtether" {
		t.Fatalf("operatorServiceAccountFromEnv() = %q, %v", got, err)
	}
}

func noStartRecord(context.Context, *backuppkg.BackupConfig) error { return nil }

func TestRunBackupJobDoesNotDumpWhenStartCannotBeRecorded(t *testing.T) {
	run := func(context.Context, *backuppkg.BackupConfig) (*backuppkg.BackupResult, error) {
		t.Fatal("a run whose start is not recorded must not dump the database")
		return nil, nil
	}
	recordStart := func(context.Context, *backuppkg.BackupConfig) error { return errRunSuperseded }
	record := func(context.Context, *backuppkg.BackupConfig, *backuppkg.BackupResult) error {
		t.Fatal("a run that never started must not record a result")
		return nil
	}

	err := runBackupJob(context.Background(), &backuppkg.BackupConfig{BackupName: "b", Namespace: "ns", RunID: "stale001"}, recordStart, run, record)

	if !errors.Is(err, errRunSuperseded) {
		t.Fatalf("runBackupJob() = %v, want an error wrapping %v", err, errRunSuperseded)
	}
}

const (
	testRunBackup    = "b"
	testRunNamespace = "ns"
	testCurrentRunID = "current1"
	testCurrentPath  = "c/app/current.sql.gz"
)

func newRunStatusClient(funcs *interceptor.Funcs) client.WithWatch {
	backup := &databasesv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: testRunBackup, Namespace: testRunNamespace},
		Status:     databasesv1alpha1.BackupStatus{Phase: "Running", RunID: testCurrentRunID, Path: testCurrentPath},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(backup).
		WithStatusSubresource(&databasesv1alpha1.Backup{}).WithInterceptorFuncs(*funcs).Build()
}

func getRunStatusBackup(t *testing.T, c client.Client) databasesv1alpha1.Backup {
	t.Helper()
	var backup databasesv1alpha1.Backup
	if err := c.Get(context.Background(), types.NamespacedName{Name: testRunBackup, Namespace: testRunNamespace}, &backup); err != nil {
		t.Fatal(err)
	}
	return backup
}

func testRunResult() *backuppkg.BackupResult {
	return &backuppkg.BackupResult{Path: "c/app/late.sql.gz", Size: 2048, Duration: 3 * time.Second}
}

func TestRecordsOfSupersededRunLeaveStatusUntouched(t *testing.T) {
	cfg := &backuppkg.BackupConfig{BackupName: testRunBackup, Namespace: testRunNamespace, RunID: "stale001"}
	records := map[string]func(client.Client) error{
		"start":  func(c client.Client) error { return recordBackupStart(context.Background(), c, cfg) },
		"result": func(c client.Client) error { return recordBackupResult(context.Background(), c, cfg, testRunResult()) },
	}
	for name, record := range records {
		t.Run(name, func(t *testing.T) {
			c := newRunStatusClient(&interceptor.Funcs{})
			before := getRunStatusBackup(t, c)

			if err := record(c); !errors.Is(err, errRunSuperseded) {
				t.Fatalf("record = %v, want %v", err, errRunSuperseded)
			}

			after := getRunStatusBackup(t, c)
			if after.ResourceVersion != before.ResourceVersion || after.Status.Path != testCurrentPath || after.Status.StartedAt != nil {
				t.Errorf("a superseded run wrote the current run's status: %+v", after.Status)
			}
		})
	}
}

func TestRecordBackupResultWritesOwnRun(t *testing.T) {
	c := newRunStatusClient(&interceptor.Funcs{})
	cfg := &backuppkg.BackupConfig{BackupName: testRunBackup, Namespace: testRunNamespace, RunID: testCurrentRunID}

	if err := recordBackupStart(context.Background(), c, cfg); err != nil {
		t.Fatal(err)
	}
	if err := recordBackupResult(context.Background(), c, cfg, testRunResult()); err != nil {
		t.Fatal(err)
	}

	got := getRunStatusBackup(t, c).Status
	if got.StartedAt == nil || got.Path != "c/app/late.sql.gz" || got.Size != "2.0 KiB" || got.Duration != "3s" {
		t.Errorf("unexpected status %+v", got)
	}
}

func TestRecordBackupResultRechecksRunAfterConcurrentReservation(t *testing.T) {
	reserved := false
	c := newRunStatusClient(&interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if !reserved {
				reserved = true
				next := getRunStatusBackup(t, c)
				next.Status = databasesv1alpha1.BackupStatus{Phase: "Pending", RunID: "next0001"}
				if err := c.Status().Update(ctx, &next); err != nil {
					return err
				}
			}
			return c.Status().Patch(ctx, obj, patch, opts...)
		},
	})
	cfg := &backuppkg.BackupConfig{BackupName: testRunBackup, Namespace: testRunNamespace, RunID: testCurrentRunID}

	if err := recordBackupResult(context.Background(), c, cfg, testRunResult()); !errors.Is(err, errRunSuperseded) {
		t.Fatalf("recordBackupResult() = %v, want %v", err, errRunSuperseded)
	}
	if got := getRunStatusBackup(t, c).Status; got.RunID != "next0001" || got.Path != "" {
		t.Errorf("the result landed on the run reserved in between: %+v", got)
	}
}

func newRestoreRunStatusClient(funcs *interceptor.Funcs) client.WithWatch {
	restore := &databasesv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: testRunBackup, Namespace: testRunNamespace},
		Status:     databasesv1alpha1.RestoreStatus{Phase: "Running", RunID: testCurrentRunID, JobName: "restore-r-" + testCurrentRunID},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(restore).
		WithStatusSubresource(&databasesv1alpha1.Restore{}).WithInterceptorFuncs(*funcs).Build()
}

func getRunStatusRestore(t *testing.T, c client.Client) databasesv1alpha1.Restore {
	t.Helper()
	var restore databasesv1alpha1.Restore
	if err := c.Get(context.Background(), types.NamespacedName{Name: testRunBackup, Namespace: testRunNamespace}, &restore); err != nil {
		t.Fatal(err)
	}
	return restore
}

func testRestoreTarget(runID string) restoreTarget {
	return restoreTarget{Key: types.NamespacedName{Name: testRunBackup, Namespace: testRunNamespace}, RunID: runID}
}

func TestRestoreRecordsOfSupersededRunLeaveStatusUntouched(t *testing.T) {
	target := testRestoreTarget("stale001")
	records := map[string]func(client.Client) error{
		"start":  func(c client.Client) error { return recordRestoreStart(context.Background(), c, target) },
		"result": func(c client.Client) error { return recordRestoreResult(context.Background(), c, target) },
	}
	for name, record := range records {
		t.Run(name, func(t *testing.T) {
			c := newRestoreRunStatusClient(&interceptor.Funcs{})
			before := getRunStatusRestore(t, c)

			if err := record(c); !errors.Is(err, errRunSuperseded) {
				t.Fatalf("record = %v, want %v", err, errRunSuperseded)
			}

			after := getRunStatusRestore(t, c)
			if after.ResourceVersion != before.ResourceVersion || after.Status.StartedAt != nil || after.Status.JobSucceededAt != nil {
				t.Errorf("a superseded run wrote the current run's status: %+v", after.Status)
			}
		})
	}
}

func TestRecordRestoreResultWritesOwnRun(t *testing.T) {
	c := newRestoreRunStatusClient(&interceptor.Funcs{})
	target := testRestoreTarget(testCurrentRunID)

	if err := recordRestoreStart(context.Background(), c, target); err != nil {
		t.Fatal(err)
	}
	if err := recordRestoreResult(context.Background(), c, target); err != nil {
		t.Fatal(err)
	}

	got := getRunStatusRestore(t, c).Status
	if got.StartedAt == nil || got.JobSucceededAt == nil || got.RunID != testCurrentRunID {
		t.Errorf("unexpected status %+v", got)
	}
}

func TestRecordRestoreResultRechecksRunAfterConcurrentReservation(t *testing.T) {
	reserved := false
	c := newRestoreRunStatusClient(&interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if !reserved {
				reserved = true
				next := getRunStatusRestore(t, c)
				next.Status = databasesv1alpha1.RestoreStatus{Phase: "Pending", RunID: "next0001", JobName: "restore-r-next0001"}
				if err := c.Status().Update(ctx, &next); err != nil {
					return err
				}
			}
			return c.Status().Patch(ctx, obj, patch, opts...)
		},
	})

	if err := recordRestoreResult(context.Background(), c, testRestoreTarget(testCurrentRunID)); !errors.Is(err, errRunSuperseded) {
		t.Fatalf("recordRestoreResult() = %v, want %v", err, errRunSuperseded)
	}
	if got := getRunStatusRestore(t, c).Status; got.RunID != "next0001" || got.JobSucceededAt != nil {
		t.Errorf("the result landed on the run reserved in between: %+v", got)
	}
}

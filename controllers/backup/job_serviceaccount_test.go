package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRestoreReconciler_JobUsesConfiguredServiceAccount(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage)
	r.ServiceAccountName = "release-dbtether"

	_, err := reconcileRestore(t, r)
	require.NoError(t, err)

	jobs := listRestoreJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Equal(t, "release-dbtether", jobs[0].Spec.Template.Spec.ServiceAccountName)
}

func TestBackupReconciler_JobUsesConfiguredServiceAccount(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	r := newRerunReconciler(backup)
	r.ServiceAccountName = "release-dbtether"

	reconcileOnce(t, r, rerunRequest())

	jobs := listBackupJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Equal(t, "release-dbtether", jobs[0].Spec.Template.Spec.ServiceAccountName)
}

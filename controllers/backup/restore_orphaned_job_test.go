package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRestoreReconciler_RecordedSuccessWithoutJobNameNeverStartsSecondJob(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	_, _, _, _, secret := newRegrantFixture()
	secret.Namespace = cluster.Spec.CredentialsSecretRef.Namespace
	succeeded := metav1.Now()
	restore.Status.JobSucceededAt = &succeeded
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, secret)

	for i := 0; i < 3; i++ {
		_, err := reconcileRestore(t, r)
		require.NoError(t, err)
	}

	assert.Empty(t, listRestoreJobs(t, r))
	assert.Contains(t, []string{"Granting", "Completed"}, getRestoreStatus(t, r).Phase)
}

func TestRestoreReconciler_JobRecordedStartThenVanishedNeverStartsSecondJob(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	_, _, _, _, secret := newRegrantFixture()
	reserveRestore(restore, "ran00001")
	started := metav1.Now()
	restore.Status.StartedAt = &started
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, secret)

	for i := 0; i < 3; i++ {
		_, err := reconcileRestore(t, r)
		require.NoError(t, err)
	}

	assert.Empty(t, listRestoreJobs(t, r))
	st := getRestoreStatus(t, r)
	assert.Equal(t, "Failed", st.Phase)
	assert.Contains(t, st.Message, "unknown")
}

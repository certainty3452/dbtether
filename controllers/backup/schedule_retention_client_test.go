package backup

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
)

func TestRetentionClient_ResolvesCredentialsSecret(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, dbtether.AddToScheme(scheme))

	bs := &dbtether.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "minio"},
		Spec: dbtether.BackupStorageSpec{
			S3:                   &dbtether.S3StorageConfig{Bucket: "b", Region: "us-east-1", Endpoint: "http://minio:9000"},
			CredentialsSecretRef: &dbtether.SecretReference{Name: "missing", Namespace: "ns"},
		},
	}
	r := &BackupScheduleReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	_, err := defaultStorageClientFactory(context.Background(), r.Client, bs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "credentialsSecretRef ns/missing not found")
}

func TestRetentionClient_BuildsWithSecretCredentials(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, dbtether.AddToScheme(scheme))

	bs := &dbtether.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "minio"},
		Spec: dbtether.BackupStorageSpec{
			S3:                   &dbtether.S3StorageConfig{Bucket: "b", Region: "us-east-1", Endpoint: "http://minio:9000"},
			CredentialsSecretRef: &dbtether.SecretReference{Name: "creds", Namespace: "ns"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
		Data:       map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("ak"), "AWS_SECRET_ACCESS_KEY": []byte("sk")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	access, sec, err := readS3Credentials(context.Background(), c, bs)
	require.NoError(t, err)
	assert.Equal(t, "ak", access)
	assert.Equal(t, "sk", sec)

	cl, err := defaultStorageClientFactory(context.Background(), c, bs)
	require.NoError(t, err)
	assert.NotNil(t, cl)
}

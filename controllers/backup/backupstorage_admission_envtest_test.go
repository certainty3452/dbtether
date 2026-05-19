//go:build envtest

package backup

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// XValidation rules are evaluated by the apiserver, so they only show up under
// envtest (the fake client doesn't run CEL). These tests pin the rule that
// credentialsSecretRef is only allowed alongside an S3 provider.
var _ = Describe("BackupStorage credentialsSecretRef admission", func() {
	It("rejects credentialsSecretRef when GCS provider is selected", func() {
		bs := &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "gcs-with-creds"},
			Spec: databasesv1alpha1.BackupStorageSpec{
				GCS: &databasesv1alpha1.GCSStorageConfig{Bucket: "b", Project: "p"},
				CredentialsSecretRef: &databasesv1alpha1.SecretReference{
					Name: "creds", Namespace: "default",
				},
			},
		}
		err := k8sClient.Create(ctx, bs)
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("credentialssecretref is only supported with s3"))
	})

	It("rejects credentialsSecretRef when Azure provider is selected", func() {
		bs := &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "azure-with-creds"},
			Spec: databasesv1alpha1.BackupStorageSpec{
				Azure: &databasesv1alpha1.AzureStorageConfig{Container: "c", StorageAccount: "s"},
				CredentialsSecretRef: &databasesv1alpha1.SecretReference{
					Name: "creds", Namespace: "default",
				},
			},
		}
		err := k8sClient.Create(ctx, bs)
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("credentialssecretref is only supported with s3"))
	})

	It("accepts credentialsSecretRef when S3 provider is selected", func() {
		bs := &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "s3-with-creds"},
			Spec: databasesv1alpha1.BackupStorageSpec{
				S3: &databasesv1alpha1.S3StorageConfig{Bucket: "b", Region: "eu-central-1"},
				CredentialsSecretRef: &databasesv1alpha1.SecretReference{
					Name: "creds", Namespace: "default",
				},
			},
		}
		Expect(k8sClient.Create(ctx, bs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, bs)).To(Succeed())
	})

	It("accepts S3 without credentialsSecretRef (IRSA / Pod Identity path)", func() {
		bs := &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "s3-irsa"},
			Spec: databasesv1alpha1.BackupStorageSpec{
				S3: &databasesv1alpha1.S3StorageConfig{Bucket: "b", Region: "eu-central-1"},
			},
		}
		Expect(k8sClient.Create(ctx, bs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, bs)).To(Succeed())
	})

	It("accepts a non-S3 provider without credentialsSecretRef", func() {
		bs := &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "gcs-wi"},
			Spec: databasesv1alpha1.BackupStorageSpec{
				GCS: &databasesv1alpha1.GCSStorageConfig{Bucket: "b", Project: "p"},
			},
		}
		Expect(k8sClient.Create(ctx, bs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, bs)).To(Succeed())
	})

	It("rejects credentialsSecretRef without any provider (diagnostic: no-provider rule fires first)", func() {
		bs := &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "creds-no-provider"},
			Spec: databasesv1alpha1.BackupStorageSpec{
				CredentialsSecretRef: &databasesv1alpha1.SecretReference{
					Name: "creds", Namespace: "default",
				},
			},
		}
		err := k8sClient.Create(ctx, bs)
		Expect(err).To(HaveOccurred())
		// Both CEL rules fire; the user-facing message must surface the real cause.
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("one of s3, gcs, or azure must be specified"))
	})

	It("rejects a CR with no provider configured", func() {
		bs := &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "no-provider"},
			Spec:       databasesv1alpha1.BackupStorageSpec{},
		}
		err := k8sClient.Create(ctx, bs)
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("one of s3, gcs, or azure must be specified"))
	})
})

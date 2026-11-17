//go:build envtest

package backup

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

var _ = Describe("Backup deletionPolicy", Ordered, func() {
	const (
		timeout     = 30 * time.Second
		interval    = 250 * time.Millisecond
		namespace   = "deletion-policy-test"
		storageName = "deletion-policy-storage"
	)

	BeforeAll(func() {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		Expect(k8sClient.Create(ctx, &databasesv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: storageName},
			Spec: databasesv1alpha1.BackupStorageSpec{
				S3: &databasesv1alpha1.S3StorageConfig{Bucket: "b", Region: "us-east-1", Endpoint: "http://127.0.0.1:1"},
			},
		})).To(Succeed())
	})

	newBackup := func(name, policy string) *databasesv1alpha1.Backup {
		return &databasesv1alpha1.Backup{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasesv1alpha1.BackupSpec{
				DatabaseRef:    databasesv1alpha1.DatabaseReference{Name: "absent-db"},
				StorageRef:     databasesv1alpha1.StorageReference{Name: storageName},
				DeletionPolicy: policy,
			},
		}
	}

	// The referenced Database never exists, so the run is settled by hand as if a Job had produced path.
	createCompletedBackup := func(name, policy, path string) *databasesv1alpha1.Backup {
		backup := newBackup(name, policy)
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), backup)).To(Succeed())
			g.Expect(controllerutil.ContainsFinalizer(backup, backupFinalizer)).To(BeTrue())
			backup.Status.Phase = "Completed"
			backup.Status.Path = path
			backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
			g.Expect(k8sClient.Status().Update(ctx, backup)).To(Succeed())
		}, timeout, interval).Should(Succeed())
		envtestFiles.AddObject(path, []byte("dump"), time.Now())
		return backup
	}

	backupGone := func(backup *databasesv1alpha1.Backup) func() bool {
		return func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(backup), &databasesv1alpha1.Backup{}))
		}
	}

	hasFile := func(path string) bool {
		_, ok := envtestFiles.GetObject(path)
		return ok
	}

	It("defaults to Retain and rejects unknown policies", func() {
		backup := newBackup("defaulted", "")
		Expect(k8sClient.Create(ctx, backup)).To(Succeed())
		Expect(backup.Spec.DeletionPolicy).To(Equal("Retain"))

		Expect(k8sClient.Create(ctx, newBackup("invalid", "Orphan"))).NotTo(Succeed())
	})

	It("holds the Backup while storage fails and releases it once the file is deleted", func() {
		const path = "deletion-policy/failing.sql.gz"
		backup := createCompletedBackup("failing", "Delete", path)
		envtestStorageFailing.Store(true)
		DeferCleanup(func() { envtestStorageFailing.Store(false) })
		Expect(k8sClient.Delete(ctx, backup)).To(Succeed())

		Eventually(func(g Gomega) {
			var events corev1.EventList
			g.Expect(k8sClient.List(ctx, &events, client.InNamespace(namespace))).To(Succeed())
			g.Expect(events.Items).To(ContainElement(And(
				HaveField("Reason", EventReasonFileDeleteFailed),
				HaveField("Type", corev1.EventTypeWarning),
				HaveField("InvolvedObject.Name", backup.Name),
				HaveField("Message", ContainSubstring(path)),
			)))
		}, timeout, interval).Should(Succeed())
		Expect(backupGone(backup)()).To(BeFalse())
		Expect(hasFile(path)).To(BeTrue())

		envtestStorageFailing.Store(false)
		Eventually(backupGone(backup), timeout, interval).Should(BeTrue())
		Expect(hasFile(path)).To(BeFalse())
	})
})

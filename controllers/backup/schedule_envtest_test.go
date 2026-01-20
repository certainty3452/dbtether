//go:build envtest

package backup

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

var _ = Describe("BackupSchedule Controller", func() {
	const (
		timeout           = time.Second * 10
		interval          = time.Millisecond * 250
		scheduleTestNS    = "schedule-test"
		testDBNameEnvtest = "test-db"
		stepCreatingNS    = "Creating a namespace"
		stepCleaningUp    = "Cleaning up"
	)

	Context("When creating a BackupSchedule", func() {
		It("Should create and update status correctly", func() {
			By(stepCreatingNS)
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: scheduleTestNS,
				},
			}
			Expect(k8sClient.Create(ctx, ns)).Should(Succeed())

			By("Creating prerequisites: BackupStorage")
			storage := &databasesv1alpha1.BackupStorage{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-storage",
				},
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3: &databasesv1alpha1.S3StorageConfig{
						Bucket: "test-bucket",
						Region: "us-east-1",
					},
				},
			}
			Expect(k8sClient.Create(ctx, storage)).Should(Succeed())

			By("Creating prerequisites: DBCluster")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-creds",
					Namespace: "default",
				},
				StringData: map[string]string{
					"username": "postgres",
					"password": "password",
				},
			}
			Expect(k8sClient.Create(ctx, secret)).Should(Succeed())

			cluster := &databasesv1alpha1.DBCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-cluster",
				},
				Spec: databasesv1alpha1.DBClusterSpec{
					Endpoint: "localhost",
					Port:     5432,
					CredentialsSecretRef: &databasesv1alpha1.SecretReference{
						Name:      "test-cluster-creds",
						Namespace: "default",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).Should(Succeed())

			By("Creating prerequisites: Database")
			db := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testDBNameEnvtest,
					Namespace: scheduleTestNS,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{Name: "test-cluster"},
				},
			}
			Expect(k8sClient.Create(ctx, db)).Should(Succeed())

			// Manually set database status to Ready
			Eventually(func() error {
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: testDBNameEnvtest, Namespace: scheduleTestNS}, db); err != nil {
					return err
				}
				db.Status.Phase = "Ready"
				db.Status.DatabaseName = "test_db"
				return k8sClient.Status().Update(ctx, db)
			}, timeout, interval).Should(Succeed())

			By("Creating a BackupSchedule")
			schedule := &databasesv1alpha1.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-schedule",
					Namespace: scheduleTestNS,
				},
				Spec: databasesv1alpha1.BackupScheduleSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{Name: testDBNameEnvtest},
					StorageRef:  databasesv1alpha1.StorageReference{Name: "test-storage"},
					Schedule:    "0 2 * * *", // Daily at 2 AM
					Retention: &databasesv1alpha1.RetentionPolicy{
						KeepLast: intPtr(7),
					},
				},
			}
			Expect(k8sClient.Create(ctx, schedule)).Should(Succeed())

			By("Checking that BackupSchedule status is updated")
			scheduleKey := types.NamespacedName{Name: "test-schedule", Namespace: scheduleTestNS}
			createdSchedule := &databasesv1alpha1.BackupSchedule{}

			Eventually(func() string {
				if err := k8sClient.Get(ctx, scheduleKey, createdSchedule); err != nil {
					return ""
				}
				return createdSchedule.Status.Phase
			}, timeout, interval).Should(Equal("Active"))

			By("Verifying NextScheduledTime is set")
			Expect(createdSchedule.Status.NextScheduledTime).NotTo(BeNil())

			By("Testing suspend functionality")
			Expect(k8sClient.Get(ctx, scheduleKey, createdSchedule)).Should(Succeed())
			createdSchedule.Spec.Suspend = true
			Expect(k8sClient.Update(ctx, createdSchedule)).Should(Succeed())

			Eventually(func() string {
				if err := k8sClient.Get(ctx, scheduleKey, createdSchedule); err != nil {
					return ""
				}
				return createdSchedule.Status.Phase
			}, timeout, interval).Should(Equal("Suspended"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, schedule)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, db)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, cluster)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, storage)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, ns)).Should(Succeed())
		})
	})

	Context("When BackupSchedule has invalid cron", func() {
		It("Should be rejected by CRD validation", func() {
			By(stepCreatingNS)
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: "schedule-test-invalid",
				},
			}
			Expect(k8sClient.Create(ctx, ns)).Should(Succeed())

			By("Creating a BackupSchedule with invalid cron - should be rejected")
			schedule := &databasesv1alpha1.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-schedule",
					Namespace: "schedule-test-invalid",
				},
				Spec: databasesv1alpha1.BackupScheduleSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "nonexistent-db"},
					StorageRef:  databasesv1alpha1.StorageReference{Name: "nonexistent-storage"},
					Schedule:    "invalid cron", // Invalid - will be rejected by CRD validation
				},
			}
			err := k8sClient.Create(ctx, schedule)
			Expect(err).Should(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("spec.schedule"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, ns)).Should(Succeed())
		})
	})

	Context("When BackupSchedule is deleted", func() {
		It("Should cleanup owned Backup resources", func() {
			const (
				nsName       = "schedule-delete-test"
				scheduleName = "test-schedule-delete"
				storageName  = "test-storage-delete"
			)

			By(stepCreatingNS)
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: nsName},
			}
			Expect(k8sClient.Create(ctx, ns)).Should(Succeed())

			By("Creating prerequisites: BackupStorage")
			storage := &databasesv1alpha1.BackupStorage{
				ObjectMeta: metav1.ObjectMeta{Name: storageName},
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3: &databasesv1alpha1.S3StorageConfig{
						Bucket: "test-bucket",
						Region: "us-east-1",
					},
				},
			}
			Expect(k8sClient.Create(ctx, storage)).Should(Succeed())

			By("Creating a BackupSchedule with far future schedule")
			schedule := &databasesv1alpha1.BackupSchedule{
				ObjectMeta: metav1.ObjectMeta{
					Name:      scheduleName,
					Namespace: nsName,
				},
				Spec: databasesv1alpha1.BackupScheduleSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "test-db"},
					StorageRef:  databasesv1alpha1.StorageReference{Name: storageName},
					Schedule:    "0 2 * * *", // Far future, won't trigger immediately
				},
			}
			Expect(k8sClient.Create(ctx, schedule)).Should(Succeed())

			By("Waiting for schedule to be active")
			scheduleKey := types.NamespacedName{Name: scheduleName, Namespace: nsName}
			Eventually(func() string {
				var s databasesv1alpha1.BackupSchedule
				if err := k8sClient.Get(ctx, scheduleKey, &s); err != nil {
					return ""
				}
				return s.Status.Phase
			}, timeout, interval).Should(Equal("Active"))

			By("Creating a Backup owned by the schedule")
			backup := &databasesv1alpha1.Backup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      scheduleName + "-20260120-0200",
					Namespace: nsName,
					Labels: map[string]string{
						"dbtether.io/schedule":           scheduleName,
						"dbtether.io/schedule-namespace": nsName,
					},
				},
				Spec: databasesv1alpha1.BackupSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "test-db"},
					StorageRef:  databasesv1alpha1.StorageReference{Name: storageName},
				},
			}
			Expect(k8sClient.Create(ctx, backup)).Should(Succeed())

			By("Deleting the BackupSchedule")
			Expect(k8sClient.Delete(ctx, schedule)).Should(Succeed())

			By("Verifying the schedule is deleted")
			Eventually(func() bool {
				var s databasesv1alpha1.BackupSchedule
				err := k8sClient.Get(ctx, scheduleKey, &s)
				return err != nil
			}, timeout, interval).Should(BeTrue())

			By("Verifying owned Backup is deleted")
			backupKey := types.NamespacedName{Name: backup.Name, Namespace: nsName}
			Eventually(func() bool {
				var b databasesv1alpha1.Backup
				err := k8sClient.Get(ctx, backupKey, &b)
				return err != nil
			}, timeout, interval).Should(BeTrue())

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, storage)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, ns)).Should(Succeed())
		})
	})
})

// intPtr is defined in schedule_controller_test.go

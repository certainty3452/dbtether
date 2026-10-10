//go:build envtest

package backup

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// CEL rules run only under envtest; pins the rule keeping an empty RetentionPolicy from deleting every backup.
var _ = Describe("BackupSchedule retention admission", func() {
	It("rejects an empty retention policy", func() {
		bs := &databasesv1alpha1.BackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "retention-empty", Namespace: "default"},
			Spec: databasesv1alpha1.BackupScheduleSpec{
				DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "db"},
				StorageRef:  databasesv1alpha1.StorageReference{Name: "storage"},
				Schedule:    "0 2 * * *",
				Retention:   &databasesv1alpha1.RetentionPolicy{},
			},
		}
		err := k8sClient.Create(ctx, bs)
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("at least one of keeplast, keepdaily, keepweekly, or keepmonthly"))
	})

	It("rejects keepLast: 0 (all-zero policy)", func() {
		bs := &databasesv1alpha1.BackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "retention-zero", Namespace: "default"},
			Spec: databasesv1alpha1.BackupScheduleSpec{
				DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "db"},
				StorageRef:  databasesv1alpha1.StorageReference{Name: "storage"},
				Schedule:    "0 2 * * *",
				Retention: &databasesv1alpha1.RetentionPolicy{
					KeepLast: intPtr(0),
				},
			},
		}
		err := k8sClient.Create(ctx, bs)
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("at least one of keeplast, keepdaily, keepweekly, or keepmonthly"))
	})

	It("accepts keepLast: 3", func() {
		bs := &databasesv1alpha1.BackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "retention-keeplast", Namespace: "default"},
			Spec: databasesv1alpha1.BackupScheduleSpec{
				DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "db"},
				StorageRef:  databasesv1alpha1.StorageReference{Name: "storage"},
				Schedule:    "0 2 * * *",
				Retention: &databasesv1alpha1.RetentionPolicy{
					KeepLast: intPtr(3),
				},
			},
		}
		Expect(k8sClient.Create(ctx, bs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, bs)).To(Succeed())
	})

	It("accepts a BackupSchedule with no retention policy at all", func() {
		bs := &databasesv1alpha1.BackupSchedule{
			ObjectMeta: metav1.ObjectMeta{Name: "retention-unset", Namespace: "default"},
			Spec: databasesv1alpha1.BackupScheduleSpec{
				DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "db"},
				StorageRef:  databasesv1alpha1.StorageReference{Name: "storage"},
				Schedule:    "0 2 * * *",
			},
		}
		Expect(k8sClient.Create(ctx, bs)).To(Succeed())
		Expect(k8sClient.Delete(ctx, bs)).To(Succeed())
	})
})

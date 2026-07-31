//go:build envtest

package backup

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// XValidation rules only run under envtest (the fake client doesn't evaluate CEL).
// This pins the rule blocking cross-namespace targets: regrantDatabaseUsers looks
// up the target Database using restore.Namespace, so a mismatch would silently miss.
var _ = Describe("Restore target admission", func() {
	It("rejects a non-empty target.databaseRef.namespace", func() {
		restore := &databasesv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "target-cross-ns", Namespace: "default"},
			Spec: databasesv1alpha1.RestoreSpec{
				Source: databasesv1alpha1.RestoreSource{
					Path:       "cluster/database/20260120-140000.sql.gz",
					StorageRef: &databasesv1alpha1.StorageReference{Name: "my-storage"},
				},
				Target: databasesv1alpha1.RestoreTarget{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name:      "my-database",
						Namespace: "other-namespace",
					},
				},
			},
		}
		err := k8sClient.Create(ctx, restore)
		Expect(err).To(HaveOccurred())
		Expect(strings.ToLower(err.Error())).To(ContainSubstring("target database must be in the restore's namespace"))
	})

	It("accepts an empty target.databaseRef.namespace", func() {
		restore := &databasesv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "target-same-ns", Namespace: "default"},
			Spec: databasesv1alpha1.RestoreSpec{
				Source: databasesv1alpha1.RestoreSource{
					Path:       "cluster/database/20260120-140000.sql.gz",
					StorageRef: &databasesv1alpha1.StorageReference{Name: "my-storage"},
				},
				Target: databasesv1alpha1.RestoreTarget{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: "my-database",
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())
		Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
	})
})

//go:build envtest

package backup

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// CEL runs only under envtest; pins the cross-namespace target block, since regrant looks up the Database by restore.Namespace.
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

var _ = Describe("Restore source admission", func() {
	const msg = "set exactly one of backupRef, latestFrom, path"

	backupRef := &databasesv1alpha1.BackupReference{Name: "b"}
	latestFrom := &databasesv1alpha1.LatestFromSource{DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "d"}}
	storageRef := &databasesv1alpha1.StorageReference{Name: "s"}

	newRestore := func(name string, src databasesv1alpha1.RestoreSource) *databasesv1alpha1.Restore {
		return &databasesv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: databasesv1alpha1.RestoreSpec{
				Source: src,
				Target: databasesv1alpha1.RestoreTarget{DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "my-database"}},
			},
		}
	}

	DescribeTable("rejects",
		func(name string, src databasesv1alpha1.RestoreSource) {
			err := k8sClient.Create(ctx, newRestore(name, src))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring(msg))
		},
		Entry("backupRef and latestFrom", "src-two-a", databasesv1alpha1.RestoreSource{BackupRef: backupRef, LatestFrom: latestFrom}),
		Entry("backupRef and path", "src-two-b", databasesv1alpha1.RestoreSource{BackupRef: backupRef, Path: "a/b.sql.gz", StorageRef: storageRef}),
		Entry("latestFrom and path", "src-two-c", databasesv1alpha1.RestoreSource{LatestFrom: latestFrom, Path: "a/b.sql.gz", StorageRef: storageRef}),
		Entry("no source", "src-none", databasesv1alpha1.RestoreSource{}),
	)

	DescribeTable("accepts",
		func(name string, src databasesv1alpha1.RestoreSource) {
			restore := newRestore(name, src)
			Expect(k8sClient.Create(ctx, restore)).To(Succeed())
			Expect(k8sClient.Delete(ctx, restore)).To(Succeed())
		},
		Entry("backupRef", "src-one-a", databasesv1alpha1.RestoreSource{BackupRef: backupRef}),
		Entry("latestFrom", "src-one-b", databasesv1alpha1.RestoreSource{LatestFrom: latestFrom}),
		Entry("path", "src-one-c", databasesv1alpha1.RestoreSource{Path: "a/b.sql.gz", StorageRef: storageRef}),
	)
})

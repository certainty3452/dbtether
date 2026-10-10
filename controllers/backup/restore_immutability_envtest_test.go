//go:build envtest

package backup

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

var _ = Describe("Restore spec immutability", func() {
	It("rejects changing spec.source of an existing Restore", func() {
		restore := &databasesv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: "immutable-source", Namespace: "default"},
			Spec: databasesv1alpha1.RestoreSpec{
				Source: databasesv1alpha1.RestoreSource{
					Path:       "cluster/database/20260120-140000.sql.gz",
					StorageRef: &databasesv1alpha1.StorageReference{Name: "my-storage"},
				},
				Target: databasesv1alpha1.RestoreTarget{
					DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "my-database"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, restore)).To(Succeed())
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, restore)) })

		restore.Spec.Source.Path = "cluster/database/20260121-140000.sql.gz"
		err := k8sClient.Update(ctx, restore)

		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue())
	})
})

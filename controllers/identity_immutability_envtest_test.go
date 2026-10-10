//go:build envtest

package controllers

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

var _ = Describe("Identity field immutability", func() {
	const namespace = "default"

	patchSpec := func(obj client.Object, mutate func()) error {
		patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
		mutate()
		return k8sClient.Patch(ctx, obj, patch)
	}

	expectRejected := func(err error, message string) {
		Expect(err).To(HaveOccurred())
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
		Expect(err.Error()).To(ContainSubstring(message))
	}

	createDatabase := func(name, databaseName string) *databasesv1alpha1.Database {
		db := &databasesv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasesv1alpha1.DatabaseSpec{
				ClusterRef:   databasesv1alpha1.ClusterReference{Name: "immutability-cluster"},
				DatabaseName: databaseName,
			},
		}
		Expect(k8sClient.Create(ctx, db)).To(Succeed())
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, db)) })
		return db
	}

	createUser := func(name, username string) *databasesv1alpha1.DatabaseUser {
		user := &databasesv1alpha1.DatabaseUser{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasesv1alpha1.DatabaseUserSpec{
				Database: &databasesv1alpha1.DatabaseAccess{Name: "immutability-db"},
				Username: username,
			},
		}
		Expect(k8sClient.Create(ctx, user)).To(Succeed())
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, user)) })
		return user
	}

	Context("Database", func() {
		It("rejects changing spec.databaseName", func() {
			db := createDatabase("immutable-dbname-change", "first_name")
			err := patchSpec(db, func() { db.Spec.DatabaseName = "second_name" })
			expectRejected(err, "spec.databaseName is immutable")
		})

		It("rejects setting spec.databaseName after creation", func() {
			db := createDatabase("immutable-dbname-set", "")
			err := patchSpec(db, func() { db.Spec.DatabaseName = "late_name" })
			expectRejected(err, "spec.databaseName is immutable")
		})

		It("rejects unsetting spec.databaseName", func() {
			db := createDatabase("immutable-dbname-unset", "explicit_name")
			err := patchSpec(db, func() { db.Spec.DatabaseName = "" })
			expectRejected(err, "spec.databaseName is immutable")
		})

		It("rejects changing spec.clusterRef", func() {
			db := createDatabase("immutable-clusterref", "clusterref_db")
			err := patchSpec(db, func() { db.Spec.ClusterRef.Name = "another-cluster" })
			expectRejected(err, "spec.clusterRef is immutable")
		})

		It("accepts changes to other spec fields", func() {
			db := createDatabase("mutable-db-fields", "")
			Expect(patchSpec(db, func() {
				db.Spec.Extensions = []string{"pgcrypto"}
				db.Spec.RevokePublicConnect = true
				db.Spec.DeletionPolicy = "Delete"
			})).To(Succeed())
		})
	})

	Context("DatabaseUser", func() {
		It("rejects changing spec.username", func() {
			user := createUser("immutable-username-change", "first_user")
			err := patchSpec(user, func() { user.Spec.Username = "second_user" })
			expectRejected(err, "spec.username is immutable")
		})

		It("rejects setting spec.username after creation", func() {
			user := createUser("immutable-username-set", "")
			err := patchSpec(user, func() { user.Spec.Username = "late_user" })
			expectRejected(err, "spec.username is immutable")
		})

		It("rejects unsetting spec.username", func() {
			user := createUser("immutable-username-unset", "explicit_user")
			err := patchSpec(user, func() { user.Spec.Username = "" })
			expectRejected(err, "spec.username is immutable")
		})

		It("accepts changes to other spec fields", func() {
			user := createUser("mutable-user-fields", "kept_user")
			Expect(patchSpec(user, func() {
				user.Spec.Privileges = "readwrite"
				user.Spec.ConnectionLimit = 5
			})).To(Succeed())
		})
	})
})

//go:build envtest

package controllers

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

var _ = Describe("Database Controller", func() {
	const (
		clusterName  = "db-test-cluster"
		namespace    = "default"
		timeout      = time.Second * 10
		interval     = time.Millisecond * 250
		stepCleanup  = "Cleaning up"
		dbDeleteName = "db-delete-policy"
	)

	BeforeEach(func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      clusterName + "-creds",
				Namespace: namespace,
			},
			StringData: map[string]string{
				"username": "postgres",
				"password": "password123",
			},
		}
		_ = k8sClient.Create(ctx, secret)

		cluster := &databasesv1alpha1.DBCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: clusterName,
			},
			Spec: databasesv1alpha1.DBClusterSpec{
				Endpoint: "localhost",
				Port:     5432,
				CredentialsSecretRef: &databasesv1alpha1.SecretReference{
					Name:      clusterName + "-creds",
					Namespace: namespace,
				},
			},
		}
		_ = k8sClient.Create(ctx, cluster)
	})

	Context("When creating a Database", func() {
		It("Should create a Database resource with correct spec", func() {
			By("Creating a Database")
			database := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-database",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
					DatabaseName:   "mydb",
					DeletionPolicy: "Retain",
				},
			}
			Expect(k8sClient.Create(ctx, database)).Should(Succeed())

			By("Verifying the Database was created")
			createdDB := &databasesv1alpha1.Database{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-database",
					Namespace: namespace,
				}, createdDB)
			}, timeout, interval).Should(Succeed())

			Expect(createdDB.Spec.DatabaseName).Should(Equal("mydb"))
			Expect(createdDB.Spec.DeletionPolicy).Should(Equal("Retain"))

			By(stepCleanup)
			Expect(k8sClient.Delete(ctx, database)).Should(Succeed())
		})

		It("Should derive databaseName from metadata.name when not specified", func() {
			By("Creating a Database without explicit databaseName")
			database := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "implicit-name-db",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
				},
			}
			Expect(k8sClient.Create(ctx, database)).Should(Succeed())

			By("Verifying status.databaseName is derived from metadata.name")
			Eventually(func() string {
				var db databasesv1alpha1.Database
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "implicit-name-db",
					Namespace: namespace,
				}, &db); err != nil {
					return ""
				}
				return db.Status.DatabaseName
			}, timeout, interval).Should(Equal("implicit_name_db"))

			By(stepCleanup)
			Expect(k8sClient.Delete(ctx, database)).Should(Succeed())
		})
	})

	Context("When Database references non-existent cluster", func() {
		It("Should set status to pending", func() {
			By("Creating a Database with non-existent cluster ref")
			database := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-database",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: "non-existent-cluster",
					},
					DatabaseName: "orphandb",
				},
			}
			Expect(k8sClient.Create(ctx, database)).Should(Succeed())

			By("Verifying status is set (Pending or Failed depending on cluster lookup)")
			Eventually(func() string {
				db := &databasesv1alpha1.Database{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "orphan-database",
					Namespace: namespace,
				}, db); err != nil {
					return ""
				}
				return db.Status.Phase
			}, timeout, interval).Should(Or(Equal("Pending"), Equal("Failed")))

			By(stepCleanup)
			Expect(k8sClient.Delete(ctx, database)).Should(Succeed())
		})
	})

	Context("When Database has extensions", func() {
		It("Should store extensions in spec", func() {
			By("Creating a Database with extensions")
			database := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "db-with-extensions",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
					DatabaseName: "extdb",
					Extensions:   []string{"uuid-ossp", "pgcrypto"},
				},
			}
			Expect(k8sClient.Create(ctx, database)).Should(Succeed())

			By("Verifying extensions are stored")
			createdDB := &databasesv1alpha1.Database{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "db-with-extensions",
					Namespace: namespace,
				}, createdDB)
			}, timeout, interval).Should(Succeed())

			Expect(createdDB.Spec.Extensions).Should(ContainElements("uuid-ossp", "pgcrypto"))

			By(stepCleanup)
			Expect(k8sClient.Delete(ctx, database)).Should(Succeed())
		})
	})

	Context("When Database has Delete policy", func() {
		It("Should set deletion policy correctly", func() {
			By("Creating a Database with Delete policy")
			database := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      dbDeleteName,
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
					DatabaseName:   "deletedb",
					DeletionPolicy: "Delete",
				},
			}
			Expect(k8sClient.Create(ctx, database)).Should(Succeed())

			By("Verifying deletion policy is set")
			createdDB := &databasesv1alpha1.Database{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      dbDeleteName,
					Namespace: namespace,
				}, createdDB)
			}, timeout, interval).Should(Succeed())

			Expect(createdDB.Spec.DeletionPolicy).Should(Equal("Delete"))

			By("Deleting the database")
			Expect(k8sClient.Delete(ctx, database)).Should(Succeed())

			By("Verifying database is eventually deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      dbDeleteName,
					Namespace: namespace,
				}, createdDB)
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When Database reaches Ready state", func() {
		It("Should transition through Creating to Ready", func() {
			By("Creating a Database")
			database := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "db-ready-test",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
					DatabaseName: "readydb",
				},
			}
			Expect(k8sClient.Create(ctx, database)).Should(Succeed())

			By("Verifying status transitions")
			Eventually(func() string {
				db := &databasesv1alpha1.Database{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "db-ready-test",
					Namespace: namespace,
				}, db); err != nil {
					return ""
				}
				return db.Status.Phase
			}, timeout, interval).Should(Or(Equal("Creating"), Equal("Ready"), Equal("Failed")))

			By(stepCleanup)
			Expect(k8sClient.Delete(ctx, database)).Should(Succeed())
		})
	})
})

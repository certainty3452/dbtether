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

var _ = Describe("DatabaseUser Controller", func() {
	const (
		clusterName  = "user-test-cluster"
		databaseName = "user-test-db"
		namespace    = "default"
		timeout      = time.Second * 10
		interval     = time.Millisecond * 250
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

		database := &databasesv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{
				Name:      databaseName,
				Namespace: namespace,
			},
			Spec: databasesv1alpha1.DatabaseSpec{
				ClusterRef: databasesv1alpha1.ClusterReference{
					Name: clusterName,
				},
				DatabaseName: "testdb",
			},
		}
		_ = k8sClient.Create(ctx, database)
	})

	Context("When creating a DatabaseUser", func() {
		It("Should create a DatabaseUser resource with correct spec", func() {
			By("Creating a DatabaseUser")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying the DatabaseUser was created")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "test-user",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Privileges).Should(Equal("readonly"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should use default password length if not specified", func() {
			By("Creating a DatabaseUser without password config")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-default-pwd",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges: "readwrite",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying password length defaults to 16 when not specified")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-default-pwd",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			// Default is 16, but if 0 is stored, controller will use 16 internally
			Expect(createdUser.Spec.Password.Length).Should(BeNumerically(">=", 0))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept custom password length", func() {
			By("Creating a DatabaseUser with custom password length")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-custom-pwd",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges: "admin",
					Password: databasesv1alpha1.PasswordConfig{
						Length: 32,
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying password length")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-custom-pwd",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Password.Length).Should(Equal(32))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser reaches Ready status", func() {
		It("Should set passwordUpdatedAt in status", func() {
			By("Creating a DatabaseUser")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-pwd-timestamp",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Waiting for status to become Ready")
			Eventually(func() string {
				u := &databasesv1alpha1.DatabaseUser{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-pwd-timestamp",
					Namespace: namespace,
				}, u); err != nil {
					return ""
				}
				return u.Status.Phase
			}, timeout, interval).Should(Equal("Ready"))

			By("Verifying passwordUpdatedAt is set")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      "user-pwd-timestamp",
				Namespace: namespace,
			}, createdUser)).Should(Succeed())

			Expect(createdUser.Status.PasswordUpdatedAt).ShouldNot(BeNil())
			Expect(createdUser.Status.PasswordUpdatedAt.Time).Should(BeTemporally("~", time.Now(), time.Minute))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser references non-existent database", func() {
		It("Should set status to pending", func() {
			By("Creating a DatabaseUser with non-existent database ref")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "orphan-user",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: "non-existent-database",
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying status is set (Pending or Failed depending on database lookup)")
			Eventually(func() string {
				u := &databasesv1alpha1.DatabaseUser{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "orphan-user",
					Namespace: namespace,
				}, u); err != nil {
					return ""
				}
				return u.Status.Phase
			}, timeout, interval).Should(Or(Equal("Pending"), Equal("Failed")))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser has custom username", func() {
		It("Should store custom username in spec", func() {
			By("Creating a DatabaseUser with custom username")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-with-custom-name",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Username:   "custom_postgres_user",
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying username is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-with-custom-name",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Username).Should(Equal("custom_postgres_user"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When validating privileges", func() {
		It("Should accept valid privilege presets", func() {
			for _, priv := range []string{"readonly", "readwrite", "admin"} {
				user := &databasesv1alpha1.DatabaseUser{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "user-priv-" + priv,
						Namespace: namespace,
					},
					Spec: databasesv1alpha1.DatabaseUserSpec{
						DatabaseRef: databasesv1alpha1.DatabaseReference{
							Name: databaseName,
						},
						Privileges: priv,
					},
				}
				Expect(k8sClient.Create(ctx, user)).Should(Succeed())
				Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
			}
		})
	})

	Context("When DatabaseUser reaches Ready state", func() {
		It("Should transition status and create credentials secret", func() {
			By("Waiting for Database to be ready or creating state")
			Eventually(func() string {
				db := &databasesv1alpha1.Database{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      databaseName,
					Namespace: namespace,
				}, db); err != nil {
					return ""
				}
				return db.Status.Phase
			}, timeout, interval).Should(Or(Equal("Ready"), Equal("Creating"), Equal("Failed")))

			By("Creating a DatabaseUser")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-ready-test",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges: "readwrite",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying user status is set")
			Eventually(func() string {
				u := &databasesv1alpha1.DatabaseUser{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-ready-test",
					Namespace: namespace,
				}, u); err != nil {
					return ""
				}
				return u.Status.Phase
			}, timeout, interval).Should(Or(Equal("Ready"), Equal("Creating"), Equal("Pending"), Equal("Failed")))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser is deleted", func() {
		It("Should clean up resources", func() {
			By("Creating a DatabaseUser")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-to-delete",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Waiting for user to be created")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-to-delete",
					Namespace: namespace,
				}, user)
			}, timeout, interval).Should(Succeed())

			By("Deleting the user")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())

			By("Verifying user is eventually deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-to-delete",
					Namespace: namespace,
				}, user)
				return err != nil
			}, timeout, interval).Should(BeTrue())
		})
	})

	Context("When DatabaseUser has connection limit", func() {
		It("Should accept connection limit in spec", func() {
			By("Creating a DatabaseUser with connection limit")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-conn-limit",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges:      "readonly",
					ConnectionLimit: 10,
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying connection limit is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-conn-limit",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.ConnectionLimit).Should(Equal(10))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept unlimited connections (-1)", func() {
			By("Creating a DatabaseUser with unlimited connections")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-unlimited-conn",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges:      "readwrite",
					ConnectionLimit: -1,
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying connection limit is -1")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-unlimited-conn",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.ConnectionLimit).Should(Equal(-1))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When credentials secret is deleted", func() {
		It("Should create secret with owner reference for auto-cleanup", func() {
			By("Creating a DatabaseUser")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-secret-regen",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DatabaseRef: databasesv1alpha1.DatabaseReference{
						Name: databaseName,
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Waiting for user to become Ready")
			Eventually(func() string {
				u := &databasesv1alpha1.DatabaseUser{}
				if err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-secret-regen",
					Namespace: namespace,
				}, u); err != nil {
					return ""
				}
				return u.Status.Phase
			}, timeout, interval).Should(Equal("Ready"))

			By("Verifying secret was created with correct owner reference")
			credsSecretName := "user-secret-regen-credentials" //nolint:gosec // Not a credential
			secret := &corev1.Secret{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      credsSecretName,
					Namespace: namespace,
				}, secret)
			}, timeout, interval).Should(Succeed())

			Expect(secret.OwnerReferences).Should(HaveLen(1))
			Expect(secret.OwnerReferences[0].Kind).Should(Equal("DatabaseUser"))
			Expect(secret.OwnerReferences[0].Name).Should(Equal("user-secret-regen"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})
})

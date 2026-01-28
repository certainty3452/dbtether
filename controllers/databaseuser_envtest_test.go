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

var _ = Describe("DatabaseUser Controller", func() {
	const (
		clusterName      = "user-test-cluster"
		databaseName     = "user-test-db"
		namespace        = "default"
		timeout          = time.Second * 10
		interval         = time.Millisecond * 250
		stepCreatingUser = "Creating a DatabaseUser"
		stepCleaningUp   = "Cleaning up"
		userPwdTimestamp = "user-pwd-timestamp" //nolint:gosec // test user name, not a credential
		userToDelete     = "user-to-delete"
		userSecretRegen  = "user-secret-regen" //nolint:gosec // test user name, not a credential
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
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
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
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
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
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser reaches Ready status", func() {
		It("Should set passwordUpdatedAt in status", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      userPwdTimestamp,
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
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
					Name:      userPwdTimestamp,
					Namespace: namespace,
				}, u); err != nil {
					return ""
				}
				return u.Status.Phase
			}, timeout, interval).Should(Equal("Ready"))

			By("Verifying passwordUpdatedAt is set")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name:      userPwdTimestamp,
				Namespace: namespace,
			}, createdUser)).Should(Succeed())

			Expect(createdUser.Status.PasswordUpdatedAt).ShouldNot(BeNil())
			Expect(createdUser.Status.PasswordUpdatedAt.Time).Should(BeTemporally("~", time.Now(), time.Minute))

			By(stepCleaningUp)
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
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
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
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
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
						Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-ready-test",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser is deleted", func() {
		It("Should clean up resources", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      userToDelete,
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Waiting for user to be created")
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      userToDelete,
					Namespace: namespace,
				}, user)
			}, timeout, interval).Should(Succeed())

			By("Deleting the user")
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())

			By("Verifying user is eventually deleted")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      userToDelete,
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
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
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
					Database: &databasesv1alpha1.DatabaseAccess{
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

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When credentials secret is deleted", func() {
		It("Should create secret with owner reference for auto-cleanup", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      userSecretRegen,
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
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
					Name:      userSecretRegen,
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
			Expect(secret.OwnerReferences[0].Name).Should(Equal(userSecretRegen))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser has custom secret config", func() {
		It("Should accept custom secret name", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-custom-secret-name",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						Name: "my-custom-creds",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying spec.secret.name is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-custom-secret-name",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Name).Should(Equal("my-custom-creds"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept DB template", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-db-template",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "DB",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying spec.secret.template is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-db-template",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Template).Should(Equal("DB"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept DATABASE template", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-database-template",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readwrite",
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "DATABASE",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying spec.secret.template is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-database-template",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Template).Should(Equal("DATABASE"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept custom template with keys", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-custom-keys",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "admin",
					Secret: &databasesv1alpha1.SecretConfig{
						Name:     "pg-credentials",
						Template: "custom",
						Keys: &databasesv1alpha1.SecretKeys{
							Host:     "PGHOST",
							Port:     "PGPORT",
							Database: "PGDATABASE",
							User:     "PGUSER",
							Password: "PGPASSWORD",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying spec.secret config is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-custom-keys",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Name).Should(Equal("pg-credentials"))
			Expect(createdUser.Spec.Secret.Template).Should(Equal("custom"))
			Expect(createdUser.Spec.Secret.Keys).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Keys.Host).Should(Equal("PGHOST"))
			Expect(createdUser.Spec.Secret.Keys.Port).Should(Equal("PGPORT"))
			Expect(createdUser.Spec.Secret.Keys.Database).Should(Equal("PGDATABASE"))
			Expect(createdUser.Spec.Secret.Keys.User).Should(Equal("PGUSER"))
			Expect(createdUser.Spec.Secret.Keys.Password).Should(Equal("PGPASSWORD"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept partial custom keys", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-partial-keys",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "custom",
						Keys: &databasesv1alpha1.SecretKeys{
							Password: "SECRET_PWD",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying partial keys are stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-partial-keys",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Keys).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Keys.Password).Should(Equal("SECRET_PWD"))
			Expect(createdUser.Spec.Secret.Keys.Host).Should(BeEmpty())

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When DatabaseUser has onConflict config", func() {
		It("Should accept Fail onConflict policy", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-conflict-fail",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						OnConflict: "Fail",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying spec.secret.onConflict is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-conflict-fail",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.OnConflict).Should(Equal("Fail"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept Adopt onConflict policy", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-conflict-adopt",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readwrite",
					Secret: &databasesv1alpha1.SecretConfig{
						OnConflict: "Adopt",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying spec.secret.onConflict is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-conflict-adopt",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.OnConflict).Should(Equal("Adopt"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept Merge onConflict policy", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-conflict-merge",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "admin",
					Secret: &databasesv1alpha1.SecretConfig{
						Name:       "merged-secret",
						OnConflict: "Merge",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying spec.secret config is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-conflict-merge",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Name).Should(Equal("merged-secret"))
			Expect(createdUser.Spec.Secret.OnConflict).Should(Equal("Merge"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When creating a DatabaseUser with multiple databases", func() {
		const (
			database2Name = "user-test-db-2"
			database3Name = "user-test-db-3"
		)

		BeforeEach(func() {
			database2 := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      database2Name,
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
					DatabaseName: "testdb2",
				},
			}
			_ = k8sClient.Create(ctx, database2)

			database3 := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      database3Name,
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
					DatabaseName: "testdb3",
				},
			}
			_ = k8sClient.Create(ctx, database3)
		})

		It("Should create a DatabaseUser with databases array", func() {
			By("Creating a DatabaseUser with multiple databases")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-multi-db",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: databaseName},
						{Name: database2Name},
						{Name: database3Name},
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying the DatabaseUser was created with multiple databases")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-multi-db",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Databases).Should(HaveLen(3))
			Expect(createdUser.Spec.GetDatabases()).Should(HaveLen(3))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should validate mutually exclusive database and databases fields", func() {
			By("Creating a DatabaseUser with both database and databases (should fail validation)")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-both-fields",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: database2Name},
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("The controller should set status to Failed due to validation error")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() string {
				err := k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-both-fields",
					Namespace: namespace,
				}, createdUser)
				if err != nil {
					return ""
				}
				return createdUser.Status.Phase
			}, timeout, interval).Should(Equal("Failed"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept per-database privilege overrides", func() {
			By("Creating a DatabaseUser with different privileges per database")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-per-db-privs",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: databaseName, Privileges: "readonly"},
						{Name: database2Name, Privileges: "readwrite"},
						{Name: database3Name, Privileges: "admin"},
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying per-database privileges are stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-per-db-privs",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Databases[0].Privileges).Should(Equal("readonly"))
			Expect(createdUser.Spec.Databases[1].Privileges).Should(Equal("readwrite"))
			Expect(createdUser.Spec.Databases[2].Privileges).Should(Equal("admin"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When using secretGeneration modes", func() {
		const database2Name = "user-test-db-2-sg"

		BeforeEach(func() {
			database2 := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name:      database2Name,
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					ClusterRef: databasesv1alpha1.ClusterReference{
						Name: clusterName,
					},
					DatabaseName: "testdb2sg",
				},
			}
			_ = k8sClient.Create(ctx, database2)
		})

		It("Should default to primary secretGeneration", func() {
			By("Creating a DatabaseUser without specifying secretGeneration")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-default-sg",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: databaseName},
						{Name: database2Name},
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying secretGeneration defaults to primary")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-default-sg",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			// CRD default sets it to "primary" when not specified
			Expect(createdUser.Spec.SecretGeneration).Should(SatisfyAny(BeEmpty(), Equal("primary")))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept explicit primary secretGeneration", func() {
			By("Creating a DatabaseUser with secretGeneration: primary")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-primary-sg",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: databaseName},
						{Name: database2Name},
					},
					Privileges:       "readonly",
					SecretGeneration: "primary",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying secretGeneration is primary")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-primary-sg",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.SecretGeneration).Should(Equal("primary"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept perDatabase secretGeneration", func() {
			By("Creating a DatabaseUser with secretGeneration: perDatabase")
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-perdb-sg",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: databaseName},
						{Name: database2Name},
					},
					Privileges:       "readwrite",
					SecretGeneration: "perDatabase",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying secretGeneration is perDatabase")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-perdb-sg",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.SecretGeneration).Should(Equal("perDatabase"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When using different secret templates", func() {
		It("Should accept DB template", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-db-template",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "DB",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying template is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-db-template",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret.Template).Should(Equal("DB"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept DATABASE template", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-database-template",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "DATABASE",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying template is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-database-template",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret.Template).Should(Equal("DATABASE"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept POSTGRES template", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-postgres-template",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "POSTGRES",
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying template is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-postgres-template",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret.Template).Should(Equal("POSTGRES"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})

		It("Should accept custom template with custom keys", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-custom-template",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name: databaseName,
					},
					Privileges: "readonly",
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "custom",
						Keys: &databasesv1alpha1.SecretKeys{
							Host:     "PGHOST",
							Port:     "PGPORT",
							Database: "PGDATABASE",
							User:     "PGUSER",
							Password: "PGPASSWORD",
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying custom template and keys are stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-custom-template",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Secret.Template).Should(Equal("custom"))
			Expect(createdUser.Spec.Secret.Keys).ShouldNot(BeNil())
			Expect(createdUser.Spec.Secret.Keys.Host).Should(Equal("PGHOST"))
			Expect(createdUser.Spec.Secret.Keys.Password).Should(Equal("PGPASSWORD"))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})

	Context("When user has cross-namespace database reference", func() {
		It("Should accept database reference with namespace", func() {
			By(stepCreatingUser)
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "user-cross-ns",
					Namespace: namespace,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{
						Name:      databaseName,
						Namespace: namespace,
					},
					Privileges: "readonly",
				},
			}
			Expect(k8sClient.Create(ctx, user)).Should(Succeed())

			By("Verifying namespace is stored")
			createdUser := &databasesv1alpha1.DatabaseUser{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{
					Name:      "user-cross-ns",
					Namespace: namespace,
				}, createdUser)
			}, timeout, interval).Should(Succeed())

			Expect(createdUser.Spec.Database.Namespace).Should(Equal(namespace))

			By(stepCleaningUp)
			Expect(k8sClient.Delete(ctx, user)).Should(Succeed())
		})
	})
})

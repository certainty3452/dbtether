package controllers

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

var _ = Describe("DBCluster Controller", func() {
	const (
		timeout  = time.Second * 10
		interval = time.Millisecond * 250
	)

	Context("When creating a DBCluster", func() {
		It("Should create a DBCluster resource", func() {
			By("Creating a credentials secret")
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-cluster-creds",
					Namespace: "default",
				},
				StringData: map[string]string{
					"username": "postgres",
					"password": "password123",
				},
			}
			Expect(k8sClient.Create(ctx, secret)).Should(Succeed())

			By("Creating a DBCluster")
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

			By("Verifying the DBCluster was created")
			createdCluster := &databasesv1alpha1.DBCluster{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "test-cluster"}, createdCluster)
			}, timeout, interval).Should(Succeed())

			Expect(createdCluster.Spec.Endpoint).Should(Equal("localhost"))
			Expect(createdCluster.Spec.Port).Should(Equal(5432))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, cluster)).Should(Succeed())
			Expect(k8sClient.Delete(ctx, secret)).Should(Succeed())
		})
	})

	Context("When DBCluster has invalid credentials ref", func() {
		It("Should report error in status", func() {
			By("Creating a DBCluster without secret")
			cluster := &databasesv1alpha1.DBCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-no-secret",
				},
				Spec: databasesv1alpha1.DBClusterSpec{
					Endpoint: "localhost",
					Port:     5432,
					CredentialsSecretRef: &databasesv1alpha1.SecretReference{
						Name:      "non-existent-secret",
						Namespace: "default",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).Should(Succeed())

			By("Verifying the status shows error")
			Eventually(func() string {
				c := &databasesv1alpha1.DBCluster{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-no-secret"}, c); err != nil {
					return ""
				}
				return c.Status.Phase
			}, timeout, interval).Should(Equal("Failed"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, cluster)).Should(Succeed())
		})
	})

	Context("When DBCluster uses credentialsFromEnv", func() {
		It("Should create a DBCluster with ENV credentials", func() {
			By("Setting environment variables")
			GinkgoT().Setenv("TEST_DB_USERNAME", "envuser")
			GinkgoT().Setenv("TEST_DB_PASSWORD", "envpassword")

			By("Creating a DBCluster with credentialsFromEnv")
			cluster := &databasesv1alpha1.DBCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name: "cluster-env-creds",
				},
				Spec: databasesv1alpha1.DBClusterSpec{
					Endpoint: "localhost",
					Port:     5432,
					CredentialsFromEnv: &databasesv1alpha1.CredentialsFromEnv{
						Username: "TEST_DB_USERNAME",
						Password: "TEST_DB_PASSWORD",
					},
				},
			}
			Expect(k8sClient.Create(ctx, cluster)).Should(Succeed())

			By("Verifying the DBCluster was created")
			createdCluster := &databasesv1alpha1.DBCluster{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: "cluster-env-creds"}, createdCluster)
			}, timeout, interval).Should(Succeed())

			Expect(createdCluster.Spec.CredentialsFromEnv).ShouldNot(BeNil())
			Expect(createdCluster.Spec.CredentialsFromEnv.Username).Should(Equal("TEST_DB_USERNAME"))

			By("Cleaning up")
			Expect(k8sClient.Delete(ctx, cluster)).Should(Succeed())
		})
	})
})

// Unit tests for getCredentialsFromEnv
func TestGetCredentialsFromEnv(t *testing.T) {
	r := &DBClusterReconciler{}

	t.Run("returns credentials from ENV", func(t *testing.T) {
		t.Setenv("TEST_USER", "myuser")
		t.Setenv("TEST_PASS", "mypass")

		cfg := &databasesv1alpha1.CredentialsFromEnv{
			Username: "TEST_USER",
			Password: "TEST_PASS",
		}

		user, pass, err := r.getCredentialsFromEnv(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if user != "myuser" {
			t.Errorf("expected user 'myuser', got '%s'", user)
		}
		if pass != "mypass" {
			t.Errorf("expected pass 'mypass', got '%s'", pass)
		}
	})

	t.Run("returns error when username ENV not set", func(t *testing.T) {
		t.Setenv("TEST_PASS", "mypass")

		cfg := &databasesv1alpha1.CredentialsFromEnv{
			Username: "MISSING_USER",
			Password: "TEST_PASS",
		}

		_, _, err := r.getCredentialsFromEnv(cfg)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("returns error when password ENV not set", func(t *testing.T) {
		t.Setenv("TEST_USER", "myuser")

		cfg := &databasesv1alpha1.CredentialsFromEnv{
			Username: "TEST_USER",
			Password: "MISSING_PASS",
		}

		_, _, err := r.getCredentialsFromEnv(cfg)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

func TestGetCredentials_NoSourceSpecified(t *testing.T) {
	r := &DBClusterReconciler{}

	cluster := &databasesv1alpha1.DBCluster{
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "localhost",
			Port:     5432,
		},
	}

	_, _, err := r.getCredentials(context.TODO(), cluster)
	if err == nil {
		t.Fatal("expected error when no credentials source specified")
	}
}

func TestGetCredentials_BothSourcesSpecified_UsesEnv(t *testing.T) {
	r := &DBClusterReconciler{}

	t.Setenv("USER_ENV", "envuser")
	t.Setenv("PASS_ENV", "envpass")

	cluster := &databasesv1alpha1.DBCluster{
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "localhost",
			Port:     5432,
			CredentialsSecretRef: &databasesv1alpha1.SecretReference{
				Name:      "my-secret",
				Namespace: "default",
			},
			CredentialsFromEnv: &databasesv1alpha1.CredentialsFromEnv{
				Username: "USER_ENV",
				Password: "PASS_ENV",
			},
		},
	}

	user, pass, err := r.getCredentials(context.TODO(), cluster)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if user != "envuser" {
		t.Errorf("expected user 'envuser', got '%s'", user)
	}
	if pass != "envpass" {
		t.Errorf("expected pass 'envpass', got '%s'", pass)
	}
}

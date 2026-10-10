//go:build envtest

package controllers

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

var _ = Describe("Database cross-namespace consent", func() {
	const (
		namespace     = "default"
		userNamespace = "consent-team"
		clusterName   = "consent-cluster"
		timeout       = 10 * time.Second
		interval      = 250 * time.Millisecond
	)

	newDatabase := func(name string, allowed ...string) *databasesv1alpha1.Database {
		return &databasesv1alpha1.Database{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: databasesv1alpha1.DatabaseSpec{
				ClusterRef:        databasesv1alpha1.ClusterReference{Name: clusterName},
				AllowedNamespaces: allowed,
			},
		}
	}

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { _ = client.IgnoreNotFound(k8sClient.Delete(ctx, obj)) })
	}

	Context("admission", func() {
		DescribeTable("rejects invalid spec.allowedNamespaces",
			func(name string, allowed []string) {
				err := k8sClient.Create(ctx, newDatabase(name, allowed...))
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "got %v", err)
				Expect(err.Error()).To(ContainSubstring("spec.allowedNamespaces"))
			},
			Entry("uppercase", "consent-upper", []string{"Team-A"}),
			Entry("uppercase in pattern", "consent-upper-glob", []string{"Box-*"}),
			Entry("question mark", "consent-question", []string{"box-?"}),
			Entry("leading dash", "consent-leading-dash", []string{"-box"}),
			Entry("longer than a DNS label", "consent-long", []string{"a123456789b123456789c123456789d123456789e123456789f123456789g123"}),
			Entry("duplicate", "consent-duplicate", []string{"team-a", "team-a"}),
		)

		It("accepts DNS-1123 labels", func() {
			create(newDatabase("consent-valid", "team-a", "0-team"))
		})

		It("accepts the single wildcard entry", func() {
			create(newDatabase("consent-any", "*"))
		})

		DescribeTable("accepts patterns",
			func(name string, allowed []string) {
				create(newDatabase(name, allowed...))
			},
			Entry("prefix", "consent-prefix", []string{"box-*"}),
			Entry("wildcard mixed with names", "consent-wildcard-mixed", []string{"*", "ns-a"}),
			Entry("suffix with a name", "consent-suffix", []string{"*-dev", "x"}),
		)
	})

	Context("DatabaseUser in another namespace", func() {
		userStatus := func() databasesv1alpha1.DatabaseUserStatus {
			var user databasesv1alpha1.DatabaseUser
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "consent-user", Namespace: userNamespace}, &user); err != nil {
				return databasesv1alpha1.DatabaseUserStatus{}
			}
			return user.Status
		}
		setAllowed := func(db *databasesv1alpha1.Database, allowed ...string) {
			patch := client.MergeFrom(db.DeepCopy())
			db.Spec.AllowedNamespaces = allowed
			Expect(k8sClient.Patch(ctx, db, patch)).To(Succeed())
		}

		It("follows spec.allowedNamespaces edits without waiting for the periodic reverify", func() {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: userNamespace},
			}))).To(Succeed())
			create(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName + "-creds", Namespace: namespace},
				StringData: map[string]string{"username": "postgres", "password": "pw"},
			})
			create(&databasesv1alpha1.DBCluster{
				ObjectMeta: metav1.ObjectMeta{Name: clusterName},
				Spec: databasesv1alpha1.DBClusterSpec{
					Endpoint:             "localhost",
					Port:                 5432,
					CredentialsSecretRef: &databasesv1alpha1.SecretReference{Name: clusterName + "-creds", Namespace: namespace},
				},
			})
			db := newDatabase("consent-shared")
			create(db)
			Eventually(func() string {
				_ = k8sClient.Get(ctx, client.ObjectKeyFromObject(db), db)
				return db.Status.Phase
			}, timeout, interval).Should(Equal("Ready"))

			create(&databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "consent-user", Namespace: userNamespace},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "consent-shared", Namespace: namespace},
				},
			})
			denied := `database default/consent-shared does not list namespace "consent-team" in spec.allowedNamespaces`
			Eventually(userStatus, timeout, interval).Should(And(
				HaveField("Phase", "Failed"), HaveField("Message", denied)))

			setAllowed(db, userNamespace)
			Eventually(userStatus, timeout, interval).Should(HaveField("Phase", "Ready"))

			setAllowed(db)
			Eventually(userStatus, timeout, interval).Should(And(
				HaveField("Phase", "Failed"), HaveField("Message", denied), HaveField("Databases", BeEmpty())))
		})
	})
})

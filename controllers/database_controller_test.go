package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func newDatabaseTestReconciler(pgCache postgres.ClientCacheInterface, objs ...client.Object) *DatabaseReconciler {
	scheme := runtime.NewScheme()
	_ = databasesv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&databasesv1alpha1.Database{}).
		Build()

	return &DatabaseReconciler{
		Client:        fakeClient,
		Scheme:        scheme,
		PGClientCache: pgCache,
	}
}

const annotationForceAdopt = "dbtether.io/force-adopt"

func TestDatabaseReconciler_ShouldDropDatabase(t *testing.T) {
	tests := []struct {
		name           string
		deletionPolicy string
		wantDrop       bool
	}{
		{"Delete policy", "Delete", true},
		{"Retain policy", "Retain", false},
		{"Empty policy defaults to Retain", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &databasesv1alpha1.Database{
				Spec: databasesv1alpha1.DatabaseSpec{
					DeletionPolicy: tt.deletionPolicy,
				},
			}
			got := db.Spec.DeletionPolicy == "Delete"
			if got != tt.wantDrop {
				t.Errorf("shouldDrop = %v, want %v", got, tt.wantDrop)
			}
		})
	}
}

func TestDatabaseReconciler_GetDatabaseName(t *testing.T) {
	r := &DatabaseReconciler{}

	tests := []struct {
		name       string
		specDBName string
		metaName   string
		want       string
	}{
		{"uses spec.databaseName when set", "custom_db", "my-db", "custom_db"},
		{"falls back to metadata.name with dash conversion", "", "my-db", "my_db"},
		{"converts multiple dashes", "", "my-app-db", "my_app_db"},
		{"no conversion needed", "", "mydb", "mydb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.metaName,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					DatabaseName: tt.specDBName,
				},
			}
			got := r.getDatabaseName(db)
			if got != tt.want {
				t.Errorf("getDatabaseName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseReconciler_PendingTimeout(t *testing.T) {
	now := metav1.Now()
	fiveMinutesAgo := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	elevenMinutesAgo := metav1.NewTime(time.Now().Add(-11 * time.Minute))

	tests := []struct {
		name         string
		phase        string
		pendingSince *metav1.Time
		expectPhase  string
	}{
		{"first Pending - stays Pending", "Pending", nil, "Pending"},
		{"Pending for 5 minutes - stays Pending", "Pending", &fiveMinutesAgo, "Pending"},
		{"Pending for 11 minutes - Failed", "Pending", &elevenMinutesAgo, "Failed"},
		{"Waiting for 11 minutes - Failed", "Waiting", &elevenMinutesAgo, "Failed"},
		{"Ready phase - stays Ready", "Ready", &fiveMinutesAgo, "Ready"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase := simulateDBPendingTimeout(tt.phase, tt.pendingSince, &now)
			if phase != tt.expectPhase {
				t.Errorf("phase = %v, want %v", phase, tt.expectPhase)
			}
		})
	}
}

func simulateDBPendingTimeout(phase string, pendingSince, now *metav1.Time) string {
	if (phase == "Pending" || phase == "Waiting") && pendingSince != nil && now.Sub(pendingSince.Time) > PendingTimeout {
		return "Failed"
	}
	return phase
}

func TestDatabaseReconciler_StatusDatabaseName(t *testing.T) {
	r := &DatabaseReconciler{}

	tests := []struct {
		name       string
		specDBName string
		metaName   string
		wantStatus string
	}{
		{"status shows spec.databaseName when set", "custom_db", "my-db", "custom_db"},
		{"status shows derived name when spec empty", "", "my-db", "my_db"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.metaName,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					DatabaseName: tt.specDBName,
				},
			}

			// Simulate what setStatus does
			db.Status.DatabaseName = r.getDatabaseName(db)

			if db.Status.DatabaseName != tt.wantStatus {
				t.Errorf("status.databaseName = %v, want %v", db.Status.DatabaseName, tt.wantStatus)
			}
		})
	}
}

func TestDatabaseReconciler_ForceAdoptAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantForce   bool
	}{
		{
			name:        "no annotations",
			annotations: nil,
			wantForce:   false,
		},
		{
			name:        "empty annotations",
			annotations: map[string]string{},
			wantForce:   false,
		},
		{
			name:        "force-adopt true",
			annotations: map[string]string{annotationForceAdopt: "true"},
			wantForce:   true,
		},
		{
			name:        "force-adopt false",
			annotations: map[string]string{annotationForceAdopt: "false"},
			wantForce:   false,
		},
		{
			name:        "force-adopt other value",
			annotations: map[string]string{annotationForceAdopt: "yes"},
			wantForce:   false, // only "true" works
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

			forceAdopt := db.Annotations[annotationForceAdopt] == "true"
			if forceAdopt != tt.wantForce {
				t.Errorf("forceAdopt = %v, want %v", forceAdopt, tt.wantForce)
			}
		})
	}
}

func TestDatabaseReconciler_OwnershipTrackedStatus(t *testing.T) {
	tests := []struct {
		name          string
		tracked       bool
		statusTracked *bool
		shouldWarn    bool
	}{
		{
			name:          "first time not tracked - should warn",
			tracked:       false,
			statusTracked: nil,
			shouldWarn:    true,
		},
		{
			name:          "first time tracked - no warn",
			tracked:       true,
			statusTracked: nil,
			shouldWarn:    false,
		},
		{
			name:          "already warned (false in status) - no warn",
			tracked:       false,
			statusTracked: boolPtr(false),
			shouldWarn:    false,
		},
		{
			name:          "was tracked, now not - should warn",
			tracked:       false,
			statusTracked: boolPtr(true),
			shouldWarn:    true,
		},
		{
			name:          "still tracked - no warn",
			tracked:       true,
			statusTracked: boolPtr(true),
			shouldWarn:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Logic from reconcileDatabase
			shouldWarn := !tt.tracked && (tt.statusTracked == nil || *tt.statusTracked)
			if shouldWarn != tt.shouldWarn {
				t.Errorf("shouldWarn = %v, want %v", shouldWarn, tt.shouldWarn)
			}
		})
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func TestDatabaseReconciler_DeletionPolicyDelete_DropFailureKeepsFinalizer(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-creds", Namespace: "default"},
		Data:       map[string][]byte{"username": []byte("postgres"), "password": []byte("pw")},
	}
	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "localhost",
			Port:     5432,
			CredentialsSecretRef: &databasesv1alpha1.SecretReference{
				Name:      "cluster-creds",
				Namespace: "default",
			},
		},
	}
	db := &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "drop-fail-db",
			Namespace:         "default",
			Finalizers:        []string{FinalizerName},
			DeletionTimestamp: &now,
		},
		Spec: databasesv1alpha1.DatabaseSpec{
			ClusterRef:     databasesv1alpha1.ClusterReference{Name: "test-cluster"},
			DeletionPolicy: "Delete",
		},
	}

	pgCache := postgres.NewMockClientCache()
	pgCache.DefaultMock.ShouldFail = true
	pgCache.DefaultMock.FailError = errors.New("postgres unreachable")

	r := newDatabaseTestReconciler(pgCache, secret, cluster, db)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "drop-fail-db", Namespace: "default"}}
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("expected error when database drop fails")
	}

	var updated databasesv1alpha1.Database
	if err := r.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&updated, FinalizerName) {
		t.Error("finalizer should still be present after a failed drop")
	}
}

func TestDatabaseReconciler_DeletionPolicyDelete_ClusterNotFoundRemovesFinalizer(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	db := &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "orphan-cluster-db",
			Namespace:         "default",
			Finalizers:        []string{FinalizerName},
			DeletionTimestamp: &now,
		},
		Spec: databasesv1alpha1.DatabaseSpec{
			ClusterRef:     databasesv1alpha1.ClusterReference{Name: "gone-cluster"},
			DeletionPolicy: "Delete",
		},
	}

	r := newDatabaseTestReconciler(postgres.NewMockClientCache(), db)

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "orphan-cluster-db", Namespace: "default"}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updated databasesv1alpha1.Database
	err := r.Get(ctx, req.NamespacedName, &updated)
	if err == nil && controllerutil.ContainsFinalizer(&updated, FinalizerName) {
		t.Error("finalizer should be removed when the DBCluster CR is gone")
	}
}

func TestDatabaseReconciler_PersistOwnershipTracked(t *testing.T) {
	ctx := context.Background()
	db := &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "ownership-db", Namespace: "default"},
	}
	r := newDatabaseTestReconciler(nil, db)
	key := types.NamespacedName{Name: "ownership-db", Namespace: "default"}

	var fetched databasesv1alpha1.Database
	if err := r.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if err := r.persistOwnershipTracked(ctx, &fetched, false); err != nil {
		t.Fatalf("persistOwnershipTracked: %v", err)
	}

	var after databasesv1alpha1.Database
	if err := r.Get(ctx, key, &after); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if after.Status.OwnershipTracked == nil || *after.Status.OwnershipTracked {
		t.Fatal("expected OwnershipTracked=false to be persisted to the API server")
	}
}

func TestDatabaseReconciler_SetStatus_PendingSincePersistedAndTimesOut(t *testing.T) {
	ctx := context.Background()
	db := &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-db", Namespace: "default"},
	}
	r := newDatabaseTestReconciler(nil, db)
	key := types.NamespacedName{Name: "pending-db", Namespace: "default"}

	var fetched databasesv1alpha1.Database
	if err := r.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if _, err := r.setStatus(ctx, &fetched, "Pending", "waiting for DBCluster"); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	var afterFirst databasesv1alpha1.Database
	if err := r.Get(ctx, key, &afterFirst); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if afterFirst.Status.PendingSince == nil {
		t.Fatal("expected PendingSince to be persisted to the API server")
	}

	stale := metav1.NewTime(time.Now().Add(-(PendingTimeout + time.Minute)))
	patch := client.MergeFrom(afterFirst.DeepCopy())
	afterFirst.Status.PendingSince = &stale
	if err := r.Status().Patch(ctx, &afterFirst, patch); err != nil {
		t.Fatalf("failed to age PendingSince: %v", err)
	}

	if _, err := r.setStatus(ctx, &afterFirst, "Pending", "still waiting"); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	var afterTimeout databasesv1alpha1.Database
	if err := r.Get(ctx, key, &afterTimeout); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if afterTimeout.Status.Phase != "Failed" {
		t.Errorf("expected timeout to flip phase to Failed, got %s", afterTimeout.Status.Phase)
	}
	if afterTimeout.Status.PendingSince != nil {
		t.Error("expected PendingSince to be cleared after timeout->Failed transition")
	}
}

func TestDatabaseReconciler_SetStatus_PendingSinceClearsOnReady(t *testing.T) {
	ctx := context.Background()
	db := &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-to-ready-db", Namespace: "default"},
	}
	r := newDatabaseTestReconciler(nil, db)
	key := types.NamespacedName{Name: "pending-to-ready-db", Namespace: "default"}

	var fetched databasesv1alpha1.Database
	if err := r.Get(ctx, key, &fetched); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if _, err := r.setStatus(ctx, &fetched, "Pending", "waiting"); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	var afterPending databasesv1alpha1.Database
	if err := r.Get(ctx, key, &afterPending); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if afterPending.Status.PendingSince == nil {
		t.Fatal("expected PendingSince to be set while Pending")
	}

	if _, err := r.setStatus(ctx, &afterPending, "Ready", "database is ready"); err != nil {
		t.Fatalf("setStatus: %v", err)
	}

	var afterReady databasesv1alpha1.Database
	if err := r.Get(ctx, key, &afterReady); err != nil {
		t.Fatalf("failed to get database: %v", err)
	}
	if afterReady.Status.PendingSince != nil {
		t.Error("expected PendingSince to be nil once phase is Ready")
	}
}

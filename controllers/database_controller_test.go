package controllers

import (
	"testing"
	"time"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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

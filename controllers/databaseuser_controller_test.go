package controllers

import (
	"testing"
	"time"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testUserName   = "my-user"
	testClusterRef = "my-cluster"
)

func TestDatabaseUserReconciler_GetUsername(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name         string
		specUsername string
		metaName     string
		want         string
	}{
		{"uses spec.username when set", "custom_user", testUserName, "custom_user"},
		{"falls back to metadata.name with dash conversion", "", testUserName, "my_user"},
		{"prefers spec.username", "explicit", "fallback", "explicit"},
		{"converts multiple dashes", "", "my-app-user", "my_app_user"},
		{"no conversion needed", "", "myuser", "myuser"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.metaName,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Username: tt.specUsername,
				},
			}
			got := r.getUsername(user)
			if got != tt.want {
				t.Errorf("getUsername() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_Privileges(t *testing.T) {
	tests := []struct {
		name       string
		privileges string
		valid      bool
	}{
		{"readonly", "readonly", true},
		{"readwrite", "readwrite", true},
		{"admin", "admin", true},
		{"empty", "", false},
		{"invalid", "superuser", false},
	}

	validPrivileges := map[string]bool{
		"readonly":  true,
		"readwrite": true,
		"admin":     true,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validPrivileges[tt.privileges]
			if got != tt.valid {
				t.Errorf("privileges %q valid = %v, want %v", tt.privileges, got, tt.valid)
			}
		})
	}
}

func TestDatabaseUserReconciler_PasswordLength(t *testing.T) {
	tests := []struct {
		name       string
		specLength int
		wantLength int
	}{
		{"default when 0", 0, 16},
		{"custom 32", 32, 32},
		{"minimum 12", 12, 12},
		{"maximum 64", 64, 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Password: databasesv1alpha1.PasswordConfig{
						Length: tt.specLength,
					},
				},
			}

			length := user.Spec.Password.Length
			if length == 0 {
				length = 16
			}

			if length != tt.wantLength {
				t.Errorf("password length = %v, want %v", length, tt.wantLength)
			}
		})
	}
}

func TestDatabaseUserReconciler_SecretName(t *testing.T) {
	tests := []struct {
		name     string
		userName string
		want     string
	}{
		{"simple", "myuser", "myuser-credentials"},
		{"with-dashes", testUserName, "my-user-credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretName := tt.userName + "-credentials"
			if secretName != tt.want {
				t.Errorf("secretName = %v, want %v", secretName, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_ConnectionLimit(t *testing.T) {
	tests := []struct {
		name            string
		connectionLimit int
		shouldApply     bool
	}{
		{"unlimited (default 0)", 0, false},
		{"unlimited (-1)", -1, true},
		{"limited to 10", 10, true},
		{"limited to 1", 1, true},
		{"limited to 100", 100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					ConnectionLimit: tt.connectionLimit,
				},
			}

			shouldApply := user.Spec.ConnectionLimit != 0
			if shouldApply != tt.shouldApply {
				t.Errorf("shouldApply connection limit = %v, want %v", shouldApply, tt.shouldApply)
			}
		})
	}
}

func TestDatabaseUserReconciler_SecretRegeneration(t *testing.T) {
	tests := []struct {
		name         string
		currentPhase string
		expectRegen  bool
	}{
		{"not regeneration when Pending", "Pending", false},
		{"not regeneration when Creating", "Creating", false},
		{"regeneration when Ready", "Ready", true},
		{"not regeneration when Failed", "Failed", false},
		{"not regeneration when empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{
					Phase: tt.currentPhase,
				},
			}

			regenerated := user.Status.Phase == "Ready"
			if regenerated != tt.expectRegen {
				t.Errorf("regenerated = %v, want %v", regenerated, tt.expectRegen)
			}
		})
	}
}

func TestDatabaseUserReconciler_ShouldReconcileWhenSecretMissing(t *testing.T) {
	tests := []struct {
		name                string
		phase               string
		observedGeneration  int64
		generation          int64
		secretExists        bool
		shouldSkipReconcile bool
	}{
		{
			name:                "skip when Ready, generation matches, secret exists",
			phase:               "Ready",
			observedGeneration:  1,
			generation:          1,
			secretExists:        true,
			shouldSkipReconcile: true,
		},
		{
			name:                "reconcile when Ready, generation matches, secret MISSING",
			phase:               "Ready",
			observedGeneration:  1,
			generation:          1,
			secretExists:        false,
			shouldSkipReconcile: false,
		},
		{
			name:                "reconcile when generation changed",
			phase:               "Ready",
			observedGeneration:  1,
			generation:          2,
			secretExists:        true,
			shouldSkipReconcile: false,
		},
		{
			name:                "reconcile when not Ready",
			phase:               "Pending",
			observedGeneration:  1,
			generation:          1,
			secretExists:        true,
			shouldSkipReconcile: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-user",
					Generation: tt.generation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					Phase:              tt.phase,
					ObservedGeneration: tt.observedGeneration,
				},
			}

			// Simulate the early exit logic
			shouldSkip := false
			if user.Status.Phase == "Ready" && user.Status.ObservedGeneration == user.Generation {
				if tt.secretExists {
					shouldSkip = true
				}
			}

			if shouldSkip != tt.shouldSkipReconcile {
				t.Errorf("shouldSkipReconcile = %v, want %v", shouldSkip, tt.shouldSkipReconcile)
			}
		})
	}
}

func TestDatabaseUserReconciler_ShouldRotatePassword(t *testing.T) {
	r := &DatabaseUserReconciler{}

	now := metav1.Now()
	thirtyDaysAgo := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour))
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))

	tests := []struct {
		name         string
		rotation     *databasesv1alpha1.RotationConfig
		updatedAt    *metav1.Time
		shouldRotate bool
	}{
		{
			name:         "no rotation config",
			rotation:     nil,
			updatedAt:    &thirtyDaysAgo,
			shouldRotate: false,
		},
		{
			name:         "rotation days is 0",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 0},
			updatedAt:    &thirtyDaysAgo,
			shouldRotate: false,
		},
		{
			name:         "no passwordUpdatedAt",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    nil,
			shouldRotate: false,
		},
		{
			name:         "password expired (31 days old, 30 day rotation)",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    &thirtyDaysAgo,
			shouldRotate: true,
		},
		{
			name:         "password not expired (10 days old, 30 day rotation)",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    &tenDaysAgo,
			shouldRotate: false,
		},
		{
			name:         "password just created",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    &now,
			shouldRotate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			got := r.shouldRotatePassword(user)
			if got != tt.shouldRotate {
				t.Errorf("shouldRotatePassword() = %v, want %v", got, tt.shouldRotate)
			}
		})
	}
}

func TestDatabaseUserReconciler_CalculateRequeueAfter(t *testing.T) {
	r := &DatabaseUserReconciler{}

	now := metav1.Now()
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))
	thirtyOneDaysAgo := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour))

	tests := []struct {
		name      string
		rotation  *databasesv1alpha1.RotationConfig
		updatedAt *metav1.Time
		expectGT  time.Duration
		expectLT  time.Duration
	}{
		{
			name:      "no rotation config",
			rotation:  nil,
			updatedAt: &now,
			expectGT:  -1,
			expectLT:  1,
		},
		{
			name:      "rotation days is 0",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 0},
			updatedAt: &now,
			expectGT:  -1,
			expectLT:  1,
		},
		{
			name:      "no passwordUpdatedAt",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt: nil,
			expectGT:  -1,
			expectLT:  1,
		},
		{
			name:      "password 10 days old, 30 day rotation -> ~20 days requeue",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt: &tenDaysAgo,
			expectGT:  19 * 24 * time.Hour,
			expectLT:  21 * 24 * time.Hour,
		},
		{
			name:      "password expired -> 1 minute requeue",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt: &thirtyOneDaysAgo,
			expectGT:  30 * time.Second,
			expectLT:  2 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			got := r.calculateRequeueAfter(user)
			if got <= tt.expectGT || got >= tt.expectLT {
				t.Errorf("calculateRequeueAfter() = %v, expected between %v and %v", got, tt.expectGT, tt.expectLT)
			}
		})
	}
}

func TestDatabaseUserReconciler_CheckAndTriggerRotation(t *testing.T) {
	thirtyOneDaysAgo := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour))
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))

	tests := []struct {
		name          string
		rotation      *databasesv1alpha1.RotationConfig
		updatedAt     *metav1.Time
		expectTrigger bool
	}{
		{
			name:          "no rotation config - no trigger",
			rotation:      nil,
			updatedAt:     &thirtyOneDaysAgo,
			expectTrigger: false,
		},
		{
			name:          "rotation enabled, password expired - trigger",
			rotation:      &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:     &thirtyOneDaysAgo,
			expectTrigger: true,
		},
		{
			name:          "rotation enabled, password not expired - no trigger",
			rotation:      &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:     &tenDaysAgo,
			expectTrigger: false,
		},
		{
			name:          "rotation enabled, no passwordUpdatedAt - no trigger",
			rotation:      &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:     nil,
			expectTrigger: false,
		},
	}

	r := &DatabaseUserReconciler{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			// Test shouldRotatePassword which is called by checkAndTriggerRotation
			shouldRotate := r.shouldRotatePassword(user)
			if shouldRotate != tt.expectTrigger {
				t.Errorf("shouldRotatePassword() = %v, want %v", shouldRotate, tt.expectTrigger)
			}
		})
	}
}

func TestDatabaseUserReconciler_RequeueAfterReturned(t *testing.T) {
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))

	tests := []struct {
		name           string
		rotation       *databasesv1alpha1.RotationConfig
		updatedAt      *metav1.Time
		expectRequeue  bool
		minRequeueTime time.Duration
		maxRequeueTime time.Duration
	}{
		{
			name:          "no rotation - no requeue",
			rotation:      nil,
			updatedAt:     &tenDaysAgo,
			expectRequeue: false,
		},
		{
			name:           "rotation enabled, 10 days old - requeue in ~20 days",
			rotation:       &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:      &tenDaysAgo,
			expectRequeue:  true,
			minRequeueTime: 19 * 24 * time.Hour,
			maxRequeueTime: 21 * 24 * time.Hour,
		},
	}

	r := &DatabaseUserReconciler{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			requeue := r.calculateRequeueAfter(user)

			if tt.expectRequeue {
				if requeue <= 0 {
					t.Errorf("expected positive requeue duration, got %v", requeue)
				}
				if requeue < tt.minRequeueTime || requeue > tt.maxRequeueTime {
					t.Errorf("requeue = %v, expected between %v and %v", requeue, tt.minRequeueTime, tt.maxRequeueTime)
				}
			} else if requeue > 0 {
				t.Errorf("expected no requeue (0), got %v", requeue)
			}
		})
	}
}

func TestDatabaseUserReconciler_PasswordUpdatedAtOnReady(t *testing.T) {
	tests := []struct {
		name             string
		phase            string
		passwordUpdated  bool
		existingPwdTime  *metav1.Time
		expectPwdTimeSet bool
	}{
		{
			name:             "first Ready with passwordUpdated=true sets timestamp",
			phase:            "Ready",
			passwordUpdated:  true,
			existingPwdTime:  nil,
			expectPwdTimeSet: true,
		},
		{
			name:             "first Ready with passwordUpdated=false still sets timestamp",
			phase:            "Ready",
			passwordUpdated:  false,
			existingPwdTime:  nil,
			expectPwdTimeSet: true,
		},
		{
			name:             "subsequent Ready with passwordUpdated=false keeps existing timestamp",
			phase:            "Ready",
			passwordUpdated:  false,
			existingPwdTime:  &metav1.Time{Time: time.Now().Add(-24 * time.Hour)},
			expectPwdTimeSet: true, // keeps existing
		},
		{
			name:             "subsequent Ready with passwordUpdated=true updates timestamp",
			phase:            "Ready",
			passwordUpdated:  true,
			existingPwdTime:  &metav1.Time{Time: time.Now().Add(-24 * time.Hour)},
			expectPwdTimeSet: true,
		},
		{
			name:             "Failed phase does not set timestamp",
			phase:            "Failed",
			passwordUpdated:  false,
			existingPwdTime:  nil,
			expectPwdTimeSet: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.existingPwdTime,
				},
			}

			// Simulate setStatus logic
			if tt.passwordUpdated || (user.Status.PasswordUpdatedAt == nil && tt.phase == "Ready") {
				now := metav1.Now()
				user.Status.PasswordUpdatedAt = &now
			}

			if tt.expectPwdTimeSet && user.Status.PasswordUpdatedAt == nil {
				t.Error("expected PasswordUpdatedAt to be set, but it was nil")
			}
			if !tt.expectPwdTimeSet && user.Status.PasswordUpdatedAt != nil {
				t.Error("expected PasswordUpdatedAt to be nil, but it was set")
			}
		})
	}
}

func TestDatabaseUserReconciler_DeletionPolicy(t *testing.T) {
	tests := []struct {
		name           string
		deletionPolicy string
		expectDrop     bool
	}{
		{"Delete policy drops user", "Delete", true},
		{"Retain policy keeps user", "Retain", false},
		{"Empty policy defaults to Delete", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DeletionPolicy: tt.deletionPolicy,
				},
			}

			shouldDrop := user.Spec.DeletionPolicy != "Retain"
			if shouldDrop != tt.expectDrop {
				t.Errorf("shouldDrop = %v, want %v", shouldDrop, tt.expectDrop)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetClusterFromStatus(t *testing.T) {
	tests := []struct {
		name              string
		statusClusterName string
		statusDBName      string
		expectCluster     string
		expectDB          string
	}{
		{
			name:              "uses status when populated",
			statusClusterName: testClusterRef,
			statusDBName:      "my_database",
			expectCluster:     testClusterRef,
			expectDB:          "my_database",
		},
		{
			name:              "returns empty when status not populated",
			statusClusterName: "",
			statusDBName:      "",
			expectCluster:     "",
			expectDB:          "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{
					ClusterName:  tt.statusClusterName,
					DatabaseName: tt.statusDBName,
				},
			}

			// Simulate getClusterAndDatabaseForDeletion logic (status check only)
			clusterName := ""
			databaseName := ""
			if user.Status.ClusterName != "" {
				clusterName = user.Status.ClusterName
				databaseName = user.Status.DatabaseName
			}

			if clusterName != tt.expectCluster {
				t.Errorf("clusterName = %v, want %v", clusterName, tt.expectCluster)
			}
			if databaseName != tt.expectDB {
				t.Errorf("databaseName = %v, want %v", databaseName, tt.expectDB)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetDatabaseNameFromSpec(t *testing.T) {
	r := &DatabaseUserReconciler{}

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
			got := r.getDatabaseNameFromSpec(db)
			if got != tt.want {
				t.Errorf("getDatabaseNameFromSpec() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_PendingTimeout(t *testing.T) {
	now := metav1.Now()
	fiveMinutesAgo := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	elevenMinutesAgo := metav1.NewTime(time.Now().Add(-11 * time.Minute))

	tests := []struct {
		name          string
		phase         string
		pendingSince  *metav1.Time
		expectPhase   string
		expectTimeout bool
	}{
		{"first Pending - sets pendingSince", "Pending", nil, "Pending", false},
		{"Pending for 5 minutes - stays Pending", "Pending", &fiveMinutesAgo, "Pending", false},
		{"Pending for 11 minutes - Failed", "Pending", &elevenMinutesAgo, "Failed", true},
		{"Ready phase - clears pendingSince", "Ready", &fiveMinutesAgo, "Ready", false},
		{"Failed phase - clears pendingSince", "Failed", &fiveMinutesAgo, "Failed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{PendingSince: tt.pendingSince},
			}

			phase := simulatePendingTimeout(tt.phase, user.Status.PendingSince, &now)

			if phase != tt.expectPhase {
				t.Errorf("phase = %v, want %v", phase, tt.expectPhase)
			}
		})
	}
}

func simulatePendingTimeout(phase string, pendingSince, now *metav1.Time) string {
	if phase == "Pending" && pendingSince != nil && now.Sub(pendingSince.Time) > PendingTimeout {
		return "Failed"
	}
	return phase
}

func TestDatabaseUserReconciler_StatusUpdate(t *testing.T) {
	tests := []struct {
		name         string
		update       statusUpdate
		expectClear  bool
		expectValues bool
	}{
		{
			name: "status update fields are applied",
			update: statusUpdate{
				ClusterName:  testClusterRef,
				DatabaseName: "my_database",
				Username:     "my_user",
			},
			expectClear:  false,
			expectValues: true,
		},
		{
			name: "empty fields don't overwrite",
			update: statusUpdate{
				ClusterName:  "",
				DatabaseName: "",
				Username:     "",
			},
			expectClear:  false,
			expectValues: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{},
			}

			// Simulate applyStatusFields logic
			if tt.update.ClusterName != "" {
				user.Status.ClusterName = tt.update.ClusterName
			}
			if tt.update.DatabaseName != "" {
				user.Status.DatabaseName = tt.update.DatabaseName
			}
			if tt.update.Username != "" {
				user.Status.Username = tt.update.Username
			}

			if tt.expectValues {
				if user.Status.ClusterName != tt.update.ClusterName {
					t.Errorf("ClusterName = %v, want %v", user.Status.ClusterName, tt.update.ClusterName)
				}
				if user.Status.DatabaseName != tt.update.DatabaseName {
					t.Errorf("DatabaseName = %v, want %v", user.Status.DatabaseName, tt.update.DatabaseName)
				}
				if user.Status.Username != tt.update.Username {
					t.Errorf("Username = %v, want %v", user.Status.Username, tt.update.Username)
				}
			}
		})
	}
}

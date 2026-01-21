package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RestoreSource specifies where to restore from
type RestoreSource struct {
	// Reference to an existing Backup resource
	// +optional
	BackupRef *BackupReference `json:"backupRef,omitempty"`

	// LatestFrom automatically finds the latest successful backup for a database
	// +optional
	LatestFrom *LatestFromSource `json:"latestFrom,omitempty"`

	// Direct path to backup file in storage (e.g., "cluster/database/20260120-140000.sql.gz")
	// Requires storageRef to be set
	// +optional
	Path string `json:"path,omitempty"`

	// Reference to BackupStorage (required when using path)
	// +optional
	StorageRef *StorageReference `json:"storageRef,omitempty"`
}

// LatestFromSource specifies to use the latest backup from a database
type LatestFromSource struct {
	// Reference to the Database to find the latest backup for
	// +kubebuilder:validation:Required
	DatabaseRef DatabaseReference `json:"databaseRef"`

	// Namespace to search for backups (defaults to same namespace as Restore)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// BackupReference references a Backup resource
type BackupReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Namespace of the Backup (defaults to same namespace as Restore)
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// RestoreTarget specifies where to restore to
type RestoreTarget struct {
	// Reference to the Database to restore into
	// +kubebuilder:validation:Required
	DatabaseRef DatabaseReference `json:"databaseRef"`
}

// RestoreSpec defines the desired state of Restore
type RestoreSpec struct {
	// Source of the backup to restore from
	// +kubebuilder:validation:Required
	Source RestoreSource `json:"source"`

	// Target database to restore into
	// +kubebuilder:validation:Required
	Target RestoreTarget `json:"target"`

	// How to handle conflicts with existing data
	// - fail: Abort if database is not empty (default)
	// - drop: Drop and recreate the database before restore
	// - overwrite: Restore over existing data (may cause conflicts)
	// +kubebuilder:validation:Enum=fail;drop;overwrite
	// +kubebuilder:default=fail
	// +optional
	OnConflict string `json:"onConflict,omitempty"`

	// Auto-delete after completion
	// +optional
	TTLAfterCompletion *metav1.Duration `json:"ttlAfterCompletion,omitempty"`
}

// RestoreStatus defines the observed state of Restore
type RestoreStatus struct {
	// Current phase of the restore operation
	// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed
	Phase string `json:"phase,omitempty"`

	// Human-readable message about the current status
	Message string `json:"message,omitempty"`

	// Hash of spec to prevent accidental re-runs
	SpecHash string `json:"specHash,omitempty"`

	// Name of the Job created for this restore
	JobName string `json:"jobName,omitempty"`

	// Path of the backup file being restored
	SourcePath string `json:"sourcePath,omitempty"`

	// Duration of the restore operation
	Duration string `json:"duration,omitempty"`

	// RunID is a unique identifier for this restore run
	RunID string `json:"runId,omitempty"`

	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rst
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.databaseRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.status.duration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Restore is the Schema for the restores API
type Restore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RestoreSpec   `json:"spec,omitempty"`
	Status RestoreStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RestoreList contains a list of Restore
type RestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Restore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Restore{}, &RestoreList{})
}

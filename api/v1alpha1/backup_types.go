package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type BackupSpec struct {
	// Reference to the Database to backup
	// +kubebuilder:validation:Required
	DatabaseRef DatabaseReference `json:"databaseRef"`

	// Reference to the BackupStorage to use
	// +kubebuilder:validation:Required
	StorageRef StorageReference `json:"storageRef"`

	// Filename template for the backup file
	// Available: .DatabaseName, .Timestamp, .Random (6 chars lowercase alphanumeric)
	// +kubebuilder:default="{{ .Timestamp }}.sql.gz"
	// +optional
	FilenameTemplate string `json:"filenameTemplate,omitempty"`

	// Auto-delete after completion. Use with caution in GitOps environments!
	// +optional
	TTLAfterCompletion *metav1.Duration `json:"ttlAfterCompletion,omitempty"`
}

// StorageReference references a BackupStorage resource
type StorageReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

type BackupStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed
	Phase string `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	// Hash of spec to prevent accidental re-runs
	SpecHash string `json:"specHash,omitempty"`

	// Name of the Job created for this backup
	JobName string `json:"jobName,omitempty"`

	// Full path to the backup file
	Path string `json:"path,omitempty"`

	// Size of the backup file (human-readable)
	Size string `json:"size,omitempty"`

	// Duration of the backup operation
	Duration string `json:"duration,omitempty"`

	// RunID is a unique identifier for this backup run, used in job name and filename
	RunID string `json:"runId,omitempty"`

	StartedAt   *metav1.Time `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bkp
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.databaseRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.size`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.status.duration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Backup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupSpec   `json:"spec,omitempty"`
	Status BackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type BackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Backup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Backup{}, &BackupList{})
}

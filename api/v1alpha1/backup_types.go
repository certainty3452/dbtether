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

	// Opaque value that only feeds the spec hash; change it to run the backup again
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Trigger string `json:"trigger,omitempty"`

	// TTL of the Kubernetes Job after it finishes. Defaults to 1 hour
	// +optional
	TTLAfterCompletion *metav1.Duration `json:"ttlAfterCompletion,omitempty"`

	// Job configuration for backup execution
	// +optional
	JobConfig *BackupJobConfig `json:"jobConfig,omitempty"`
}

// BackupJobConfig configures the Kubernetes Job that performs the backup
type BackupJobConfig struct {
	// BackoffLimit specifies the number of retries before marking this backup failed.
	// Defaults to 3.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	// +kubebuilder:default=3
	// +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`

	// ActiveDeadlineSeconds specifies the duration in seconds relative to the startTime
	// that the backup job may be active before the system tries to terminate it.
	// This is a hard timeout for the entire backup operation.
	// +kubebuilder:validation:Minimum=60
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`

	// TTLSecondsAfterFailed specifies the TTL for the Job after it fails.
	// This allows keeping failed jobs longer for debugging.
	// Defaults to 12 hours (43200 seconds).
	// +optional
	TTLSecondsAfterFailed *int32 `json:"ttlSecondsAfterFailed,omitempty"`
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

	// Full path to the backup file produced by the current run
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

	// FailureReason provides a machine-readable failure reason (e.g., BackoffLimitExceeded, DeadlineExceeded)
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage provides human-readable details about the failure
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`

	// FailedAttempts is the number of failed pod attempts before the job failed
	// +optional
	FailedAttempts int32 `json:"failedAttempts,omitempty"`

	// LastPodName is the name of the last pod that ran for this backup (useful for log retrieval)
	// +optional
	LastPodName string `json:"lastPodName,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bkp
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.databaseRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.status.size`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.status.duration`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Observed",type=integer,JSONPath=`.status.observedGeneration`,priority=1

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

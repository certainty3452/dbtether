package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RetentionPolicy defines how long to keep backups
type RetentionPolicy struct {
	// Keep the last N backups regardless of age
	// +optional
	KeepLast *int `json:"keepLast,omitempty"`

	// Keep daily backups for N days (first backup of each day)
	// +optional
	KeepDaily *int `json:"keepDaily,omitempty"`

	// Keep weekly backups for N weeks (first backup of each week)
	// +optional
	KeepWeekly *int `json:"keepWeekly,omitempty"`

	// Keep monthly backups for N months (first backup of each month)
	// +optional
	KeepMonthly *int `json:"keepMonthly,omitempty"`
}

type BackupScheduleSpec struct {
	// Reference to the Database to backup
	// +kubebuilder:validation:Required
	DatabaseRef DatabaseReference `json:"databaseRef"`

	// Reference to the BackupStorage to use
	// +kubebuilder:validation:Required
	StorageRef StorageReference `json:"storageRef"`

	// Cron schedule in standard cron format (e.g., "0 2 * * *" for 2 AM daily)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(\S+\s+){4}\S+$`
	Schedule string `json:"schedule"`

	// Filename template for backup files (inherited by created Backups)
	// Available: .DatabaseName, .Timestamp, .RunID
	// +kubebuilder:default="{{ .Timestamp }}.sql.gz"
	// +optional
	FilenameTemplate string `json:"filenameTemplate,omitempty"`

	// Retention policy for automatic cleanup of old backups
	// +optional
	Retention *RetentionPolicy `json:"retention,omitempty"`

	// Suspend stops scheduling new backups (does not affect running backups)
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

type BackupScheduleStatus struct {
	// +kubebuilder:validation:Enum=Active;Suspended;Failed
	Phase string `json:"phase,omitempty"`

	Message string `json:"message,omitempty"`

	// Time of the last backup attempt
	LastBackupTime *metav1.Time `json:"lastBackupTime,omitempty"`

	// Name of the last successful backup
	LastSuccessfulBackup string `json:"lastSuccessfulBackup,omitempty"`

	// Next scheduled backup time
	NextScheduledTime *metav1.Time `json:"nextScheduledTime,omitempty"`

	// Number of backups managed by this schedule (in S3)
	ManagedBackups int `json:"managedBackups,omitempty"`

	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=bks
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.spec.databaseRef.name`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Backup",type=date,JSONPath=`.status.lastBackupTime`
// +kubebuilder:printcolumn:name="Next",type=string,format=date-time,JSONPath=`.status.nextScheduledTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type BackupSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupScheduleSpec   `json:"spec,omitempty"`
	Status BackupScheduleStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type BackupScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupSchedule{}, &BackupScheduleList{})
}

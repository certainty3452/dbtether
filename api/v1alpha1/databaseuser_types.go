package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DatabaseUserSpec struct {
	// Simple case: single database reference
	// Mutually exclusive with Databases
	// +optional
	Database *DatabaseAccess `json:"database,omitempty"`

	// Multiple databases: list of database references
	// Mutually exclusive with Database
	// +optional
	// +kubebuilder:validation:MinItems=1
	Databases []DatabaseAccess `json:"databases,omitempty"`

	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	Username string `json:"username,omitempty"`

	// Default privileges for all databases (can be overridden per-database)
	// +kubebuilder:validation:Enum=readonly;readwrite;admin
	// +kubebuilder:default=readonly
	Privileges string `json:"privileges,omitempty"`

	// +optional
	AdditionalGrants []TableGrant `json:"additionalGrants,omitempty"`

	// +optional
	Password PasswordConfig `json:"password,omitempty"`

	// +optional
	Rotation *RotationConfig `json:"rotation,omitempty"`

	// +optional
	// +kubebuilder:validation:Minimum=-1
	// +kubebuilder:default=-1
	ConnectionLimit int `json:"connectionLimit,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Delete
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// +optional
	Secret *SecretConfig `json:"secret,omitempty"`

	// SecretGeneration controls how secrets are created for multiple databases
	// - primary (default): single secret with first database
	// - perDatabase: separate secret for each database (same password, different database field)
	// +optional
	// +kubebuilder:validation:Enum=primary;perDatabase
	// +kubebuilder:default=primary
	SecretGeneration string `json:"secretGeneration,omitempty"`
}

// DatabaseAccess defines access to a single database
type DatabaseAccess struct {
	// Reference to Database resource
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Override default privileges for this database
	// +optional
	// +kubebuilder:validation:Enum=readonly;readwrite;admin
	Privileges string `json:"privileges,omitempty"`
}

// DatabaseReference is a reference to a Database resource (used by Backup, BackupSchedule, Restore)
type DatabaseReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// GetDatabases returns a unified list of databases from either Database or Databases field
func (s *DatabaseUserSpec) GetDatabases() []DatabaseAccess {
	if s.Database != nil {
		return []DatabaseAccess{*s.Database}
	}
	return s.Databases
}

// HasDatabases returns true if at least one database is configured
func (s *DatabaseUserSpec) HasDatabases() bool {
	return s.Database != nil || len(s.Databases) > 0
}

type TableGrant struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Tables []string `json:"tables"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinItems=1
	Privileges []string `json:"privileges"`
}

type PasswordConfig struct {
	// +kubebuilder:default=16
	// +kubebuilder:validation:Minimum=12
	// +kubebuilder:validation:Maximum=64
	Length int `json:"length,omitempty"`
}

type RotationConfig struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=365
	Days int `json:"days"`
}

type SecretConfig struct {
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=raw;DB;DATABASE;POSTGRES;custom
	// +kubebuilder:default=raw
	Template string `json:"template,omitempty"`

	// +optional
	Keys *SecretKeys `json:"keys,omitempty"`

	// +optional
	// +kubebuilder:validation:Enum=Fail;Adopt;Merge
	// +kubebuilder:default=Fail
	OnConflict string `json:"onConflict,omitempty"`
}

type SecretKeys struct {
	// +optional
	Host string `json:"host,omitempty"`
	// +optional
	Port string `json:"port,omitempty"`
	// +optional
	Database string `json:"database,omitempty"`
	// +optional
	User string `json:"user,omitempty"`
	// +optional
	Password string `json:"password,omitempty"`
}

type DatabaseUserStatus struct {
	// +kubebuilder:validation:Enum=Pending;Creating;Ready;Failed
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`

	// ClusterName is the name of the DBCluster this user belongs to
	ClusterName string `json:"clusterName,omitempty"`

	// Username is the PostgreSQL username
	Username string `json:"username,omitempty"`

	// Per-database access status
	Databases []DatabaseAccessStatus `json:"databases,omitempty"`

	// DatabasesSummary for printer column display (e.g., "db1 (+2)")
	// +optional
	DatabasesSummary string `json:"databasesSummary,omitempty"`

	// Primary secret name (for first database or single secret mode)
	SecretName        string       `json:"secretName,omitempty"`
	PasswordUpdatedAt *metav1.Time `json:"passwordUpdatedAt,omitempty"`
	PendingSince      *metav1.Time `json:"pendingSince,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
}

// DatabaseAccessStatus represents the status of access to a single database
type DatabaseAccessStatus struct {
	// Name of the Database resource
	Name string `json:"name"`

	// Namespace of the Database resource (empty if same as user)
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// DatabaseName is the actual PostgreSQL database name
	DatabaseName string `json:"databaseName,omitempty"`

	// Phase indicates the status of access to this database
	// +kubebuilder:validation:Enum=Pending;Ready;Failed
	Phase string `json:"phase,omitempty"`

	// Privileges granted on this database
	Privileges string `json:"privileges,omitempty"`

	// SecretName for this database (only set when secretGeneration=perDatabase)
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Message contains additional information about the status
	// +optional
	Message string `json:"message,omitempty"`
}

// GetDatabaseNames returns a comma-separated list of database names for display
func (s *DatabaseUserStatus) GetDatabaseNames() string {
	if len(s.Databases) == 0 {
		return ""
	}
	names := make([]string, len(s.Databases))
	for i, db := range s.Databases {
		names[i] = db.DatabaseName
	}
	result := ""
	for i, name := range names {
		if i > 0 {
			result += ","
		}
		result += name
	}
	return result
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.status.clusterName`
// +kubebuilder:printcolumn:name="Databases",type=string,JSONPath=`.status.databasesSummary`
// +kubebuilder:printcolumn:name="Username",type=string,JSONPath=`.status.username`
// +kubebuilder:printcolumn:name="Privileges",type=string,JSONPath=`.spec.privileges`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type DatabaseUser struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseUserSpec   `json:"spec,omitempty"`
	Status DatabaseUserStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DatabaseUserList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DatabaseUser `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DatabaseUser{}, &DatabaseUserList{})
}

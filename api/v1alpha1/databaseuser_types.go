package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DatabaseUserSpec struct {
	// +kubebuilder:validation:Required
	DatabaseRef DatabaseReference `json:"databaseRef"`

	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	Username string `json:"username,omitempty"`

	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=readonly;readwrite;admin
	Privileges string `json:"privileges"`

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
}

type DatabaseReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +optional
	Namespace string `json:"namespace,omitempty"`
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

type DatabaseUserStatus struct {
	// +kubebuilder:validation:Enum=Pending;Creating;Ready;Failed
	Phase              string       `json:"phase,omitempty"`
	Message            string       `json:"message,omitempty"`
	SecretName         string       `json:"secretName,omitempty"`
	PasswordUpdatedAt  *metav1.Time `json:"passwordUpdatedAt,omitempty"`
	PendingSince       *metav1.Time `json:"pendingSince,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	ClusterName        string       `json:"clusterName,omitempty"`
	DatabaseName       string       `json:"databaseName,omitempty"`
	Username           string       `json:"username,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.databaseName`
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

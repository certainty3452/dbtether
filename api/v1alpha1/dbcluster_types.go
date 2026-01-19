package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DBClusterSpec struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// +kubebuilder:default=5432
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int `json:"port,omitempty"`

	// +optional
	CredentialsSecretRef *SecretReference `json:"credentialsSecretRef,omitempty"`

	// +optional
	CredentialsFromEnv *CredentialsFromEnv `json:"credentialsFromEnv,omitempty"`
}

type SecretReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
}

// CredentialsFromEnv specifies environment variable names containing credentials.
// The operator reads these ENV vars at runtime from its own environment.
type CredentialsFromEnv struct {
	// Username is the name of the environment variable containing the database username
	// +kubebuilder:validation:Required
	Username string `json:"username"`

	// Password is the name of the environment variable containing the database password
	// +kubebuilder:validation:Required
	Password string `json:"password"`
}

type DBClusterStatus struct {
	// +kubebuilder:validation:Enum=Pending;Connected;Failed
	Phase              string             `json:"phase,omitempty"`
	Message            string             `json:"message,omitempty"`
	PostgresVersion    string             `json:"postgresVersion,omitempty"`
	LastCheckTime      metav1.Time        `json:"lastCheckTime,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=dbc
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.spec.endpoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.postgresVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type DBCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DBClusterSpec   `json:"spec,omitempty"`
	Status DBClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DBClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DBCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DBCluster{}, &DBClusterList{})
}

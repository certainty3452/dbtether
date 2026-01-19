package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type DatabaseSpec struct {
	// +kubebuilder:validation:Required
	ClusterRef ClusterReference `json:"clusterRef"`

	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:validation:MaxLength=63
	DatabaseName string `json:"databaseName,omitempty"`

	// +optional
	Extensions []string `json:"extensions,omitempty"`

	// +kubebuilder:validation:Enum=Delete;Retain
	// +kubebuilder:default=Retain
	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// +optional
	RevokePublicConnect bool `json:"revokePublicConnect,omitempty"`
}

type ClusterReference struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

type DatabaseStatus struct {
	// +kubebuilder:validation:Enum=Pending;Creating;Ready;Failed;Waiting;Deleting
	Phase              string             `json:"phase,omitempty"`
	Message            string             `json:"message,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
	DatabaseName       string             `json:"databaseName,omitempty"`
	PendingSince       *metav1.Time       `json:"pendingSince,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef.name`
// +kubebuilder:printcolumn:name="Database",type=string,JSONPath=`.status.databaseName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type Database struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatabaseSpec   `json:"spec,omitempty"`
	Status DatabaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type DatabaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Database `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Database{}, &DatabaseList{})
}

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// S3StorageConfig defines S3-specific configuration
type S3StorageConfig struct {
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// +kubebuilder:validation:Required
	Region string `json:"region"`

	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// GCSStorageConfig defines GCS-specific configuration
type GCSStorageConfig struct {
	// +kubebuilder:validation:Required
	Bucket string `json:"bucket"`

	// +kubebuilder:validation:Required
	Project string `json:"project"`
}

// AzureStorageConfig defines Azure Blob Storage-specific configuration
type AzureStorageConfig struct {
	// +kubebuilder:validation:Required
	Container string `json:"container"`

	// +kubebuilder:validation:Required
	StorageAccount string `json:"storageAccount"`
}

type BackupStorageSpec struct {
	// S3 storage configuration (mutually exclusive with gcs and azure)
	// +optional
	S3 *S3StorageConfig `json:"s3,omitempty"`

	// GCS storage configuration (mutually exclusive with s3 and azure)
	// +optional
	GCS *GCSStorageConfig `json:"gcs,omitempty"`

	// Azure Blob storage configuration (mutually exclusive with s3 and gcs)
	// +optional
	Azure *AzureStorageConfig `json:"azure,omitempty"`

	// Path template for directory structure
	// Available: .ClusterName, .DatabaseName, .Year, .Month, .Day
	// +kubebuilder:default="{{ .ClusterName }}/{{ .DatabaseName }}"
	// +optional
	PathTemplate string `json:"pathTemplate,omitempty"`

	// Optional: credentials secret reference. If not set, uses cloud-native auth (OIDC/Pod Identity)
	// +optional
	CredentialsSecretRef *SecretReference `json:"credentialsSecretRef,omitempty"`
}

type BackupStorageStatus struct {
	// +kubebuilder:validation:Enum=Ready;Failed
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`

	// Detected provider type (s3, gcs, azure)
	Provider string `json:"provider,omitempty"`

	// Last time the storage was validated
	LastValidation     metav1.Time `json:"lastValidation,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bs
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.status.provider`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type BackupStorage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupStorageSpec   `json:"spec,omitempty"`
	Status BackupStorageStatus `json:"status,omitempty"`
}

// GetProvider returns the provider type based on which config is set
func (b *BackupStorage) GetProvider() string {
	if b.Spec.S3 != nil {
		return "s3"
	}
	if b.Spec.GCS != nil {
		return "gcs"
	}
	if b.Spec.Azure != nil {
		return "azure"
	}
	return ""
}

// +kubebuilder:object:root=true

type BackupStorageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupStorage `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupStorage{}, &BackupStorageList{})
}

// Package config provides configuration loading for the dbtether operator.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DefaultConfigPath is the path where the ConfigMap is mounted in the operator pod.
const DefaultConfigPath = "/etc/dbtether/config.yaml"

// Config is the root configuration structure for the operator.
type Config struct {
	Backup BackupConfig `yaml:"backup"`
}

// BackupConfig contains configuration for backup jobs.
type BackupConfig struct {
	// MaxConcurrentPerCluster limits concurrent backup jobs per DBCluster
	MaxConcurrentPerCluster int `yaml:"maxConcurrentPerCluster"`

	// PodAnnotations are added to all backup job pods
	// Example: karpenter.sh/do-not-disrupt: "true"
	PodAnnotations map[string]string `yaml:"podAnnotations"`

	// PodLabels are added to all backup job pods (in addition to required labels)
	PodLabels map[string]string `yaml:"podLabels"`

	// JobLabels are added to backup Job objects (in addition to required labels)
	JobLabels map[string]string `yaml:"jobLabels"`

	// Resources for backup job pods
	Resources ResourcesConfig `yaml:"resources"`
}

// ResourcesConfig defines resource limits/requests for pods.
type ResourcesConfig struct {
	Limits   map[string]string `yaml:"limits"`
	Requests map[string]string `yaml:"requests"`
}

// ToK8sResources converts config to Kubernetes ResourceRequirements.
func (r *ResourcesConfig) ToK8sResources() corev1.ResourceRequirements {
	result := corev1.ResourceRequirements{}

	if len(r.Limits) > 0 {
		result.Limits = make(corev1.ResourceList)
		for k, v := range r.Limits {
			if q, err := resource.ParseQuantity(v); err == nil {
				result.Limits[corev1.ResourceName(k)] = q
			}
		}
	}

	if len(r.Requests) > 0 {
		result.Requests = make(corev1.ResourceList)
		for k, v := range r.Requests {
			if q, err := resource.ParseQuantity(v); err == nil {
				result.Requests[corev1.ResourceName(k)] = q
			}
		}
	}

	return result
}

// Load reads configuration from a YAML file.
func Load(path string) (*Config, error) {
	// #nosec G304 - path is from trusted DBTETHER_CONFIG_PATH env var (set by Helm)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	cfg := &Config{
		Backup: BackupConfig{
			MaxConcurrentPerCluster: 3, // default
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	return cfg, nil
}

// LoadOrDefault loads config from file or returns defaults if file doesn't exist.
func LoadOrDefault(path string) *Config {
	if path == "" {
		return defaultConfig()
	}

	cfg, err := Load(path)
	if err != nil {
		// Log warning but continue with defaults
		return defaultConfig()
	}

	return cfg
}

func defaultConfig() *Config {
	return &Config{
		Backup: BackupConfig{
			MaxConcurrentPerCluster: 3,
		},
	}
}

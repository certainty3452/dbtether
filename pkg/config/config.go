package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const DefaultConfigPath = "/etc/dbtether/config.yaml"

type Config struct {
	Backup BackupConfig `yaml:"backup"`
}

type BackupConfig struct {
	MaxConcurrentPerCluster int `yaml:"maxConcurrentPerCluster"`

	PodAnnotations map[string]string `yaml:"podAnnotations"`

	PodLabels map[string]string `yaml:"podLabels"`

	JobLabels map[string]string `yaml:"jobLabels"`

	Resources ResourcesConfig `yaml:"resources"`
}

type ResourcesConfig struct {
	Limits   map[string]string `yaml:"limits"`
	Requests map[string]string `yaml:"requests"`
}

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

func Load(path string) (*Config, error) {
	// #nosec G304 - path is from trusted DBTETHER_CONFIG_PATH env var (set by Helm)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	cfg := &Config{
		Backup: BackupConfig{
			MaxConcurrentPerCluster: 3,
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	return cfg, nil
}

func LoadOrDefault(path string) *Config {
	if path == "" {
		return defaultConfig()
	}

	cfg, err := Load(path)
	if err != nil {
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

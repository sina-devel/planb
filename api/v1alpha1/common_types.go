package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// S3Credentials points at a Secret (in the same namespace) holding S3 access keys.
type S3Credentials struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key holding the access key ID.
	// +kubebuilder:default=accessKey
	// +optional
	AccessKeyIDKey string `json:"accessKeyIdKey,omitempty"`
	// Key holding the secret access key.
	// +kubebuilder:default=secretKey
	// +optional
	SecretAccessKeyKey string `json:"secretAccessKeyKey,omitempty"`
}

// S3Spec describes a bucket on any S3-compatible service (AWS S3, MinIO, Ceph RGW, ...).
type S3Spec struct {
	// Endpoint host[:port], without scheme. e.g. s3.amazonaws.com or minio.minio.svc:9000
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`
	// Bucket name.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`
	// Prefix inside the bucket.
	// +optional
	Prefix string `json:"prefix,omitempty"`
	// Region of the bucket.
	// +optional
	Region string `json:"region,omitempty"`
	// Insecure uses plain HTTP instead of HTTPS.
	// +optional
	Insecure bool `json:"insecure,omitempty"`
	// SkipTLSVerify disables TLS certificate verification.
	// +optional
	SkipTLSVerify bool `json:"skipTLSVerify,omitempty"`
	// Credentials secret. When omitted, anonymous / IAM-provided credentials are used.
	// +optional
	Credentials *S3Credentials `json:"credentials,omitempty"`
}

// PostgresCredentials points at a Secret (in the same namespace) holding database credentials.
type PostgresCredentials struct {
	// Name of the Secret.
	Name string `json:"name"`
	// +kubebuilder:default=username
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`
	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// PostgresConnection describes how to reach a PostgreSQL server.
type PostgresConnection struct {
	// Host of the PostgreSQL server.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`
	// SSLMode passed to libpq (disable, allow, prefer, require, verify-ca, verify-full).
	// +kubebuilder:validation:Enum=disable;allow;prefer;require;verify-ca;verify-full
	// +kubebuilder:default=prefer
	// +optional
	SSLMode string `json:"sslMode,omitempty"`
	// Credentials secret.
	Credentials PostgresCredentials `json:"credentials"`
}

// JobOptions customise the Job that performs a backup or restore.
type JobOptions struct {
	// Image overrides the agent image (it must contain the planb binary and PostgreSQL client tools).
	// +optional
	Image string `json:"image,omitempty"`
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	// +optional
	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty"`
	// +kubebuilder:default=1
	// +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
}

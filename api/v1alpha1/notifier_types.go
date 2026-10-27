package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NotificationEvent is something a Notifier can be told about.
// +kubebuilder:validation:Enum=BackupCompleted;BackupFailed;RestoreCompleted;RestoreFailed
type NotificationEvent string

const (
	EventBackupCompleted  NotificationEvent = "BackupCompleted"
	EventBackupFailed     NotificationEvent = "BackupFailed"
	EventRestoreCompleted NotificationEvent = "RestoreCompleted"
	EventRestoreFailed    NotificationEvent = "RestoreFailed"
)

// SecretKeyRef selects a key of a Secret in the Notifier's namespace.
type SecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key in the Secret.
	Key string `json:"key"`
}

// +kubebuilder:validation:XValidation:rule="has(self.url) != has(self.urlSecret)",message="exactly one of url or urlSecret is required"
type WebhookNotifier struct {
	// URL receiving a JSON POST for every event.
	// +optional
	URL string `json:"url,omitempty"`
	// URLSecret reads the URL from a Secret, for URLs that embed a token.
	// +optional
	URLSecret *SecretKeyRef `json:"urlSecret,omitempty"`
	// Headers added to every request, e.g. Authorization.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}

// SlackNotifier posts to a Slack incoming webhook. Mattermost and Rocket.Chat accept the same format.
type SlackNotifier struct {
	// WebhookURL of the incoming webhook.
	WebhookURL SecretKeyRef `json:"webhookURL"`
	// Channel overrides the webhook's default channel (where supported).
	// +optional
	Channel string `json:"channel,omitempty"`
}

// TelegramNotifier sends messages through a Telegram bot.
type TelegramNotifier struct {
	// BotToken of the bot.
	BotToken SecretKeyRef `json:"botToken"`
	// ChatID of a user, group or channel (e.g. -1001234567890 or @channelname).
	// +kubebuilder:validation:MinLength=1
	ChatID string `json:"chatID"`
	// APIURL of the Bot API, for a self-hosted Bot API server or a reverse proxy.
	// +kubebuilder:default="https://api.telegram.org"
	// +optional
	APIURL string `json:"apiURL,omitempty"`
}

// EmailTLS is how the SMTP connection is secured.
// +kubebuilder:validation:Enum=StartTLS;TLS;None
type EmailTLS string

const (
	EmailStartTLS EmailTLS = "StartTLS"
	EmailTLSOn    EmailTLS = "TLS"
	EmailTLSNone  EmailTLS = "None"
)

// EmailNotifier sends mail through an SMTP server.
type EmailNotifier struct {
	// Host of the SMTP server.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +kubebuilder:default=587
	// +optional
	Port int32 `json:"port,omitempty"`
	// TLS: StartTLS (required upgrade, usually port 587), TLS (implicit, usually 465) or None.
	// +kubebuilder:default=StartTLS
	// +optional
	TLS EmailTLS `json:"tls,omitempty"`
	// SkipTLSVerify disables certificate verification.
	// +optional
	SkipTLSVerify bool `json:"skipTLSVerify,omitempty"`
	// From address.
	// +kubebuilder:validation:MinLength=1
	From string `json:"from"`
	// To addresses.
	// +kubebuilder:validation:MinItems=1
	To []string `json:"to"`
	// Credentials for SMTP AUTH (PLAIN). Omit for servers without authentication.
	// +optional
	Credentials *EmailCredentials `json:"credentials,omitempty"`
}

// EmailCredentials is a Secret with the SMTP username and password.
type EmailCredentials struct {
	// Name of the Secret.
	Name string `json:"name"`
	// +kubebuilder:default=username
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`
	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="[has(self.webhook), has(self.slack), has(self.telegram), has(self.email)].filter(x, x).size() == 1",message="exactly one of webhook, slack, telegram or email is required"
type NotifierSpec struct {
	// Events to notify about.
	// +kubebuilder:default={BackupFailed,RestoreFailed}
	// +kubebuilder:validation:MinItems=1
	// +optional
	Events []NotificationEvent `json:"events,omitempty"`
	// Selector limits notifications to Backups/Restores with matching labels,
	// e.g. {matchLabels: {planb.io/schedule: nightly}}. Empty matches everything in the namespace.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// Suspend stops sending notifications.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// +optional
	Webhook *WebhookNotifier `json:"webhook,omitempty"`
	// +optional
	Slack *SlackNotifier `json:"slack,omitempty"`
	// +optional
	Telegram *TelegramNotifier `json:"telegram,omitempty"`
	// +optional
	Email *EmailNotifier `json:"email,omitempty"`
}

// Type returns the configured channel.
func (s *NotifierSpec) Type() string {
	switch {
	case s.Webhook != nil:
		return "webhook"
	case s.Slack != nil:
		return "slack"
	case s.Telegram != nil:
		return "telegram"
	case s.Email != nil:
		return "email"
	}
	return ""
}

type NotifierPhase string

const (
	NotifierPhaseReady  NotifierPhase = "Ready"
	NotifierPhaseFailed NotifierPhase = "Failed"
)

type NotifierStatus struct {
	// Type of the configured channel.
	// +optional
	Type string `json:"type,omitempty"`
	// +optional
	Phase NotifierPhase `json:"phase,omitempty"`
	// Message describes the last failure, if any.
	// +optional
	Message string `json:"message,omitempty"`
	// LastSentTime of the last successful notification.
	// +optional
	LastSentTime *metav1.Time `json:"lastSentTime,omitempty"`
	// LastFailureTime of the last failed notification.
	// +optional
	LastFailureTime *metav1.Time `json:"lastFailureTime,omitempty"`
	// LastTestRequest is the value of the planb.io/test annotation that was last handled.
	// +optional
	LastTestRequest string `json:"lastTestRequest,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.status.type`
// +kubebuilder:printcolumn:name="Events",type=string,JSONPath=`.spec.events`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Sent",type=date,JSONPath=`.status.lastSentTime`

// Notifier sends a message when Backups or Restores in its namespace complete or fail.
type Notifier struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NotifierSpec   `json:"spec,omitempty"`
	Status NotifierStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NotifierList contains a list of Notifier.
type NotifierList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Notifier `json:"items"`
}

func init() {
	register(&Notifier{}, &NotifierList{})
}

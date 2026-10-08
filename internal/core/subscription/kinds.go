package subscription

import "github.com/trypando/pando/internal/adapter/api"

// Kind is one of Pando's own notifications — the ones it sends to particular
// people without anybody subscribing — and whether it reaches them unless they
// say otherwise (R-373).
type Kind struct {
	Kind        api.NotificationKind `json:"kind"`
	Label       string               `json:"label"`
	Description string               `json:"description"`
	DefaultOn   bool                 `json:"default_on"`
}

// Kinds is every notification Pando sends on its own, in the order the
// preferences screen lists them.
var Kinds = []Kind{
	{api.NotifyAppFailed, "An app failed", "An app you own failed and Pando stopped restarting it.", true},
	{api.NotifyDeployFailed, "A deploy failed", "A deploy you started, or of an app you own, failed.", true},
	{api.NotifyDeployApproval, "Deploy approvals", "A deploy is waiting for your approval, or one you asked for was answered.", true},
	{api.NotifyPolicyViolation, "Below the security minimum", "An app you own is below the installation's minimum security score.", true},
	{api.NotifyBackupFailed, "A backup failed", "A backup of an app you own was not taken.", true},
	{api.NotifyUpdateAvailable, "Pando updates", "A newer Pando is released. Sent to people who may upgrade it.", true},
	{api.NotifyUpgradeFailed, "Upgrade failed", "An in-place upgrade of Pando did not finish.", true},
	{api.NotifySubscriptionDisabled, "A subscription was turned off", "One of your event subscriptions kept failing and Pando turned it off.", true},
	{api.NotifyAuditSinkDisabled, "An audit destination was turned off", "A destination the audit log is sent to kept failing and Pando turned it off. Sent to people who may send the audit log off the installation.", true},

	// R-266: the launcher tile is the notification, so this is off unless a
	// person turns it on.
	{api.NotifyAppShared, "An app was shared with you", "Somebody gave you use of an app. It is in your launcher either way.", false},
}

// defaultOn is whether a kind reaches a person who has not chosen. A kind not
// in the list — one a later Pando added and this one does not know — is on,
// because a notification nobody can turn on is worse than one somebody turns
// off.
func defaultOn(kind api.NotificationKind) bool {
	for _, k := range Kinds {
		if k.Kind == kind {
			return k.DefaultOn
		}
	}
	return true
}

// IsKind reports whether kind is one Pando sends on its own.
func IsKind(kind string) bool {
	for _, k := range Kinds {
		if string(k.Kind) == kind {
			return true
		}
	}
	return false
}

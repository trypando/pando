package httpapi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR252_AHandoverSaysWhatItChanges asserts the sentence a handover of the
// default returns (issue #220): both adapters by name, and that apps keep
// what they were added with only where a spec names the adapter.
func TestR252_AHandoverSaysWhatItChanges(t *testing.T) {
	require.Equal(t, "This makes Cloudflare the default routing adapter in place of Localhost. "+
		"Apps already added keep the routing adapter they were added with; apps added from now on use Cloudflare.",
		defaultHandover("Cloudflare", "Localhost", "routing"))
	require.Equal(t, "This makes Main the default image registry adapter in place of Old.",
		defaultHandover("Main", "Old", "image_registry"))
	require.Equal(t, "This makes Splunk the default audit sink adapter in place of Syslog.",
		defaultHandover("Splunk", "Syslog", "audit_sink"))
	require.Equal(t, "This makes OpenAI the default AI adapter in place of Anthropic.",
		defaultHandover("OpenAI", "Anthropic", "ai"))

	require.Equal(t, "Localhost", orID("Localhost", "rte_loopback"))
	require.Equal(t, "rte_loopback", orID("", "rte_loopback"), "an adapter saved without a name is named by its ID")
}

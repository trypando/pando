package sinkkit

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR384_PrefixesReadAsAStringOrAList asserts R-384's action filters read
// both the form's comma-separated string and a script's JSON array, drop
// empty entries, and refuse anything else with an example of what works.
func TestR384_PrefixesReadAsAStringOrAList(t *testing.T) {
	for raw, want := range map[string]Prefixes{
		`"app.use, deploy., "`:   {"app.use", "deploy."},
		`["app.use", " grant."]`: {"app.use", "grant."},
		`""`:                     nil,
	} {
		var p Prefixes
		require.NoError(t, json.Unmarshal([]byte(raw), &p), raw)
		require.Equal(t, want, p, raw)
	}

	var p Prefixes
	err := json.Unmarshal([]byte(`{"app":true}`), &p)
	require.ErrorContains(t, err, "comma-separated list, such as app.use, deploy")
	err = json.Unmarshal([]byte(`7`), &p)
	require.ErrorContains(t, err, "comma-separated list")
}

// TestR384_CommonSettingsAreNormalized asserts R-384's shared settings: the
// defaults core is told, and a refusal for each value it cannot act on that
// names the adapter and the valid answers.
func TestR384_CommonSettingsAreNormalized(t *testing.T) {
	var c Common
	require.NoError(t, c.Normalize("syslog", 200))
	require.Equal(t, Common{Format: api.AuditFormatNative, Start: "oldest", MaxBatch: 200}, c)
	require.Equal(t, api.AuditSinkCapabilities{MaxBatch: 200, Format: api.AuditFormatNative, Transport: "syslog", Endpoint: "h:1"},
		c.Capabilities("syslog", "h:1"))

	c = Common{Format: api.AuditFormatOCSF, Start: "now", MaxBatch: 5, Actions: Prefixes{"grant."}, Exclude: Prefixes{"grant.view"}}
	require.NoError(t, c.Normalize("https", 500))
	require.Equal(t, api.AuditSinkCapabilities{MaxBatch: 5, Format: api.AuditFormatOCSF, Transport: "https", Endpoint: "e",
		Actions: []string{"grant."}, Exclude: []string{"grant.view"}, StartAtNow: true}, c.Capabilities("https", "e"))

	for _, tc := range []struct {
		c    Common
		want string
	}{
		{Common{Format: "cef"}, `https: "cef" is not an event format. Valid answers: native`},
		{Common{Start: "yesterday"}, `https: "yesterday" is not a starting point. Valid answers: oldest`},
		{Common{MaxBatch: -3}, "https: max_batch is -3. Set it to a positive number of events per delivery, such as 500"},
	} {
		require.ErrorContains(t, tc.c.Normalize("https", 500), tc.want)
	}
}

// TestR190_RefuseInlineKeepsCredentialsOutOfPlainConfig asserts R-190: a
// credential key at the top level of the unencrypted configuration is
// refused, by name; one under credentials is not; and configuration that is
// not an object is refused as unreadable.
func TestR190_RefuseInlineKeepsCredentialsOutOfPlainConfig(t *testing.T) {
	require.NoError(t, RefuseInline("https", nil, "token"))
	require.NoError(t, RefuseInline("https", json.RawMessage(`{"url":"https://h/","credentials":{"token":"x"}}`), "token"))

	err := RefuseInline("https", json.RawMessage(`{"url":"https://h/","token":"tk_value_123"}`), "token", "secret_url")
	require.ErrorContains(t, err, "https: token is in this adapter's stored configuration, which is unencrypted; set it as a credential instead")
	require.NotContains(t, err.Error(), "tk_value_123", "R-194: the refused value is never quoted")

	require.ErrorContains(t, RefuseInline("syslog", json.RawMessage(`["not","an","object"]`), "client_key"), "syslog: reading configuration")
}

// TestCertPoolReadsPEMOrTrustsTheSystem asserts an empty CA setting means the
// system's roots (a nil pool) and one with no certificate in it is refused
// with what a valid one looks like.
func TestCertPoolReadsPEMOrTrustsTheSystem(t *testing.T) {
	pool, err := CertPool("syslog", "  \n")
	require.NoError(t, err)
	require.Nil(t, pool)

	_, err = CertPool("syslog", "not a certificate")
	require.ErrorContains(t, err, "syslog: the CA certificate has no certificate Pando can read")
	require.ErrorContains(t, err, "-----BEGIN CERTIFICATE-----")
}

func TestFieldsDescribeTheCommonSettings(t *testing.T) {
	keys := map[string]string{}
	for _, f := range Fields(200) {
		keys[f.Key] = f.Default
	}
	require.Equal(t, map[string]string{"actions": "", "exclude": "", "format": api.AuditFormatNative, "max_batch": "200", "start": "oldest"}, keys)
}

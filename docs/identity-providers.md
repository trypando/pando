# Signing in with an identity provider

Pando signs people in with a local username and password, or through your organization's identity
provider over **OpenID Connect** or **SAML 2.0**, and accepts **SCIM 2.0** pushes so that accounts
and group memberships follow the provider. Everything here is also in the console, under
**Sign-in**; the API is at `/api/v1/identity-providers` and the CLI is `pando idp`.

The provider decides *who someone is* and *which of its groups they are in*. What anyone can do in
Pando is always decided in Pando (R-044, R-078): a provider cannot send a role.

- [Before you start](#before-you-start)
- [How it fits together](#how-it-fits-together)
- [Okta](#okta) · [Microsoft Entra ID](#microsoft-entra-id) · [Google Workspace](#google-workspace) ·
  [Keycloak](#keycloak) · [Authentik](#authentik) · [Any other provider](#any-other-provider)
- [SCIM provisioning](#scim-provisioning)
- [Groups](#groups)
- [Linking people who already have accounts](#linking-people-who-already-have-accounts)
- [Turning off password sign-in](#turning-off-password-sign-in)
- [Two-factor authentication on local accounts](#two-factor-authentication-on-local-accounts)
- [How quickly access ends](#how-quickly-access-ends)
- [When something goes wrong](#when-something-goes-wrong)

## Before you start

**Set `PANDO_SERVER_EXTERNAL_URL`** to the address people reach Pando at, such as
`https://pando.example.com`. The redirect URI, ACS URL and entity ID you register with a provider are
built from it, and must match to the character. Without it Pando uses the address of whoever is
looking at the Sign-in screen, which is fine on a laptop and wrong behind a proxy.

You need an account holding `install.adapters.manage` — the built-in Administrator role has it.

## How it fits together

1. **Add a provider.** It starts **off**, so nobody sees a button that does not work yet. Choose a
   preset for a known provider; it fills in what that provider needs and says where to find the rest.
2. **Register Pando with the provider**, using the addresses the provider's card shows:
   - OpenID Connect: the **redirect URI**, `…/api/v1/auth/providers/<id>/callback`.
   - SAML: the **ACS URL** (the same address) and the **entity ID**, which is also where Pando's SP
     metadata is published: `…/api/v1/auth/providers/<id>/metadata`.
3. **Test sign-in.** You sign in at the provider and come back to a report of every claim it sent, the
   identity, email, name and groups Pando read from them, and what a real sign-in would have done —
   reach an existing account, create one, or refuse, and why. Nobody is signed in.
4. **Turn it on.** The sign-in page shows a button for it. A sign-in can start on Pando's own address
   or on an app's own hostname (`/.pando/login`); either way the person comes back signed in where
   they started.

Changes take effect at the next sign-in. There is no restart.

**New people.** By default someone the provider signs in who has no Pando account is refused, and told
to ask an administrator, with the ID the administrator needs. Turn on **Create an account at someone's
first sign-in** (just-in-time provisioning) to make accounts instead; a new account has no access
until it is given some or is in a group that has some. Host policy's **Don't create accounts at first
sign-in** refuses it everywhere, whatever each provider says.

## Okta

**OpenID Connect**

1. In the Okta Admin Console, **Applications → Create App Integration → OIDC → Web Application**.
2. **Sign-in redirect URI**: Pando's redirect URI. **Assignments**: the people or groups who may use
   Pando.
3. To send groups: **Sign On → OpenID Connect ID Token → Groups claim type: Filter**, name `groups`,
   filter *Matches regex* `.*` (or narrower).
4. In Pando, add an OpenID Connect provider with the **Okta** preset. Issuer is
   `https://<your-okta-domain>` for the org authorization server, or
   `https://<your-okta-domain>/oauth2/default` for the default custom one. Paste the client ID and
   secret.

**SAML**

1. **Create App Integration → SAML 2.0**. **Single sign-on URL**: Pando's ACS URL. **Audience URI**:
   Pando's entity ID.
2. **Attribute statements**: `okta_user_id` → `user.getInternalProperty("id")`, `email` →
   `user.email`, `displayName` → `user.displayName`. **Group attribute statement**: `groups`,
   *Matches regex* `.*`.
3. Copy the app's **Metadata URL** into Pando, with the **Okta** SAML preset.

The preset identifies people by `okta_user_id`, Okta's user ID, rather than the NameID: Okta's
Persistent NameID is the application username, which changes when the username does. It also does not
treat the email addresses Okta sends as verified, since Okta can let people edit theirs.

**SCIM**: in the app's **General** settings enable **SCIM provisioning**; under **Provisioning →
Integration**, set the **SCIM connector base URL** to Pando's SCIM base URL, **Unique identifier field
for users** to `userName`, **Authentication Mode** to *HTTP Header* with Pando's SCIM token, and tick
*Push New Users*, *Push Profile Updates* and *Push Groups*. Then **To App**: enable *Create*, *Update*
and *Deactivate Users*. Pando matches pushed people by `externalId`, which Okta sets to its user ID —
the same ID it sends as `sub` over OIDC and as `okta_user_id` over SAML. For SAML, set the provider's
**SCIM identity attribute** to `externalId`, since SAML's default is `userName`.

## Microsoft Entra ID

**OpenID Connect**

1. **App registrations → New registration**. **Redirect URI**: *Web*, Pando's redirect URI.
2. **Certificates & secrets → New client secret**; copy its value.
3. **Token configuration → Add groups claim**: *Security groups*, ID token *Group ID*. Entra sends
   groups as object IDs; they appear in Pando as synced groups named by ID, so link each to a Pando
   group to give it a name.
4. **API permissions**: keep the delegated Microsoft Graph permission **User.Read**, which the preset
   asks for in its scopes.
5. In Pando, use the **Microsoft Entra ID** preset: issuer
   `https://login.microsoftonline.com/<tenant-id>/v2.0` (tenant-specific — `common` will not verify),
   **subject claim `oid`**. Entra's `sub` differs per application, so `oid` is what stays the same.

When a person is in more than 200 groups, Entra leaves the groups out of the ID token, and Pando reads
them from Microsoft Graph with the sign-in's access token. If Graph cannot be asked, the sign-in is
refused with what to grant, rather than recording that the person is in no groups. To skip the
lookup, set the groups claim to *Groups assigned to the application*: the token then carries only
those, and stays under the limit.

Entra does not vouch for email addresses (there is no `email_verified` claim, and a user can have an
unverified `email`), so Pando never links an Entra identity to an existing account by email. Link
those by hand.

**SAML**: **Enterprise applications → New application → Create your own → SAML**. **Identifier
(Entity ID)**: Pando's entity ID. **Reply URL**: the ACS URL. Add a group claim under **Attributes &
Claims**; groups arrive as object IDs, so link each to a Pando group to name it. Paste the **App
Federation Metadata Url** into Pando with the **Microsoft Entra ID** SAML preset. Over SAML, Entra sends
a link instead of the groups when a person is in more than 150, and Pando refuses that sign-in: send
only groups assigned to the application, or use OpenID Connect.

**SCIM**: in the enterprise application, **Provisioning → Automatic**. **Tenant URL**: Pando's SCIM
base URL; **Secret Token**: Pando's SCIM token. Under **Mappings → Provision users**, map
**`objectId` → `externalId`**, so a pushed person and an OIDC sign-in (subject claim `oid`) are the
same identity; for SAML with the default UPN NameID, set the provider's SCIM identity attribute to
`userName` instead. Entra re-sends `active: true` every cycle; Pando only lifts a suspension Entra
made, never one an administrator made.

## Google Workspace

1. In Google Cloud, **APIs & Services → Credentials → Create OAuth client ID → Web application**, with
   Pando's redirect URI.
2. In Pando, use the **Google Workspace** preset and set **Google Workspace domain** to your domain.
   Without it, *anyone with a Google account* can pass Google's sign-in.

Google sends no groups over OpenID Connect, so give access through Pando groups, or use SAML
(below).

**SAML**: in the Admin console, **Apps → Web and mobile apps → Add custom SAML app**. **ACS URL** and
**Entity ID** are Pando's; **Name ID** *Primary email*. Map *Primary email* to `email` and **Group
membership** to `groups`. Download the IdP metadata and paste it into Pando with the **Google
Workspace** SAML preset.

## Keycloak

**OpenID Connect**: in the realm, **Clients → Create client**, OpenID Connect, **Client
authentication** on, **Valid redirect URIs**: Pando's redirect URI. Under the client's **Client
scopes → dedicated scope → Add mapper → Group Membership**, name `groups`, *Full group path* off.
Issuer: `https://<keycloak-host>/realms/<realm>`.

**SAML**: **Create client → SAML**, **Client ID**: Pando's entity ID, **Valid redirect URIs** and
**Assertion Consumer Service POST Binding URL**: the ACS URL. Under **Keys**, turn **Client signature
required** off (Pando's requests are not signed). Add a **Group list** mapper named `groups`. Metadata
URL: `https://<keycloak-host>/realms/<realm>/protocol/saml/descriptor`.

Pando's own tests run against Keycloak, over both protocols.

## Authentik

**OpenID Connect**: **Applications → Providers → Create → OAuth2/OpenID Provider**, confidential,
**Redirect URIs**: Pando's redirect URI, and a **Signing Key**, such as the self-signed certificate
Authentik creates; then an **Application** using it. Without a signing key Authentik signs ID tokens
with HS256, which Pando cannot check against a published key, so it refuses the provider. Issuer:
`https://<authentik-host>/application/o/<application-slug>/` — with the trailing slash. Authentik
sends `groups` in the `profile` scope.

**SAML**: **Create → SAML Provider**, **ACS URL** and **Audience** Pando's; download its metadata.

## Any other provider

Anything that speaks OpenID Connect with discovery, or SAML 2.0 with metadata, works. Leave the preset
as *Another provider* and set the claim or attribute names your provider uses; the test sign-in shows
exactly what it sends.

What Pando requires:

- **OpenID Connect**: the authorization code flow. Pando always uses PKCE (S256), a nonce and a state,
  verifies the ID token's signature, issuer, audience and expiry, and fills missing claims from the
  userinfo endpoint unless told not to.
- **SAML**: SP-initiated sign-in over HTTP-Redirect, with the response posted back. **A signature is
  required** on the response or the assertion; an unsigned response is refused whatever it says.
  Pando checks the issuer, audience, recipient, destination and `InResponseTo`, allows three minutes
  of clock difference either way, and accepts each assertion ID once. A transient NameID is refused:
  it changes at every sign-in. **IdP-initiated sign-in** (from the provider's dashboard) is off by
  default, because such a response is not tied to the browser presenting it; turn on **Allow sign-in
  from the provider's dashboard** only if you need it.

## SCIM provisioning

**Turn on SCIM** on a provider's card to get a SCIM base URL and a bearer token. The token is shown
once; *Replace SCIM token* retires it. It works only for SCIM, and only for that provider's people and
groups.

With SCIM on:

- **Users** pushed are created (turned on or not), updated, suspended and reactivated. **Deprovisioning
  suspends; it never deletes** (R-049), and a suspension ends the person's sessions at once (R-048). A
  `DELETE` suspends the account and removes it from the provider's groups; the account and its data
  stay.
- A suspension an **administrator** made is not lifted by the provider sending `active: true` again.
- An account the provider's sign-in already created is adopted rather than refused.
- **Group membership comes from SCIM**, not from sign-in claims, and changes take effect on the next
  request (R-079).
- The provider's revocation mode becomes *push*: removing someone at the provider ends their access
  within the two-minute window every revocation has (design 06 §3.1).

Which SCIM attribute is the sign-in identity is set per provider (**SCIM identity attribute**):
`externalId` (the default for OpenID Connect) or `userName` (the default for SAML). A pushed account
and a sign-in meet when that attribute holds the same value the provider signs the person in with.

Endpoints: `ServiceProviderConfig`, `ResourceTypes`, `Schemas`, `Users` and `Groups`, with `GET`,
`POST`, `PUT`, `PATCH` and `DELETE`, filters of the form `attribute eq "value"` on `userName`,
`externalId`, `emails`, `displayName` and `id`, and `startIndex`/`count` paging. Bulk, sorting,
ETags and password changes are not supported.

## Groups

Every group a provider names — in sign-in claims, or pushed by SCIM — is a **synced group** in Pando,
listed under **Groups and roles** with where it comes from. Its members are the provider's to change.
Give it an installation role or app access like any group.

To give a provider's group the access an existing Pando group already has, use **Provider groups** on
the Pando group: everyone in a linked provider group counts as a member, live, for as long as the
provider keeps them in it.

Without SCIM, a person's synced groups are set from their groups claim at every sign-in; a provider
that sends no groups claim leaves them in none of its groups.

## Linking people who already have accounts

Two identities from different providers are two accounts unless an administrator links them (R-045).
On an account's page, **Link identity** attaches a provider's identity to it — the provider's ID for the
person, which a test sign-in shows and which a refused sign-in tells the person. Linking adds a way in;
it never merges accounts. If the identity already reaches another account, **move it here** keeps that
account, suspended, as an alias: never deleted, because apps may hold data under its ID (O-1).

**Link to an existing account by verified email**, per provider, does this automatically at a first
sign-in — only when the provider vouches for the address (OpenID Connect's `email_verified`, or a SAML
provider marked **The provider's email addresses are verified**), only when exactly one active account
has it, and never onto an account that already has an identity from that provider. Leave it off for
any provider where people can set their own email.

## Turning off password sign-in

Once people sign in through a provider, turn password sign-in off with **Turn off password sign-in**
on the **Local users** card of the Sign-in screen, or the same switch under Policy — they are one host
policy setting, `disable_password_sign_in`, and changing it needs `install.policy.manage`. The sign-in
page then shows only the provider buttons, and a username and password are refused by the API as well. Pando refuses it while no provider is on, and
refuses turning off the last provider while it is set.

**Break glass**: if every provider becomes unreachable, whoever runs the installation can run, on the
host:

```
pando admin enable-password-sign-in
pando admin reset-password <username>    # if a password is also needed
```

Both work against the database directly and are recorded in the audit log.

## Two-factor authentication on local accounts

A local account can add an authenticator app, such as 1Password, Google Authenticator or Authy. Sign-in
then asks for the 6-digit code it shows after the password. Accounts from an identity provider get
their second factor from the provider instead.

**To set it up**, open **Your settings** and choose **Set up** under **Two-factor authentication**.
Scan the QR code with the app, or type in the key shown beside it, and enter the code the app shows.
Save the ten recovery codes Pando shows next: each one signs in once in place of a code, and Pando
does not show them again. Pando also offers this once, at a person's first sign-in. From the CLI,
`pando two-factor setup` does the same.

**To require it** on every local account, turn on **Require two-factor authentication for local
accounts** under Policy (`require_local_two_factor`, needs `install.policy.manage`). Anyone without it
is shown the setup page at their next request and can do nothing else until they finish. Nobody can
turn theirs off while the policy is on.

**If someone loses their phone and their recovery codes**, an administrator opens their account under
**Accounts** and chooses **Reset two-factor**, or runs `pando user reset-two-factor <user-id>`. That
signs them out everywhere. They sign in with their password, and set it up again if the policy
requires it. If nobody else can sign in, whoever runs the installation can run, on the host:

```
pando admin reset-two-factor <username>
```

## How quickly access ends

Each provider's card states it (R-047, R-050):

| Provider | Removing someone ends their access |
|---|---|
| Local accounts | within 2 minutes of suspending them in Pando |
| With SCIM | within 2 minutes of the provider pushing the change |
| Without SCIM | when their Pando session ends: the provider's **session length**, 12 hours by default |

Without SCIM, Pando does not hear from the provider between sign-ins. Shorten **Session length** if
that window is too long, or turn on SCIM.

## When something goes wrong

Every failed sign-in says why on the sign-in page, and is in the audit log as `session.denied` with the
reason. The test sign-in shows the same message for an administrator, with everything the provider
sent. The common ones:

- *The ID token … did not verify* — wrong issuer or client ID, or the clocks on Pando's host and the
  provider disagree.
- *The identity provider refused Pando's client credentials* — the client secret is wrong or expired.
- *…refused to complete the sign-in… Check that the redirect URI…* — the redirect URI registered with
  the provider is not exactly the one Pando shows.
- *The SAML response did not verify: …* — the reason follows: an unsigned response, a certificate
  that rolled over (Pando re-reads metadata URLs daily; **Check connection** re-reads now), or an
  audience or ACS URL that does not match.
- *…you do not have a Pando account yet* — just-in-time accounts are off; link the identity, push the
  person with SCIM, or turn them on.
- *That sign-in was started in a different browser* — the link that finishes a sign-in only works in
  the browser that started it, which is what stops someone signing you in as them.

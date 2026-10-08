# Licensing

Pando is licensed under the **GNU Affero General Public License, version 3**, and is also available
under a **commercial license**.

This page describes which one applies to you. [`LICENSE`](LICENSE) is the full AGPL text and is what
actually governs; this is a summary, not legal advice.

## Running Pando

Running Pando to host your own applications — for yourself, your team, your company, or your
customers — is covered by the AGPL at no cost and with no further obligation. You can modify it, run
the modified version, and keep those modifications private.

The AGPL's network clause (§13) applies to Pando itself, not to the applications Pando hosts.
Deploying your company's internal tools behind Pando does not place those tools under the AGPL.

## Contributing

Adapters are compiled into the binary rather than loaded as plugins, so new runtime, routing,
builder, secrets, services, identity or notification providers are contributed as pull requests.

Pando is dual-licensed, which works only if the maintainer can license every part of it under any
terms. Contributors therefore sign the [Contributor License Agreement](CONTRIBUTOR_AGREEMENT.md),
which is based on the Harmony Individual Contributor License Agreement:

- You keep the copyright in your contribution.
- You grant Benjamin Meeker, who maintains Pando, a perpetual, irrevocable license to use, change,
  sublicense, sell and transfer your contribution under any terms. The license passes to any
  successor, such as a company formed to hold Pando.
- Your contribution is always also available under the license Pando uses on the day you submit it,
  which is currently the AGPL-3.0.

You sign once, by posting a one-line comment on your first pull request. The **Contributor agreement**
check asks for it and records it.

If those terms don't work for you, raise it in the pull request before it is merged.

## Redistributing or reselling Pando

Under the AGPL, if you distribute Pando or operate a modified version as a service that other people
use, those users must be able to obtain the source of the version you are running, including your
modifications. This applies to Pando's source, not to the applications it hosts for you.

A commercial license removes that requirement. It covers:

- Operating Pando as a hosted service without publishing your modifications.
- Embedding Pando in a proprietary product distributed to customers.
- Shipping Pando inside an appliance or image under your own terms.
- Organizations whose policies or contracts prohibit copyleft dependencies.

Terms are negotiated case by case. Open an issue describing what you intend to do, or contact
[bemeek-io](https://github.com/bemeek-io).

## Which license applies

The AGPL, unless you hold a signed commercial agreement. No file in this repository is under
different terms from the rest, and nothing here is commercially licensed by default.

## Why AGPL first

Relicensing from AGPL to a permissive license is possible with the agreement of the copyright
holders. Going the other direction is not, because every copy already distributed stays permissive.
Starting with the AGPL keeps both options available, and the commercial license covers the cases the
AGPL does not.

## Third-party components

Dependencies retain their own licenses. Two ship as files in this repository rather than being
fetched at build time:

- **Webfonts** — Newsreader, Public Sans and IBM Plex Mono, in
  `.claude/skills/pando-design/assets/fonts/`, each under the SIL Open Font License 1.1. The license
  text and copyright notices are in
  [`OFL.txt`](.claude/skills/pando-design/assets/fonts/OFL.txt) alongside them.
- **Go and npm dependencies** — listed in `go.mod` and `console/package.json`.

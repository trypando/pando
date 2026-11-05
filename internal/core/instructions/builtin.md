# Building apps on Pando

Pando hosts apps from a repository, an uploaded folder or a published image. It signs people in,
puts every request through its own proxy, and runs each app from a spec it keeps. These are the
rules for writing an app that runs well here, and for doing it safely.

## Working through Pando's tools

- Create the app, let Pando detect how it runs, answer its questions, review the plan, then deploy.
  Read every question and plan note in full; each says what is missing and what answer is valid.
- A plan error (`PLAN_*`) blocks the deploy. Fix the cause in the app or the spec. Do not look for a
  way around it.
- A refusal from host policy or a permission check is a decision an administrator made. Tell the
  person you are working for what was refused and why. Do not try another route to the same action.
- The spec is the only record of how the app runs. Nothing is read from the repository at deploy
  time, so a change to how the app runs is a change to the spec.

## Sign-in and identity

- Pando signs people in before a request reaches the app. Do not build a login page, password
  storage or user accounts unless the person asks for them.
- Each request carries a signed JWT in `X-Pando-Assertion`. To know who is asking, verify it against
  the keys at `/.well-known/jwks.json` on Pando's address, and check that `aud` is this app's ID.
- Key a user's data on the `sub` claim. It does not change when a person's email does.
- `X-Pando-User`, `X-Pando-Email` and `X-Pando-Groups` are not verified. Use them for display, never
  to decide what someone may do.
- In an app shared publicly, every visitor's `sub` is `anonymous`. Set your own cookie if the app
  needs to tell visitors apart.
- A request without `X-Pando-Assertion` did not come through Pando. An app may refuse it.
- Pando answers `/.pando` on every hostname it serves, and removes cookies whose names begin with
  `pando_` before forwarding a request. Do not use that path or that cookie prefix.

## Configuration and secrets

- Listen on the port in the `PORT` environment variable, on `0.0.0.0`.
- Read configuration from environment variables. List each one in `.env.example` with a placeholder
  value: Pando reads that file to find what the app needs, and a URL scheme there
  (`postgres://`, `redis://`) tells it which service to provide.
- Never commit a secret, key, token or password, and never put one in the spec. Set secret values
  with Pando's secret tools.
- Never write a secret, token or full request header to the app's logs.

## Data

- An app's filesystem is replaced on every deploy. Store data only in a declared volume (a named
  volume in a compose file, or a volume added in Pando) or in a database.
- Use a database through its connection URL variable, such as `DATABASE_URL`, rather than a file
  inside the container.

## Network

- Outbound traffic may be limited by host policy. Honor `HTTP_PROXY` and `HTTPS_PROXY` when they are
  set; where egress is restricted, they are the only way out.
- Ask for the smallest set of outbound destinations the app needs.

## Writing secure code

- Validate every input on the server, including input from the app's own pages.
- Build database queries with parameters. Never concatenate input into SQL, shell commands, file
  paths or templates.
- Escape output for where it goes: HTML, attributes, URLs, JSON.
- Protect requests that change data from cross-site forgery: same-site cookies and a token, or a
  check of the `Origin` header.
- Never turn off TLS certificate checks, and never `eval` or deserialize untrusted input.
- Add only dependencies that exist, are maintained and are widely used. Check a package's name on its
  registry before adding it. Commit the lockfile.
- In a Dockerfile, run the app as a user other than root, and copy in only what it needs.
- Show users a short error message, and keep stack traces and internal details in the logs.

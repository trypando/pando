# Laravel apps get their own build plan, issue #58

Laravel 13 (`laravel/laravel` at `13.x`, the deploy QA case `gh-laravel`) did not deploy. Detection
read it as a PHP buildpack app and nixpacks planned it. nixpacks 1.41 installs PHP from a pinned Nix
package set older than the 8.3 that `composer.json` requires, and runs `composer install
--ignore-platform-reqs`, so the mismatch surfaced only when PHP parsed syntax it did not know:
`syntax error, unexpected token "{"`. Nothing in the log named a version.

Following the other language plans from issue #55 (`notes-deploy-qa-issue-55.md`), the builder now
plans a Laravel app itself, on official images (`internal/adapter/builder/buildkit/laravel.go`). The
plan is written into `.nixpacks/Dockerfile`, so it is stored in the spec, shown for review and
replayed like any generated plan (R-020). This note records each `[P]` default it sets and why.

## What counts as a Laravel app

An `artisan` file beside a `composer.json` whose `require` names `laravel/framework`. A package built
on Laravel has no `artisan` and is not one. Such an app is planned before its `package.json` is
looked at: the package.json builds the app's assets and is neither a static site nor a Node server.
Detection reads the repository as PHP for the same reason; it had said "Node.js project", because
`package.json` is first in the manifest list.

## PHP version `[P]`

The official `php:<minor>-apache` image, where the minor is the **newest published release that
`require.php` allows**: `^8.3` builds on 8.5, `~8.2.0` on 8.2, `>=8.1 <8.4` on 8.3. A constraint is
a floor the author tested from, and current releases are what dependencies resolve best against. A
`config.platform.php` in `composer.json` wins over `require.php`: it is the PHP the author resolves
dependencies for. With no constraint, or one nothing published satisfies, the newest release is
used and the platform check (below) says what did not fit. The published minors are 7.4 to 8.5.

Composer's constraint syntax is read by a small evaluator in `laravel.go` (caret, tilde with
Composer's own two-part meaning, comparisons, wildcards, hyphen ranges, `|` and `||`). Pando has no
semver dependency, and one written for npm would read `~8.2` differently from Composer.

## Composer, and a mismatch that names the versions (R-105)

Dependencies are installed in the official `composer:2` image `[P]`, because it carries `git` and
`unzip`, which the PHP image does not and which Composer needs to fetch a package. Its own PHP is not
the app's, so Composer is told the app's: the plan's first stage runs `php -r 'echo PHP_VERSION;'`
in the runtime image, and the install stage sets `platform.php` to that exact version before
`composer install --no-dev`. There is no `--ignore-platform-reqs`: a package that needs a newer PHP
fails the install with Composer's own message, which names the version required and the version
installed.

A locked install is the exception (issue #90). It resolves nothing, and Composer 2.10 checks the
lock against the PHP it runs on itself whatever `platform.php` says, so a lock capping PHP at 8.4
failed on the Composer image's 8.5 though the app runs on 8.4. With a `composer.lock` the install
also passes `--ignore-platform-req=php`, and the PHP version is judged by the runtime stage's
platform check below, against the PHP the app does run on.

Extensions are the one thing ignored at install (`--ignore-platform-req='ext-*'`), because the
Composer image's extensions are not the app's. They are checked where they are installed: the
runtime stage runs `composer check-platform-reqs --no-dev`, and on failure prints what that means —
which PHP Pando chose, from which `require.php`, and the two ways to fix it.

The autoloader is dumped in the runtime stage, with `--optimize`, so `post-autoload-dump` scripts
(`artisan package:discover`) run on the app's PHP.

## Extensions `[P]`

- Built into every official PHP image, so never installed: `ctype`, `curl`, `dom`, `fileinfo`,
  `mbstring`, `openssl`, `pdo_sqlite`, `tokenizer`, `xml` and the rest of the default build. Laravel's
  own requirements are all in this list.
- Always installed: `bcmath` and `pdo_mysql`. They are Laravel's documented requirements that are not
  built in and compile from PHP's source with no system library, and MySQL is the database a Laravel
  app is most often pointed at.
- Installed when needed: whatever `ext-*` the app's `composer.json` or the packages in its
  `composer.lock` require, and `pdo_pgsql` when `.env.example` chooses `DB_CONNECTION=pgsql`. This
  covers `intl`, `zip`, `gd`, `pgsql` and the others that need a Debian library, which is installed
  with `apt-get` first, and `redis`, `apcu`, `imagick` and the others that come from PECL. `intl`
  and `zip` are not installed unasked, overriding the issue's list: the skeleton requires neither,
  `zip` matters only to Composer (which runs in its own image), and each costs a system library and a
  compile on every build.
- `opcache` is compiled into PHP from 8.5 and is not built again there.
- An extension Pando has no recipe for is left for the platform check to name, rather than guessed at.

## Assets

When `package.json` has a `build` script (Laravel 11 and later ship Vite), the assets are built in
a stage on top of the app itself, after its extensions, vendor directory and autoloader, with Node
copied in from `node:<major>-slim` (the same Node version and install command as a Node app's
plan), and `public/build` is copied into the image. Vite plugins run artisan: the starter kits'
Wayfinder plugin runs `php artisan wayfinder:generate`, which failed with `php: not found` when the
assets were built on the Node image (issue #90). Tailwind scans the framework's views, and the
starter kits import Livewire and Flux styles out of `vendor/`.

## Serving `[P]`

Apache with `mod_php`, from the same `php:<minor>-apache` image, with `public/` as the document root,
`mod_rewrite` on and `AllowOverride All` so Laravel's own `public/.htaccess` routes every request
that is not a file to `index.php`. Apache over `php artisan serve` because the built-in server is a
development server; over nginx and php-fpm because the official image carries Apache and needs
nothing installed. It listens on 80 and the plan says so with `EXPOSE`, so detection proposes that
port.

`storage/` and `bootstrap/cache/` are created if missing and owned by `www-data`, the user Apache
runs as.

## Start: migrations, then Apache `[P]`

The start command runs `php artisan migrate --force --no-interaction`, hands `storage`,
`bootstrap/cache` and `database` back to `www-data`, and execs Apache. A new Laravel app keeps its
sessions and cache in the database, so until its tables exist every page is an error; migrating is
the step Laravel's own `setup` script and `create-project` run. With the default SQLite connection
the migrate command creates `database/database.sqlite`. A migration that fails stops the app from
starting, and the deploy shows its output.

That SQLite file is inside the container. No volume is inferred for it (R-201): the undeclared
persistence warning covers it, and a real deployment points `DB_*` at a database.

## Logs `[P]`

The image sets `LOG_CHANNEL=stderr`, so the app's log is its output, which Pando shows. Laravel's
default writes a file inside the container that nobody reads. An app that sets `LOG_CHANNEL` itself
keeps its own.

## `APP_KEY` is a required slot (R-132)

Laravel encrypts sessions and cookies with `APP_KEY` and answers every page with "No application
encryption key has been specified" without one. It is a secret the person deploying owns, so it is
asked for, as `SECRET_KEY_BASE` is for Rails, and not generated. Every Laravel `.env.example` names
it with no value; on its own that became an empty variable, unset at deploy. For a Laravel app an
empty declaration is replaced by the slot, and the slot's evidence says what a valid key is and how
to make one (`php artisan key:generate --show`). The rest of `.env.example` is read as before: a
name with no value is unset at deploy, so Laravel's own config defaults apply (production, debug off,
SQLite).

## Not addressed here

- **Deploy QA was not run for this change.** The sandbox it was written in refuses container network
  access, so no image that installs dependencies could be built. `gh-laravel` passing deploy QA
  without AI, the first *done when* of issue #58, is still to be confirmed. Its fixture also needs a
  valid key in `facts.slots.APP_KEY`: the harness fills an unanswered slot with 27 random characters,
  which Laravel refuses as a key, and a person would paste the output of `key:generate`.
- Laravel's trusted proxies are the app's configuration. Behind Pando's proxy on HTTPS, an app that
  does not trust it generates `http://` URLs.
- Queue workers and the scheduler are separate processes Pando does not start.

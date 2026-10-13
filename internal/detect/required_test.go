package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// requiredSlots is the required slots on a draft's primary workload, by key.
func requiredSlots(t *testing.T, d detect.Draft) map[string]spec.Slot {
	t.Helper()
	assembled := detect.Assemble("", spec.Source{}, d)
	primary, ok := assembled.PrimaryWorkload()
	require.True(t, ok)
	out := map[string]spec.Slot{}
	for _, e := range primary.Env {
		if e.SlotRef == nil {
			continue
		}
		if s, ok := assembled.Slot(*e.SlotRef); ok && s.Required {
			out[e.Key] = s
		}
	}
	return out
}

// envValues is the plain values on a draft's primary workload, by key.
func envValues(t *testing.T, d detect.Draft) map[string]string {
	t.Helper()
	assembled := detect.Assemble("", spec.Source{}, d)
	primary, ok := assembled.PrimaryWorkload()
	require.True(t, ok)
	out := map[string]string{}
	for _, e := range primary.Env {
		if e.Value != nil {
			out[e.Key] = *e.Value
		}
	}
	return out
}

func imageDraft() detect.Draft {
	return detect.Draft{Workloads: []spec.Workload{{Name: "web", Image: "example/app", Primary: true, Exposed: true}}}
}

// TestR132_AVariableATrialLogSaysIsMissingIsAskedFor asserts R-132.
//
// The app stopped and said which variable it needed, and the next thing the
// person deploying saw was a crash loop: nothing had asked for the value. Each
// log here is the shape a real app or published image printed; none is
// matched by knowing the app, only by the shape of the sentence.
func TestR132_AVariableATrialLogSaysIsMissingIsAskedFor(t *testing.T) {
	cases := []struct {
		name   string
		log    string
		want   []string
		format map[string]spec.SecretFormat
		not    []string
	}{
		{name: "quoted, is not set",
			log:  "Container running as root...\nEnvironment variable 'SECRET_KEY' is not set. Aborting...\n",
			want: []string{"SECRET_KEY"}, format: map[string]spec.SecretFormat{"SECRET_KEY": spec.FormatRandom}},
		{name: "must be set",
			log:  "2026-10-12 15:09:12 [ERROR]: Environment variable FLATNOTES_USERNAME must be set.\n",
			want: []string{"FLATNOTES_USERNAME"}},
		{name: "bare, is not set",
			log:  "API_TOKEN is not set. This service needs API_TOKEN to call its upstream API; set it and restart.\n",
			want: []string{"API_TOKEN"}},
		{name: "a list of them",
			log:  "Missing required environment variables: SECRET_KEY, ADMIN_PASSWORD. Set them before starting the app.\n",
			want: []string{"SECRET_KEY", "ADMIN_PASSWORD"}},
		{name: "or its _FILE form",
			log:  "main.go:39: failed to read shared secret: PS_SHARED_SECRET or PS_SHARED_SECRET_FILE must be set\n",
			want: []string{"PS_SHARED_SECRET"}, not: []string{"PS_SHARED_SECRET_FILE"}},
		{name: "you need to define",
			log: "You need to define the PGADMIN_DEFAULT_EMAIL and PGADMIN_DEFAULT_PASSWORD or " +
				"PGADMIN_DEFAULT_PASSWORD_FILE environment variables.\n",
			want: []string{"PGADMIN_DEFAULT_EMAIL", "PGADMIN_DEFAULT_PASSWORD"},
			not:  []string{"PGADMIN_DEFAULT_PASSWORD_FILE"}},
		{name: "docker run hint",
			log: "ERROR: CouchDB 3.0+ will no longer run in \"Admin Party\"\n       mode. You *MUST* specify an admin user and\n" +
				"       password, either via your own .ini file or with\n" +
				"       \"-e COUCHDB_USER=admin -e COUCHDB_PASSWORD=password\"\n       to set it via \"docker run\".\n",
			want: []string{"COUCHDB_USER", "COUCHDB_PASSWORD"}},
		{name: "a docker link variable is not asked for",
			log: "[ERROR] Missing JOOMLA_DB_HOST and MYSQL_PORT_3306_TCP environment variables.\n" +
				" Did you forget to --link some_mysql_container:mysql or set an external db\n with -e JOOMLA_DB_HOST=hostname:port?\n",
			want: []string{"JOOMLA_DB_HOST"}, not: []string{"MYSQL_PORT_3306_TCP"}},
		{name: "the next line names it",
			log: "-----\nThe application key is missing, halting init!\nYou can generate a key with: docker run -it --rm image appkey\n" +
				"And apply it to the APP_KEY environment variable.\n",
			want: []string{"APP_KEY"}, format: map[string]spec.SecretFormat{"APP_KEY": spec.FormatLaravelKey}},
		{name: "rails",
			log: "bin/rails aborted!\nArgumentError: Missing `secret_key_base` for 'production' environment, set this " +
				"string with `bin/rails credentials:edit` (ArgumentError)\n",
			want: []string{"SECRET_KEY_BASE"}, format: map[string]spec.SecretFormat{"SECRET_KEY_BASE": spec.FormatRandomLong}},
		{name: "laravel",
			log:  "local.ERROR: No application encryption key has been specified. {\"exception\":\"[object] (Illuminate\\\\Encryption\\\\MissingAppKeyException\n",
			want: []string{"APP_KEY"}, format: map[string]spec.SecretFormat{"APP_KEY": spec.FormatLaravelKey}},
		{name: "phoenix",
			log:  "** (RuntimeError) environment variable SECRET_KEY_BASE is missing.\nYou can generate one by calling: mix phx.gen.secret\n",
			want: []string{"SECRET_KEY_BASE"}, format: map[string]spec.SecretFormat{"SECRET_KEY_BASE": spec.FormatRandomLong}},
		{name: "envalid",
			log: "================================\n Missing environment variables:\n    JWT_SECRET: undefined\n" +
				" Exiting with error code 1\n================================\n",
			want: []string{"JWT_SECRET"}},
		{name: "adonisjs",
			log: "   EnvValidationException:\n   - Missing environment variable \"APP_KEY\"\n- Missing environment variable \"PORT\"\n" +
				"- Missing environment variable \"LOG_LEVEL\"\n",
			want: []string{"APP_KEY", "LOG_LEVEL"}, not: []string{"PORT"}},
		{name: "t3-env",
			log:  "❌ Invalid environment variables: { SUPPORT_EMAIL: [ 'Required' ], NEXT_PUBLIC_BASE_URL: [ 'Required' ] }\n",
			want: []string{"SUPPORT_EMAIL", "NEXT_PUBLIC_BASE_URL"}},
		{name: "django's deploy check",
			log: "SystemCheckError: System check identified some issues:\n\nWARNINGS:\n?: (security.W009) Your SECRET_KEY has less " +
				"than 50 characters, less than 5 unique characters, or it's prefixed with 'django-insecure-'\n",
			want: []string{"SECRET_KEY"}, format: map[string]spec.SecretFormat{"SECRET_KEY": spec.FormatDjangoKey}},
		{name: "a KeyError from os.environ",
			log: "  File \"/app/mysite/settings.py\", line 84, in <module>\n    'NAME': os.environ[\"PGDATABASE\"],\n" +
				"  File \"<frozen os>\", line 717, in __getitem__\nKeyError: 'PGDATABASE'\n",
			want: []string{"PGDATABASE"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			draft, _ := detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true, Log: tc.log})
			got := requiredSlots(t, draft)
			for _, key := range tc.want {
				slot, ok := got[key]
				require.True(t, ok, "%s is asked for; got %v", key, got)
				require.NotEmpty(t, slot.Evidence, "and says why")
				if f, ok := tc.format[key]; ok {
					require.Equal(t, f, slot.Format, "%s is generated in the shape the app accepts", key)
				}
			}
			for _, key := range tc.not {
				require.NotContains(t, got, key)
			}
		})
	}
}

// TestR107_ADatabaseTheLogNamesIsASlotAndOneItDoesNotIsNot asserts R-107 and
// R-131. A log naming the variable that was supposed to say where the
// database is has mentioned the database, so it becomes a slot typed by that
// name and can be provisioned. A refused connection that names nothing is the
// correct failure R-107 describes, and Pando does not invent one.
func TestR107_ADatabaseTheLogNamesIsASlotAndOneItDoesNotIsNot(t *testing.T) {
	draft, _ := detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true,
		Log: "level=INFO msg=\"The default value for DATABASE_URL is used\"\ndial tcp [::1]:5432: connect: connection refused\n"})
	got := requiredSlots(t, draft)
	require.Equal(t, spec.SlotPostgres, got["DATABASE_URL"].Type)

	draft, _ = detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true,
		Log: "Error 111 connecting to localhost:6379. Connection refused..\n" +
			"Failed to connect to redis using environment variable PAPERLESS_REDIS.\n"})
	got = requiredSlots(t, draft)
	require.Equal(t, spec.SlotRedis, got["PAPERLESS_REDIS"].Type)

	draft, _ = detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true,
		Log: "[2026-10-12 16:55:03] ERROR connect ECONNREFUSED 127.0.0.1:3306\n\"Unknown database error\"\n"})
	require.Empty(t, draft.Slots, "nothing named, nothing invented")
}

// TestR132_AnAppThatServesIsNotAskedForWhatItsLogMentions asserts R-132's
// other half, and O-4's restraint: an app that says a variable is unset and
// serves anyway has not said it needs it. Blocking its deploy on a value it
// runs without is the false block O-4 warns of.
func TestR132_AnAppThatServesIsNotAskedForWhatItsLogMentions(t *testing.T) {
	log := "WARN: SENTRY_DSN is not set\nlistening on :3000\n"
	draft, _ := detect.ApplyTrial(imageDraft(), nil, detect.Trial{
		Ran: true, Started: true, ObservedPorts: []int{3000}, Log: log})
	require.Empty(t, draft.Slots)

	// And one that stops, but says the missing value is fine, is not either.
	draft, _ = detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true,
		Log: "SENTRY_DSN is not set, error reporting disabled\npanic: open /data/db: no such file\n"})
	require.Empty(t, draft.Slots)
}

// TestR105_AnEmptyENVLineIsNotADetectionFailure asserts R-105: a Dockerfile is
// whatever the repository holds, and `ENV` with nothing after it — a trailing
// continuation leaves one — is read past rather than panicking detection.
func TestR105_AnEmptyENVLineIsNotADetectionFailure(t *testing.T) {
	rails := memSource{
		"Dockerfile":            "FROM ruby:3.4\nENV   \nENV RAILS_ENV=production\nEXPOSE 80\n",
		"Gemfile":               "gem 'rails'\n",
		"config/application.rb": "", "bin/rails": "",
	}
	result, err := auction().Run(context.Background(), rails)
	require.NoError(t, err)
	require.Contains(t, requiredSlots(t, result.Winner.Draft), "SECRET_KEY_BASE",
		"the rest of the Dockerfile is still read")
}

// TestR132_AFrameworkThatRequiresAKeyIsAskedForItHoweverItIsBuilt asserts
// R-132. The key a framework cannot start without is asked for whether the
// app is built by Railpack, from its own Dockerfile, or run from the image its
// maintainer publishes — and typed, so Generate makes one the app accepts.
func TestR132_AFrameworkThatRequiresAKeyIsAskedForItHoweverItIsBuilt(t *testing.T) {
	rails := memSource{
		"Dockerfile":            "FROM ruby:3.4\nENV RAILS_ENV=\"production\" \\\n    BUNDLE_DEPLOYMENT=\"1\"\nRUN SECRET_KEY_BASE_DUMMY=1 ./bin/rails assets:precompile\nEXPOSE 80\nCMD [\"./bin/thrust\", \"./bin/rails\", \"server\"]\n",
		"Gemfile":               "gem 'rails'\n",
		"config/application.rb": "", "bin/rails": "", "config/credentials.yml.enc": "x",
	}
	result, err := auction().Run(context.Background(), rails)
	require.NoError(t, err)
	require.Equal(t, spec.FormatRandomLong, requiredSlots(t, result.Winner.Draft)["SECRET_KEY_BASE"].Format)

	// The same repository, run from the image its maintainer publishes.
	job := &detect.Job{
		Auction:  detect.NewAuction(detect.DockerfileDetector{}),
		Registry: publishedProbe{images: []detect.PublishedImage{{Ref: "ghcr.io/acme/campfire:latest", Registry: "ghcr.io"}}},
		Runtime:  fixedTrial{ports: []int{80}},
	}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceGit, URL: "https://github.com/acme/campfire"}, rails)
	require.NoError(t, err)
	require.Equal(t, "registry", p.Winner.Detector)
	require.Contains(t, requiredSlots(t, p.Winner.Draft), "SECRET_KEY_BASE")

	// A Dockerfile that bakes the key into the image has answered it.
	baked := memSource{
		"Dockerfile": "FROM ruby\nENV SECRET_KEY_BASE=abc\nEXPOSE 80\n", "config/application.rb": "", "bin/rails": "",
	}
	result, err = auction().Run(context.Background(), baked)
	require.NoError(t, err)
	require.NotContains(t, requiredSlots(t, result.Winner.Draft), "SECRET_KEY_BASE")

	// Laravel, from a Dockerfile.
	laravel := memSource{
		"Dockerfile":    "FROM serversideup/php:8.4-fpm-nginx\nEXPOSE 8080\n",
		"artisan":       "#!/usr/bin/env php\n",
		"composer.json": `{"require": {"laravel/framework": "^11.0"}}`,
		".env.example":  "APP_NAME=Laravel\nAPP_KEY=\nDB_CONNECTION=sqlite\n",
	}
	result, err = auction().Run(context.Background(), laravel)
	require.NoError(t, err)
	slots := requiredSlots(t, result.Winner.Draft)
	require.Equal(t, spec.FormatLaravelKey, slots["APP_KEY"].Format)
	require.Equal(t, spec.SlotUnknown, slots["APP_KEY"].Type)

	// Phoenix: runtime.exs raises without it.
	phoenix := memSource{
		"mix.exs":    "defmodule Hello.MixProject do\nend\n",
		"Dockerfile": "FROM elixir\nEXPOSE 4000\n",
		"config/runtime.exs": "if config_env() == :prod do\n  secret_key_base =\n    System.get_env(\"SECRET_KEY_BASE\") ||\n" +
			"      raise \"\"\"\n      environment variable SECRET_KEY_BASE is missing.\n      \"\"\"\n" +
			"  database_url = System.fetch_env!(\"DATABASE_URL\")\nend\n",
	}
	result, err = auction().Run(context.Background(), phoenix)
	require.NoError(t, err)
	slots = requiredSlots(t, result.Winner.Draft)
	require.Equal(t, spec.FormatRandomLong, slots["SECRET_KEY_BASE"].Format)
	require.Equal(t, spec.SlotPostgres, slots["DATABASE_URL"].Type, "a database the app names is one it can be given (R-131)")

	// Symfony: the committed .env leaves APP_SECRET empty.
	symfony := memSource{
		"composer.json":    `{"require": {"symfony/framework-bundle": "^7"}}`,
		".env":             "APP_ENV=prod\nAPP_SECRET=\n",
		"public/index.php": "<?php\n",
	}
	result, err = auction().Run(context.Background(), symfony)
	require.NoError(t, err)
	require.Contains(t, requiredSlots(t, result.Winner.Draft), "APP_SECRET")

	// Strapi: its keys, named with a placeholder in .env.example and read
	// with no default by its config.
	strapi := memSource{
		"package.json": `{"scripts": {"start": "strapi start", "build": "strapi build"}, "dependencies": {"@strapi/strapi": "5.0.0"}}`,
		".env.example": "HOST=0.0.0.0\nPORT=1337\nAPP_KEYS=\"toBeModified1,toBeModified2\"\nAPI_TOKEN_SALT=tobemodified\n" +
			"ADMIN_JWT_SECRET=tobemodified\nTRANSFER_TOKEN_SALT=tobemodified\nJWT_SECRET=tobemodified\n",
		"config/server.js": "module.exports = ({ env }) => ({ host: env('HOST', '0.0.0.0'), app: { keys: env.array('APP_KEYS') } });\n",
	}
	result, err = auction().Run(context.Background(), strapi)
	require.NoError(t, err)
	slots = requiredSlots(t, result.Winner.Draft)
	for _, key := range []string{"APP_KEYS", "API_TOKEN_SALT", "ADMIN_JWT_SECRET", "TRANSFER_TOKEN_SALT", "JWT_SECRET"} {
		require.Contains(t, slots, key)
	}
	require.NotContains(t, slots, "HOST")
}

// TestR132_APhoenixReleaseIsStartedWithItsServerOn asserts R-132 for the value
// a Phoenix app needs and nobody has to be asked for: runtime.exs enables the
// endpoint only when PHX_SERVER is set, and a release started any way but
// bin/server ran without a web server (issue #90).
func TestR132_APhoenixReleaseIsStartedWithItsServerOn(t *testing.T) {
	phoenix := memSource{
		"mix.exs":    "defmodule Hello.MixProject do\nend\n",
		"Dockerfile": "FROM elixir\nEXPOSE 4000\n",
		"config/runtime.exs": "import Config\n\nif System.get_env(\"PHX_SERVER\") do\n" +
			"  config :hello, HelloWeb.Endpoint, server: true\nend\n",
	}
	result, err := auction().Run(context.Background(), phoenix)
	require.NoError(t, err)
	require.Equal(t, "true", envValues(t, result.Winner.Draft)["PHX_SERVER"])
	require.NotContains(t, requiredSlots(t, result.Winner.Draft), "PHX_SERVER", "nothing is asked")

	// An image that sets it already keeps its own.
	phoenix["Dockerfile"] = "FROM elixir\nENV PHX_SERVER=1\nEXPOSE 4000\n"
	result, err = auction().Run(context.Background(), phoenix)
	require.NoError(t, err)
	require.NotContains(t, envValues(t, result.Winner.Draft), "PHX_SERVER")
}

// TestR132_AnEnvironmentSchemaSaysWhatIsRequired asserts R-132. An app that
// validates its environment at start has written down what it cannot run
// without: a key with no default and nothing making it optional. A setting
// the template gives a real sample for takes it; a secret is asked for.
func TestR132_AnEnvironmentSchemaSaysWhatIsRequired(t *testing.T) {
	adonis := memSource{
		"Dockerfile":   "FROM node:22\nENV NODE_ENV=production\nEXPOSE 8080\nCMD [\"node\", \"./bin/server.js\"]\n",
		"package.json": `{"dependencies": {"@adonisjs/core": "^6"}}`,
		".env.example": "TZ=UTC\nPORT=3333\nHOST=localhost\nLOG_LEVEL=info\nAPP_KEY=\nNODE_ENV=development\nSESSION_DRIVER=cookie\n",
		"start/env.ts": "export default await Env.create(new URL('../', import.meta.url), {\n" +
			"  NODE_ENV: Env.schema.enum(['development', 'production', 'test'] as const),\n  PORT: Env.schema.number(),\n" +
			"  APP_KEY: Env.schema.string(),\n  HOST: Env.schema.string({ format: 'host' }),\n  LOG_LEVEL: Env.schema.string(),\n" +
			"  SENTRY_DSN: Env.schema.string.optional(),\n  SESSION_DRIVER: Env.schema.enum(['cookie', 'memory'] as const),\n})\n",
	}
	result, err := auction().Run(context.Background(), adonis)
	require.NoError(t, err)
	slots := requiredSlots(t, result.Winner.Draft)
	values := envValues(t, result.Winner.Draft)
	require.Contains(t, slots, "APP_KEY")
	require.NotContains(t, slots, "SENTRY_DSN", "optional is optional")
	require.NotContains(t, slots, "PORT", "Pando sets PORT itself")
	require.Equal(t, "0.0.0.0", values["HOST"], "a container listens everywhere, whatever the sample says")
	require.Equal(t, "info", values["LOG_LEVEL"], "a real sample is a default")
	require.Equal(t, "cookie", values["SESSION_DRIVER"])
	require.Empty(t, values["NODE_ENV"], "the Dockerfile sets it, so it is left as the image has it")
	require.NotContains(t, slots, "NODE_ENV")

	// envalid, in a compose service built from the repository.
	kutt := memSource{
		"docker-compose.yml": "services:\n  server:\n    build:\n      context: .\n    environment:\n      DB_FILENAME: \"/var/lib/kutt/data.sqlite\"\n    ports:\n      - 3000:3000\n",
		"Dockerfile":         "FROM node:22\nENV NODE_ENV=production\nEXPOSE 3000\n",
		"server/env.js": "const { cleanEnv, num, str, bool } = require(\"envalid\");\nconst spec = {\n" +
			"  PORT: num({ default: 3000 }),\n  DB_FILENAME: str({ default: \"db/data\" }),\n" +
			"  JWT_SECRET: str({ devDefault: \"securekey\" }),\n  MAIL_HOST: str({ default: \"\" }),\n};\n" +
			"module.exports = cleanEnv(process.env, spec);\n",
	}
	result, err = detect.NewAuction(detect.ComposeDetector{}).Run(context.Background(), kutt)
	require.NoError(t, err)
	slots = requiredSlots(t, result.Winner.Draft)
	require.Contains(t, slots, "JWT_SECRET", "devDefault does not apply in production")
	require.Len(t, slots, 1)

	// t3-env, in a service whose Dockerfile sits in a subdirectory.
	rallly := memSource{
		"docker-compose.yml": "services:\n  web:\n    build:\n      context: .\n      dockerfile: ./apps/web/Dockerfile\n" +
			"    environment:\n      - DATABASE_URL=https://example.com/db\n    ports:\n      - 3000:3000\n",
		"apps/web/Dockerfile": "FROM node:22\nEXPOSE 3000\n",
		"apps/web/src/env.ts": "import { createEnv } from \"@t3-oss/env-nextjs\";\nimport * as z from \"zod\";\n" +
			"export const env = createEnv({\n  server: {\n    DATABASE_URL: z.url(),\n    NODE_ENV: z\n      .enum([\"development\", \"production\"])\n" +
			"      .default(\"development\"),\n    SECRET_PASSWORD: z.string().min(32),\n    SMTP_HOST: z.string().optional(),\n" +
			"    SUPPORT_EMAIL: z.email(),\n  },\n  client: {\n    NEXT_PUBLIC_POSTHOG_KEY: z.string(),\n  },\n});\n",
	}
	result, err = detect.NewAuction(detect.ComposeDetector{}).Run(context.Background(), rallly)
	require.NoError(t, err)
	slots = requiredSlots(t, result.Winner.Draft)
	require.Contains(t, slots, "SECRET_PASSWORD")
	require.Contains(t, slots, "SUPPORT_EMAIL")
	require.NotContains(t, slots, "SMTP_HOST")
	require.NotContains(t, slots, "NODE_ENV")
	require.NotContains(t, slots, "DATABASE_URL", "the compose file set it")
	require.Contains(t, slots, "NEXT_PUBLIC_POSTHOG_KEY", "t3-env checks the client block on the server too")
}

// TestR132_PythonThatReadsAVariableWithNoFallbackRequiresIt asserts R-132.
// `os.environ["X"]` at import raises KeyError when X is unset, so the app
// does not start — read in a function, it does not run at import, and
// `os.environ.get` has a fallback.
func TestR132_PythonThatReadsAVariableWithNoFallbackRequiresIt(t *testing.T) {
	src := memSource{
		"requirements.txt": "fastapi\nuvicorn\n",
		"main.py": "import os\nSECRET_KEY = os.environ[\"SECRET_KEY\"]\nADMIN_PASSWORD = os.environ['ADMIN_PASSWORD']\n" +
			"DEBUG = os.environ.get(\"DEBUG\")\nos.environ[\"TZ\"] = \"UTC\"\n\ndef handler():\n    return os.environ[\"LATER\"]\n" +
			"if os.getenv(\"DB\") == \"mysql\":\n    USER = os.environ[\"DB_USER\"]\n",
	}
	result, err := auction().Run(context.Background(), src)
	require.NoError(t, err)
	slots := requiredSlots(t, result.Winner.Draft)
	require.Contains(t, slots, "SECRET_KEY")
	require.Contains(t, slots, "ADMIN_PASSWORD")
	require.NotContains(t, slots, "DEBUG")
	require.NotContains(t, slots, "TZ", "an assignment is not a read")
	require.NotContains(t, slots, "LATER", "a function body does not run at import")
	require.NotContains(t, slots, "DB_USER", "nor is one under a condition required of everybody")

	django := memSource{
		"requirements.txt": "django\n",
		"manage.py":        "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'mysite.settings')\n",
		"mysite/settings.py": "import os\nSECRET_KEY = os.environ[\"SECRET_KEY\"]\nDATABASES = {\n    'default': {\n" +
			"        'NAME': os.environ[\"PGDATABASE\"],\n    }\n}\n",
	}
	result, err = auction().Run(context.Background(), django)
	require.NoError(t, err)
	slots = requiredSlots(t, result.Winner.Draft)
	require.Equal(t, spec.FormatDjangoKey, slots["SECRET_KEY"].Format)
	require.Contains(t, slots, "PGDATABASE")
}

// TestR132_AComposeTemplatePlaceholderIsNoValue asserts R-132. A compose
// file's env_file template that says `SESSION_SECRET=change-me` has said the
// app needs one and left it to the reader, as `SESSION_SECRET=` would; a real
// sample stays a setting with a default.
func TestR132_AComposeTemplatePlaceholderIsNoValue(t *testing.T) {
	src := memSource{
		"compose.yaml": "services:\n  web:\n    build: .\n    env_file: .env\n    ports:\n      - \"8080:8080\"\n",
		"Dockerfile":   "FROM golang\nEXPOSE 8080\n",
		".env.example": "# Copy to .env and fill in.\nSESSION_SECRET=change-me\nAPP_TITLE=Pastebin\n",
	}
	result, err := detect.NewAuction(detect.ComposeDetector{}).Run(context.Background(), src)
	require.NoError(t, err)
	slots := requiredSlots(t, result.Winner.Draft)
	require.Contains(t, slots, "SESSION_SECRET")
	require.Equal(t, spec.FormatRandom, slots["SESSION_SECRET"].Format)
	require.NotContains(t, slots, "APP_TITLE")
}

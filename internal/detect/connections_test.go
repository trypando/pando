package detect_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// parts is each variable on a draft's primary workload that takes part of a
// slot, as "SLOT.part".
func parts(t *testing.T, d detect.Draft) map[string]string {
	t.Helper()
	assembled := detect.Assemble("", spec.Source{}, d)
	primary, ok := assembled.PrimaryWorkload()
	require.True(t, ok)
	out := map[string]string{}
	for _, e := range primary.Env {
		if e.SlotRef == nil {
			continue
		}
		_, declared := assembled.Slot(*e.SlotRef)
		require.True(t, declared, "%s refers to a slot the spec declares", e.Key)
		if e.SlotPart != "" {
			out[e.Key] = *e.SlotRef + "." + string(e.SlotPart)
		}
	}
	return out
}

// TestR131_SeparateDatabaseVariablesAreOneSlotPandoCanProvision asserts R-131
// and R-130. Railway's Django template reads PGHOST, PGPORT, PGUSER,
// PGPASSWORD and PGDATABASE with no fallback. Asked for as five values, they
// could only be filled by somebody who already had a database; as the parts of
// one PostgreSQL slot, Pando provisions one and fills all five from it.
func TestR131_SeparateDatabaseVariablesAreOneSlotPandoCanProvision(t *testing.T) {
	src := memSource{
		"requirements.txt": "django\ngunicorn\npsycopg2\n",
		"manage.py":        "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'mysite.settings')\n",
		"mysite/settings.py": "import os\nDATABASES = {\n    'default': {\n        'ENGINE': 'django.db.backends.postgresql',\n" +
			"        'NAME': os.environ[\"PGDATABASE\"],\n        'USER': os.environ[\"PGUSER\"],\n" +
			"        'PASSWORD': os.environ[\"PGPASSWORD\"],\n        'HOST': os.environ[\"PGHOST\"],\n" +
			"        'PORT': os.environ[\"PGPORT\"],\n    }\n}\n",
	}
	result, err := auction().Run(context.Background(), src)
	require.NoError(t, err)
	d := result.Winner.Draft

	slots := requiredSlots(t, d)
	require.Equal(t, spec.SlotPostgres, slots["PGHOST"].Type, "one PostgreSQL")
	require.Equal(t, map[string]string{
		"PGHOST": "PGHOST.host", "PGPORT": "PGHOST.port", "PGUSER": "PGHOST.user",
		"PGPASSWORD": "PGHOST.password", "PGDATABASE": "PGHOST.database",
	}, parts(t, d))
	for _, s := range d.Slots {
		require.NotEqual(t, spec.SlotUnknown, s.Type, "no value is asked for on its own: %s", s.Key)
	}
}

// TestR131_ADatabaseTheLogNamesByHostIsTypedByWhatItTriedToReach asserts
// R-131 and R-107. Joomla's image names JOOMLA_DB_HOST and, once it has one,
// JOOMLA_DB_PASSWORD; its link variable MYSQL_PORT_3306_TCP says which kind
// of database. DB alone says nothing about which, and SMTP_HOST beside a
// MySQL error is still a mail server.
func TestR131_ADatabaseTheLogNamesByHostIsTypedByWhatItTriedToReach(t *testing.T) {
	log := "[ERROR] Missing JOOMLA_DB_HOST and MYSQL_PORT_3306_TCP environment variables.\n" +
		"--- Pando ran the app again with stand-in values for JOOMLA_DB_HOST ---\n" +
		"[ERROR] Missing required JOOMLA_DB_PASSWORD environment variable.\n"
	d, _ := detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true, Log: log})
	require.Equal(t, spec.SlotMySQL, requiredSlots(t, d)["JOOMLA_DB_HOST"].Type)
	require.Equal(t, map[string]string{
		"JOOMLA_DB_HOST": "JOOMLA_DB_HOST.host", "JOOMLA_DB_PASSWORD": "JOOMLA_DB_HOST.password",
	}, parts(t, d))

	// Nothing says which database: left as the values the log asked for.
	d, _ = detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true,
		Log: "DB_HOST is not set\nDB_PASSWORD is not set\n"})
	require.Empty(t, parts(t, d))
	require.Equal(t, spec.SlotUnknown, requiredSlots(t, d)["DB_HOST"].Type)

	d, _ = detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true,
		Log: "SMTP_HOST is not set\nSMTP_PASSWORD is not set\nmysql: connection refused\n"})
	require.Empty(t, parts(t, d))
}

// TestR131_ALaravelAppThatCannotReachItsDatabaseIsGivenOne asserts R-131.
// Laravel names its connection in the exception ("Connection: mysql") and reads
// the database from DB_HOST, DB_PORT, DB_DATABASE, DB_USERNAME and
// DB_PASSWORD; BookStack's image waited for one forever.
func TestR131_ALaravelAppThatCannotReachItsDatabaseIsGivenOne(t *testing.T) {
	log := "Waiting for DB to be available\n\n   Illuminate\\Database\\QueryException \n\n" +
		"  SQLSTATE[HY000] [2002] No such file or directory (Connection: mysql, Host: localhost, Port: 3306, " +
		"Database: database_database, SQL: select exists (select 1))\n"
	d, _ := detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Log: log})
	require.Equal(t, spec.SlotMySQL, requiredSlots(t, d)["DB_HOST"].Type)
	require.Equal(t, map[string]string{
		"DB_HOST": "DB_HOST.host", "DB_PORT": "DB_HOST.port", "DB_DATABASE": "DB_HOST.database",
		"DB_USERNAME": "DB_HOST.user", "DB_PASSWORD": "DB_HOST.password",
	}, parts(t, d))
	require.Equal(t, "mysql", envValues(t, d)["DB_CONNECTION"])

	// One somebody already pointed somewhere is theirs.
	host := "db.internal"
	given := imageDraft()
	given.Workloads[0].Env = []spec.EnvEntry{{Key: "DB_HOST", Value: &host, Source: spec.EnvFromUser}}
	d, _ = detect.ApplyTrial(given, nil, detect.Trial{Ran: true, Started: true, Log: log})
	require.Empty(t, parts(t, d))
}

// TestR132_WhatAnAppNamesNextIsFoundBeforeItIsDeployed asserts R-132 and
// R-097. Flatnotes names one missing variable a start: given a stand-in for
// the first, it names the second. Each is asked for before the deploy, not
// discovered in its crash loop — and the stand-ins are the trial's alone.
func TestR132_WhatAnAppNamesNextIsFoundBeforeItIsDeployed(t *testing.T) {
	var tried []map[string]string
	job := &detect.Job{Auction: detect.NewAuction(), Runtime: trialResults{caps: probing(),
		results: func(req api.TrialRequest) api.TrialResult {
			env := map[string]string{}
			for k, v := range req.Env {
				env[k] = v.Reveal()
			}
			tried = append(tried, env)
			code := 1
			for _, key := range []string{"FLATNOTES_USERNAME", "FLATNOTES_PASSWORD", "FLATNOTES_SECRET_KEY"} {
				if env[key] == "" {
					return api.TrialResult{Started: true, ExitCode: &code,
						Log: "[ERROR]: Environment variable " + key + " must be set.\n"}
				}
			}
			return api.TrialResult{Started: true, ObservedPorts: []int{8080}, HTTPAnswers: answers(8080, 200)}
		}}}
	p, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "dullage/flatnotes:v5"}, nil)
	require.NoError(t, err)
	require.Len(t, tried, 4, "one run, then one for each variable it named")
	require.NotEmpty(t, tried[3]["FLATNOTES_SECRET_KEY"])

	primary, ok := p.DraftSpec.PrimaryWorkload()
	require.True(t, ok)
	require.Equal(t, 8080, primary.Ports[0].Number, "the run that served answered the port")
	for _, key := range []string{"FLATNOTES_USERNAME", "FLATNOTES_PASSWORD", "FLATNOTES_SECRET_KEY"} {
		slot, ok := p.DraftSpec.Slot(key)
		require.True(t, ok, "%s is asked for", key)
		require.True(t, slot.Required)
		require.Nil(t, slot.Resolution, "and nothing filled it: the stand-in was the trial's")
	}
	for _, e := range primary.Env {
		require.Nil(t, e.Value, "%s carries no stand-in", e.Key)
	}
	require.Contains(t, p.TrialLog, "stand-in values for FLATNOTES_USERNAME", "the log says what was tried")
}

// TestR132_AnAlternativeListIsNotAskedFor asserts R-132 and O-4. "Either A or
// all of [B, C, D]" asks for A; Zipline was asked for five values it said it
// does without, and filled DATABASE_HOST with a word.
func TestR132_AnAlternativeListIsNotAskedFor(t *testing.T) {
	log := "[ERROR  server] either DATABASE_URL or all of [DATABASE_USERNAME, DATABASE_PASSWORD, DATABASE_HOST, " +
		"DATABASE_PORT, DATABASE_NAME] not set, exiting...\n" +
		"[ERROR  config.validate] CORE_SECRET: Invalid input: expected string, received undefined\n"
	d, _ := detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, Crashed: true, Log: log})
	slots := requiredSlots(t, d)
	require.Contains(t, slots, "DATABASE_URL")
	require.Contains(t, slots, "CORE_SECRET", "zod 4's own words, unbracketed")
	for _, key := range []string{"DATABASE_USERNAME", "DATABASE_PASSWORD", "DATABASE_HOST", "DATABASE_PORT", "DATABASE_NAME"} {
		require.NotContains(t, slots, key)
	}
}

// TestR132_AnAppThatServesWithAnInvalidEnvironmentIsAskedForWhatItNames
// asserts R-132 and R-130. Homarr opens its port and answers every page with a
// 500 because "Invalid environment variables"; the variable it names is
// asked for, in the shape its message gives — 64 hexadecimal characters.
func TestR132_AnAppThatServesWithAnInvalidEnvironmentIsAskedForWhatItNames(t *testing.T) {
	log := "❌ Invalid environment variables: [\n  {\n    path: [ 'SECRET_ENCRYPTION_KEY' ],\n" +
		"    message: 'SECRET_ENCRYPTION_KEY is required, please generate a 64 character secret in hex format'\n  }\n]\n"
	d, _ := detect.ApplyTrial(imageDraft(), nil, detect.Trial{Ran: true, Started: true, ObservedPorts: []int{7575}, Log: log})
	slot, ok := requiredSlots(t, d)["SECRET_ENCRYPTION_KEY"]
	require.True(t, ok)
	require.Equal(t, spec.FormatHex64, slot.Format)
}

// TestR130_AFrameworkThatReadsDATABASE_URLIsGivenADatabase asserts R-130 and
// R-132. Rails merges DATABASE_URL into a database.yml that names an adapter
// and no host, and a Django settings module that does the same with
// dj_database_url; either way, nothing set means a local socket no container
// has. An app on SQLite is asked for nothing.
func TestR130_AFrameworkThatReadsDATABASE_URLIsGivenADatabase(t *testing.T) {
	rails := memSource{
		"Gemfile":               "source 'https://rubygems.org'\ngem 'rails'\ngem 'pg'\n",
		"config/application.rb": "", "bin/rails": "", "config/master.key": "x",
		"config/database.yml": "default: &default\n  adapter: postgresql\n  encoding: unicode\n\n" +
			"development:\n  <<: *default\n  database: app_development\n\n" +
			"production:\n  <<: *default\n  database: app_production\n  password: <%= ENV[\"APP_DATABASE_PASSWORD\"] %>\n",
	}
	result, err := auction().Run(context.Background(), rails)
	require.NoError(t, err)
	require.Equal(t, spec.SlotPostgres, requiredSlots(t, result.Winner.Draft)["DATABASE_URL"].Type)

	rails["config/database.yml"] = "default: &default\n  adapter: mysql2\n\nproduction:\n  <<: *default\n  database: app\n"
	result, err = auction().Run(context.Background(), rails)
	require.NoError(t, err)
	require.Equal(t, spec.SlotMySQL, requiredSlots(t, result.Winner.Draft)["DATABASE_URL"].Type)

	rails["config/database.yml"] = "production:\n  adapter: sqlite3\n  database: storage/production.sqlite3\n"
	result, err = auction().Run(context.Background(), rails)
	require.NoError(t, err)
	require.NotContains(t, requiredSlots(t, result.Winner.Draft), "DATABASE_URL")

	django := memSource{
		"requirements.txt": "django\ndj-database-url\n",
		"manage.py":        "os.environ.setdefault(\"DJANGO_SETTINGS_MODULE\", \"config.settings\")\n",
		"config/settings.py": "import os\nimport dj_database_url\nDATABASES = {\n    \"default\": {\n" +
			"        \"ENGINE\": \"django.db.backends.postgresql\",\n        \"NAME\": \"blog\",\n    },\n}\n" +
			"if \"DATABASE_URL\" in os.environ:\n    DATABASES[\"default\"] = dj_database_url.config()\n",
	}
	result, err = auction().Run(context.Background(), django)
	require.NoError(t, err)
	require.Equal(t, spec.SlotPostgres, requiredSlots(t, result.Winner.Draft)["DATABASE_URL"].Type)
}

// TestR130_ANodeAppThatConnectsAtStartNeedsThatDatabase asserts R-130 and
// R-132. hackathon-starter calls mongoose.connect(process.env.MONGODB_URI)
// at start: its .env.example names the variable, the name says MongoDB, and
// the read says the app has nothing without it.
func TestR130_ANodeAppThatConnectsAtStartNeedsThatDatabase(t *testing.T) {
	src := memSource{
		"package.json": `{"name":"hackathon","scripts":{"start":"node app.js"},"dependencies":{"express":"4","mongoose":"8"}}`,
		".env.example": "BASE_URL=http://localhost:8080\nMONGODB_URI=\nSESSION_SECRET=\n",
		"app.js": "const mongoose = require('mongoose');\nmongoose.connect(process.env.MONGODB_URI);\n" +
			"const redis = createClient({ url: process.env.REDIS_URL || 'redis://localhost' });\n",
	}
	result, err := auction().Run(context.Background(), src)
	require.NoError(t, err)
	slots := requiredSlots(t, result.Winner.Draft)
	require.Equal(t, spec.SlotMongoDB, slots["MONGODB_URI"].Type)
	require.NotContains(t, slots, "REDIS_URL", "a read with a fallback is not required")
}

// TestR132_AKeyOnlyOneFrameworkReadsIsAskedForWhenLeftEmpty asserts R-132.
// SECRET_KEY_BASE (Rails, Phoenix) and NEXTAUTH_SECRET (NextAuth) stop the app
// without a value. A compose env_file template that says how to generate one
// — `[generate with <openssl rand -base64 36>]` — has given none.
func TestR132_AKeyOnlyOneFrameworkReadsIsAskedForWhenLeftEmpty(t *testing.T) {
	src := memSource{
		"docker-compose.yml": "services:\n  web:\n    image: ghcr.io/example/web:1\n    env_file:\n      - .env\n" +
			"    ports:\n      - 3000:3000\n",
		".env.sample": "NEXTAUTH_URL=http://localhost:3000\nNEXTAUTH_SECRET=[generate with <openssl rand -base64 36>]\n" +
			"MEILI_MASTER_KEY=[generate with <openssl rand -base64 36 | tr -dc 'A-Za-z0-9'>]\n",
	}
	result, err := detect.NewAuction(detect.ComposeDetector{}).Run(context.Background(), src)
	require.NoError(t, err)
	slots := requiredSlots(t, result.Winner.Draft)
	require.Contains(t, slots, "NEXTAUTH_SECRET")
	require.Contains(t, slots, "MEILI_MASTER_KEY")
	require.NotContains(t, slots, "NEXTAUTH_URL")

	// Declared empty in a .env.example, it is asked for in Phoenix's shape.
	phoenix := memSource{
		"Dockerfile":   "FROM elixir:1.18\nEXPOSE 4000\nCMD [\"/app/bin/server\"]\n",
		".env.example": "SECRET_KEY_BASE=\nPHX_HOST=localhost\n",
	}
	result, err = auction().Run(context.Background(), phoenix)
	require.NoError(t, err)
	require.Equal(t, spec.FormatRandomLong, requiredSlots(t, result.Winner.Draft)["SECRET_KEY_BASE"].Format)
}

// TestR096_ATemplateURLAtTheFilesOwnServiceIsKept asserts R-096 and R-131.
// Claper's .env.sample points DATABASE_URL at the compose file's own `db`
// service. That is the address `docker compose up` gives the app, not the
// author's machine, so it is kept — and Pando provisions the database it
// names and fills the variable from it, as it does for one in `environment:`.
func TestR096_ATemplateURLAtTheFilesOwnServiceIsKept(t *testing.T) {
	src := memSource{
		"docker-compose.yml": "services:\n  db:\n    image: postgres:15\n    environment:\n      POSTGRES_PASSWORD: claper\n" +
			"  app:\n    image: ghcr.io/claperco/claper:latest\n    ports:\n      - 4000:4000\n    env_file: .env\n",
		".env.sample": "BASE_URL=http://localhost:4000\nDATABASE_URL=postgres://claper:claper@db:5432/claper\n" +
			"SECRET_KEY_BASE=0LZiQBLw4WvqPlz4cz8RsHJlxNiSqM9B48y4ChyJ5v1oA0L/TPIqRjQNdPZN3iEG # Generate with `mix phx.gen.secret`\n",
	}
	result, err := detect.NewAuction(detect.ComposeDetector{}).Run(context.Background(), src)
	require.NoError(t, err)
	d := result.Winner.Draft
	assembled := detect.Assemble("", spec.Source{}, d)
	slot, ok := assembled.Slot("DATABASE_URL")
	require.True(t, ok, "the database is a dependency")
	require.Equal(t, spec.SlotPostgres, slot.Type)
	require.NotNil(t, slot.Resolution)
	require.Equal(t, spec.ResolutionProvisioned, slot.Resolution.Mode)
	for _, w := range assembled.Workloads {
		require.NotEqual(t, "db", w.Name, "and not a container beside the app")
	}
	require.Equal(t, spec.FormatRandomLong, requiredSlots(t, d)["SECRET_KEY_BASE"].Format,
		"a sample's secret is not carried over, and the one asked for is one Phoenix accepts")
}

// The stand-in for a connection never reaches anything.
func TestR097_AStandInConnectsToNothing(t *testing.T) {
	for _, log := range []string{"DATABASE_URL is not set\n", "REDIS_URL is required\n", "MONGODB_URI must be set\n"} {
		var seen string
		job := &detect.Job{Auction: detect.NewAuction(), Runtime: trialResults{caps: probing(),
			results: func(req api.TrialRequest) api.TrialResult {
				code := 1
				for _, v := range req.Env {
					seen += v.Reveal()
				}
				return api.TrialResult{Started: true, ExitCode: &code, Log: log}
			}}}
		_, err := job.Run(context.Background(), "app_1", spec.Source{Type: spec.SourceImage, Image: "example/web:1"}, nil)
		require.NoError(t, err)
		require.True(t, strings.Contains(seen, ".invalid"), "%q: %s", log, seen)
	}
}

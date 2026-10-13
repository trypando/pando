package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// planFor plans root the way a build does and returns the Dockerfile.
func planInDir(t *testing.T, root string) string {
	t.Helper()
	name, err := buildpackPlan(api.BuildRequest{}, root)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(planDir, "Dockerfile"), name)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	return string(body)
}

// TestR095_LanguagesRailpackDoesNotPlanArePlannedByPando asserts R-095's
// boundary: Railpack wraps convention-matching for the languages it has a
// provider for, and for those it has none for — or plans as a static site —
// Pando writes the plan, on each language's official image, with the build
// and start its own tooling documents (issue #90). Each reaches the planner
// without Railpack on PATH: none of them asks it anything.
func TestR095_LanguagesRailpackDoesNotPlanArePlannedByPando(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cases := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"a Crystal shard with a target", map[string]string{
			"shard.yml":          "name: kemal-hello\nversion: 0.1.0\n\ntargets:\n  kemal-hello:\n    main: src/kemal_hello.cr\n\ndependencies:\n  kemal:\n    github: kemalcr/kemal\n",
			"src/kemal_hello.cr": "require \"kemal\"\n",
			"public/style.css":   "",
		}, []string{"FROM " + crystalImage, "shards build kemal-hello --release --production", "EXPOSE 3000", `CMD ["exec ./bin/kemal-hello"]`}},
		{"a Crystal shard on SQLite with no target", map[string]string{
			"shard.yml":   "name: blog\ndependencies:\n  sqlite3:\n    github: crystal-lang/crystal-sqlite3\n",
			"src/blog.cr": "",
		}, []string{"libsqlite3-dev", "crystal build --release src/blog.cr -o bin/blog", `CMD ["exec ./bin/blog"]`}},
		{"a plumber API", map[string]string{
			"plumber.R": "library(jsonlite)\n#* @get /mean\nfunction() { stats::rnorm(1) }\n",
		}, []string{"FROM " + rImage, "install2.r --error --skipinstalled plumber jsonlite", "EXPOSE 8000", "plumber::pr_run(plumber::pr('plumber.R'), host = '0.0.0.0'"}},
		{"a Shiny app in ui.R and server.R", map[string]string{
			"ui.R": "library(shiny)\nfluidPage()\n", "server.R": "function(input, output) {}\n",
		}, []string{"install2.r --error --skipinstalled shiny\n", "EXPOSE 3838", "shiny::runApp('.', host = '0.0.0.0'"}},
		{"a Leiningen uberjar", map[string]string{
			"project.clj": "(defproject ring-hello \"0.1.0\"\n  :main ring-hello.core\n  :profiles {:uberjar {:aot :all :uberjar-name \"ring-hello-standalone.jar\"}})\n",
		}, []string{"FROM " + leinImage, "lein uberjar", "target/ring-hello-standalone.jar", `CMD ["exec java -jar /app.jar"]`}},
		// reitit's ring-integrant example: no :aot and no :gen-class, so the
		// uberjar has no main class (issue #90).
		{"a Leiningen :main with no class of its own", map[string]string{
			"project.clj":            "(defproject ring-integrant-example \"0.1.0\"\n  :main example.server)\n",
			"src/example/server.clj": "(ns example.server\n  (:require [integrant.core :as ig]))\n(defn -main [])\n",
		}, []string{"lein uberjar", `CMD ["exec java -cp /app.jar clojure.main -m example.server"]`}},
		{"a Leiningen :main with :gen-class", map[string]string{
			"project.clj":         "(defproject my-app \"0.1.0\"\n  :main my-app.core)\n",
			"src/my_app/core.clj": "(ns my-app.core\n  (:gen-class))\n(defn -main [])\n",
		}, []string{`CMD ["exec java -jar /app.jar"]`}},
		{"a Leiningen project with a Procfile", map[string]string{
			"project.clj": "(defproject x \"0.1.0\"\n  :main x.web\n  :uberjar-name \"x.jar\")\n",
			"Procfile":    "web: java -jar target/x.jar\n",
		}, []string{"lein uberjar", `CMD ["java -jar target/x.jar"]`}},
		{"a deps.edn project with a server alias", map[string]string{
			"deps.edn": "{:paths [\"src\"]\n :aliases {:dev {:extra-paths [\"dev\"]}\n           :server {:main-opts [\"-m\" \"usermanager.system\"]}}}\n",
		}, []string{"FROM " + toolsDepsImage, "RUN clojure -P -M:server", `CMD ["exec clojure -M:server"]`}},
		{"a deps.edn project with one -main", map[string]string{
			"deps.edn":                    "{:paths [\"src\" \"resources\"]}\n",
			"src/usermanager/main.clj":    "(ns usermanager.main)\n(defn -main [& args])\n",
			"src/usermanager/handler.clj": "(ns usermanager.handler)\n",
		}, []string{"RUN clojure -P", `CMD ["exec clojure -M -m usermanager.main"]`}},
		{"an sbt project with native-packager", map[string]string{
			"build.sbt": "enablePlugins(JavaAppPackaging)\n", "project/plugins.sbt": `addSbtPlugin("com.github.sbt" % "sbt-native-packager" % "1.11.1")` + "\n",
		}, []string{"FROM " + sbtJDKImage, "sbt -batch " + sbtNoDocs + " stage", `exec \"$(cat /app/.pando-start)\"`}},
		{"a Play app", map[string]string{
			"build.sbt": "enablePlugins(PlayScala)\n", "project/plugins.sbt": `addSbtPlugin("org.playframework" % "sbt-plugin" % "3.0.12")` + "\n",
		}, []string{"sbt -batch " + sbtNoDocs + " stage", "install -y --no-install-recommends curl git", "COPY --from=" + sbtNodeImage + " /usr/local/bin/node", "-Dplay.http.secret.key=\\\"$APPLICATION_SECRET\\\"", "-Dplay.filters.hosts.allowed.0=.", "EXPOSE 9000"}},
		{"an sbt project with neither", map[string]string{"build.sbt": "scalaVersion := \"3.3.1\"\n"},
			[]string{"export Runtime / fullClasspath", "discoveredMainClasses", `exec java -cp \"$(cat /app/.pando-classpath)\"`}},
		{"a Stack project", map[string]string{
			"stack.yaml": "resolver: lts-18.3\n", "package.yaml": "name: x\nexecutables:\n  example-servant-minimal:\n    main: exe/Main.hs\n",
		}, []string{"FROM " + haskellImage + " AS build", "stack build --install-ghc --copy-bins --local-bin-path /out", `CMD ["exec /usr/local/bin/example-servant-minimal"]`}},
		{"a PSGI app", map[string]string{"cpanfile": "requires 'Dancer2';\n", "app.psgi": "use Dancer2;\n"},
			[]string{"FROM " + perlImage, "cpanm --notest --installdeps .", "cpanm --notest Plack", "EXPOSE 5000", "plackup --host 0.0.0.0 --port ${PORT:-5000} app.psgi"}},
		{"a Mojolicious app with a Procfile", map[string]string{
			"cpanfile": "requires 'Mojolicious';\n", "app.pl": "use Mojolicious::Lite;\n",
			"Procfile": "web: perl app.pl daemon -m production -l http://*:3080\n",
		}, []string{"cpanm --notest --installdeps .", `CMD ["perl app.pl daemon -m production -l http://*:3080"]`}},
		{"a setup.py package with a console script", map[string]string{
			"setup.py":                  "setup(name='hello-aiohttp', entry_points={'console_scripts': ['hello-aiohttp=hello_aiohttp.__main__:main']})\n",
			"hello_aiohttp/__init__.py": "",
			"hello_aiohttp/__main__.py": "parser.add_argument('--host', default='0.0.0.0')\nparser.add_argument('--port', type=int, required=True)\n",
		}, []string{"FROM " + setupPyImage, "pip install --no-cache-dir .", `CMD ["exec hello-aiohttp --host 0.0.0.0 --port ${PORT:-8000}"]`}},
		{"PHP with no composer.json, served from public/", map[string]string{
			"public/index.php": "<?php echo 'hi';", "src/helpers.php": "<?php",
		}, []string{"FROM " + plainPHPImage, "APACHE_DOCUMENT_ROOT=/var/www/html/public", "a2enmod rewrite", `CMD ["exec apache2-foreground"]`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := planInDir(t, writeTree(t, tc.files))
			for _, want := range tc.want {
				require.Contains(t, body, want)
			}
		})
	}
}

// Each plan declines what it cannot read, rather than guessing, and the
// repository goes on to Railpack as before.
func TestALanguagePlanDeclinesWhatItCannotRead(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"a Stack project with two executables": {
			"stack.yaml": "resolver: lts-19.16\n", "package.yaml": "executables:\n  a:\n    main: A.hs\n  b:\n    main: B.hs\n",
		},
		"a deps.edn library":                  {"deps.edn": "{:paths [\"src\"]}\n", "src/x/core.clj": "(ns x.core)\n"},
		"a Leiningen library":                 {"project.clj": "(defproject x \"0.1.0\")\n"},
		"an app.R that is not Shiny":          {"app.R": "print('hi')\n"},
		"Perl with nothing to run":            {"cpanfile": "requires 'Moo';\n"},
		"a setup.py beside requirements.txt":  {"setup.py": "", "requirements.txt": "flask\n"},
		"PHP with an index.php at the root":   {"index.php": "<?php"},
		"PHP with a composer.json":            {"composer.json": "{}", "public/index.php": "<?php"},
		"no shard target and no main to find": {"shard.yml": "name: lib\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok, err := languagePlan(writeTree(t, files))
			require.NoError(t, err)
			require.False(t, ok)
		})
	}
}

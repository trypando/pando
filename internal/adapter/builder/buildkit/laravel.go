package buildkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Laravel apps are planned by Pando rather than by nixpacks.
//
// nixpacks 1.41 installs PHP from a pinned Nix package set older than the 8.3
// Laravel 13 requires, and installs dependencies with --ignore-platform-reqs,
// so the mismatch surfaced only when PHP parsed syntax it did not know:
// `syntax error, unexpected token "{"`, with nothing in the log naming the
// version (issue #58). The official PHP images carry every current release,
// and Composer can be told which one it is installing for.

// phpReleases are the PHP minors published as official php images, oldest
// first. A Laravel app is built on the newest one its composer.json allows.
var phpReleases = []string{"7.4", "8.0", "8.1", "8.2", "8.3", "8.4", "8.5"}

// composerImage is where dependencies are installed. [P] The official Composer
// image, because it carries git and unzip, which the PHP image does not and
// which Composer needs to fetch a package; the major is pinned and the rest
// floats, as the Node images do.
const composerImage = "composer:2"

// phpBuiltin are the extensions every official PHP image already has, compiled
// in or enabled. Requiring one needs nothing installed.
var phpBuiltin = map[string]bool{
	"core": true, "ctype": true, "curl": true, "date": true, "dom": true, "fileinfo": true,
	"filter": true, "ftp": true, "hash": true, "iconv": true, "json": true, "libxml": true,
	"mbstring": true, "mysqlnd": true, "openssl": true, "pcre": true, "pdo": true,
	"pdo_sqlite": true, "phar": true, "posix": true, "random": true, "readline": true,
	"reflection": true, "session": true, "simplexml": true, "sodium": true, "spl": true,
	"sqlite3": true, "standard": true, "tokenizer": true, "xml": true, "xmlreader": true,
	"xmlwriter": true, "zlib": true,
}

// phpLibraries are the Debian packages an extension is compiled against, for
// the extensions docker-php-ext-install builds from PHP's own source. An
// extension listed with none needs no system library.
var phpLibraries = map[string][]string{
	"bcmath": nil, "calendar": nil, "exif": nil, "gettext": nil, "mysqli": nil,
	"opcache": nil, "pcntl": nil, "pdo_mysql": nil, "shmop": nil, "sockets": nil,
	"sysvmsg": nil, "sysvsem": nil, "sysvshm": nil,
	"bz2":       {"libbz2-dev"},
	"gd":        {"libfreetype6-dev", "libjpeg62-turbo-dev", "libpng-dev", "libwebp-dev"},
	"gmp":       {"libgmp-dev"},
	"intl":      {"libicu-dev"},
	"ldap":      {"libldap2-dev"},
	"pdo_pgsql": {"libpq-dev"},
	"pgsql":     {"libpq-dev"},
	"soap":      {"libxml2-dev"},
	"xsl":       {"libxslt1-dev"},
	"zip":       {"libzip-dev"},
}

// phpPECL are extensions that are not part of PHP's source and come from PECL,
// with the Debian packages they build against.
var phpPECL = map[string][]string{
	"apcu": nil, "igbinary": nil, "redis": nil, "xdebug": nil,
	"imagick": {"libmagickwand-dev"},
	"mongodb": {"libssl-dev"},
}

// laravelAlways are the extensions a Laravel app gets whether or not it asks.
// [P] Laravel's documented server requirements that are not built in, less
// those needing a system library: bcmath for its number helpers, pdo_mysql
// because MySQL is the database a Laravel app is most often pointed at. Both
// compile from PHP's own source with nothing downloaded. intl, zip and
// pdo_pgsql need system libraries and are installed when the app requires
// them, or, for pdo_pgsql, when its .env.example chooses PostgreSQL.
var laravelAlways = []string{"bcmath", "pdo_mysql"}

type laravelApp struct {
	PHP        string   // a minor, 8.4
	Constraint string   // what composer.json said, for the plan's comments
	Extensions []string // to install, sorted
	Assets     bool     // package.json has a build script
	Node       string
	Install    string // how to install the Node dependencies
}

var (
	composerExt    = regexp.MustCompile(`^ext-([A-Za-z0-9_]+)$`)
	safeConstraint = regexp.MustCompile(`[^0-9A-Za-z.*^~<>=|, @-]`)
)

// readLaravelApp reports a Laravel application: an artisan script beside a
// composer.json that requires the framework. A package that depends on
// Laravel is not one — it has no artisan.
func readLaravelApp(contextDir string) (laravelApp, bool) {
	if !fileExists(contextDir, "artisan") {
		return laravelApp{}, false
	}
	var manifest struct {
		Require map[string]string `json:"require"`
		Config  struct {
			Platform map[string]string `json:"platform"`
		} `json:"config"`
	}
	if json.Unmarshal([]byte(readFile(contextDir, "composer.json")), &manifest) != nil {
		return laravelApp{}, false
	}
	if _, ok := manifest.Require["laravel/framework"]; !ok {
		return laravelApp{}, false
	}

	app := laravelApp{Constraint: strings.TrimSpace(manifest.Require["php"])}
	// A platform the author pinned is the PHP they resolve dependencies for,
	// and so the one they run on.
	if pinned := strings.TrimSpace(manifest.Config.Platform["php"]); pinned != "" {
		app.PHP = phpForConstraint(pinned)
	} else {
		app.PHP = phpForConstraint(app.Constraint)
	}

	want := map[string]bool{}
	for _, e := range laravelAlways {
		want[e] = true
	}
	for name := range manifest.Require {
		if m := composerExt.FindStringSubmatch(name); m != nil {
			want[strings.ToLower(m[1])] = true
		}
	}
	// The packages the lockfile installs require extensions of their own.
	var lock struct {
		Packages []struct {
			Require map[string]string `json:"require"`
		} `json:"packages"`
	}
	if json.Unmarshal([]byte(readFile(contextDir, "composer.lock")), &lock) == nil {
		for _, p := range lock.Packages {
			for name := range p.Require {
				if m := composerExt.FindStringSubmatch(name); m != nil {
					want[strings.ToLower(m[1])] = true
				}
			}
		}
	}
	if dotenvValue(readFile(contextDir, ".env.example"), "DB_CONNECTION") == "pgsql" {
		want["pdo_pgsql"] = true
	}
	for name := range want {
		if phpBuiltin[name] {
			continue
		}
		// opcache is compiled into PHP from 8.5, and cannot be built again.
		if name == "opcache" && phpAtLeast(app.PHP, "8.5") {
			continue
		}
		_, core := phpLibraries[name]
		_, pecl := phpPECL[name]
		// An extension Pando does not know how to install is left for the
		// platform check to name, rather than guessed at.
		if core || pecl {
			app.Extensions = append(app.Extensions, name)
		}
	}
	sort.Strings(app.Extensions)

	if body := readFile(contextDir, "package.json"); body != "" {
		var pkg struct {
			Scripts map[string]string `json:"scripts"`
			Engines map[string]any    `json:"engines"`
		}
		if json.Unmarshal([]byte(body), &pkg) == nil && strings.TrimSpace(pkg.Scripts["build"]) != "" {
			app.Assets = true
			app.Node = nodeMajor(contextDir, pkg.Engines)
			app.Install = installCommand(contextDir)
		}
	}
	return app, true
}

// dotenvValue is the value a dotenv file gives a name, unquoted, or "".
func dotenvValue(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && strings.TrimSpace(k) == key {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// phpForConstraint is the newest published PHP minor a Composer version
// constraint allows. [P] The newest, because a constraint is a floor the
// author tested from and current releases are what dependencies resolve
// best against. A constraint nothing published satisfies, or none at all, gets
// the newest release, and the build's platform check says what did not fit.
func phpForConstraint(constraint string) string {
	newest := phpReleases[len(phpReleases)-1]
	if strings.TrimSpace(constraint) == "" {
		return newest
	}
	for i := len(phpReleases) - 1; i >= 0; i-- {
		if minorSatisfies(phpReleases[i], constraint) {
			return phpReleases[i]
		}
	}
	return newest
}

func phpAtLeast(have, want string) bool {
	return compareVersions(parseVersion(have), parseVersion(want)) >= 0
}

// minorSatisfies reports whether some release of a PHP minor meets a Composer
// constraint: `^8.3`, `>=8.2 <8.5`, `~8.2.0`, `8.3.*`, `^8.2|^8.3`.
func minorSatisfies(minor, constraint string) bool {
	base := parseVersion(minor)
	for patch := 0; patch < 100; patch++ {
		if satisfies([3]int{base[0], base[1], patch}, constraint) {
			return true
		}
	}
	return false
}

func satisfies(v [3]int, constraint string) bool {
	normalized := strings.ReplaceAll(constraint, "||", "|")
	for _, alternative := range strings.Split(normalized, "|") {
		if allOf(v, alternative) {
			return true
		}
	}
	return false
}

// allOf is one alternative of a constraint: every part must hold. Parts are
// separated by commas or spaces; `8.1 - 8.3` is a range.
func allOf(v [3]int, alternative string) bool {
	fields := strings.Fields(strings.ReplaceAll(alternative, ",", " "))
	if len(fields) == 0 {
		return false
	}
	for i := 0; i < len(fields); i++ {
		if i+2 < len(fields) && fields[i+1] == "-" {
			if !compare(v, ">=", fields[i]) || !upTo(v, fields[i+2]) {
				return false
			}
			i += 2
			continue
		}
		if !part(v, fields[i]) {
			return false
		}
	}
	return true
}

// upTo is the upper end of a hyphen range, which includes every release of a
// partial version: `- 8.3` allows 8.3.9.
func upTo(v [3]int, upper string) bool {
	if strings.Count(upper, ".") >= 2 {
		return compare(v, "<=", upper)
	}
	return compare(v, "<", bump(upper))
}

func part(v [3]int, p string) bool {
	p = strings.TrimPrefix(strings.TrimSpace(p), "v")
	switch {
	case p == "*" || p == "":
		return true
	case strings.HasPrefix(p, "^"):
		low := strings.TrimPrefix(p, "^")
		lv := parseVersion(low)
		var high [3]int
		switch {
		case lv[0] > 0:
			high = [3]int{lv[0] + 1, 0, 0}
		case lv[1] > 0:
			high = [3]int{0, lv[1] + 1, 0}
		default:
			high = [3]int{0, 0, lv[2] + 1}
		}
		return compareVersions(v, lv) >= 0 && compareVersions(v, high) < 0
	case strings.HasPrefix(p, "~"):
		// Composer's tilde lets the last given part rise: ~8.2 is >=8.2 <9,
		// ~8.2.1 is >=8.2.1 <8.3.
		low := strings.TrimPrefix(p, "~")
		lv := parseVersion(low)
		var high [3]int
		if strings.Count(low, ".") >= 2 {
			high = [3]int{lv[0], lv[1] + 1, 0}
		} else {
			high = [3]int{lv[0] + 1, 0, 0}
		}
		return compareVersions(v, lv) >= 0 && compareVersions(v, high) < 0
	case strings.HasPrefix(p, ">="), strings.HasPrefix(p, "<="), strings.HasPrefix(p, "!="):
		return compare(v, p[:2], p[2:])
	case strings.HasPrefix(p, ">"), strings.HasPrefix(p, "<"):
		return compare(v, p[:1], p[1:])
	case strings.HasPrefix(p, "=="):
		return exact(v, p[2:])
	case strings.HasPrefix(p, "="):
		return exact(v, p[1:])
	default:
		return exact(v, p)
	}
}

// exact is a version with no operator, where a missing or wildcard part
// matches anything: `8.3` and `8.3.*` both allow 8.3.9.
func exact(v [3]int, want string) bool {
	parts := strings.Split(strings.TrimSpace(want), ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		if parts[i] == "*" || parts[i] == "x" {
			return true
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil || n != v[i] {
			return false
		}
	}
	return true
}

func compare(v [3]int, op, raw string) bool {
	c := compareVersions(v, parseVersion(strings.TrimSpace(strings.TrimPrefix(raw, "v"))))
	switch op {
	case ">=":
		return c >= 0
	case "<=":
		return c <= 0
	case ">":
		return c > 0
	case "<":
		return c < 0
	case "!=":
		return c != 0
	}
	return false
}

// bump is the first version past every release of a partial one: 8.3 → 8.4.
func bump(raw string) string {
	v := parseVersion(raw)
	if strings.Count(raw, ".") == 0 {
		return strconv.Itoa(v[0]+1) + ".0.0"
	}
	return fmt.Sprintf("%d.%d.0", v[0], v[1]+1)
}

// parseVersion reads up to three numeric parts; a stability suffix or a
// wildcard counts as zero.
func parseVersion(raw string) [3]int {
	var v [3]int
	raw, _, _ = strings.Cut(strings.TrimSpace(raw), "-")
	raw, _, _ = strings.Cut(raw, "@")
	for i, p := range strings.SplitN(raw, ".", 3) {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil {
			v[i] = n
		}
	}
	return v
}

func compareVersions(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// laravelVhost serves public/ and lets Laravel's own .htaccess route every
// request that is not a file to index.php.
const laravelVhost = `<VirtualHost *:80>
  DocumentRoot /var/www/html/public
  <Directory /var/www/html/public>
    AllowOverride All
    Require all granted
  </Directory>
</VirtualHost>
`

// laravelStart runs the app's migrations and then Apache. [P] Migrations at
// start, because a new Laravel app keeps its sessions and cache in the
// database and answers every page with an error until its tables exist; it is
// the step Laravel's own setup script runs. The default SQLite database is
// created by the migrate command. Everything the app writes is handed to the
// user Apache runs as, since the migrate command ran as root.
const laravelStart = "php artisan migrate --force --no-interaction && " +
	"chown -R www-data:www-data storage bootstrap/cache database && " +
	"exec apache2-foreground"

// writeLaravelPlan writes the plan into .pando-plan/, where detection collects a
// plan and the build replays it (R-020).
//
// Dependencies are installed in the Composer image, for the PHP version the
// app will run on: Composer is told that version (`platform.php`), taken from
// the runtime image itself, so a package that needs a newer PHP fails the
// install with a message naming both versions (R-105). Extensions are checked
// later, in the runtime image where they are installed: the Composer image's
// extensions are not the app's.
func writeLaravelPlan(contextDir string, app laravelApp) (string, error) {
	var b strings.Builder
	// Read out of the repository and written into the plan's comments and
	// messages, so only what a version constraint is made of.
	constraint := safeConstraint.ReplaceAllString(app.Constraint, "")
	if constraint == "" {
		constraint = "none"
	}
	fmt.Fprintf(&b, "# A Laravel app on PHP %s, served by Apache from public/.\n", app.PHP)
	fmt.Fprintf(&b, "# composer.json requires PHP %s; %s is the newest release it allows.\n", constraint, app.PHP)
	fmt.Fprintf(&b, "FROM php:%s-apache AS runtime\n", app.PHP)
	b.WriteString("RUN php -r 'echo PHP_VERSION;' > /php-version\n\n")

	fmt.Fprintf(&b, "FROM %s AS vendor\n", composerImage)
	b.WriteString("WORKDIR /app\n")
	b.WriteString("COPY . .\n")
	b.WriteString("COPY --from=runtime /php-version /php-version\n")
	b.WriteString(`RUN composer config --global platform.php "$(cat /php-version)" && \` + "\n")
	b.WriteString("    composer install --no-dev --no-interaction --no-progress --prefer-dist " +
		"--no-scripts --no-autoloader --ignore-platform-req='ext-*'\n\n")

	if app.Assets {
		// The vendor directory is there because Laravel's front ends import
		// from it: Tailwind scans the framework's views, and starter kits
		// import Livewire and Flux styles out of vendor/.
		fmt.Fprintf(&b, "FROM node:%s-slim AS assets\n", app.Node)
		b.WriteString("WORKDIR /app\n")
		b.WriteString("COPY . .\n")
		b.WriteString("COPY --from=vendor /app/vendor vendor\n")
		fmt.Fprintf(&b, "RUN %s\n", app.Install)
		b.WriteString("RUN npm run build\n\n")
	}

	b.WriteString("FROM runtime\n")
	b.WriteString("WORKDIR /var/www/html\n")
	b.WriteString(extensionSteps(app.Extensions))
	fmt.Fprintf(&b, "RUN a2enmod rewrite && printf %s > /etc/apache2/sites-available/000-default.conf\n",
		printfFormat(laravelVhost))
	fmt.Fprintf(&b, "COPY --from=%s /usr/bin/composer /usr/bin/composer\n", composerImage)
	b.WriteString("COPY . .\n")
	b.WriteString("COPY --from=vendor /app/vendor vendor\n")
	if app.Assets {
		b.WriteString("COPY --from=assets /app/public/build public/build\n")
	}
	// The platform check, against the PHP and extensions the app runs on. Its
	// own output lists each requirement and whether it was met; the line
	// after says what that means for this build.
	fmt.Fprintf(&b, `RUN COMPOSER_ALLOW_SUPERUSER=1 composer check-platform-reqs --no-dev || { echo "This app's PHP dependencies need what PHP $(php -r 'echo PHP_VERSION;') here does not provide: each line above marked failed or missing names one. Pando chose PHP %s from composer.json's require.php (%s). Change require.php to a version the dependencies accept, or require the missing ext-* extension in composer.json so Pando installs it." >&2; exit 1; }`+"\n",
		app.PHP, constraint)
	b.WriteString("RUN COMPOSER_ALLOW_SUPERUSER=1 composer dump-autoload --no-dev --optimize --no-interaction\n")
	b.WriteString("RUN mkdir -p storage/app/public storage/framework/cache/data storage/framework/sessions " +
		"storage/framework/views storage/logs bootstrap/cache database && \\\n" +
		"    chown -R www-data:www-data storage bootstrap/cache database\n")
	// Laravel's default log is a file inside the container, where nobody
	// reads it. [P] stderr, so the app's log is its output. The app's own
	// LOG_CHANNEL, when it sets one, still wins.
	b.WriteString("ENV LOG_CHANNEL=stderr\n")
	b.WriteString("EXPOSE 80\n")
	b.WriteString(`ENTRYPOINT ["/bin/sh", "-c"]` + "\n")
	// Unescaped, so the stored plan reads `&&` rather than `\u0026\u0026`.
	var start bytes.Buffer
	enc := json.NewEncoder(&start)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(laravelStart); err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "CMD [%s]\n", strings.TrimSpace(start.String()))
	return writePlan(contextDir, b.String())
}

// extensionSteps installs PHP extensions: system libraries first, then those
// built from PHP's source, then those from PECL.
func extensionSteps(extensions []string) string {
	var packages, core, pecl []string
	seen := map[string]bool{}
	add := func(libs []string) {
		for _, l := range libs {
			if !seen[l] {
				seen[l] = true
				packages = append(packages, l)
			}
		}
	}
	for _, e := range extensions {
		if libs, ok := phpLibraries[e]; ok {
			core = append(core, e)
			add(libs)
		} else if libs, ok := phpPECL[e]; ok {
			pecl = append(pecl, e)
			add(libs)
		}
	}
	sort.Strings(packages)

	var b strings.Builder
	if len(packages) > 0 {
		fmt.Fprintf(&b, "RUN apt-get update && apt-get install -y --no-install-recommends %s && rm -rf /var/lib/apt/lists/*\n",
			strings.Join(packages, " "))
	}
	for _, e := range core {
		if e == "gd" {
			b.WriteString("RUN docker-php-ext-configure gd --with-freetype --with-jpeg --with-webp\n")
			break
		}
	}
	if len(core) > 0 {
		fmt.Fprintf(&b, "RUN docker-php-ext-install -j$(nproc) %s\n", strings.Join(core, " "))
	}
	if len(pecl) > 0 {
		fmt.Fprintf(&b, "RUN pecl install %s && docker-php-ext-enable %s\n",
			strings.Join(pecl, " "), strings.Join(pecl, " "))
	}
	return b.String()
}

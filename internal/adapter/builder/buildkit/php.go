package buildkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// PHP apps Railpack plans, and the ones it cannot.
//
// Railpack builds PHP on FrankenPHP, choosing the image from composer.json's
// require.php. It can read a plain version or a caret — "8.3", "^8.2" — and
// nothing else: ">=7.4", "~8.2.0 || ~8.3.0" and ">=5.4.8" all stopped planning
// with "No version available for php …", so CodeIgniter 3, Kirby and Adminer
// were each asked for a start command instead (issue #90). And it looks for
// PHP only at the root: an index.php under public/ with no composer.json was
// served by Caddy as static files, PHP source and all.

// railpackPHPReleases are the PHP minors Railpack's FrankenPHP images are
// published for, newest first. [P] A constraint that allows several gets the
// newest, as phpForConstraint does for Laravel.
var railpackPHPReleases = []string{"8.4", "8.3", "8.2"}

// railpackReadsPHP matches the require.php Railpack resolves itself.
var railpackReadsPHP = regexp.MustCompile(`^\^?(\d+)\.(\d+)(\.\d+)?$`)

// railpackPHPConstraint is what Railpack should be told composer.json's
// require.php is, and false when it can read the one there.
func railpackPHPConstraint(constraint string) (string, bool) {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" {
		return "", false
	}
	if m := railpackReadsPHP.FindStringSubmatch(constraint); m != nil {
		// Read, but a release too old for a FrankenPHP image (^7.4) has no
		// image either.
		if phpAtLeast(m[1]+"."+m[2], railpackPHPReleases[len(railpackPHPReleases)-1]) {
			return "", false
		}
	}
	for _, minor := range railpackPHPReleases {
		if minorSatisfies(minor, constraint) {
			return minor, true
		}
	}
	return "", false
}

// pinRailpackPHP gives Railpack a composer.json whose require.php it can
// read, for as long as it plans, and returns what puts the repository's own
// back. The checkout is a throwaway clone, and the plan Railpack writes copies
// composer.json from the build context, which is the original again by then:
// Composer installs against the constraint the author wrote, on a PHP that
// meets it.
func pinRailpackPHP(contextDir string) (restore func()) {
	restore = func() {}
	file := filepath.Join(contextDir, "composer.json")
	original, err := os.ReadFile(file) //nolint:gosec // G304: the checkout's own manifest.
	if err != nil {
		return restore
	}
	var manifest map[string]any
	if json.Unmarshal(original, &manifest) != nil {
		return restore
	}
	require, _ := manifest["require"].(map[string]any)
	constraint, _ := require["php"].(string)
	pinned, ok := railpackPHPConstraint(constraint)
	if !ok {
		return restore
	}
	require["php"] = pinned
	body, err := json.MarshalIndent(manifest, "", "    ")
	if err != nil {
		return restore
	}
	// G306: the checkout's own file, rewritten for the planner and restored.
	if os.WriteFile(file, body, 0o644) != nil { //nolint:gosec
		return restore
	}
	return func() { _ = os.WriteFile(file, original, 0o644) } //nolint:gosec // as above
}

// phpWebRoots are the directories a PHP app's front controller is served
// from, by convention: Slim, Laravel and Symfony's public/, Nette's www/,
// Drupal's and Craft's web/, and htdocs/.
var phpWebRoots = []string{"public", "www", "web", "htdocs"}

// phpWebRoot is the directory a PHP app with no index.php at its root serves
// from, relative to the root, or "" for the root itself.
//
// A conventional web root first. Failing that, a single directory holding an
// index.php — Adminer keeps its entry point in adminer/ — or, of several, the
// one named after the Composer package.
func phpWebRoot(contextDir string) string {
	if fileExists(contextDir, "index.php") {
		return ""
	}
	for _, dir := range phpWebRoots {
		if fileExists(contextDir, dir+"/index.php") {
			return dir
		}
	}
	found, _ := filepath.Glob(filepath.Join(contextDir, "*", "index.php"))
	var dirs []string
	for _, f := range found {
		dir := filepath.Base(filepath.Dir(f))
		if !underNotServed(dir) && safeRelative(dir) {
			dirs = append(dirs, dir)
		}
	}
	if len(dirs) == 1 {
		return dirs[0]
	}
	var manifest struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(readFile(contextDir, "composer.json")), &manifest) == nil && manifest.Name != "" {
		want := path.Base(manifest.Name)
		for _, d := range dirs {
			if d == want {
				return d
			}
		}
	}
	return ""
}

// underNotServed are directories that hold a PHP app's tests, tooling and
// dependencies rather than what it serves.
func underNotServed(dir string) bool {
	switch strings.ToLower(dir) {
	case "vendor", "tests", "test", "docs", "doc", "examples", "example", "node_modules", "plugins", "src", "lib":
		return true
	}
	return strings.HasPrefix(dir, ".")
}

// composerStart is the server a composer.json's start script runs — `php -S
// 0.0.0.0:8000 router.php`, for an app whose router script is its whole
// front controller — or "".
func composerStart(contextDir string) string {
	var manifest struct {
		Scripts map[string]json.RawMessage `json:"scripts"`
	}
	if json.Unmarshal([]byte(readFile(contextDir, "composer.json")), &manifest) != nil {
		return ""
	}
	for _, name := range []string{"start", "serve"} {
		raw, ok := manifest.Scripts[name]
		if !ok {
			continue
		}
		var lines []string
		var one string
		if json.Unmarshal(raw, &one) == nil {
			lines = []string{one}
		} else {
			_ = json.Unmarshal(raw, &lines)
		}
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "php -S ") && safeCommand(line) {
				return line
			}
		}
	}
	return ""
}

// --- PHP with no composer.json ---------------------------------------------

// plainPHPRoot is the web root of a PHP app Railpack cannot see: no
// composer.json and no index.php at the root, but one in a web root. Railpack
// served such an app as static files (issue #90). "" when this is not one.
func plainPHPRoot(contextDir string) string {
	if fileExists(contextDir, "composer.json") || fileExists(contextDir, "index.php") {
		return ""
	}
	for _, dir := range phpWebRoots {
		if fileExists(contextDir, dir+"/index.php") {
			return dir
		}
	}
	return ""
}

// plainPHPImage is the PHP a plain PHP app runs on. [P] The official image
// with Apache, which serves a directory of PHP files the way the shared host
// such an app was written for did, .htaccess included; the newest PHP minor
// Railpack also builds on.
const plainPHPImage = "php:8.4-apache"

// plainPHPDockerfile serves root with Apache and mod_php. Only root is the
// document root, so the code beside it (src/, a config file) is never served.
func plainPHPDockerfile(root string) string {
	return fmt.Sprintf(`# A PHP app with no composer.json, served from %[1]s/ by Apache.
FROM %[2]s
ENV APACHE_DOCUMENT_ROOT=/var/www/html/%[1]s
RUN a2enmod rewrite \
 && sed -ri -e 's!/var/www/html!${APACHE_DOCUMENT_ROOT}!g' /etc/apache2/sites-available/*.conf \
 && sed -ri -e 's!/var/www/!${APACHE_DOCUMENT_ROOT}!g' /etc/apache2/apache2.conf /etc/apache2/conf-available/*.conf \
 && sed -ri -e 's!AllowOverride None!AllowOverride All!g' /etc/apache2/apache2.conf
COPY . /var/www/html/
EXPOSE 80
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec apache2-foreground"]
`, root, plainPHPImage)
}

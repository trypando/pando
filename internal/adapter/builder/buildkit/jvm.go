package buildkit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// JVM apps are planned by Pando rather than by nixpacks.
//
// nixpacks 1.41 refuses Gradle 9 outright ("Unsupported Gradle version"),
// builds Maven projects on a JDK of its choosing rather than the one the
// project declares, and has no plan at all for Java sources with no build tool
// (issue #55). The build tool the repository carries already says how to build
// it; what it needs is that tool on the JDK the project names, which the
// official images provide.
//
// What the build produces is not always one runnable jar (issue #90). A WAR is
// deployed into a servlet container; Quarkus writes target/quarkus-app/ and
// runs quarkus-run.jar from it; Helidon's jar names target/libs/ on its
// manifest class path; Gradle's application plugin makes a distribution with a
// start script and no runnable jar at all. The build file says which, and the
// plan runs each the way its own documentation does.

// jvmDefault is the JDK a project that names none is built and run on. [P] 21,
// the current LTS that Spring Boot 3 and 4 and Gradle 9 all support.
const jvmDefault = 21

// jvmReleases are the JDKs published as eclipse-temurin images. A project that
// names one in between is built on the next one up, which runs what it
// compiles.
var jvmReleases = []int{8, 11, 17, 21, 25}

// What a JVM build produces, and so how the image runs it.
const (
	// jvmJar is a runnable jar, with what its manifest's class path names
	// beside it, or Quarkus's quarkus-app/. Run with java -jar.
	jvmJar = "jar"
	// jvmDist is the distribution Gradle's application plugin installs: lib/
	// and a start script in bin/.
	jvmDist = "dist"
	// jvmWar is a web archive, deployed to Tomcat as the root application.
	jvmWar = "war"
	// jvmLiberty is a WAR for Open Liberty, packaged by the project's own
	// Liberty plugin with the server into one runnable jar.
	jvmLiberty = "liberty"
)

type jvmBuild struct {
	Tool      string // gradle, maven, javac
	JDK       int
	MainClass string // javac only

	// Output is what the build produces: jvmJar, jvmDist, jvmWar or
	// jvmLiberty. Empty for javac.
	Output string

	// Javax is a WAR written against javax.servlet, which Tomcat 10 and later
	// do not load. Only for jvmWar.
	Javax bool

	// Task is the Gradle task that builds what runs. Gradle only.
	Task string
}

var (
	gradleToolchain = regexp.MustCompile(`(?:JavaLanguageVersion\.of|jvmToolchain)\(\s*(\d+)\s*\)`)
	gradleSource    = regexp.MustCompile(`(?:sourceCompatibility|targetCompatibility|jvmTarget)\s*=\s*['"]?(?:JavaVersion\.VERSION_)?(?:1_)?(\d+)`)
	mavenVersion    = regexp.MustCompile(`<(?:java\.version|maven\.compiler\.release|maven\.compiler\.source|release)>\s*(?:1\.)?(\d+)\s*<`)
	javaMain        = regexp.MustCompile(`public\s+static\s+void\s+main\s*\(`)
	javaPackage     = regexp.MustCompile(`(?m)^\s*package\s+`)

	mavenWar = regexp.MustCompile(`<packaging>\s*war\s*</packaging>`)

	// gradleWarPlugin and gradleAppPlugin find a core plugin applied in any of
	// the three ways a build script can: `war` alone in a plugins block,
	// `id("war")` or `id 'war'`, or `apply plugin: 'war'`.
	gradleWarPlugin = regexp.MustCompile(`(?m)^\s*(?:war\s*$|id\s*\(?\s*["']war["']|apply\s+plugin\s*:\s*["']war["'])`)
	gradleAppPlugin = regexp.MustCompile(`(?m)^\s*(?:application\s*$|application\s*\{|id\s*\(?\s*["']application["']|apply\s+plugin\s*:\s*["']application["'])`)

	// javaxServlet is a build that names the javax servlet or Java EE API. A
	// build naming neither javax nor jakarta is taken as Jakarta, which is
	// what every current framework is written against.
	javaxServlet = regexp.MustCompile(`javax\.servlet|<groupId>\s*javax\s*</groupId>|javax:javaee`)
)

// gradlePluginsApplying are plugins that apply Gradle's application plugin
// themselves: Ktor's and Play's.
var gradlePluginsApplying = []string{"io.ktor.plugin", "org.playframework.play"}

// readJVMBuild reports a JVM project Pando knows how to build.
func readJVMBuild(contextDir string) (jvmBuild, bool) {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(contextDir, name))
		return err == nil
	}

	switch {
	case exists("gradlew") && (exists("build.gradle") || exists("build.gradle.kts")):
		return gradleBuild(contextDir), true
	case exists("pom.xml"):
		return mavenBuild(contextDir), true
	}
	return javacBuild(contextDir)
}

// gradleBuild is how a project with a Gradle wrapper is built.
func gradleBuild(contextDir string) jvmBuild {
	script := readFile(contextDir, "build.gradle") + readFile(contextDir, "build.gradle.kts")
	// A version catalog names the plugins a script applies by alias.
	plugins := script + readFile(contextDir, filepath.Join("gradle", "libs.versions.toml"))
	b := jvmBuild{Tool: "gradle", JDK: gradleJDK(script), Output: jvmJar, Task: "assemble"}
	switch {
	case strings.Contains(plugins, "org.springframework.boot"), strings.Contains(plugins, "io.quarkus"):
		// Spring Boot's executable jar or WAR, and Quarkus's quarkus-app/,
		// are what each documents running, even with the application or
		// war plugin applied. assemble builds either; the plain jar Spring
		// Boot leaves beside its own is passed over.
	case gradleWarPlugin.MatchString(script):
		b.Output, b.Javax = jvmWar, isJavax(plugins)
	case gradleAppPlugin.MatchString(script) || containsAny(plugins, gradlePluginsApplying):
		b.Output, b.Task = jvmDist, "installDist"
	}
	return b
}

// mavenBuild is how a project with a pom.xml is built.
func mavenBuild(contextDir string) jvmBuild {
	pom := readFile(contextDir, "pom.xml")
	b := jvmBuild{Tool: "maven", JDK: jdkFrom(pom, mavenVersion), Output: jvmJar}
	// A Spring Boot WAR is executable, and runs as its jar would.
	if !mavenWar.MatchString(pom) || strings.Contains(pom, "spring-boot-maven-plugin") {
		return b
	}
	if strings.Contains(pom, "liberty-maven-plugin") {
		b.Output = jvmLiberty
	} else {
		b.Output, b.Javax = jvmWar, isJavax(pom)
	}
	return b
}

// javacBuild is Java sources and no build tool: compiled as they are, and the
// class with main is run. Only sources at the top level in the default package
// — a package tree with no build file is not something to guess at.
func javacBuild(contextDir string) (jvmBuild, bool) {
	sources, _ := filepath.Glob(filepath.Join(contextDir, "*.java"))
	sort.Strings(sources)
	var main string
	for _, s := range sources {
		body, err := os.ReadFile(s)
		if err != nil || javaPackage.Match(body) {
			return jvmBuild{}, false
		}
		if javaMain.Match(body) && main == "" {
			main = strings.TrimSuffix(filepath.Base(s), ".java")
		}
	}
	if main == "" {
		return jvmBuild{}, false
	}
	return jvmBuild{Tool: "javac", JDK: jvmDefault, MainClass: main}, true
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func isJavax(build string) bool {
	return javaxServlet.MatchString(build) && !strings.Contains(build, "jakarta.")
}

// gradleJDK is the JDK a Gradle build runs on.
//
// A toolchain is a requirement, not a minimum: Gradle looks for exactly that
// release and, with no download repository configured, fails when the image
// has another ("Cannot find a Java installation on your machine matching:
// {languageVersion=17}", issue #90). So a toolchain is built on the release it
// names. Temurin publishes every release from 17 on; an older one between the
// LTS releases is rounded up, and the build says what it could not find.
func gradleJDK(script string) int {
	if m := gradleToolchain.FindStringSubmatch(script); m != nil {
		if v, err := strconv.Atoi(m[1]); err == nil && (v >= 17 || publishedJDK(v)) {
			return v
		}
	}
	return jdkFrom(script, gradleToolchain, gradleSource)
}

func publishedJDK(v int) bool {
	for _, release := range jvmReleases {
		if release == v {
			return true
		}
	}
	return false
}

// jdkFrom is the JDK a build file declares, rounded up to a published one.
func jdkFrom(text string, patterns ...*regexp.Regexp) int {
	for _, p := range patterns {
		if m := p.FindStringSubmatch(text); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				return releaseAtLeast(v)
			}
		}
	}
	return jvmDefault
}

// releaseAtLeast is the oldest published LTS JDK no older than v.
func releaseAtLeast(v int) int {
	for _, release := range jvmReleases {
		if release >= v {
			return release
		}
	}
	return jvmReleases[len(jvmReleases)-1]
}

// safeClass accepts a Java class name and nothing else.
var safeClass = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// jvmAppDir is where the build's output goes in the image. Not the working
// directory, which holds the repository's files: an app that reads its views
// or configuration from where it was started finds them there, as it does
// when its README runs `java -jar target/app.jar` from the project (issue
// #90). The Go plan runs its program the same way.
const jvmAppDir = "/opt/app"

// collectJar puts the build's runnable jar in /out/app.jar, with what it needs
// beside it.
//
// Quarkus's quarkus-app/ is copied whole: quarkus-run.jar finds the
// application from its own directory, under any name. Otherwise the largest
// jar is the one with the application and its dependencies in it — a shaded,
// assembled or Spring Boot jar beside the thin one it was made from — and the
// directories a manifest class path names are copied next to it, as Helidon's
// libs/ is. A jar with no Main-Class cannot be run with java -jar, and the
// build says so rather than the app failing to start.
func collectJar(output, jars string) string {
	return fmt.Sprintf(`RUN set -e; mkdir -p /out; \
    if [ -f %[1]s/quarkus-app/quarkus-run.jar ]; then \
      cp -r %[1]s/quarkus-app/. /out/ && mv /out/quarkus-run.jar /out/app.jar; \
    else \
      built="$(ls -S %[2]s/*.jar %[2]s/*.war 2>/dev/null | grep -v -e '-plain.jar$' -e '-sources.jar$' -e '-javadoc.jar$' -e '-tests.jar$' -e '/original-' | head -n 1)"; \
      if [ -z "$built" ]; then echo "The build finished without writing a jar to %[2]s/, so there is nothing for Pando to run. Add a Dockerfile that says how this app is built and started." >&2; exit 1; fi; \
      cp "$built" /out/app.jar; \
      for d in libs lib dependency; do if [ -d "%[2]s/$d" ]; then cp -r "%[2]s/$d" /out/; fi; done; \
      (cd /tmp && jar xf /out/app.jar META-INF/MANIFEST.MF); \
      if ! grep -qs '^Main-Class:' /tmp/META-INF/MANIFEST.MF; then echo "$built has no Main-Class in its manifest, so java -jar cannot start it. Make the build produce a runnable jar, for example with the Spring Boot, Shade or Shadow plugin or the Gradle application plugin, or add a Dockerfile that says how the app starts." >&2; exit 1; fi; \
    fi
`, output, jars)
}

// collectWar puts the build's web archive in /out/app.war.
func collectWar(dir string) string {
	return fmt.Sprintf(`RUN mkdir -p /out && war="$(ls -S %[1]s/*.war 2>/dev/null | head -n 1)" && \
    if [ -z "$war" ]; then echo "The build finished without writing a WAR to %[1]s/, so there is nothing for Pando to deploy. Add a Dockerfile that says how this app is built and started." >&2; exit 1; fi && \
    cp "$war" /out/app.war
`, dir)
}

// collectDist puts the application plugin's installed distribution in /out,
// with its start script linked as bin/pando-start. The script is named after
// the project, which is the install directory's name too; Gradle's start
// scripts follow a link to find their own directory.
const collectDist = `RUN name="$(ls build/install 2>/dev/null | head -n 1)" && \
    if [ -z "$name" ]; then echo "installDist finished without writing build/install/, so there is nothing for Pando to run. Add a Dockerfile that says how this app is built and started." >&2; exit 1; fi && \
    mkdir -p /out && cp -r "build/install/$name/." /out/ && rm -f /out/bin/*.bat && \
    script="$name" && if [ ! -f "/out/bin/$script" ]; then script="$(ls /out/bin | head -n 1)"; fi && \
    ln -s "$script" /out/bin/pando-start
`

// tomcatFor is the Tomcat line and JRE a WAR runs on. Tomcat 11 is the
// current line for Jakarta EE and needs Java 17; Tomcat 9 is the last that
// loads javax.servlet.
func tomcatFor(b jvmBuild) (string, int) {
	jre := releaseAtLeast(b.JDK)
	if b.Javax {
		return "9.0", jre
	}
	return "11.0", max(jre, 17)
}

// writeJVMPlan writes the plan into .pando-plan/, where detection collects a plan
// and the build replays it (R-020).
func writeJVMPlan(contextDir string, b jvmBuild) (string, error) {
	var build, collect, runtime string
	switch b.Tool {
	case "gradle":
		const jars = "build/libs"
		if !safeClass.MatchString(b.Task) {
			return "", errs.Newf(errs.BuildFailed, "%q is not a Gradle task Pando can run.", b.Task)
		}
		build = fmt.Sprintf(`# A Gradle project, built with its own wrapper on JDK %[1]d.
FROM eclipse-temurin:%[1]d-jdk AS build
WORKDIR /app
COPY . .
RUN chmod +x gradlew && ./gradlew --no-daemon %[2]s -x test
`, b.JDK, b.Task)
		switch b.Output {
		case jvmDist:
			collect = collectDist
		case jvmWar:
			collect = collectWar(jars)
		default:
			collect = collectJar("build", jars)
		}

	case "maven":
		goals := "package"
		if b.Output == jvmLiberty {
			// The project's Liberty plugin makes the server, installs the
			// features its server.xml names, deploys the WAR into it and
			// packages the lot as one runnable jar. Not as a loose
			// application, which points at the build's own directories.
			goals = "package liberty:create liberty:install-feature liberty:deploy liberty:package -DlooseApplication=false -Dinclude=runnable"
		}
		// MAVEN_CONFIG is unset for the wrapper: the maven image sets it to
		// /root/.m2, and mvnw passes it on as an argument, which Maven reads
		// as a lifecycle phase named "/root/.m2" (issue #55).
		build = fmt.Sprintf(`# A Maven project, built on JDK %[1]d.
FROM maven:3-eclipse-temurin-%[1]d AS build
WORKDIR /app
COPY . .
RUN if [ -f mvnw ]; then chmod +x mvnw && env -u MAVEN_CONFIG ./mvnw -B -DskipTests %[2]s; else mvn -B -DskipTests %[2]s; fi
`, b.JDK, goals)
		if b.Output == jvmWar {
			collect = collectWar("target")
		} else {
			collect = collectJar("target", "target")
		}

	case "javac":
		if !safeClass.MatchString(b.MainClass) {
			return "", errs.Newf(errs.BuildFailed, "%q is not a Java class name Pando can run.", b.MainClass)
		}
		return writePlan(contextDir, fmt.Sprintf(`# Java sources with no build tool, compiled as they are on JDK %[1]d.
FROM eclipse-temurin:%[1]d-jdk
WORKDIR /app
COPY . .
RUN mkdir -p /app/classes && javac -d /app/classes *.java
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec java -cp /app/classes %[2]s"]
`, b.JDK, b.MainClass))

	default:
		return "", errs.Newf(errs.BuildFailed, "Pando cannot build a %s project.", b.Tool)
	}

	switch b.Output {
	case jvmWar:
		line, jre := tomcatFor(b)
		runtime = fmt.Sprintf(`
# The WAR is the root application of a Tomcat %[1]s server.
FROM tomcat:%[1]s-jre%[2]d
RUN rm -rf /usr/local/tomcat/webapps/*
COPY --from=build /out/app.war /usr/local/tomcat/webapps/ROOT.war
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec catalina.sh run"]
`, line, jre)
	case jvmDist:
		runtime = fmt.Sprintf(`
FROM eclipse-temurin:%[1]d-jre
WORKDIR /app
COPY . .
COPY --from=build /out/ %[2]s/
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec %[2]s/bin/pando-start"]
`, b.JDK, jvmAppDir)
	default:
		runtime = fmt.Sprintf(`
FROM eclipse-temurin:%[1]d-jre
WORKDIR /app
COPY . .
COPY --from=build /out/ %[2]s/
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec java -jar %[2]s/app.jar"]
`, b.JDK, jvmAppDir)
	}
	return writePlan(contextDir, build+collect+runtime)
}

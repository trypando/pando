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

// jvmDefault is the JDK a project that names none is built and run on. [P] 21,
// the current LTS that Spring Boot 3 and 4 and Gradle 9 all support.
const jvmDefault = 21

// jvmReleases are the JDKs published as eclipse-temurin images. A project that
// names one in between is built on the next one up, which runs what it
// compiles.
var jvmReleases = []int{8, 11, 17, 21, 25}

type jvmBuild struct {
	Tool      string // gradle, maven, javac
	JDK       int
	MainClass string // javac only
}

var (
	gradleToolchain = regexp.MustCompile(`JavaLanguageVersion\.of\(\s*(\d+)\s*\)`)
	gradleSource    = regexp.MustCompile(`(?:sourceCompatibility|targetCompatibility|jvmTarget)\s*=\s*['"]?(?:JavaVersion\.VERSION_)?(?:1_)?(\d+)`)
	mavenVersion    = regexp.MustCompile(`<(?:java\.version|maven\.compiler\.release|maven\.compiler\.source|release)>\s*(?:1\.)?(\d+)\s*<`)
	javaMain        = regexp.MustCompile(`public\s+static\s+void\s+main\s*\(`)
	javaPackage     = regexp.MustCompile(`(?m)^\s*package\s+`)
)

// readJVMBuild reports a JVM project Pando knows how to build.
func readJVMBuild(contextDir string) (jvmBuild, bool) {
	read := func(name string) string {
		body, _ := os.ReadFile(filepath.Join(contextDir, name))
		return string(body)
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(contextDir, name))
		return err == nil
	}

	switch {
	case exists("gradlew") && (exists("build.gradle") || exists("build.gradle.kts")):
		script := read("build.gradle") + read("build.gradle.kts")
		return jvmBuild{Tool: "gradle", JDK: jdkFrom(script, gradleToolchain, gradleSource)}, true

	case exists("pom.xml"):
		return jvmBuild{Tool: "maven", JDK: jdkFrom(read("pom.xml"), mavenVersion)}, true
	}

	// Java sources and no build tool: compiled as they are, and the class with
	// main is run. Only sources at the top level in the default package — a
	// package tree with no build file is not something to guess at.
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

// jdkFrom is the JDK a build file declares, rounded up to a published one.
func jdkFrom(text string, patterns ...*regexp.Regexp) int {
	for _, p := range patterns {
		if m := p.FindStringSubmatch(text); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				for _, release := range jvmReleases {
					if release >= v {
						return release
					}
				}
				return jvmReleases[len(jvmReleases)-1]
			}
		}
	}
	return jvmDefault
}

// safeClass accepts a Java class name and nothing else.
var safeClass = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

// writeJVMPlan writes the plan into .pando-plan/, where detection collects a plan
// and the build replays it (R-020).
func writeJVMPlan(contextDir string, b jvmBuild) (string, error) {
	var content string
	switch b.Tool {
	case "gradle":
		content = fmt.Sprintf(`# A Gradle project, built with its own wrapper on JDK %[1]d.
FROM eclipse-temurin:%[1]d-jdk AS build
WORKDIR /app
COPY . .
RUN chmod +x gradlew && (./gradlew --no-daemon bootJar -x test || ./gradlew --no-daemon build -x test)
RUN mkdir -p /out && cp "$(ls build/libs/*.jar | grep -v -- '-plain.jar' | head -n 1)" /out/app.jar

FROM eclipse-temurin:%[1]d-jre
WORKDIR /app
COPY --from=build /out/app.jar /app/app.jar
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec java -jar /app/app.jar"]
`, b.JDK)

	case "maven":
		// MAVEN_CONFIG is unset for the wrapper: the maven image sets it to
		// /root/.m2, and mvnw passes it on as an argument, which Maven reads
		// as a lifecycle phase named "/root/.m2" (issue #55).
		content = fmt.Sprintf(`# A Maven project, built on JDK %[1]d.
FROM maven:3-eclipse-temurin-%[1]d AS build
WORKDIR /app
COPY . .
RUN if [ -f mvnw ]; then chmod +x mvnw && env -u MAVEN_CONFIG ./mvnw -B -DskipTests package; else mvn -B -DskipTests package; fi
RUN mkdir -p /out && cp "$(ls target/*.jar | grep -v -e '-sources.jar' -e '-javadoc.jar' -e '/original-' | head -n 1)" /out/app.jar

FROM eclipse-temurin:%[1]d-jre
WORKDIR /app
COPY --from=build /out/app.jar /app/app.jar
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec java -jar /app/app.jar"]
`, b.JDK)

	case "javac":
		if !safeClass.MatchString(b.MainClass) {
			return "", errs.Newf(errs.BuildFailed, "%q is not a Java class name Pando can run.", b.MainClass)
		}
		content = fmt.Sprintf(`# Java sources with no build tool, compiled as they are on JDK %[1]d.
FROM eclipse-temurin:%[1]d-jdk
WORKDIR /app
COPY . .
RUN mkdir -p /app/classes && javac -d /app/classes *.java
ENTRYPOINT ["/bin/sh", "-c"]
CMD ["exec java -cp /app/classes %[2]s"]
`, b.JDK, b.MainClass)

	default:
		return "", errs.Newf(errs.BuildFailed, "Pando cannot build a %s project.", b.Tool)
	}

	name := filepath.Join(planDir, "Dockerfile")
	full := filepath.Join(contextDir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	// G306: a generated build input in the build context, read by the rootless
	// builder as a different user. It holds no secret.
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec
		return "", errs.Wrap(errs.BuildFailed, "Could not prepare the build.", err)
	}
	return name, nil
}

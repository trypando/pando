package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR095_AJVMProjectIsBuiltWithItsOwnToolOnItsOwnJDK asserts R-095.
//
// nixpacks refused Gradle 9, built Maven on a JDK the project did not ask for,
// and had no plan for Java with no build tool (issue #55).
func TestR095_AJVMProjectIsBuiltWithItsOwnToolOnItsOwnJDK(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  jvmBuild
	}{
		"gradle toolchain": {
			files: map[string]string{"gradlew": "", "build.gradle": "java { toolchain { languageVersion = JavaLanguageVersion.of(17) } }"},
			want:  jvmBuild{Tool: "gradle", JDK: 17, Output: jvmJar, Task: "assemble"},
		},
		"maven java.version": {
			files: map[string]string{"pom.xml": "<properties><java.version>24</java.version></properties>"},
			want:  jvmBuild{Tool: "maven", JDK: 25, Output: jvmJar},
		},
		"maven with nothing declared": {
			files: map[string]string{"pom.xml": "<project/>"},
			want:  jvmBuild{Tool: "maven", JDK: jvmDefault, Output: jvmJar},
		},
		"plain java": {
			files: map[string]string{"Main.java": "public class Main { public static void main(String[] a) {} }"},
			want:  jvmBuild{Tool: "javac", JDK: jvmDefault, MainClass: "Main"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := readJVMBuild(writeFiles(t, tc.files))
			require.True(t, ok)
			require.Equal(t, tc.want, got)
		})
	}

	_, ok := readJVMBuild(writeFiles(t, map[string]string{"Lib.java": "package x; public class Lib {}"}))
	require.False(t, ok, "a package tree with no build file is not guessed at")
}

// TestR095_AGradleToolchainIsBuiltOnTheJDKItNames asserts R-095.
//
// Gradle looks for exactly the release a toolchain names and fails on any
// other, and Kotlin's jvmToolchain(17) was not read at all: the build ran on
// the default 21 and stopped with "Cannot find a Java installation ...
// matching: {languageVersion=17}" (issue #90). A toolchain between the LTS
// releases is built on that release; a source or target compatibility is a
// minimum, and rounds up.
func TestR095_AGradleToolchainIsBuiltOnTheJDKItNames(t *testing.T) {
	cases := map[string]struct {
		script string
		want   int
	}{
		"kotlin jvmToolchain":           {"kotlin {\n    jvmToolchain(17)\n}\n", 17},
		"kotlin jvmToolchain off-LTS":   {"kotlin { jvmToolchain(24) }", 24},
		"java toolchain off-LTS":        {"java { toolchain { languageVersion.set(JavaLanguageVersion.of(24)) } }", 24},
		"source compatibility rounds":   {"java { sourceCompatibility = JavaVersion.VERSION_24 }", 25},
		"an unpublished old toolchain":  {"kotlin { jvmToolchain(9) }", 11},
		"nothing declared":              {"plugins { java }", jvmDefault},
		"a toolchain outranks a target": {"java { sourceCompatibility = '11' }\nkotlin { jvmToolchain(17) }", 17},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, ok := readJVMBuild(writeFiles(t, map[string]string{"gradlew": "", "build.gradle.kts": tc.script}))
			require.True(t, ok)
			require.Equal(t, tc.want, b.JDK)
		})
	}
}

// TestR095_AJVMBuildRunsWhatItsBuildProduces asserts R-095.
//
// Every JVM plan copied target/*.jar and ran it with java -jar. A WAR, Open
// Liberty's WAR, Gradle's application distribution and Play's had no jar to
// run, and Helidon's thin jar lost the libs/ its manifest names (issue #90).
// The build file says which of these a project makes.
func TestR095_AJVMBuildRunsWhatItsBuildProduces(t *testing.T) {
	cases := map[string]struct {
		files  map[string]string
		output string
		task   string
		javax  bool
	}{
		"a maven WAR": {
			files:  map[string]string{"pom.xml": "<packaging>war</packaging><artifactId>jakarta.servlet-api</artifactId>"},
			output: jvmWar,
		},
		"a maven javax WAR": {
			files:  map[string]string{"pom.xml": "<packaging>war</packaging><groupId>javax.servlet</groupId>"},
			output: jvmWar, javax: true,
		},
		"an Open Liberty WAR": {
			files:  map[string]string{"pom.xml": "<packaging>war</packaging><artifactId>liberty-maven-plugin</artifactId>"},
			output: jvmLiberty,
		},
		"a Spring Boot WAR runs as its jar would": {
			files:  map[string]string{"pom.xml": "<packaging>war</packaging><artifactId>spring-boot-maven-plugin</artifactId>"},
			output: jvmJar,
		},
		"Quarkus": {
			files:  map[string]string{"pom.xml": "<packaging>quarkus</packaging><artifactId>quarkus-maven-plugin</artifactId>"},
			output: jvmJar,
		},
		"gradle's application plugin": {
			files:  map[string]string{"gradlew": "", "build.gradle.kts": "plugins {\n    kotlin(\"jvm\") version \"2.1.0\"\n    application\n}\napplication { mainClass.set(\"MainKt\") }\n"},
			output: jvmDist, task: "installDist",
		},
		"the application plugin by id": {
			files:  map[string]string{"gradlew": "", "build.gradle": "plugins {\n  id 'application'\n}\n"},
			output: jvmDist, task: "installDist",
		},
		"Ktor's plugin applies it": {
			files:  map[string]string{"gradlew": "", "build.gradle.kts": "plugins { id(\"io.ktor.plugin\") version \"3.6.0\" }"},
			output: jvmDist, task: "installDist",
		},
		"Play's plugin, named in the version catalog": {
			files: map[string]string{
				"gradlew": "", "build.gradle.kts": "plugins {\n    alias(libs.plugins.play)\n}\n",
				"gradle/libs.versions.toml": "[plugins]\nplay = { id = \"org.playframework.play\", version = \"3.1.0\" }\n",
			},
			output: jvmDist, task: "installDist",
		},
		"Spring Boot with the application plugin": {
			files:  map[string]string{"gradlew": "", "build.gradle": "plugins {\n  id 'org.springframework.boot' version '3.5.0'\n  id 'application'\n}\n"},
			output: jvmJar, task: "assemble",
		},
		"gradle's war plugin": {
			files:  map[string]string{"gradlew": "", "build.gradle": "plugins {\n  id 'war'\n}\n"},
			output: jvmWar, task: "assemble",
		},
		"a plain gradle jar": {
			files:  map[string]string{"gradlew": "", "build.gradle": "plugins {\n  id 'java'\n}\n"},
			output: jvmJar, task: "assemble",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b, ok := readJVMBuild(withFiles(t, tc.files))
			require.True(t, ok)
			require.Equal(t, tc.output, b.Output)
			require.Equal(t, tc.task, b.Task)
			require.Equal(t, tc.javax, b.Javax)
		})
	}
}

// Each output runs the way its framework documents: a WAR as Tomcat's root
// application, Liberty packaged with its server by the project's own plugin, a
// distribution from its start script, and a jar — or Quarkus's quarkus-app/ —
// with what its manifest names beside it. The app's working directory is the
// repository, where an app that reads views/ or conf/ at run time finds them.
func TestR095_EachJVMOutputHasItsOwnRuntime(t *testing.T) {
	plan := func(b jvmBuild) string {
		t.Helper()
		root := t.TempDir()
		name, err := writeJVMPlan(root, b)
		require.NoError(t, err)
		body, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err)
		return string(body)
	}

	jar := plan(jvmBuild{Tool: "maven", JDK: 21, Output: jvmJar})
	require.Contains(t, jar, "cp -r target/quarkus-app/. /out/")
	require.Contains(t, jar, `for d in libs lib dependency; do if [ -d "target/$d" ]`)
	require.Contains(t, jar, "has no Main-Class in its manifest")
	require.Contains(t, jar, "FROM eclipse-temurin:21-jre\nWORKDIR /app\nCOPY . .\nCOPY --from=build /out/ /opt/app/")
	require.Contains(t, jar, `CMD ["exec java -jar /opt/app/app.jar"]`)

	liberty := plan(jvmBuild{Tool: "maven", JDK: 17, Output: jvmLiberty})
	require.Contains(t, liberty, "package liberty:create liberty:install-feature liberty:deploy liberty:package -DlooseApplication=false -Dinclude=runnable")
	require.Contains(t, liberty, `CMD ["exec java -jar /opt/app/app.jar"]`)

	war := plan(jvmBuild{Tool: "maven", JDK: 11, Output: jvmWar})
	require.Contains(t, war, "FROM tomcat:11.0-jre17", "Tomcat 11 needs Java 17")
	require.Contains(t, war, "COPY --from=build /out/app.war /usr/local/tomcat/webapps/ROOT.war")
	require.Contains(t, war, `CMD ["exec catalina.sh run"]`)
	require.Contains(t, plan(jvmBuild{Tool: "maven", JDK: 8, Output: jvmWar, Javax: true}), "FROM tomcat:9.0-jre8")
	require.Contains(t, plan(jvmBuild{Tool: "gradle", JDK: 24, Output: jvmWar, Task: "assemble"}), "FROM tomcat:11.0-jre25",
		"a JDK between LTS releases runs on the next Tomcat image up")

	dist := plan(jvmBuild{Tool: "gradle", JDK: 24, Output: jvmDist, Task: "installDist"})
	require.Contains(t, dist, "FROM eclipse-temurin:24-jdk AS build")
	require.Contains(t, dist, "./gradlew --no-daemon installDist -x test")
	require.Contains(t, dist, `cp -r "build/install/$name/." /out/`)
	require.Contains(t, dist, "FROM eclipse-temurin:24-jre")
	require.Contains(t, dist, `CMD ["exec /opt/app/bin/pando-start"]`)

	gradleJar := plan(jvmBuild{Tool: "gradle", JDK: 17, Output: jvmJar, Task: "assemble"})
	require.Contains(t, gradleJar, "cp -r build/quarkus-app/. /out/")
	require.Contains(t, gradleJar, "ls -S build/libs/*.jar build/libs/*.war")
}

func TestAJVMPlanStartsThroughAShellLikeEveryPlan(t *testing.T) {
	root := t.TempDir()
	name, err := writeJVMPlan(root, jvmBuild{Tool: "maven", JDK: 21, Output: jvmJar})
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "FROM maven:3-eclipse-temurin-21 AS build")
	require.Contains(t, string(body), `ENTRYPOINT ["/bin/sh", "-c"]`)
	require.Contains(t, string(body), `CMD ["exec java -jar /opt/app/app.jar"]`)
}

// A JDK newer than any published image is built on the newest one, which is
// the closest thing to what the project asked for.
func TestAJDKNewerThanAnyReleaseGetsTheNewest(t *testing.T) {
	b, ok := readJVMBuild(writeFiles(t, map[string]string{
		"gradlew": "", "build.gradle.kts": "java { sourceCompatibility = JavaVersion.VERSION_99 }",
	}))
	require.True(t, ok)
	require.Equal(t, jvmReleases[len(jvmReleases)-1], b.JDK)
}

// Java sources with no class that has a main have nothing to run, and are
// left to nixpacks.
func TestJavaSourcesWithoutAMainAreNotPlanned(t *testing.T) {
	_, ok := readJVMBuild(writeFiles(t, map[string]string{"Util.java": "public class Util { static int x() { return 1; } }"}))
	require.False(t, ok)
}

// Each build tool gets its own plan, and each starts through a shell. The
// javac plan runs the main class by name, so a name that is not a Java class
// is refused before it reaches the Dockerfile, as is a Gradle task that is
// not a plain name, and a tool Pando has no plan for.
func TestEachJVMBuildToolHasItsOwnPlan(t *testing.T) {
	gradle := t.TempDir()
	name, err := writeJVMPlan(gradle, jvmBuild{Tool: "gradle", JDK: 17, Output: jvmJar, Task: "assemble"})
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(gradle, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "FROM eclipse-temurin:17-jdk AS build")
	require.Contains(t, string(body), "./gradlew --no-daemon assemble -x test")
	require.Contains(t, string(body), "FROM eclipse-temurin:17-jre")

	javac := t.TempDir()
	name, err = writeJVMPlan(javac, jvmBuild{Tool: "javac", JDK: 21, MainClass: "Main"})
	require.NoError(t, err)
	body, err = os.ReadFile(filepath.Join(javac, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "javac -d /app/classes *.java")
	require.Contains(t, string(body), `CMD ["exec java -cp /app/classes Main"]`)

	_, err = writeJVMPlan(t.TempDir(), jvmBuild{Tool: "javac", JDK: 21, MainClass: "Main; rm -rf /"})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "is not a Java class name")

	_, err = writeJVMPlan(t.TempDir(), jvmBuild{Tool: "gradle", JDK: 21, Output: jvmJar, Task: "build; rm -rf /"})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "is not a Gradle task")

	_, err = writeJVMPlan(t.TempDir(), jvmBuild{Tool: "sbt", JDK: 21})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "sbt")
}

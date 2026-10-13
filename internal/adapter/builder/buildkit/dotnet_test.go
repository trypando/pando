package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	webProject     = `<Project Sdk="Microsoft.NET.Sdk.Web"><PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup></Project>`
	wasmProject    = `<Project Sdk="Microsoft.NET.Sdk.BlazorWebAssembly"><PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup></Project>`
	libraryProject = `<Project Sdk="Microsoft.NET.Sdk"><PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup></Project>`
)

// TestR095_TheWebProjectInADotnetSolutionIsTheOneBuilt asserts R-095.
//
// A Blazor Web App is a solution: the server project, the WebAssembly client
// it serves, and often class libraries and tests, each in its own directory
// under a .sln or .slnx. Pando looked only for a project at the top, found
// none, and asked how to build the app (issue #90). The server is the one
// project with the web SDK.
func TestR095_TheWebProjectInADotnetSolutionIsTheOneBuilt(t *testing.T) {
	blazor := withFiles(t, map[string]string{
		"BlazorApp.sln":                            "",
		"BlazorApp/BlazorApp.csproj":               webProject,
		"BlazorApp.Client/BlazorApp.Client.csproj": wasmProject,
		"BlazorApp/bin/Debug/Stale/Stale.csproj":   webProject,
		"BlazorApp.Client/obj/Generated.csproj":    webProject,
	})
	b, ok := readDotnetBuild(blazor)
	require.True(t, ok)
	require.Equal(t, dotnetBuild{Project: "BlazorApp/BlazorApp.csproj", Assembly: "BlazorApp", Version: "10.0"}, b)

	name, err := writeDotnetPlan(blazor, b)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(blazor, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "RUN dotnet publish BlazorApp/BlazorApp.csproj -c Release -o /out -p:PublishProfile=\n",
		"a publish profile that pushes a container is not followed: the build has no container runtime")
	require.Contains(t, string(body), `CMD ["exec dotnet BlazorApp.dll"]`)

	// ServiceStack's template: a .slnx, the app beside its libraries and tests.
	slnx := withFiles(t, map[string]string{
		"MyApp.slnx":         "<Solution/>",
		"MyApp/MyApp.csproj": webProject,
		"MyApp.ServiceModel/MyApp.ServiceModel.csproj":         libraryProject,
		"MyApp.ServiceInterface/MyApp.ServiceInterface.csproj": libraryProject,
		"MyApp.Tests/MyApp.Tests.csproj":                       libraryProject,
	})
	b, ok = readDotnetBuild(slnx)
	require.True(t, ok)
	require.Equal(t, "MyApp/MyApp.csproj", b.Project)

	// Two levels down, where src/ layouts keep it.
	src := withFiles(t, map[string]string{"src/Shop.Web/Shop.Web.csproj": webProject, "src/Shop.Core/Shop.Core.csproj": libraryProject})
	b, ok = readDotnetBuild(src)
	require.True(t, ok)
	require.Equal(t, "src/Shop.Web/Shop.Web.csproj", b.Project)
}

// Two web projects are a choice that is not Pando's, no web project is not an
// app Pando knows how to run, and a path that could not be written plainly
// into a Dockerfile is not written into one. Each is left to Railpack, and
// detection asks.
func TestADotnetSolutionIsNotGuessedAt(t *testing.T) {
	_, ok := readDotnetBuild(withFiles(t, map[string]string{"Admin/Admin.csproj": webProject, "Shop/Shop.csproj": webProject}))
	require.False(t, ok, "two web apps")

	_, ok = readDotnetBuild(withFiles(t, map[string]string{"Lib/Lib.csproj": libraryProject, "Lib.Tests/Lib.Tests.csproj": libraryProject}))
	require.False(t, ok, "a solution of libraries")

	_, ok = readDotnetBuild(withFiles(t, map[string]string{"My App/My App.csproj": webProject}))
	require.False(t, ok, "a path with a space")

	_, ok = readDotnetBuild(withFiles(t, map[string]string{"A.csproj": webProject, "B.csproj": webProject}))
	require.False(t, ok, "several projects at the top")
}

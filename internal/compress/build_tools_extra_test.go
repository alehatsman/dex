package compress

import (
	"strings"
	"testing"
)

func TestCompressDocker_DropsArrowAndRemoveLines(t *testing.T) {
	lines := []string{
		"Step 1/3 : FROM ubuntu:22.04",
		" ---> abc123def456",
		"Removing intermediate container a1b2c3d4",
		"Successfully built deadbeef",
	}
	out := CompressDocker(lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, " --->") {
		t.Error("layer hash lines should be dropped")
	}
	if strings.Contains(joined, "Removing intermediate") {
		t.Error("Removing intermediate lines should be dropped")
	}
	if !strings.Contains(joined, "Successfully built") {
		t.Error("success line should be retained")
	}
}

func TestCompressDocker_AllNoise_ReturnsOriginal(t *testing.T) {
	lines := []string{
		"Removing intermediate container aaa",
		"Removing intermediate container bbb",
	}
	out := CompressDocker(lines)
	if len(out) != len(lines) {
		t.Errorf("all-noise: expected original %d lines, got %d", len(lines), len(out))
	}
}

func TestCompressMake_DropsDirectoryChatter(t *testing.T) {
	lines := []string{
		"make[1]: Entering directory '/src'",
		"cc -O2 -o foo foo.c",
		"make[1]: Leaving directory '/src'",
		"make[1]: Nothing to be done for 'all'.",
	}
	out := CompressMake(lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "Entering directory") {
		t.Error("directory chatter should be dropped")
	}
	if strings.Contains(joined, "Nothing to be done") {
		t.Error("no-op messages should be dropped")
	}
	if !strings.Contains(joined, "cc -O2") {
		t.Error("compiler line should be retained")
	}
}

func TestCompressMake_AllNoise_ReturnsOriginal(t *testing.T) {
	lines := []string{
		"make: Entering directory '/src'",
		"make: Leaving directory '/src'",
	}
	out := CompressMake(lines)
	if len(out) != len(lines) {
		t.Errorf("all-noise: expected original %d lines, got %d", len(lines), len(out))
	}
}

func TestCompressCmake_DropsProgressLines(t *testing.T) {
	lines := []string{
		"[  5%] Building CXX object CMakeFiles/foo.dir/foo.cpp.o",
		"[ 50%] Linking CXX executable foo",
		"error: some build failure",
		"[100%] Built target foo",
		"-------------------------------",
	}
	out := CompressCmake(lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "[  5%]") || strings.Contains(joined, "[100%]") {
		t.Error("progress lines should be dropped")
	}
	if strings.Contains(joined, "------") {
		t.Error("separator lines should be dropped")
	}
	if !strings.Contains(joined, "error: some build failure") {
		t.Error("error line should be retained")
	}
}

func TestIsMavenNoise(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"Downloading from central: https://repo1.maven.org/...", true},
		{"Downloaded from central: https://repo1.maven.org/...", true},
		{"Progress (1): 12/48 KB", true},
		{"[INFO] BUILD SUCCESS", false},
	}
	for _, c := range cases {
		got := IsMavenNoise(c.line)
		if got != c.want {
			t.Errorf("IsMavenNoise(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestIsGradleNoise(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"Download https://jcenter.bintray.com/...", true},
		{"> 45%", true},
		{"BUILD SUCCESSFUL in 3s", false},
	}
	for _, c := range cases {
		got := IsGradleNoise(c.line)
		if got != c.want {
			t.Errorf("IsGradleNoise(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestCompressMaven_FiltersMavenNoise(t *testing.T) {
	lines := []string{
		"Downloading from central: https://repo1.maven.org/artifact",
		"[INFO] BUILD SUCCESS",
	}
	out := CompressMaven("mvn clean install", lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "Downloading") {
		t.Error("Maven download noise should be filtered")
	}
	if !strings.Contains(joined, "BUILD SUCCESS") {
		t.Error("BUILD SUCCESS should be retained")
	}
}

func TestCompressMaven_FilterGradleNoise(t *testing.T) {
	lines := []string{
		"Download https://jcenter.bintray.com/...",
		"> 75%",
		"BUILD SUCCESSFUL in 3s",
	}
	out := CompressMaven("gradle build", lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "Download https") {
		t.Error("Gradle download noise should be filtered")
	}
	if !strings.Contains(joined, "BUILD SUCCESSFUL") {
		t.Error("BUILD SUCCESSFUL should be retained")
	}
}

func TestCompressTerraform_DropsHeartbeat(t *testing.T) {
	lines := []string{
		"aws_instance.web: Still creating... [30s elapsed]",
		"  30.0s elapsed",
		"aws_instance.web: Creation complete after 45s",
	}
	out := CompressTerraform(lines)
	joined := strings.Join(out, "\n")
	if strings.Contains(joined, "Still creating") {
		t.Error("heartbeat lines should be dropped")
	}
	if strings.Contains(joined, "30.0s elapsed") {
		t.Error("elapsed lines should be dropped")
	}
	if !strings.Contains(joined, "Creation complete") {
		t.Error("completion line should be retained")
	}
}

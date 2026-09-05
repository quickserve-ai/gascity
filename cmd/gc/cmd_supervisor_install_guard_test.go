package main

import (
	"bytes"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestInstallSupervisorSystemdBinaryMismatchGuard(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}

	for _, tc := range []struct {
		name           string
		existingBinary string
		force          bool
		wantCode       int
	}{
		{
			name:           "refuses different existing binary without force",
			existingBinary: "/opt/gascity/bin/gc",
			wantCode:       1,
		},
		{
			name:           "allows matching existing binary",
			existingBinary: "CURRENT",
			wantCode:       0,
		},
		{
			name:           "allows different existing binary with force",
			existingBinary: "/opt/gascity/bin/gc",
			force:          true,
			wantCode:       0,
		},
		{
			name:     "allows fresh install",
			wantCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			gcHome := filepath.Join(homeDir, ".gc")
			currentBinary := filepath.Join(homeDir, "bin", "gc")
			t.Setenv("HOME", homeDir)
			t.Setenv("GC_HOME", gcHome)
			setSupervisorInstallForceForTest(t, tc.force)

			data := supervisorInstallGuardServiceData(gcHome, currentBinary)
			unitPath := supervisorSystemdServicePath()
			var original []byte
			if tc.existingBinary != "" {
				existingBinary := tc.existingBinary
				if existingBinary == "CURRENT" {
					existingBinary = currentBinary
				}
				original = []byte("[Unit]\nDescription=test\n\n[Service]\nExecStart=" + existingBinary + " supervisor run\n")
				if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(unitPath, original, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			oldRun := supervisorSystemctlRun
			oldActive := supervisorSystemctlActive
			var calls []string
			supervisorSystemctlRun = func(args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			}
			supervisorSystemctlActive = func(_ string) bool {
				return false
			}
			t.Cleanup(func() {
				supervisorSystemctlRun = oldRun
				supervisorSystemctlActive = oldActive
			})

			var stdout, stderr bytes.Buffer
			if code := installSupervisorSystemd(data, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("installSupervisorSystemd code = %d, want %d; stderr=%q", code, tc.wantCode, stderr.String())
			}

			if tc.wantCode == 1 {
				if len(calls) != 0 {
					t.Fatalf("systemctl calls = %v, want none when existing unit is refused", calls)
				}
				got, err := os.ReadFile(unitPath)
				if err != nil {
					t.Fatalf("ReadFile(%q): %v", unitPath, err)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("refused install rewrote unit:\n got: %q\nwant: %q", got, original)
				}
				for _, want := range []string{"existing unit", "/opt/gascity/bin/gc", currentBinary, "--force"} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("stderr = %q, want %q", stderr.String(), want)
					}
				}
				return
			}

			joined := strings.Join(calls, "\n")
			if !strings.Contains(joined, "--user start "+supervisorSystemdServiceName()) {
				t.Fatalf("systemctl calls = %v, want service start", calls)
			}
			got, err := os.ReadFile(unitPath)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", unitPath, err)
			}
			if !strings.Contains(string(got), "ExecStart="+currentBinary+" supervisor run") {
				t.Fatalf("installed unit = %q, want current gc binary %q", got, currentBinary)
			}
		})
	}
}

func TestInstallSupervisorLaunchdBinaryMismatchGuard(t *testing.T) {
	skipUnlessDarwinLaunchd(t)

	stubSupervisorLaunchdUnloaded(t)
	for _, tc := range []struct {
		name           string
		existingBinary string
		force          bool
		wantCode       int
	}{
		{
			name:           "refuses different existing binary without force",
			existingBinary: "/opt/gascity/bin/gc",
			wantCode:       1,
		},
		{
			name:           "allows matching existing binary",
			existingBinary: "CURRENT",
			wantCode:       0,
		},
		{
			name:           "allows different existing binary with force",
			existingBinary: "/opt/gascity/bin/gc",
			force:          true,
			wantCode:       0,
		},
		{
			name:     "allows fresh install",
			wantCode: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			homeDir := t.TempDir()
			gcHome := filepath.Join(homeDir, ".gc")
			currentBinary := filepath.Join(homeDir, "bin", "gc")
			if err := os.MkdirAll(filepath.Dir(currentBinary), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(currentBinary, []byte("#!/bin/sh\nprintf 'gc version test\\n'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", homeDir)
			t.Setenv("GC_HOME", gcHome)
			setSupervisorInstallForceForTest(t, tc.force)

			data := supervisorInstallGuardServiceData(gcHome, currentBinary)
			plistPath := supervisorLaunchdPlistPath()
			var original []byte
			if tc.existingBinary != "" {
				existingBinary := tc.existingBinary
				if existingBinary == "CURRENT" {
					existingBinary = currentBinary
				}
				original = supervisorInstallGuardLaunchdPlist(existingBinary)
				if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(plistPath, original, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			oldRun := supervisorLaunchctlRun
			var calls []string
			supervisorLaunchctlRun = func(args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return nil
			}
			t.Cleanup(func() {
				supervisorLaunchctlRun = oldRun
			})

			var stdout, stderr bytes.Buffer
			if code := installSupervisorLaunchd(data, &stdout, &stderr); code != tc.wantCode {
				t.Fatalf("installSupervisorLaunchd code = %d, want %d; stderr=%q", code, tc.wantCode, stderr.String())
			}

			if tc.wantCode == 1 {
				if len(calls) != 0 {
					t.Fatalf("launchctl calls = %v, want none when existing plist is refused", calls)
				}
				got, err := os.ReadFile(plistPath)
				if err != nil {
					t.Fatalf("ReadFile(%q): %v", plistPath, err)
				}
				if !bytes.Equal(got, original) {
					t.Fatalf("refused install rewrote plist:\n got: %q\nwant: %q", got, original)
				}
				for _, want := range []string{"existing plist", "/opt/gascity/bin/gc", currentBinary, "--force"} {
					if !strings.Contains(stderr.String(), want) {
						t.Fatalf("stderr = %q, want %q", stderr.String(), want)
					}
				}
				return
			}

			joined := strings.Join(calls, "\n")
			if !strings.Contains(joined, "bootstrap "+supervisorLaunchdDomain()+" "+plistPath) {
				t.Fatalf("launchctl calls = %v, want plist bootstrap", calls)
			}
			got, err := os.ReadFile(plistPath)
			if err != nil {
				t.Fatalf("ReadFile(%q): %v", plistPath, err)
			}
			if !strings.Contains(string(got), "<string>"+currentBinary+"</string>") {
				t.Fatalf("installed plist = %q, want current gc binary %q", got, currentBinary)
			}
		})
	}
}

func TestSupervisorSystemdExecStartBinaryParsesQuotedAndUnquotedPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		unit string
		want string
	}{
		{
			name: "unquoted",
			unit: "[Service]\nExecStart=/usr/local/bin/gc supervisor run\n",
			want: "/usr/local/bin/gc",
		},
		{
			name: "quoted",
			unit: "[Service]\nExecStart=\"/Applications/Gas City/gc\" supervisor run\n",
			want: "/Applications/Gas City/gc",
		},
		{
			name: "quoted escaped",
			unit: "[Service]\nExecStart=\"/opt/Gas \\\"City\\\"/gc\" supervisor run\n",
			want: "/opt/Gas \"City\"/gc",
		},
		{
			name: "missing",
			unit: "[Service]\nRestart=always\n",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorSystemdExecStartBinary(tc.unit); got != tc.want {
				t.Fatalf("supervisorSystemdExecStartBinary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSupervisorLaunchdPlistGCPathExtractsProgramArgument(t *testing.T) {
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.gascity.supervisor</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Applications/Gas &amp; City/gc</string>
        <string>supervisor</string>
        <string>run</string>
    </array>
</dict>
</plist>`
	if got := supervisorLaunchdPlistGCPath(plist); got != "/Applications/Gas & City/gc" {
		t.Fatalf("supervisorLaunchdPlistGCPath() = %q, want escaped path decoded", got)
	}
	if got := supervisorLaunchdPlistGCPath("<plist><dict></dict></plist>"); got != "" {
		t.Fatalf("supervisorLaunchdPlistGCPath(missing ProgramArguments) = %q, want empty", got)
	}
}

func TestLaunchdPrintReportsAbsent(t *testing.T) {
	for _, message := range []string{
		`Bad request.\nCould not find service "com.gastown.daemon" in domain for user gui: 501`,
		`Could not find service "com.gastown.daemon"`,
		`No such process`,
	} {
		if !launchdPrintReportsAbsent(message) {
			t.Fatalf("launchdPrintReportsAbsent(%q) = false, want true", message)
		}
	}
	if launchdPrintReportsAbsent("permission denied") {
		t.Fatal("permission error classified as absent service")
	}
}

func TestValidateSupervisorLaunchdPlist(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("launchd plist validation only applies on darwin")
	}
	const label = "com.gascity.supervisor.test"
	valid := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>` + label + `</string>
<key>ProgramArguments</key><array><string>GC_PATH</string><string>supervisor</string><string>run</string></array>
</dict></plist>`
	tests := []struct {
		name    string
		content string
		script  string
		wantErr string
	}{
		{name: "valid", content: valid, script: "#!/bin/sh\nprintf 'gc version test\\n'\n"},
		{name: "silent executable", content: valid, script: "#!/bin/sh\nexit 0\n", wantErr: "empty output"},
		{name: "malformed XML", content: `<plist><dict>`, script: "#!/bin/sh\nprintf 'gc version test\\n'\n", wantErr: "plutil validation failed"},
		{name: "missing label", content: strings.Replace(valid, label, "com.gascity.other", 1), script: "#!/bin/sh\nprintf 'gc version test\\n'\n", wantErr: "launchd label"},
		{name: "missing executable", content: strings.Replace(valid, "GC_PATH", "/missing/gc", 1), script: "#!/bin/sh\nprintf 'gc version test\\n'\n", wantErr: "gc executable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "supervisor.plist")
			gcPath := filepath.Join(t.TempDir(), "gc")
			if err := os.WriteFile(gcPath, []byte(tc.script), 0o700); err != nil {
				t.Fatal(err)
			}
			content := strings.Replace(tc.content, "GC_PATH", gcPath, 1)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			err := validateSupervisorLaunchdPlist(path, label, gcPath)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validateSupervisorLaunchdPlist() error = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validateSupervisorLaunchdPlist() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestSupervisorSameBinaryComparesCleanPathAndInode(t *testing.T) {
	missingA := filepath.Join(t.TempDir(), "bin", "..", "bin", "gc")
	missingB := filepath.Clean(missingA)
	if !supervisorSameBinary(missingA, missingB) {
		t.Fatalf("supervisorSameBinary() = false, want true for equivalent cleaned paths")
	}

	dir := t.TempDir()
	gcPath := filepath.Join(dir, "gc")
	if err := os.WriteFile(gcPath, []byte("gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "gc-hardlink")
	if err := os.Link(gcPath, linkPath); err != nil {
		t.Skipf("hardlink unavailable on this filesystem: %v", err)
	}
	if !supervisorSameBinary(gcPath, linkPath) {
		t.Fatalf("supervisorSameBinary() = false, want true for hardline binaries")
	}

	otherPath := filepath.Join(dir, "other-gc")
	if err := os.WriteFile(otherPath, []byte("other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if supervisorSameBinary(gcPath, otherPath) {
		t.Fatalf("supervisorSameBinary() = true, want false for different files")
	}
	if supervisorSameBinary(filepath.Join(dir, "missing-a"), filepath.Join(dir, "missing-b")) {
		t.Fatalf("supervisorSameBinary() = true, want false for different missing files")
	}
}

func TestSupervisorInstallCommandRegistersForceFlag(t *testing.T) {
	setSupervisorInstallForceForTest(t, false)

	var stdout, stderr bytes.Buffer
	cmd := newSupervisorInstallCmd(&stdout, &stderr)
	if cmd.Flags().Lookup("force") == nil {
		t.Fatal("supervisor install command missing --force flag")
	}
	if err := cmd.Flags().Set("force", "true"); err != nil {
		t.Fatalf("setting --force flag: %v", err)
	}
	if !supervisorInstallForce {
		t.Fatal("setting --force did not enable supervisorInstallForce")
	}
}

func setSupervisorInstallForceForTest(t *testing.T, force bool) {
	t.Helper()
	oldForce := supervisorInstallForce
	supervisorInstallForce = force
	t.Cleanup(func() {
		supervisorInstallForce = oldForce
	})
}

func supervisorInstallGuardServiceData(gcHome, gcPath string) *supervisorServiceData {
	return &supervisorServiceData{
		GCPath:        gcPath,
		LogPath:       filepath.Join(gcHome, "supervisor.log"),
		GCHome:        gcHome,
		XDGRuntimeDir: "",
		LaunchdLabel:  supervisorLaunchdLabel(),
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
}

func supervisorInstallGuardLaunchdPlist(gcPath string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>ProgramArguments</key>
    <array>
        <string>` + xmlEscape(gcPath) + `</string>
        <string>supervisor</string>
        <string>run</string>
    </array>
</dict>
</plist>
`)
}

package main

import (
	"os"
	"path/filepath"
	"testing"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: x
  context:
    cluster: c
    user: u
current-context: x
users:
- name: u
  user:
    token: t
`

func writeKubeconfig(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildClientset(t *testing.T) {
	tests := []struct {
		name    string
		flag    func(dir string) string
		env     func(dir string) string
		wantErr bool
	}{
		{
			name: "KUBECONFIG is used when no flag is given",
			flag: func(string) string { return "" },
			env:  func(dir string) string { return writeKubeconfig(t, dir) },
		},
		{
			name: "the flag is used when it is given",
			flag: func(dir string) string { return writeKubeconfig(t, dir) },
			env:  func(string) string { return "" },
		},
		{
			name:    "no flag, no KUBECONFIG and no home config is an error",
			flag:    func(string) string { return "" },
			env:     func(string) string { return "" },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", filepath.Join(dir, "home"))
			t.Setenv("KUBECONFIG", tt.env(dir))
			_, err := buildClientset(tt.flag(dir))
			if (err != nil) != tt.wantErr {
				t.Fatalf("buildClientset error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

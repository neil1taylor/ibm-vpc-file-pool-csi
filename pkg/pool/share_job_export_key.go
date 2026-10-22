package pool

import (
	"fmt"
	"strings"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// How an export Job gets the backup key (docs/specs/2026-10-21-backup-cos-export.md).
//
// The Job holds only references: the wrapped key and the IBM Cloud API key are
// Secrets mounted as files, and an optional emergency key is another. At run
// time the driver binary in mode unwrap-backup-key asks Key Protect to unwrap
// the key and writes it to a file in a memory-backed volume. The script reads
// it into the environment of Kopia and deletes the file. The controller never
// reads these Secrets and never calls Key Protect.
const (
	driverBinary = "/usr/local/bin/vpc-file-pool-csi"

	wrappedKeyVolume    = "backup-key-wrapped"
	apiKeyVolume        = "keyprotect-apikey"
	emergencyKeyVolume  = "backup-key-emergency"
	memoryKeyVolume     = "backup-key-memory"
	wrappedKeyDir       = "/var/run/pool-csi/backup-key-wrapped"
	apiKeyDir           = "/var/run/pool-csi/keyprotect-apikey" //nolint:gosec // a folder path, not a credential
	emergencyKeyDir     = "/var/run/pool-csi/backup-key-emergency"
	defaultMemoryKeyDir = "/run/backup-key"
)

func secretFileVolume(name, secret, key string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: secret,
		Items:      []corev1.KeyToPath{{Key: key, Path: key}},
	}}}
}

// keyVolumes are the Secret volumes and the memory volume of an export Job.
func keyVolumes(e ExportRepo) []corev1.Volume {
	if e.EmergencyOnly {
		return []corev1.Volume{secretFileVolume(emergencyKeyVolume, e.EmergencyKeySecret, ExportEmergencyKeyKey)}
	}
	vols := []corev1.Volume{
		secretFileVolume(wrappedKeyVolume, e.BackupKeySecret, ExportWrappedKeyKey),
		secretFileVolume(apiKeyVolume, e.KeyProtect.APIKeySecret, ExportAPIKeyKey),
	}
	if e.EmergencyKeySecret != "" {
		vols = append(vols, secretFileVolume(emergencyKeyVolume, e.EmergencyKeySecret, ExportEmergencyKeyKey))
	}
	limit := resource.MustParse("1Mi")
	return append(vols, corev1.Volume{Name: memoryKeyVolume, VolumeSource: corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory, SizeLimit: &limit},
	}})
}

// keyMounts mounts the volumes of keyVolumes into the main container.
func keyMounts(e ExportRepo) []corev1.VolumeMount {
	if e.EmergencyOnly {
		return []corev1.VolumeMount{{Name: emergencyKeyVolume, MountPath: emergencyKeyDir, ReadOnly: true}}
	}
	mounts := []corev1.VolumeMount{
		{Name: wrappedKeyVolume, MountPath: wrappedKeyDir, ReadOnly: true},
		{Name: apiKeyVolume, MountPath: apiKeyDir, ReadOnly: true},
	}
	if e.EmergencyKeySecret != "" {
		mounts = append(mounts, corev1.VolumeMount{Name: emergencyKeyVolume, MountPath: emergencyKeyDir, ReadOnly: true})
	}
	return append(mounts, corev1.VolumeMount{Name: memoryKeyVolume, MountPath: defaultMemoryKeyDir})
}

func (p exportParams) memoryKeyDir() string { return orDefault(p.keyDir, defaultMemoryKeyDir) }

// unwrapCommand is the command that writes the readable key to the file named
// by --key-out (added by unwrapStep).
func (p exportParams) unwrapCommand() string {
	if p.unwrapCmd != "" {
		return p.unwrapCmd
	}
	e := p.repo
	words := []string{
		driverBinary, "--mode=unwrap-backup-key",
		"--kp-region=" + util.ShellQuote(e.KeyProtect.Region),
		"--kp-instance=" + util.ShellQuote(e.KeyProtect.InstanceID),
		"--kp-root-key=" + util.ShellQuote(e.KeyProtect.RootKeyID),
		"--wrapped-key-file=" + util.ShellQuote(wrappedKeyDir+"/"+ExportWrappedKeyKey),
		"--api-key-file=" + util.ShellQuote(apiKeyDir+"/"+ExportAPIKeyKey),
	}
	if e.EmergencyKeySecret != "" {
		words = append(words, "--emergency-key-file="+util.ShellQuote(emergencyKeyDir+"/"+ExportEmergencyKeyKey))
	}
	return strings.Join(words, " ")
}

// unwrapStep makes Kopia's password. With set -e a failed unwrap ends the
// script before Kopia runs. The key is only in the environment of the shell,
// where Kopia reads it; the file is removed at once.
func (p exportParams) unwrapStep(b *strings.Builder) {
	if !p.useKeyProtect {
		return
	}
	if p.repo.EmergencyOnly {
		// Key Protect is not asked: the readable key is a mounted Secret file.
		fmt.Fprintf(b, "KOPIA_PASSWORD=$(cat %s)\nexport KOPIA_PASSWORD\n", util.ShellQuote(emergencyKeyDir+"/"+ExportEmergencyKeyKey))
		return
	}
	fmt.Fprintf(b, "keyfile=%s\n", util.ShellQuote(p.memoryKeyDir()+"/key"))
	fmt.Fprintf(b, "%s --key-out=\"$keyfile\"\n", p.unwrapCommand())
	b.WriteString("KOPIA_PASSWORD=$(cat \"$keyfile\")\nexport KOPIA_PASSWORD\nrm -f \"$keyfile\"\n")
}

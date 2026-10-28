package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/consoleplugin"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/failover"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	_ "github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics" // Register Prometheus collectors via init()
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/monitoring"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/nodemetrics"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/nodestats"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/poolbench"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmsnap"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/webhook"
)

var version = "dev"

// driverNamespace is where the Helm chart installs the driver. Leader
// election and share data Jobs use it.
const driverNamespace = pool.DefaultDriverNamespace

func main() {
	var (
		endpoint                string
		nodeID                  string
		mode                    string
		region                  string
		vpcID                   string
		subnetID                string
		kubeletDir              string
		cloneWorkerInterval     time.Duration
		goldenImageSyncInterval time.Duration
		cdiCopyPodMemoryLimit   string
		webhooks                webhookOptions

		// Node mode metrics flags.
		nodeMetricsAddr          string
		nodeMetricsAccount       string
		nodeMetricsCertDir       string
		controllerMetricsCertDir string
		mon                      monitoringOptions
		shareStatsInterval       time.Duration

		// Receiver mode flags.
		receiverBasePath      string
		receiverAuthTokenFile string
		receiverListenAddr    string
		receiverTLSCertFile   string
		receiverTLSKeyFile    string

		// Replication controller flags.
		driverImage string

		// VM disk snapshot flags.
		vmSnapshots vmSnapshotOptions
		browse      browseOptions
		gw          gatewayOptions

		// Sync-client mode flags.
		receiverEndpoint string
		authTokenFile    string
		caCertFile       string
		sourcePath       string
		destBasePath     string
		subPath          string
		subvolumeName    string
		metadataJSON     string

		// Apply-vm-disk mode flags.
		stagingPath string
		diskPath    string
		vmCopy      vmCopyFlags

		// PoolBench mode flags.
		poolBench poolbench.Options

		// Send-vm-disk mode flags (it also uses the sync-client flags).
		vmGroup string
		copyID  string

		// Failover mode flags (the controller also uses failoverSource).
		nfsMountPath   string
		failoverAction string
		failoverRewind string
		failoverVMs    string
		failoverForce  bool
		failoverExpect string
		failoverSource string

		// Restore-vm-disk mode flags.
		backupDisk    string
		outDisk       string
		restoreMoment string
		restoreFiles  struct{ spec, paths string }
		unwrap        unwrapOptions

		// Console plugin modes.
		consolePluginName string
	)

	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	flag.StringVar(&nodeID, "node-id", "", "Node ID")
	flag.StringVar(&mode, "mode", "controller", "Driver mode: controller, node, receiver, sync-client, check-receiver, apply-vm-disk, send-vm-disk, restore-vm-disk, restore-files, unwrap-backup-key, register-console-plugin, unregister-console-plugin, enable-sidecar-gate, or failover")
	flag.StringVar(&consolePluginName, "console-plugin-name", "ibm-vpc-file-pool-csi", "ConsolePlugin to turn on or off in the OpenShift console (register-console-plugin and unregister-console-plugin modes)")
	flag.StringVar(&region, "region", "", "IBM Cloud region (e.g. us-south); auto-discovered from secret provider if omitted")
	flag.StringVar(&vpcID, "vpc-id", "", "IBM Cloud VPC ID for creating file share mount targets")
	flag.StringVar(&subnetID, "subnet-id", "", "Comma-separated IBM Cloud worker subnet IDs; each share's mount target goes in the one in the pool's zone")
	flag.StringVar(&kubeletDir, "kubelet-dir", "/var/lib/kubelet", "Kubelet root directory (ROKS uses /var/data/kubelet)")
	flag.DurationVar(&cloneWorkerInterval, "clone-worker-interval", pool.DefaultCloneWorkerInterval, "Interval between clone worker poll cycles")
	flag.DurationVar(&goldenImageSyncInterval, "golden-image-sync-interval", pool.DefaultGoldenImageInterval, "Interval between golden image syncer poll cycles")
	flag.StringVar(&cdiCopyPodMemoryLimit, "cdi-copy-pod-memory-limit", "8Gi", "Keep the memory limit of CDI's copy pods (OpenShift Virtualization) at least this high; empty turns the check off")

	// Browse sessions (single-file restore, issue #92).
	flag.StringVar(&browse.image, "browse-image", "", "Image of the browse helper; empty turns browse sessions off (controller mode)")
	flag.StringVar(&browse.namespace, "browse-namespace", "ibm-vpc-file-pool-csi-browse", "Namespace of the browse sessions: their helper pods and Secrets. It holds nothing else (controller mode)")
	flag.StringVar(&browse.pullSecrets, "browse-image-pull-secrets", "", "Comma-separated names of the Secrets, in the driver namespace, that pull the browse image from a private registry")
	flag.DurationVar(&browse.cfg.Idle, "browse-idle", 15*time.Minute, "End a browse session after this long without use")
	flag.DurationVar(&browse.cfg.MaxLife, "browse-max-life", 2*time.Hour, "End a browse session after this long, even if in use")
	flag.IntVar(&browse.cfg.MaxSessions, "browse-max-sessions", 5, "Open browse sessions in the cluster; more wait")
	flag.IntVar(&browse.cfg.MaxPerNamespace, "browse-max-sessions-per-namespace", 2, "Open browse sessions for the claims of one namespace; more wait")
	flag.Int64Var(&browse.restoreMaxBytes, "file-restore-max-bytes", 200<<30, "The most one file restore writes, in bytes; a bigger one is refused (controller mode)")
	flag.BoolVar(&browse.cosEnabled, "browse-cos", true, "Let browse sessions and file restores read backups that were exported to COS (controller mode)")
	flag.StringVar(&browse.cosCPU, "browse-cos-cpu-limit", "", "CPU limit of the helper pod that reads a backup in COS; empty means 1 (controller mode)")
	flag.StringVar(&browse.cosMemory, "browse-cos-memory-limit", "", "Memory limit of the helper pod that reads a backup in COS; empty means 1Gi (controller mode)")
	flag.DurationVar(&browse.cfg.COSStartTimeout, "browse-cos-start-timeout", 5*time.Minute, "End a browse session on COS whose helper is not ready after this long (controller mode)")
	flag.StringVar(&browse.guestImage, "browse-guest-image", "", "Image of the helper that reads the file systems inside a VM disk (libguestfs); empty turns browsing inside VM disks off (controller mode)")
	flag.BoolVar(&browse.guestKVM, "browse-guest-kvm", false, "Ask the VM disk helper pod for the KVM device (devices.kubevirt.io/kvm); set it only where nodes have one (controller mode)")
	flag.StringVar(&browse.guestCPU, "browse-guest-cpu-limit", "", "CPU limit of the VM disk helper pod; empty means 1 (controller mode)")
	flag.StringVar(&browse.guestMemory, "browse-guest-memory-limit", "", "Memory limit of the VM disk helper pod; empty means 1Gi (controller mode)")
	flag.DurationVar(&browse.cfg.GuestStartTimeout, "browse-guest-start-timeout", 20*time.Minute, "End a browse session on a VM disk whose helper is not ready after this long (controller mode)")
	flag.IntVar(&browse.restoreMaxRunning, "file-restore-max-running", 5, "How many file restores run at once in the cluster; more wait (controller mode)")

	// Backup gateway flags (gateway mode).
	flag.StringVar(&gw.listen, "gateway-listen", ":8443", "Address the backup gateway listens on (backup-gateway mode)")
	flag.StringVar(&gw.certFile, "gateway-tls-cert-file", "", "Serving certificate of the backup gateway (PEM); read again when it changes")
	flag.StringVar(&gw.keyFile, "gateway-tls-key-file", "", "Serving key of the backup gateway (PEM)")
	flag.DurationVar(&gw.sessionWait, "gateway-session-wait", 20*time.Second, "How long a request that asks to wait holds for a browse session to be ready (keep it under the proxy timeout)")
	flag.Int64Var(&gw.maxDownload, "gateway-max-download-bytes", 50<<30, "Largest single file or tar a user may download")
	flag.IntVar(&gw.maxOpenRestores, "gateway-max-open-restores", 10, "File restores one user may have that have not ended; more are refused")
	flag.StringVar(&gw.browseNamespace, "gateway-browse-namespace", "ibm-vpc-file-pool-csi-browse", "Namespace of the browse sessions: their Secrets and helper pods (backup-gateway mode)")
	flag.DurationVar(&gw.authCache, "gateway-auth-cache", 10*time.Second, "How long a token review is kept; 0 checks every call")

	// Replication controller flags.
	flag.StringVar(&driverImage, "driver-image", "", "Driver container image for receiver-mode sync-client Jobs")
	flag.StringVar(&vmSnapshots.method, "vm-snapshot-method", "auto", "Snapshots of VM disks: auto (instant where the KubeVirt Sidecar feature gate is on) or copy (never instant)")
	flag.StringVar(&vmSnapshots.launcherImage, "virt-launcher-image", "", "KubeVirt virt-launcher image for VM disk snapshot Jobs; empty reads it from virt-operator")
	flag.StringVar(&nodeMetricsAddr, "node-metrics-addr", ":9810", "Listen address of the node agent's metrics endpoint (per-share used bytes and NFS latency and IOPS); empty turns it off (node mode)")
	flag.StringVar(&nodeMetricsCertDir, "node-metrics-cert-dir", "", "Directory with tls.crt and tls.key for the node metrics endpoint; empty makes a self-signed certificate (node mode)")
	flag.StringVar(&controllerMetricsCertDir, "metrics-cert-dir", "", "Directory with tls.crt and tls.key for the controller metrics endpoint (HTTPS on :8080, token login); empty makes a self-signed certificate (controller mode)")
	flag.StringVar(&mon.namespace, "monitoring-namespace", "", "Namespace of the metrics Services that user workload monitoring scrapes; the controller keeps their Endpoints and warns if user workload monitoring is off. Empty turns this off (controller mode)")
	flag.StringVar(&mon.controllerService, "monitoring-controller-service", "", "Name of the controller metrics Service in the monitoring namespace (controller mode)")
	flag.StringVar(&mon.nodeService, "monitoring-node-service", "", "Name of the node agent metrics Service in the monitoring namespace (controller mode)")
	flag.StringVar(&mon.selector, "monitoring-selector", "", "Labels shared by all driver pods, key=value,key=value (controller mode)")
	flag.IntVar(&mon.controllerPort, "monitoring-controller-port", 8080, "Port of the controller metrics endpoint (controller mode)")
	flag.StringVar(&mon.poolBenchService, "monitoring-poolbench-service", "", "Name of the PoolBench metrics Service in the monitoring namespace (controller mode)")
	flag.IntVar(&mon.poolBenchPort, "monitoring-poolbench-port", 0, "Port of the PoolBench metrics endpoint; 0 leaves PoolBench out (controller mode)")
	flag.IntVar(&mon.nodePort, "monitoring-node-port", 0, "Port of the node agent metrics endpoint; 0 leaves the node agents out (controller mode)")
	flag.DurationVar(&shareStatsInterval, "share-stats-interval", 30*time.Second, "How often the node agent reads the NFS statistics of its shares (node mode)")
	flag.BoolVar(&webhooks.enabled, "enable-webhooks", true, "Serve the validating admission webhooks (needs a serving certificate)")
	flag.IntVar(&webhooks.port, "webhook-port", 9443, "Port of the admission webhook server")

	// Receiver flags.
	flag.StringVar(&receiverBasePath, "receiver-base-path", "/data", "NFS mount point inside receiver container")
	flag.StringVar(&receiverAuthTokenFile, "receiver-auth-token-file", "/etc/replication/token", "Path to file containing bearer token for receiver auth")
	flag.StringVar(&receiverListenAddr, "receiver-listen-addr", ":8443", "HTTP listen address for receiver")
	flag.StringVar(&receiverTLSCertFile, "receiver-tls-cert-file", "", "TLS certificate file for receiver HTTPS (empty = plain HTTP)")
	flag.StringVar(&receiverTLSKeyFile, "receiver-tls-key-file", "", "TLS private key file for receiver HTTPS (empty = plain HTTP)")

	// Sync-client flags.
	flag.StringVar(&receiverEndpoint, "receiver-endpoint", "", "Destination receiver URL for sync-client mode")
	flag.StringVar(&authTokenFile, "auth-token-file", "/etc/replication/token", "Path to file containing bearer token for sync-client auth")
	flag.StringVar(&caCertFile, "ca-cert-file", "", "CA certificate file for verifying receiver TLS cert (empty = system trust store)")
	flag.StringVar(&sourcePath, "source-path", "", "Mounted source NFS path for sync-client mode")
	flag.StringVar(&destBasePath, "dest-base-path", "", "Destination base path for sync-client mode")
	flag.StringVar(&subPath, "sub-path", "", "SubVolume sub path for sync-client mode")
	flag.StringVar(&subvolumeName, "subvolume-name", "", "SubVolume CR name for sync-client mode")
	flag.StringVar(&metadataJSON, "metadata-json", "", "Serialized SubVolumeMetadata JSON for sync-client mode")

	// Apply-vm-disk flags.
	flag.StringVar(&stagingPath, "staging-path", "", "VM disk copy staging file for apply-vm-disk mode")
	flag.StringVar(&diskPath, "disk-path", "", "Disk image to replace in apply-vm-disk mode")
	flag.StringVar(&vmCopy.from, "from", "", "Moment the far-site disk shows now, RFC 3339 (apply-vm-disk mode; empty for a first copy)")
	flag.StringVar(&vmCopy.to, "to", "", "Moment the copy shows, RFC 3339 (apply-vm-disk mode)")
	flag.BoolVar(&vmCopy.full, "full", false, "The copy holds the whole disk (apply-vm-disk mode)")
	flag.DurationVar(&vmCopy.keep, "keep", 24*time.Hour, "How long restore points are kept (apply-vm-disk mode)")

	// Failover flags.
	flag.StringVar(&nfsMountPath, "nfs-mount-path", "", "Mount path of the destination share (failover mode)")
	flag.StringVar(&failoverAction, "failover-action", "", "Plan or Execute (failover mode)")
	flag.StringVar(&failoverRewind, "rewind-to", "", "Use the newest moment at or before this time, RFC 3339 (failover mode)")
	flag.StringVar(&failoverVMs, "vms", "", "Comma-separated VMs (namespace/name) to fail over; empty means all (failover mode)")
	flag.BoolVar(&failoverForce, "force", false, "Go on although a copy may still be applied to a disk (failover mode)")
	flag.StringVar(&failoverExpect, "failover-expect", "", "JSON of the confirmed plan; an Execute refuses if the share no longer matches it (failover mode)")
	flag.StringVar(&failoverSource, "failover-default-source", "", "Destination claim a Failover reads when it names none (controller mode)")

	// Restore-vm-disk flags.
	flag.StringVar(&backupDisk, "backup-disk", "", "Backed-up VM disk image to restore from (restore-vm-disk mode)")
	flag.StringVar(&outDisk, "out-disk", "", "New disk image to write; it must not exist (restore-vm-disk mode)")
	flag.StringVar(&restoreMoment, "restore-moment", "", "Moment to restore, RFC 3339; empty for the latest backup (restore-vm-disk mode)")

	// Restore-files flags.
	flag.StringVar(&restoreFiles.spec, "restore-files-spec", "", "JSON of the file restore: mode, where the backup and the target are mounted, owner (restore-files mode)")
	flag.StringVar(&restoreFiles.paths, "restore-files-paths", "", "File that lists the paths to restore, one per line in escaped form; paths are never arguments (restore-files mode)")

	// Unwrap-backup-key flags.
	flag.StringVar(&unwrap.region, "kp-region", "", "Key Protect region (unwrap-backup-key mode)")
	flag.StringVar(&unwrap.instance, "kp-instance", "", "Key Protect instance ID (unwrap-backup-key mode)")
	flag.StringVar(&unwrap.rootKey, "kp-root-key", "", "Key Protect root key ID (unwrap-backup-key mode)")
	flag.StringVar(&unwrap.wrappedKeyFile, "wrapped-key-file", "", "File with the backup key wrapped by Key Protect (unwrap-backup-key mode)")
	flag.StringVar(&unwrap.apiKeyFile, "api-key-file", "", "File with the IBM Cloud API key for Key Protect (unwrap-backup-key mode)")
	flag.StringVar(&unwrap.emergencyKeyFile, "emergency-key-file", "", "Optional file with the readable backup key, used only when Key Protect is down (unwrap-backup-key mode)")
	flag.StringVar(&unwrap.keyOut, "key-out", "", "File to write the readable key to, mode 0400; it must not exist (unwrap-backup-key mode)")

	flag.StringVar(&poolBench.CollectorAddr, "poolbench-collector-addr", ":8080", "Where the collector listens for the test VMs (poolbench mode)")
	flag.StringVar(&poolBench.CollectorURL, "poolbench-collector-url", "", "How the test VMs reach the collector, for example http://<service>.<namespace>.svc.cluster.local:8080 (poolbench mode)")
	flag.StringVar(&poolBench.MetricsAddr, "poolbench-metrics-addr", ":8443", "Metrics endpoint, HTTPS with token login (poolbench mode)")
	flag.StringVar(&poolBench.MetricsCertDir, "poolbench-metrics-cert-dir", "", "Directory with tls.crt and tls.key for the metrics endpoint; empty makes a self-signed certificate (poolbench mode)")
	flag.StringVar(&poolBench.ResultsAddr, "poolbench-results-addr", ":8444", "Where the raw results are served to the console's plugin proxy, over HTTPS with the metrics certificate (poolbench mode)")
	flag.StringVar(&poolBench.HealthAddr, "poolbench-health-addr", ":8081", "Health probe endpoint (poolbench mode)")
	flag.StringVar(&poolBench.ResultsDir, "poolbench-results-dir", "/results", "Folder for the raw results of the test VMs (poolbench mode)")
	flag.DurationVar(&poolBench.Interval, "poolbench-interval", 15*time.Second, "Time between two passes of the runner (poolbench mode)")
	flag.StringVar(&poolBench.RunRole, "poolbench-run-role", "", "ClusterRole the service binds to itself in each run's namespace (poolbench mode)")
	flag.StringVar(&poolBench.ServiceAccount, "poolbench-service-account", "", "The service's own service account, for that binding (poolbench mode)")
	flag.StringVar(&poolBench.NodeMetricsService, "node-metrics-service", "", "The node agents' metrics Service, read for the share counters (poolbench and controller modes)")
	flag.StringVar(&nodeMetricsAccount, "node-metrics-account", "", "Service account whose short-lived token the controller sends to the node agents; it may only read their metrics (controller mode)")
	flag.StringVar(&poolBench.NodeMetricsCA, "node-metrics-ca", "", "File with the CA of the node agents' metrics certificate (poolbench and controller modes)")

	flag.StringVar(&vmGroup, "vm-group", "", "namespace/name of the VM that owns the disk (send-vm-disk mode)")
	flag.StringVar(&copyID, "copy-id", "", "Name of this copy attempt; a repeat with the same name resumes (send-vm-disk mode)")

	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("Starting IBM VPC File Pool CSI Driver", "version", version, "mode", mode)

	switch mode {
	case "controller":
		if err := vmSnapshots.check(); err != nil {
			klog.ErrorS(err, "Invalid flag")
			os.Exit(1)
		}
		runController(endpoint, nodeID, region, vpcID, subnetID, kubeletDir, controllerMetricsCertDir, mon, cloneWorkerInterval, goldenImageSyncInterval, cdiCopyPodMemoryLimit, driverImage, failoverSource, webhooks, vmSnapshots, browse, nodeAgentOptions{service: poolBench.NodeMetricsService, caFile: poolBench.NodeMetricsCA, account: nodeMetricsAccount})
	case "backup-gateway":
		runGateway(gw)
	case "receiver":
		runReceiver(receiverBasePath, receiverAuthTokenFile, receiverListenAddr, receiverTLSCertFile, receiverTLSKeyFile)
	case "sync-client":
		runSyncClient(receiverEndpoint, authTokenFile, sourcePath, destBasePath, subPath, subvolumeName, metadataJSON, caCertFile)
	case "check-receiver":
		runCheckReceiver(receiverEndpoint, authTokenFile, caCertFile)
	case "apply-vm-disk":
		runApplyVMDisk(stagingPath, diskPath, vmCopy)
	case "failover":
		runFailover(nfsMountPath, failoverAction, failoverRewind, failoverVMs, failoverForce, failoverExpect)
	case "restore-vm-disk":
		runRestoreVMDisk(backupDisk, outDisk, restoreMoment)
	case "restore-files":
		runRestoreFiles(restoreFiles.spec, restoreFiles.paths)
	case "unwrap-backup-key":
		runUnwrapBackupKey(unwrap)
	case "send-vm-disk":
		runSendVMDisk(receiverEndpoint, authTokenFile, caCertFile, stagingPath, destBasePath, subPath, subvolumeName, copyID, vmGroup, metadataJSON, vmCopy)
	case "register-console-plugin", "unregister-console-plugin":
		runConsolePlugin(mode, consolePluginName)
	case "enable-sidecar-gate":
		runEnableSidecarGate()
	case "poolbench":
		ctrl.SetLogger(klog.NewKlogr())
		poolBench.Namespace = os.Getenv("POD_NAMESPACE")
		if poolBench.Namespace == "" {
			poolBench.Namespace = driverNamespace
		}
		if err := poolbench.Run(ctrl.SetupSignalHandler(), ctrl.GetConfigOrDie(), poolBench); err != nil {
			klog.ErrorS(err, "PoolBench service stopped")
			os.Exit(1)
		}
	default:
		runNode(endpoint, nodeID, mode, nodeMetricsAddr, nodeMetricsCertDir, shareStatsInterval)
	}
}

// runEnableSidecarGate turns on the KubeVirt Sidecar feature gate that instant
// VM snapshots need. The Helm chart runs it as a post-install and post-upgrade
// hook Job when vmSnapshots.enableSidecarGate is true.
func runEnableSidecarGate() {
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{})
	if err != nil {
		klog.ErrorS(err, "Failed to create Kubernetes client")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := pool.EnableSidecarGate(ctx, c); err != nil {
		klog.ErrorS(err, "Failed to turn on the KubeVirt Sidecar feature gate")
		os.Exit(1)
	}
	klog.InfoS("KubeVirt Sidecar feature gate is on")
}

// runConsolePlugin turns the console plugin on or off in the OpenShift console.
// The Helm chart runs it as post-install, post-upgrade and pre-delete hook Jobs.
func runConsolePlugin(mode, name string) {
	c, err := client.New(ctrl.GetConfigOrDie(), client.Options{})
	if err != nil {
		klog.ErrorS(err, "Failed to create Kubernetes client")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if mode == "register-console-plugin" {
		err = consoleplugin.Register(ctx, c, name)
	} else {
		err = consoleplugin.Unregister(ctx, c, name)
	}
	if err != nil {
		klog.ErrorS(err, "Failed to change the console plugins", "mode", mode, "plugin", name)
		os.Exit(1)
	}
	klog.InfoS("Console plugins updated", "mode", mode, "plugin", name)
}

// browseOptions are the flags for browse sessions.
type browseOptions struct {
	image       string
	pullSecrets string
	namespace   string
	cfg         pool.BrowseConfig
	// cosEnabled lets sessions and restores read backups exported to COS. Their
	// helpers run in the driver namespace, where the COS and Key Protect Secrets
	// are, with the limits cosCPU and cosMemory.
	cosEnabled  bool
	cosCPU      string
	guestImage  string
	guestKVM    bool
	guestCPU    string
	guestMemory string
	cosMemory   string
	// restoreMaxBytes is the most one file restore writes.
	restoreMaxBytes int64
	// restoreMaxRunning is how many file restores run at once.
	restoreMaxRunning int
}

// nodeAgentOptions say where to read the node agents' share counters, so that
// the copy of a running VM's disks can follow the load on its share.
type nodeAgentOptions struct {
	service string
	caFile  string
	// account is the service account whose short-lived token the controller
	// sends to the node agents. It may only read their metrics.
	account string
}

// webhookOptions configures the admission webhook server.
type webhookOptions struct {
	enabled bool
	port    int
}

// monitoringOptions configures the metrics Endpoints and the user workload
// monitoring check. An empty namespace turns both off.
type monitoringOptions struct {
	namespace         string
	controllerService string
	nodeService       string
	selector          string
	controllerPort    int
	nodePort          int
	// The PoolBench service's metrics, when it is on.
	poolBenchService string
	poolBenchPort    int
}

// config turns the flags into the syncer's configuration.
func (o monitoringOptions) config(driverNS string) (monitoring.Config, error) {
	sel := map[string]string{}
	for _, kv := range strings.Split(o.selector, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return monitoring.Config{}, fmt.Errorf("--monitoring-selector %q: want key=value,key=value", o.selector)
		}
		sel[k] = v
	}
	cfg := monitoring.Config{DriverNamespace: driverNS, MonitoringNamespace: o.namespace, Selector: sel}
	cfg.Targets = append(cfg.Targets, monitoring.Target{Service: o.controllerService, Component: "controller", Port: int32(o.controllerPort)}) //nolint:gosec // a port number
	if o.nodePort != 0 {
		cfg.Targets = append(cfg.Targets, monitoring.Target{Service: o.nodeService, Component: "node", Port: int32(o.nodePort)}) //nolint:gosec // a port number
	}
	if o.poolBenchPort != 0 {
		cfg.Targets = append(cfg.Targets, monitoring.Target{Service: o.poolBenchService, Component: "poolbench", Port: int32(o.poolBenchPort)}) //nolint:gosec // a port number
	}
	return cfg, nil
}

func runController(endpoint, nodeID, region, vpcID, subnetID, kubeletDir, metricsCertDir string, mon monitoringOptions, cloneWorkerInterval, goldenImageSyncInterval time.Duration, cdiCopyPodMemoryLimit, driverImage, failoverSource string, webhooks webhookOptions, vmSnapshots vmSnapshotOptions, browse browseOptions, nodeAgents nodeAgentOptions) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add CRD types to scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add corev1 types to scheme")
		os.Exit(1)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add storagev1 types to scheme")
		os.Exit(1)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add batchv1 types to scheme")
		os.Exit(1)
	}
	// The share latency reader lists the node agents' EndpointSlices.
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add discoveryv1 types to scheme")
		os.Exit(1)
	}

	// readyCh is closed once the VPC client is initialized. It gates:
	// - CSI controller methods (return Unavailable until ready)
	// - /readyz health check (returns 503 until ready)
	// - Probe() identity RPC (returns Ready=false until ready)
	readyCh := make(chan struct{})

	restConfig := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "ibm-vpc-file-pool-csi-leader",
		LeaderElectionNamespace: driverNamespace,
		HealthProbeBindAddress:  ":8081",
		Metrics:                 nodemetrics.SecureOptions(":8080", metricsCertDir),
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    webhooks.port,
			CertDir: "/tmp/k8s-webhook-server/serving-certs",
		}),
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create controller-runtime manager")
		os.Exit(1)
	}

	// Register health checks.
	if err = mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		klog.ErrorS(err, "Failed to register healthz check")
		os.Exit(1)
	}
	if err = mgr.AddReadyzCheck("vpc-client", func(_ *http.Request) error {
		select {
		case <-readyCh:
			return nil
		default:
			return fmt.Errorf("VPC client not yet initialized")
		}
	}); err != nil {
		klog.ErrorS(err, "Failed to register readyz check")
		os.Exit(1)
	}

	k8sClient := k8s.NewClient(mgr.GetClient())

	// Create kubernetes.Clientset for secret-common-lib.
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.ErrorS(err, "Failed to create kubernetes clientset")
		os.Exit(1)
	}

	ctx := context.Background()
	netCfg := discoverNetworkConfig(ctx, clientset, vpcID, subnetID)
	network := pool.NewShareNetwork(netCfg.vpcID, netCfg.subnetIDs, netCfg.clusterID)

	// Create reconciler and pool manager with nil VPC client — will be set
	// once the secret-provider sidecar is ready.
	reconciler := pool.NewFileSharePoolReconciler(k8sClient, nil)
	if err = reconciler.SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "Failed to set up reconciler")
		os.Exit(1)
	}
	rebind := &pool.ClaimRebindReconciler{Reader: mgr.GetClient(), K8sClient: k8sClient, DriverName: driver.DriverName}
	if err = rebind.SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "Failed to set up the claim rebind reconciler")
		os.Exit(1)
	}

	// Register validating webhooks for CRD resources. The manager starts the
	// webhook server only when a webhook is registered, so with webhooks
	// disabled the controller needs no serving certificate.
	if webhooks.enabled {
		if err = registerWebhooks(mgr, failoverSource); err != nil {
			klog.ErrorS(err, "Failed to register webhooks")
			os.Exit(1)
		}
		klog.V(2).InfoS("Registered validating webhooks for all CRD types", "port", webhooks.port)
	} else {
		klog.InfoS("Admission webhooks disabled")
	}

	stagingBasePath := filepath.Join(kubeletDir, "plugins/vpc-file-pool.csi.ibm.io/staging")

	poolManager := pool.NewManager(k8sClient, nil, nil, stagingBasePath)
	// The controller has no NFS access, so folder removal and copies on
	// shares run as Jobs in the driver's namespace. The runner reads without
	// the cache: a stale read could report a copy as stopped too early.
	shareJobClient, err := client.New(restConfig, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		klog.ErrorS(err, "Failed to create the share data Job client")
		os.Exit(1)
	}
	shareRunner := pool.NewShareJobRunner(shareJobClient, driverNamespace, pool.DefaultCloneImage)
	// Snapshots of running VM disks run the driver itself in a Job.
	shareRunner.SetDriverImage(driverImage)
	poolManager.SetShareData(shareRunner)

	// Run CSI gRPC server in a goroutine — serves immediately, but controller
	// methods return Unavailable until readyCh is closed.
	d, err := driver.NewDriver(driver.Config{
		Name:        driver.DriverName,
		Version:     version,
		NodeID:      nodeID,
		Endpoint:    endpoint,
		Mode:        "controller",
		K8sClient:   k8sClient,
		PoolManager: poolManager,
		Ready:       readyCh,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create driver")
		os.Exit(1)
	}

	go func() {
		if runErr := d.Run(); runErr != nil {
			klog.ErrorS(runErr, "CSI gRPC server failed")
			os.Exit(1)
		}
	}()

	// Initialize VPC client in the background. The secret-provider sidecar may
	// take up to 150s to become ready. This goroutine retries until it succeeds,
	// then injects the client into the reconciler and pool manager and signals
	// readiness by closing readyCh.
	go func() {
		var vpcClient *ibmcloud.Client
		var initErr error
		for attempt := 1; attempt <= 30; attempt++ {
			func() {
				defer func() {
					if r := recover(); r != nil {
						initErr = fmt.Errorf("secret provider panic: %v", r)
					}
				}()
				vpcClient, initErr = ibmcloud.NewClient(clientset, region)
			}()
			if initErr == nil {
				break
			}
			klog.V(2).InfoS("Waiting for secret provider sidecar", "attempt", attempt, "err", initErr)
			time.Sleep(5 * time.Second)
		}
		if initErr != nil {
			klog.ErrorS(initErr, "Failed to create VPC API client after retries — controller will not become ready", "region", region)
			return
		}

		defaultResourceGroup := vpcClient.ResourceGroupID()

		poolManager.SetVPCClient(vpcClient)
		poolManager.SetDefaultResourceGroup(defaultResourceGroup)
		poolManager.SetShareNetwork(network)
		reconciler.SetVPCClient(vpcClient)
		reconciler.SetVPCConfig(network, defaultResourceGroup)

		close(readyCh)
		klog.InfoS("VPC client initialized, controller is ready")
	}()

	// Start the background clone worker for async volume clones.
	// It creates Kubernetes Jobs to perform cp -a (controller pod has no NFS access).
	// The signal context ensures graceful shutdown alongside the manager.
	signalCtx := ctrl.SetupSignalHandler()
	nfsOps := pool.NewRealNFSOperations()
	cloneWorker := pool.NewCloneWorker(k8sClient, shareJobClient, pool.DefaultCloneImage)
	if cloneWorkerInterval > 0 {
		cloneWorker.SetInterval(cloneWorkerInterval)
	}
	go cloneWorker.Run(signalCtx)

	// Start the background golden image syncer for KubeVirt VM templates.
	goldenImageSyncer := pool.NewGoldenImageSyncer(k8sClient, mgr.GetClient())
	if goldenImageSyncInterval > 0 {
		goldenImageSyncer.SetInterval(goldenImageSyncInterval)
	}
	reconciler.SetGoldenImageRemover(goldenImageSyncer)

	// Keep CDI's copy pods big enough to write onto NFS. Runs on the leader only.
	if cdiCopyPodMemoryLimit != "" {
		if addErr := mgr.Add(pool.NewCDILimitKeeper(shareJobClient, cdiCopyPodMemoryLimit)); addErr != nil {
			klog.ErrorS(addErr, "Failed to add the CDI copy limit check")
			os.Exit(1)
		}
	}
	go goldenImageSyncer.Run(signalCtx)

	// Start the background replication controller for cross-region DR. It
	// reads without the cache: the cache would watch every pod in the cluster,
	// and the controller may only get and list pods.
	replClient, err := client.New(restConfig, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		klog.ErrorS(err, "Failed to create the replication client")
		os.Exit(1)
	}
	replController := pool.NewReplicationController(k8sClient, replClient, nfsOps)
	// The backup controller reports what replication did, so replication wakes
	// it after each pass. See the Job watch below.
	backupController := pool.NewBackupController(replClient)
	backupController.SetShareData(shareRunner)
	backupController.SetFileRestore(shareRunner, pool.FileRestoreConfig{MaxBytes: browse.restoreMaxBytes, MaxRunning: browse.restoreMaxRunning})
	if browse.image != "" {
		// Helper pods and session Secrets live in their own namespace, which holds
		// nothing else, so the gateway's right to read Secrets reaches nothing
		// else. The pods mount NFS directly, so they can run there.
		browseRunner := pool.NewShareJobRunner(shareJobClient, browse.namespace, pool.DefaultCloneImage)
		browseRunner.SetBrowseImage(browse.image)
		browse.cfg.Namespace = browse.namespace
		if browse.guestImage != "" {
			browseRunner.SetBrowseGuest(browse.guestImage, pool.BrowseGuestConfig{KVM: browse.guestKVM, CPU: browse.guestCPU, Memory: browse.guestMemory})
		}
		if browse.pullSecrets != "" {
			names := strings.Split(browse.pullSecrets, ",")
			browseRunner.SetBrowsePullSecrets(names)
			browse.cfg.PullSecrets, browse.cfg.PullSecretNamespace = names, driverNamespace
		}
		var browseJobs pool.BrowseJobs = browseRunner
		if browse.cosEnabled {
			// The COS key and the Key Protect key are Secrets of the driver
			// namespace, and a pod mounts only Secrets of its own namespace. So these
			// helpers run there, and the keys are not copied to where the gateway can
			// read Secrets.
			cosRunner := pool.NewShareJobRunner(shareJobClient, driverNamespace, pool.DefaultCloneImage)
			cosRunner.SetBrowseImage(browse.image)
			if browse.pullSecrets != "" {
				cosRunner.SetBrowsePullSecrets(strings.Split(browse.pullSecrets, ","))
			}
			if err := cosRunner.SetBrowseCOSResources(browse.cosCPU, browse.cosMemory); err != nil {
				klog.ErrorS(err, "Bad limits for the helper that reads COS")
				os.Exit(1)
			}
			browseJobs = &pool.BrowseRouter{Folder: browseRunner, COS: cosRunner}
			browse.cfg.COSNamespace = driverNamespace
		}
		backupController.SetBrowse(browseJobs, browse.cfg)
	}
	replController.SetOnProcessed(backupController.Kick)
	hookOrchestrator := hooks.NewOrchestrator(
		hooks.NewExecHook(clientset, restConfig),
		hooks.NewHTTPHook(nil),
	)
	replController.SetOrchestrator(hookOrchestrator)
	replController.SetDriverImage(driverImage)
	replController.SetReplicationImage(driverImage)
	// Copies of running VM disks run commands in the VM pods, as hooks do.
	vmCopier := vmcopy.NewCopier(hooks.NewPodExecer(clientset, restConfig))
	replController.SetVMCopier(vmCopier)
	// Without the node agents' metrics the copies run at a fixed speed.
	var shareLatency *pool.LatencyMeter
	if nodeAgents.service != "" && nodeAgents.account != "" {
		httpClient, herr := nodestats.NewHTTPClient(nodeAgents.caFile, nodeAgents.service+"."+driverNamespace+".svc")
		if herr != nil {
			klog.ErrorS(herr, "Failed to set up the node agents' metrics client; VM disk copies run at a fixed speed")
		} else {
			shareLatency = &pool.LatencyMeter{
				Source: &nodestats.Reader{Client: replClient, Namespace: driverNamespace, Service: nodeAgents.service,
					HTTP:  httpClient,
					Token: (&nodestats.ServiceAccountTokens{Clientset: clientset, Namespace: driverNamespace, Name: nodeAgents.account, Now: time.Now}).Token},
				MinOps: 50, Window: 3, MinGap: 10 * time.Second, Now: time.Now,
			}
			replController.SetShareLatency(shareLatency)
		}
	}
	go replController.Run(signalCtx)

	// Watch the Jobs in the driver's namespace, so a controller that waits for
	// a Job sees it finish within seconds and not at its next tick.
	jobInformers := informers.NewSharedInformerFactoryWithOptions(clientset, 0, informers.WithNamespace(driverNamespace))
	if _, addErr := jobInformers.Batch().V1().Jobs().Informer().AddEventHandler(
		pool.NewJobFinishHandler(replController.Kick, backupController.Kick)); addErr != nil {
		klog.ErrorS(addErr, "Failed to watch Jobs")
		os.Exit(1)
	}
	jobInformers.Start(signalCtx.Done())

	// Snapshots of running VM disks are QEMU copies. The same commands run in
	// the VM pod; a timer on the leader moves each copy forward.
	poolManager.SetVMSnapshots(vmCopier, replClient)
	if shareLatency != nil {
		poolManager.SetVMSnapshotLatency(shareLatency)
	}

	// Instant snapshots of VM disks change the disk's file chain in the VM
	// pod, or with a Job in the virt-launcher image when the VM is stopped.
	// See docs/VM-INSTANT-SNAPSHOTS.md.
	if vmSnapshots.method == "auto" {
		image := vmSnapshots.launcherImage
		if image == "" {
			image = discoverLauncherImage(signalCtx, clientset, replClient)
		}
		if image == "" {
			klog.InfoS("No KubeVirt virt-launcher image found; VM disk snapshots are copies")
		} else {
			shareRunner.SetLauncherImage(image)
			poolManager.SetInstantSnapshots(vmsnap.New(hooks.NewPodExecer(clientset, restConfig)), shareRunner, replClient)
			klog.V(2).InfoS("Instant VM disk snapshots are on where the KubeVirt Sidecar feature gate is on", "launcherImage", image)
		}
	}
	if addErr := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		poolManager.RunVMSnapshots(ctx, pool.DefaultVMSnapshotInterval)
		return nil
	})); addErr != nil {
		klog.ErrorS(addErr, "Failed to add the VM snapshot loop")
		os.Exit(1)
	}

	// Backup policies: label the volumes they select and keep one
	// ReplicationPolicy for each source pool. Runs on the leader only.
	if addErr := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		backupController.Run(ctx)
		return nil
	})); addErr != nil {
		klog.ErrorS(addErr, "Failed to add the backup policy controller")
		os.Exit(1)
	}

	// Failovers: a Job reads the destination claim and plans or carries out a
	// failover. Runs on the leader only.
	failoverController := failover.NewController(replClient, driverNamespace, driverImage, failoverSource)
	if addErr := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		failoverController.Run(ctx)
		return nil
	})); addErr != nil {
		klog.ErrorS(addErr, "Failed to add the failover controller")
		os.Exit(1)
	}

	// Metrics for OpenShift's user workload monitoring: keep the Endpoints of the
	// Services in the monitoring namespace, and say if nothing will evaluate alerts.
	if mon.namespace != "" {
		monCfg, cfgErr := mon.config(driverNamespace)
		if cfgErr != nil {
			klog.ErrorS(cfgErr, "Invalid monitoring flags")
			os.Exit(1)
		}
		if addErr := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
			monitoring.WarnIfAlertsNotEvaluated(ctx, clientset, mon.namespace)
			monitoring.NewSyncer(clientset, monCfg).Run(ctx, time.Minute)
			return nil
		})); addErr != nil {
			klog.ErrorS(addErr, "Failed to add the monitoring Endpoints sync")
			os.Exit(1)
		}
	}

	// mgr.Start blocks until signal or error
	klog.InfoS("Starting controller-runtime manager with leader election")
	if err := mgr.Start(signalCtx); err != nil {
		klog.ErrorS(err, "Controller manager failed")
		os.Exit(1)
	}
}

func runReceiver(basePath, authTokenFile, listenAddr, tlsCertFile, tlsKeyFile string) {
	// Read auth token from file.
	tokenBytes, err := os.ReadFile(authTokenFile) //nolint:gosec // G304: path comes from the operator-set --auth-token-file flag
	if err != nil {
		klog.ErrorS(err, "Failed to read receiver auth token file", "path", authTokenFile)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		klog.ErrorS(nil, "Receiver auth token file is empty", "path", authTokenFile)
		os.Exit(1)
	}

	if err := replication.ValidateToken(token); err != nil {
		klog.ErrorS(err, "Receiver auth token is not acceptable", "path", authTokenFile)
		os.Exit(1)
	}
	receiver := replication.NewReceiver(basePath, token, listenAddr)
	if tlsCertFile != "" && tlsKeyFile != "" {
		receiver.SetTLS(tlsCertFile, tlsKeyFile)
	}

	signalCtx := ctrl.SetupSignalHandler()
	klog.InfoS("Starting replication receiver", "basePath", basePath, "listenAddr", listenAddr, "tls", tlsCertFile != "")
	if err := receiver.Start(signalCtx); err != nil {
		klog.ErrorS(err, "Replication receiver failed")
		os.Exit(1)
	}
}

// newSyncClient reads the auth token and the optional CA certificate and makes
// the client. It exits if either file is not usable.
func newSyncClient(endpoint, authTokenFile, caCertFile string) *replication.SyncClient {
	// Read auth token from file.
	tokenBytes, err := os.ReadFile(authTokenFile) //nolint:gosec // G304: path comes from the operator-set --auth-token-file flag
	if err != nil {
		klog.ErrorS(err, "Failed to read auth token file", "path", authTokenFile)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		klog.ErrorS(nil, "Auth token file is empty", "path", authTokenFile)
		os.Exit(1)
	}
	if caCertFile == "" {
		return replication.NewSyncClient(endpoint, token)
	}
	caCertPEM, err := os.ReadFile(caCertFile) //nolint:gosec // G304: path comes from the operator-set CA cert flag
	if err != nil {
		klog.ErrorS(err, "Failed to read CA cert file", "path", caCertFile)
		os.Exit(1)
	}
	client, err := replication.NewSyncClientWithCA(endpoint, token, caCertPEM)
	if err != nil {
		klog.ErrorS(err, "Invalid CA cert file", "path", caCertFile)
		os.Exit(1)
	}
	klog.V(2).InfoS("Using custom CA certificate for receiver TLS", "caCertFile", caCertFile)
	return client
}

func runSyncClient(endpoint, authTokenFile, sourcePath, destBasePath, subPath, svName, metadataJSON, caCertFile string) {
	if endpoint == "" {
		klog.ErrorS(nil, "receiver-endpoint is required for sync-client mode")
		os.Exit(1)
	}
	if sourcePath == "" || destBasePath == "" || subPath == "" || svName == "" {
		klog.ErrorS(nil, "source-path, dest-base-path, sub-path, and subvolume-name are required for sync-client mode")
		os.Exit(1)
	}

	client := newSyncClient(endpoint, authTokenFile, caCertFile)

	var metadata []byte
	if metadataJSON != "" {
		metadata = []byte(metadataJSON)
	}

	ctx := context.Background()
	klog.InfoS("Starting sync-client",
		"endpoint", endpoint,
		"sourcePath", sourcePath,
		"destBasePath", destBasePath,
		"subPath", subPath,
		"subVolume", svName,
	)

	bytesWritten, err := client.SyncSubVolume(ctx, sourcePath, destBasePath, subPath, svName, metadata)
	if err != nil {
		klog.ErrorS(err, "Sync-client failed")
		os.Exit(1)
	}

	klog.InfoS("Sync-client completed successfully", "subVolume", svName, "bytesWritten", bytesWritten)

	// Write bytesWritten to termination-log so the replication controller can
	// read it from the completed pod's container status.
	_ = os.WriteFile("/dev/termination-log", []byte(fmt.Sprintf(`{"bytesWritten":%d}`, bytesWritten)), 0o600)
}

// checkReceiverResult is the one result line of a receiver check ("ok", or the short reason) and the exit code.
func checkReceiverResult(err error) (string, int) {
	if err == nil {
		return "ok", 0
	}
	var checkErr *replication.CheckError
	if errors.As(err, &checkErr) {
		return checkErr.Reason, 1
	}
	return err.Error(), 1
}

// runCheckReceiver tests a receiver the way a replication Job reaches it: the address, the
// certificate and the token. It writes one result line to the output and to the termination log
// (so the console can read it from the Pod status), and exits 1 when the check fails.
func runCheckReceiver(endpoint, authTokenFile, caCertFile string) {
	if endpoint == "" {
		klog.ErrorS(nil, "receiver-endpoint is required for check-receiver mode")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	line, code := checkReceiverResult(replication.CheckReceiver(ctx, endpoint, authTokenFile, caCertFile))
	fmt.Println(line)
	_ = os.WriteFile("/dev/termination-log", []byte(line), 0o600)
	if code != 0 {
		os.Exit(code)
	}
}

func runNode(endpoint, nodeID, mode, metricsAddr, metricsCertDir string, statsInterval time.Duration) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add CRD types to scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add corev1 types to scheme")
		os.Exit(1)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add storagev1 types to scheme")
		os.Exit(1)
	}

	restConfig := ctrl.GetConfigOrDie()
	ctrlClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		klog.ErrorS(err, "Failed to create controller-runtime client")
		os.Exit(1)
	}
	k8sClient := k8s.NewClient(ctrlClient)

	if metricsAddr != "" {
		startShareMetrics(metricsAddr, metricsCertDir, statsInterval, restConfig, k8sClient)
	}

	d, err := driver.NewDriver(driver.Config{
		Name:      driver.DriverName,
		Version:   version,
		NodeID:    nodeID,
		Endpoint:  endpoint,
		Mode:      mode,
		K8sClient: k8sClient,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create driver")
		os.Exit(1)
	}

	if err := d.Run(); err != nil {
		klog.ErrorS(err, "Driver failed")
		os.Exit(1)
	}
}

// startShareMetrics serves the per-share used bytes and NFS statistics of this
// node on addr over HTTPS, in the background. Callers need a token that the API
// server accepts and may "get" /metrics. If it cannot start it logs and carries
// on: missing metrics must not stop volumes from mounting.
func startShareMetrics(addr, certDir string, interval time.Duration, restConfig *rest.Config, k8sClient k8s.Client) {
	httpClient, err := rest.HTTPClientFor(restConfig)
	if err != nil {
		klog.ErrorS(err, "Cannot serve node metrics; continuing without them")
		return
	}
	srv, err := nodemetrics.NewSecureServer(addr, certDir, restConfig, httpClient)
	if err != nil {
		klog.ErrorS(err, "Cannot serve node metrics; continuing without them")
		return
	}
	c := nodemetrics.NewCollector(nodemetrics.Config{
		DriverName:     driver.DriverName,
		ReadMountStats: nodemetrics.ReadProcMountStats,
		Statfs:         nodemetrics.Statfs,
		Resolve:        nodemetrics.ResolverFromPools(k8sClient.ListFileSharePools),
	})
	if err := ctrlmetrics.Registry.Register(c); err != nil {
		klog.ErrorS(err, "Cannot register node metrics; continuing without them")
		return
	}
	ctx := context.Background()
	go c.Run(ctx, interval)
	go func() {
		if err := srv.Start(ctx); err != nil {
			klog.ErrorS(err, "Node metrics server stopped")
		}
	}()
	klog.InfoS("Serving node metrics over HTTPS", "addr", addr)
}

// networkConfig is the cluster network that share mount targets join.
type networkConfig struct {
	vpcID     string
	subnetIDs []string
	clusterID string
}

// discoverNetworkConfig fills in the VPC ID and worker subnets from the
// ibm-cloud-provider-data configmap unless the flags set them, and the cluster
// ID from the cluster-info configmap. Both configmaps exist on ROKS and ROVS.
// Uses the clientset (not the manager client) because the manager cache is not
// started yet.
func discoverNetworkConfig(ctx context.Context, clientset kubernetes.Interface, vpcID, subnetIDs string) networkConfig {
	if vpcID == "" || subnetIDs == "" {
		if cm, err := clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "ibm-cloud-provider-data", metav1.GetOptions{}); err == nil {
			if vpcID == "" {
				vpcID = cm.Data["vpc_id"]
			}
			if subnetIDs == "" {
				subnetIDs = cm.Data["vpc_subnet_ids"]
			}
		} else {
			klog.ErrorS(err, "Cannot read ibm-cloud-provider-data; set --vpc-id and --subnet-id")
		}
	}

	cfg := networkConfig{vpcID: vpcID}
	for _, id := range strings.Split(subnetIDs, ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.subnetIDs = append(cfg.subnetIDs, id)
		}
	}
	if cm, err := clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "cluster-info", metav1.GetOptions{}); err == nil {
		cfg.clusterID = cm.Data["cluster_id"]
	} else {
		klog.ErrorS(err, "Cannot read cluster-info; pools must set securityGroupIDs")
	}

	klog.V(2).InfoS("Share network", "vpcID", cfg.vpcID, "subnetIDs", cfg.subnetIDs, "clusterID", cfg.clusterID)
	return cfg
}

// registerWebhooks registers the validating webhook for each CRD type.
func registerWebhooks(mgr ctrl.Manager, failoverSource string) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.FileSharePool{}).
		WithValidator(&webhook.FileSharePoolValidator{}).Complete(); err != nil {
		return fmt.Errorf("register FileSharePool webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.SubVolume{}).
		WithValidator(&webhook.SubVolumeValidator{}).Complete(); err != nil {
		return fmt.Errorf("register SubVolume webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.ReplicationPolicy{}).
		WithValidator(&webhook.ReplicationPolicyValidator{}).Complete(); err != nil {
		return fmt.Errorf("register ReplicationPolicy webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.Failover{}).
		WithValidator(&webhook.FailoverValidator{Reader: mgr.GetAPIReader(), DefaultSource: failoverSource}).Complete(); err != nil {
		return fmt.Errorf("register Failover webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.BackupPolicy{}).
		WithValidator(&webhook.BackupPolicyValidator{}).Complete(); err != nil {
		return fmt.Errorf("register BackupPolicy webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.BackupRestore{}).
		WithValidator(&webhook.BackupRestoreValidator{}).Complete(); err != nil {
		return fmt.Errorf("register BackupRestore webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.BackupFileRestore{}).
		WithValidator(&webhook.BackupFileRestoreValidator{}).Complete(); err != nil {
		return fmt.Errorf("register BackupFileRestore webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.PoolBench{}).
		WithValidator(&webhook.PoolBenchValidator{}).Complete(); err != nil {
		return fmt.Errorf("register PoolBench webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.Snapshot{}).
		WithValidator(&webhook.SnapshotValidator{}).Complete(); err != nil {
		return fmt.Errorf("register Snapshot webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.VolumeGroupSnapshot{}).
		WithValidator(&webhook.VolumeGroupSnapshotValidator{}).Complete(); err != nil {
		return fmt.Errorf("register VolumeGroupSnapshot webhook: %w", err)
	}
	// The VM hook webhook reads without the cache: a VM must start with the
	// newest chain of its disks.
	mgr.GetWebhookServer().Register(webhook.VMIHookPath, &ctrlwebhook.Admission{Handler: &webhook.VMIHookInjector{Client: mgr.GetAPIReader()}})
	return nil
}

// vmCopyFlags are the apply-vm-disk flags that describe the copy.
type vmCopyFlags struct {
	from, to string
	full     bool
	keep     time.Duration
}

// applyOptions turns the flags into replication.ApplyOptions.
func (f vmCopyFlags) applyOptions(now time.Time) (replication.ApplyOptions, error) {
	opts := replication.ApplyOptions{Full: f.full, Keep: f.keep, Now: now}
	var err error
	if opts.To, err = time.Parse(time.RFC3339Nano, f.to); err != nil {
		return opts, fmt.Errorf("--to: %w", err)
	}
	if f.from != "" {
		if opts.From, err = time.Parse(time.RFC3339Nano, f.from); err != nil {
			return opts, fmt.Errorf("--from: %w", err)
		}
	}
	return opts, nil
}

// applyVMDiskMessage is the termination message of an apply-vm-disk Job.
func applyVMDiskMessage(res replication.VMDiskResult) string {
	msg := struct {
		BytesWritten        int64       `json:"bytesWritten"`
		RestorePoints       int         `json:"restorePoints"`
		OldestRestorePoint  *time.Time  `json:"oldestRestorePoint,omitempty"`
		RestorePointMoments []time.Time `json:"restorePointMoments,omitempty"`
	}{res.ChangedBytes, res.RestorePoints, res.OldestRestorePoint, res.RestorePointMoments}
	b, _ := json.Marshal(msg)
	return string(b)
}

// runApplyVMDisk applies a staged VM disk copy to the far-site disk. A
// replication Job runs it.
func runApplyVMDisk(stagingPath, diskPath string, f vmCopyFlags) {
	if stagingPath == "" || diskPath == "" {
		klog.ErrorS(nil, "staging-path and disk-path are required for apply-vm-disk mode")
		os.Exit(1)
	}
	opts, err := f.applyOptions(time.Now())
	if err != nil {
		klog.ErrorS(err, "Invalid apply-vm-disk flags")
		os.Exit(1)
	}
	res, err := replication.ApplyVMDisk(stagingPath, diskPath, opts)
	if err != nil {
		klog.ErrorS(err, "Failed to apply VM disk copy", "staging", stagingPath, "disk", diskPath)
		os.Exit(1)
	}
	klog.InfoS("Applied VM disk copy", "disk", diskPath, "changedBytes", res.ChangedBytes,
		"rolledBack", res.RolledBack, "restorePoints", res.RestorePoints)
	// The replication controller reads the result from the termination log.
	_ = os.WriteFile("/dev/termination-log", []byte(applyVMDiskMessage(res)), 0o600)
}

// sendVMDiskArgs turns the flags into the arguments of replication.SendVMDisk.
func sendVMDiskArgs(f vmCopyFlags, destBasePath, subPath, svName, copyID, vmGroup string) (replication.VMSendArgs, error) {
	opts, err := f.applyOptions(time.Now())
	if err != nil {
		return replication.VMSendArgs{}, err
	}
	a := replication.VMSendArgs{DestBasePath: destBasePath, SubPath: subPath, SVName: svName, CopyID: copyID,
		Full: f.full, To: opts.To, VMGroup: vmGroup, Keep: f.keep}
	if f.from != "" {
		a.From = &opts.From
	}
	return a, nil
}

// sendVMDiskMessage is the termination message of a send-vm-disk Job. A copy
// the receiver refused because its disk is at another moment is reported, not
// failed, so the Job is not retried: the controller makes the next copy full.
func sendVMDiskMessage(res replication.VMSendResult, wrongBase *replication.WrongBaseError) string {
	msg := struct {
		BytesWritten        int64       `json:"bytesWritten"`
		RestorePoints       int32       `json:"restorePoints"`
		OldestRestorePoint  *time.Time  `json:"oldestRestorePoint,omitempty"`
		RestorePointMoments []time.Time `json:"restorePointMoments,omitempty"`
		WrongBase           bool        `json:"wrongBase,omitempty"`
		DiskMoment          string      `json:"diskMoment,omitempty"`
	}{res.SentBytes, res.RestorePoints, res.OldestRestorePoint, res.RestorePointMoments, false, ""}
	if wrongBase != nil {
		msg.WrongBase, msg.DiskMoment = true, wrongBase.DiskMoment
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

// runSendVMDisk sends a staged VM disk copy to a receiver at the far site, and
// the volume's metadata file. A replication Job runs it.
func runSendVMDisk(endpoint, authTokenFile, caCertFile, stagingPath, destBasePath, subPath, svName, copyID, vmGroup, metadataJSON string, f vmCopyFlags) {
	if endpoint == "" || stagingPath == "" || destBasePath == "" || subPath == "" || svName == "" || copyID == "" {
		klog.ErrorS(nil, "receiver-endpoint, staging-path, dest-base-path, sub-path, subvolume-name and copy-id are required for send-vm-disk mode")
		os.Exit(1)
	}
	args, err := sendVMDiskArgs(f, destBasePath, subPath, svName, copyID, vmGroup)
	if err != nil {
		klog.ErrorS(err, "Invalid send-vm-disk flags")
		os.Exit(1)
	}
	client := newSyncClient(endpoint, authTokenFile, caCertFile)
	ctx := context.Background()
	// The metadata goes first, as in direct NFS mode. If the Job is retried
	// after the staging file was deleted, nothing is left to send.
	if metadataJSON != "" {
		if err = client.UploadVMMetadata(ctx, destBasePath, subPath, svName, []byte(metadataJSON)); err != nil {
			klog.ErrorS(err, "Failed to upload the volume metadata", "subVolume", svName)
			os.Exit(1)
		}
	}
	res, err := replication.SendVMDiskFile(ctx, client, stagingPath, args)
	var wrongBase *replication.WrongBaseError
	if errors.As(err, &wrongBase) {
		klog.ErrorS(err, "The far-site disk is not at the moment this copy starts from; the next copy will be a full copy", "subVolume", svName)
		_ = os.WriteFile("/dev/termination-log", []byte(sendVMDiskMessage(res, wrongBase)), 0o600)
		return
	}
	if err != nil {
		klog.ErrorS(err, "Failed to send VM disk copy", "staging", stagingPath, "subVolume", svName)
		os.Exit(1)
	}
	klog.InfoS("Sent VM disk copy", "subVolume", svName, "sentBytes", res.SentBytes, "changedBytes", res.ChangedBytes,
		"restorePoints", res.RestorePoints, "alreadyApplied", res.AlreadyApplied)
	_ = os.WriteFile("/dev/termination-log", []byte(sendVMDiskMessage(res, nil)), 0o600)
}

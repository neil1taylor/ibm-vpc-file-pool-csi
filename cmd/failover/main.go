// Command failover is a CLI tool (and kubectl plugin) for performing
// cross-region disaster recovery failover of pool-based SubVolumes.
//
// It reads .subvolume-metadata.json files from the destination NFS mount
// (written by the replication controller) and creates SubVolume CRs, PVs,
// and PVCs on the DR cluster.
//
// Usage:
//
//	kubectl failover plan     --nfs-mount-path /repl/dst/10.245.3.8
//	kubectl failover execute  --nfs-mount-path /repl/dst/10.245.3.8 --dr-pool dr-production --dr-share-ip 10.245.3.8 [--dry-run]
//	kubectl failover status
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/failover"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	subcommand := os.Args[1]
	switch subcommand {
	case "plan":
		if err := runPlan(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "execute":
		if err := runExecute(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "status":
		if err := runStatus(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("kubectl-failover %s\n", version)
	case "--help", "-h", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", subcommand)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `kubectl-failover — Cross-region DR failover for pool-based SubVolumes

Usage:
  kubectl failover plan     [flags]    Scan destination NFS and build a failover plan
  kubectl failover execute  [flags]    Create SubVolume/PV/PVC resources on the DR cluster
  kubectl failover status   [flags]    Check failover progress
  kubectl failover version             Print version

Plan flags:
  --nfs-mount-path     Local mount path of the destination NFS share (required)

Execute flags:
  --nfs-mount-path     Local mount path of the destination NFS share (required)
  --dr-pool            FileSharePool name on the DR cluster (required)
  --dr-share-ip        DR share mount target IP (required)
  --dr-export-path     NFS export path for VPC access mode (optional)
  --namespace, -n      Override PVC namespace (optional)
  --dry-run            Print what would happen without making changes
  --kubeconfig         Path to kubeconfig for the DR cluster (default: ~/.kube/config)

Status flags:
  --kubeconfig         Path to kubeconfig for the DR cluster (default: ~/.kube/config)
`)
}

// parseFlags is a simple flag parser that handles --key=value and --key value forms.
func parseFlags(args []string) map[string]string {
	flags := make(map[string]string)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		arg = strings.TrimLeft(arg, "-")

		if idx := strings.Index(arg, "="); idx >= 0 {
			flags[arg[:idx]] = arg[idx+1:]
			continue
		}

		// Handle boolean flags.
		if arg == "dry-run" {
			flags[arg] = "true"
			continue
		}

		// Handle -n shorthand for namespace.
		if arg == "n" {
			arg = "namespace"
		}

		if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			flags[arg] = args[i+1]
			i++
		} else {
			flags[arg] = "true"
		}
	}
	return flags
}

func buildClient(kubeconfig string) (client.Client, error) {
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig from %s: %w", kubeconfig, err)
	}

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create controller-runtime client: %w", err)
	}
	return c, nil
}

func flagOrDefault(flags map[string]string, key, defaultVal string) string {
	if v, ok := flags[key]; ok && v != "" {
		return v
	}
	return defaultVal
}

func runPlan(args []string) error {
	flags := parseFlags(args)
	nfsMountPath := flags["nfs-mount-path"]

	if nfsMountPath == "" {
		return fmt.Errorf("--nfs-mount-path is required")
	}

	nfsOps := pool.NewRealNFSOperations()
	planner := failover.NewPlanner(nfsOps)

	plan, err := planner.Plan(nfsMountPath)
	if err != nil {
		return err
	}

	if len(plan.SubVolumes) == 0 {
		fmt.Printf("No SubVolume metadata found at %s/pvcs/\n", nfsMountPath)
		return nil
	}

	fmt.Printf("Failover Plan\n")
	fmt.Printf("=============\n")
	fmt.Printf("NFS Mount Path:  %s\n", nfsMountPath)
	fmt.Printf("SubVolumes:      %d\n", len(plan.SubVolumes))
	fmt.Printf("RPO (max age):   %s\n\n", plan.RPO.Round(time.Second))

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SUBVOLUME\tPVC\tNAMESPACE\tSIZE (GiB)\tLAST SYNC\tPOLICY")
	for _, sv := range plan.SubVolumes {
		age := time.Since(sv.LastSyncTime).Round(time.Second)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s ago\t%s\n",
			sv.SubVolumeName, sv.PVCName, sv.PVCNamespace,
			sv.RequestedGB, age, sv.PolicyName)
	}
	_ = w.Flush()

	fmt.Printf("\nTo execute failover, run:\n")
	fmt.Printf("  kubectl failover execute --nfs-mount-path %s --dr-pool <POOL> --dr-share-ip <IP>\n", nfsMountPath)

	return nil
}

func runExecute(args []string) error {
	flags := parseFlags(args)
	nfsMountPath := flags["nfs-mount-path"]
	drPool := flags["dr-pool"]
	drShareIP := flags["dr-share-ip"]
	drExportPath := flags["dr-export-path"]
	namespace := flags["namespace"]
	kubeconfig := flags["kubeconfig"]
	dryRun := flags["dry-run"] == "true"

	if nfsMountPath == "" {
		return fmt.Errorf("--nfs-mount-path is required")
	}
	if drPool == "" {
		return fmt.Errorf("--dr-pool is required")
	}
	if drShareIP == "" {
		return fmt.Errorf("--dr-share-ip is required")
	}

	// Build the plan first.
	nfsOps := pool.NewRealNFSOperations()
	planner := failover.NewPlanner(nfsOps)
	plan, err := planner.Plan(nfsMountPath)
	if err != nil {
		return err
	}

	if len(plan.SubVolumes) == 0 {
		fmt.Printf("No SubVolume metadata found at %s/pvcs/\n", nfsMountPath)
		return nil
	}

	opts := failover.ExecuteOptions{
		DRPoolName:        drPool,
		DRShareIP:         drShareIP,
		DRExportPath:      drExportPath,
		NamespaceOverride: namespace,
		DryRun:            dryRun,
	}

	if dryRun {
		// Dry-run doesn't need a real client.
		exec := failover.NewExecutor(nil, os.Stdout)
		return exec.Execute(context.Background(), plan, opts)
	}

	c, err := buildClient(kubeconfig)
	if err != nil {
		return err
	}

	exec := failover.NewExecutor(c, os.Stdout)
	return exec.Execute(context.Background(), plan, opts)
}

func runStatus(args []string) error {
	flags := parseFlags(args)
	kubeconfig := flags["kubeconfig"]

	c, err := buildClient(kubeconfig)
	if err != nil {
		return err
	}

	reporter := failover.NewStatusReporter(c)
	statuses, err := reporter.Report(context.Background())
	if err != nil {
		return err
	}

	if len(statuses) == 0 {
		fmt.Println("No failover SubVolumes found on this cluster.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SUBVOLUME\tPVC\tNAMESPACE\tPVC PHASE\tPOLICY")
	for _, s := range statuses {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			s.SubVolumeName, s.PVCName, s.PVCNamespace, s.PVCPhase, s.PolicyName)
	}
	_ = w.Flush()

	return nil
}

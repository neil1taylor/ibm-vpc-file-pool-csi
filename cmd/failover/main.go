// Command failover is a CLI tool (and kubectl plugin) for performing
// cross-region disaster recovery failover of pool-based SubVolumes.
//
// It reads .subvolume-metadata.json files from the destination NFS mount
// (written by the replication controller) and creates SubVolume CRs, PVs,
// and PVCs on the DR cluster.
//
// Usage:
//
//	kubectl failover plan     --nfs-mount-path /repl/dst/10.245.3.8 [--list-moments] [--rewind-to TIME]
//	kubectl failover execute  --nfs-mount-path /repl/dst/10.245.3.8 --dr-pool dr-production --dr-share-ip 10.245.3.8 [--dry-run] [--rewind-to TIME] [--force]
//	kubectl failover status
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
  --list-moments       For each running VM, list the moments all its disks can show, and exit
  --rewind-to          Use the newest moment at or before this time (RFC 3339) for running VMs
  --force              Go on although a copy may still be applied to a disk

Execute flags:
  --nfs-mount-path     Local mount path of the destination NFS share (required)
  --dr-pool            FileSharePool name on the DR cluster (required)
  --dr-share-ip        DR share mount target IP (required)
  --dr-export-path     NFS export path for VPC access mode (optional)
  --namespace, -n      Override PVC namespace (optional)
  --rewind-to          Use the newest moment at or before this time (RFC 3339) for running VMs
  --force              Go on although a copy may still be applied to a disk. An unfinished copy is rolled back first
  --prepare-only       Only roll back and rewind running VM disks, then stop. Needs no cluster and no --dr-pool or --dr-share-ip
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
		if arg == "dry-run" || arg == "list-moments" || arg == "force" || arg == "prepare-only" {
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

func runPlan(args []string) error {
	flags := parseFlags(args)
	nfsMountPath := flags["nfs-mount-path"]

	if nfsMountPath == "" {
		return fmt.Errorf("--nfs-mount-path is required")
	}

	if flags["list-moments"] == "true" {
		return runListMoments(flags)
	}

	plan, _, err := planFromFlags(pool.NewRealNFSOperations(), flags, time.Now())
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
	printVMPlan(os.Stdout, plan)

	fmt.Printf("\nTo execute failover, run:\n")
	fmt.Printf("  kubectl failover execute --nfs-mount-path %s --dr-pool <POOL> --dr-share-ip <IP>\n", nfsMountPath)

	return nil
}

// runListMoments lists the moments each running VM can show. It does not
// choose one, so it works even where choosing would be refused.
func runListMoments(flags map[string]string) error {
	nfsMountPath := flags["nfs-mount-path"]
	plan, err := failover.NewPlanner(pool.NewRealNFSOperations()).Plan(nfsMountPath)
	if err != nil {
		return err
	}
	if len(plan.SubVolumes) == 0 {
		fmt.Printf("No SubVolume metadata found at %s/pvcs/\n", nfsMountPath)
		return nil
	}
	vmOpts, err := vmOptionsFromFlags(flags, time.Now())
	if err != nil {
		return err
	}
	moments, err := plan.CommonMoments(vmOpts)
	if err != nil {
		return err
	}
	printMoments(os.Stdout, moments)
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
	prepareOnly := flags["prepare-only"] == "true"

	if nfsMountPath == "" {
		return fmt.Errorf("--nfs-mount-path is required")
	}
	if !prepareOnly && drPool == "" {
		return fmt.Errorf("--dr-pool is required")
	}
	if !prepareOnly && drShareIP == "" {
		return fmt.Errorf("--dr-share-ip is required")
	}

	// Build the plan first.
	plan, _, err := planFromFlags(pool.NewRealNFSOperations(), flags, time.Now())
	if err != nil {
		return err
	}

	if len(plan.SubVolumes) == 0 {
		fmt.Printf("No SubVolume metadata found at %s/pvcs/\n", nfsMountPath)
		return nil
	}

	printVMPlan(os.Stdout, plan)

	if prepareOnly {
		// Roll back and rewind running VM disks now, ahead of a failover. No
		// cluster is needed, and nothing is created on it.
		fmt.Println("Preparing running VM disks (rolling back unfinished copies, rewinding where needed)...")
		if err = plan.MaterializeVMs(); err != nil {
			return err
		}
		for _, sv := range plan.SubVolumes {
			if sv.Rewind {
				fmt.Printf("  %s rewound into %s\n", sv.SubVolumeName, sv.SubPath)
			}
		}
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

	// Rewinding copies a whole disk, so it can take minutes.
	fmt.Println("Preparing running VM disks (rolling back unfinished copies, rewinding where needed)...")
	if err = plan.MaterializeVMs(); err != nil {
		return err
	}

	exec := failover.NewExecutor(c, os.Stdout)
	return exec.Execute(context.Background(), plan, opts)
}

// planFromFlags builds the plan for the share named by --nfs-mount-path. It
// calls the same function as the driver's failover Job, so the two cannot
// differ. It returns the options it used, for listing moments.
func planFromFlags(nfsOps pool.NFSOperations, flags map[string]string, now time.Time) (*failover.FailoverPlan, failover.VMOptions, error) {
	vmOpts, err := vmOptionsFromFlags(flags, now)
	if err != nil {
		return nil, vmOpts, err
	}
	plan, err := failover.BuildPlan(nfsOps, flags["nfs-mount-path"], vmOpts)
	return plan, vmOpts, err
}

// vmOptionsFromFlags reads the flags that choose the moment of running VM disks.
func vmOptionsFromFlags(flags map[string]string, now time.Time) (failover.VMOptions, error) {
	o := failover.VMOptions{Force: flags["force"] == "true", Now: now}
	if v := flags["rewind-to"]; v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return o, fmt.Errorf("--rewind-to: %w", err)
		}
		o.RewindTo = &t
	}
	return o, nil
}

// printVMPlan lists, for each running VM, the moment its disks will show and
// which disks must be rewound to reach it. Volumes that are not VM disks are
// left out.
func printVMPlan(out io.Writer, plan *failover.FailoverPlan) {
	groups := map[string][]failover.FailoverSubVolumePlan{}
	for _, sv := range plan.SubVolumes {
		if sv.VMGroup != "" {
			groups[sv.VMGroup] = append(groups[sv.VMGroup], sv)
		}
	}
	if len(groups) == 0 {
		return
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)
	_, _ = fmt.Fprintf(out, "\nRunning VM disks\n")
	for _, g := range names {
		_, _ = fmt.Fprintf(out, "  VM %s shows the moment %s\n", g, groups[g][0].VMMoment.UTC().Format(time.RFC3339))
		for _, sv := range groups[g] {
			how := "in place"
			if sv.Rewind {
				how = "rewind (copies the whole disk into a new folder)"
			}
			_, _ = fmt.Fprintf(out, "    %s: %s\n", sv.SubVolumeName, how)
		}
	}
}

// printMoments lists the moments each VM's disks can all show, newest first.
func printMoments(out io.Writer, moments map[string][]time.Time) {
	if len(moments) == 0 {
		_, _ = fmt.Fprintln(out, "No running VM disks found.")
		return
	}
	names := make([]string, 0, len(moments))
	for g := range moments {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		_, _ = fmt.Fprintf(out, "VM %s\n", g)
		for _, m := range moments[g] {
			_, _ = fmt.Fprintf(out, "  %s\n", m.UTC().Format(time.RFC3339))
		}
	}
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

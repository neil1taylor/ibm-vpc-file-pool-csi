// Command migrate is a CLI tool (and kubectl plugin) for migrating PVCs from
// the stock IBM VPC File CSI driver to the pool-based CSI driver.
//
// Usage:
//
//	kubectl migrate plan --namespace default --storage-class ibm-vpc-block-5iops-tier --target-pool general-purpose
//	kubectl migrate execute --namespace default --pvc my-data --target-pool general-purpose --target-storage-class ibm-vpc-file-pool
//	kubectl migrate status --namespace default
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/migrate"
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
		fmt.Printf("kubectl-migrate %s\n", version)
	case "--help", "-h", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n\n", subcommand)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `kubectl-migrate — Migrate PVCs from stock IBM VPC File CSI to pool CSI driver

Usage:
  kubectl migrate plan     [flags]    Discover PVCs and build a migration plan
  kubectl migrate execute  [flags]    Migrate a specific PVC
  kubectl migrate status   [flags]    Check migration progress
  kubectl migrate version             Print version

Plan flags:
  --namespace, -n        Kubernetes namespace (default: "default")
  --storage-class        Source StorageClass name to filter PVCs (required)
  --target-pool          Target FileSharePool name (required)
  --kubeconfig           Path to kubeconfig (default: ~/.kube/config)

Execute flags:
  --namespace, -n              Kubernetes namespace (default: "default")
  --pvc                        Source PVC name to migrate (required)
  --target-pool                Target FileSharePool name (required)
  --target-storage-class       Target StorageClass on the pool driver (required)
  --dry-run                    Print what would happen without making changes
  --kubeconfig                 Path to kubeconfig (default: ~/.kube/config)

Status flags:
  --namespace, -n        Kubernetes namespace (default: "default")
  --kubeconfig           Path to kubeconfig (default: ~/.kube/config)
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

		// Handle boolean flags (flags with no value).
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

func buildClientset(kubeconfig string) (kubernetes.Interface, error) {
	if kubeconfig == "" {
		if home := homedir.HomeDir(); home != "" {
			kubeconfig = filepath.Join(home, ".kube", "config")
		}
	}

	// Try in-cluster config first if kubeconfig doesn't exist.
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubeconfig from %s: %w", kubeconfig, err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}
	return clientset, nil
}

func runPlan(args []string) error {
	flags := parseFlags(args)
	namespace := flagOrDefault(flags, "namespace", "default")
	storageClass := flags["storage-class"]
	targetPool := flags["target-pool"]
	kubeconfig := flags["kubeconfig"]

	if storageClass == "" {
		return fmt.Errorf("--storage-class is required")
	}
	if targetPool == "" {
		return fmt.Errorf("--target-pool is required")
	}

	clientset, err := buildClientset(kubeconfig)
	if err != nil {
		return err
	}

	planner := migrate.NewPlanner(clientset)
	ctx := context.Background()
	plan, err := planner.Plan(ctx, namespace, storageClass, targetPool)
	if err != nil {
		return err
	}

	if len(plan.PVCs) == 0 {
		fmt.Printf("No PVCs found using StorageClass %q in namespace %q.\n", storageClass, namespace)
		return nil
	}

	fmt.Printf("Migration Plan\n")
	fmt.Printf("==============\n")
	fmt.Printf("Namespace:          %s\n", plan.Namespace)
	fmt.Printf("Source StorageClass: %s\n", plan.SourceStorageClass)
	fmt.Printf("Target Pool:        %s\n", plan.TargetPool)
	fmt.Printf("PVCs found:         %d\n", len(plan.PVCs))
	fmt.Printf("Total size:         %d GiB\n\n", plan.TotalSizeGB)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSIZE (GiB)\tPHASE\tSHARE ID\tPODS")
	for _, pvc := range plan.PVCs {
		pods := "(none)"
		if len(pvc.Pods) > 0 {
			pods = strings.Join(pvc.Pods, ", ")
		}
		shareID := pvc.ShareID
		if shareID == "" {
			shareID = "(unknown)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\n",
			pvc.Name, pvc.SizeGB, pvc.Phase, shareID, pods)
	}
	_ = w.Flush()

	fmt.Printf("\nTo migrate a PVC, run:\n")
	fmt.Printf("  kubectl migrate execute --namespace %s --pvc <PVC_NAME> --target-pool %s --target-storage-class <POOL_SC>\n",
		namespace, targetPool)

	return nil
}

func runExecute(args []string) error {
	flags := parseFlags(args)
	namespace := flagOrDefault(flags, "namespace", "default")
	pvcName := flags["pvc"]
	targetPool := flags["target-pool"]
	targetSC := flags["target-storage-class"]
	kubeconfig := flags["kubeconfig"]
	dryRun := flags["dry-run"] == "true"

	if pvcName == "" {
		return fmt.Errorf("--pvc is required")
	}
	if targetPool == "" {
		return fmt.Errorf("--target-pool is required")
	}
	if targetSC == "" {
		return fmt.Errorf("--target-storage-class is required")
	}

	clientset, err := buildClientset(kubeconfig)
	if err != nil {
		return err
	}

	executor := migrate.NewExecutor(clientset, os.Stdout)
	ctx := context.Background()
	result, err := executor.Execute(ctx, namespace, pvcName, targetPool, targetSC, dryRun)
	if err != nil {
		return err
	}

	fmt.Println()
	if result.Success {
		fmt.Printf("SUCCESS: %s\n", result.Message)
	} else {
		fmt.Printf("FAILED: %s\n", result.Message)
		os.Exit(1)
	}
	return nil
}

func runStatus(args []string) error {
	flags := parseFlags(args)
	namespace := flagOrDefault(flags, "namespace", "default")
	kubeconfig := flags["kubeconfig"]

	clientset, err := buildClientset(kubeconfig)
	if err != nil {
		return err
	}

	executor := migrate.NewExecutor(clientset, os.Stdout)
	ctx := context.Background()
	statuses, err := executor.Status(ctx, namespace)
	if err != nil {
		return err
	}

	if len(statuses) == 0 {
		fmt.Printf("No migration pods found in namespace %q.\n", namespace)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "POD\tSOURCE PVC\tTARGET PVC\tPHASE\tSTARTED")
	for _, s := range statuses {
		started := "(pending)"
		if s.StartTime != nil {
			started = s.StartTime.Format("2006-01-02 15:04:05")
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			s.PodName, s.SourcePVC, s.TargetPVC, s.Phase, started)
	}
	_ = w.Flush()
	return nil
}

func flagOrDefault(flags map[string]string, key, defaultVal string) string {
	if v, ok := flags[key]; ok && v != "" {
		return v
	}
	return defaultVal
}

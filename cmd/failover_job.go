package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/failover"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"k8s.io/klog/v2"
)

// failoverJobMessage runs what `kubectl failover` runs, on the share at
// mountPath, and returns the termination message for the controller: the
// result, or the reason for the failure. A failure returns both the message
// and the error.
func failoverJobMessage(nfsOps pool.NFSOperations, mountPath, action, rewindTo, vms string, force bool, expectJSON string, now time.Time) (string, error) {
	res, err := runFailoverRequest(nfsOps, mountPath, action, rewindTo, vms, force, expectJSON, now)
	out := failover.Outcome{Result: res}
	if err != nil {
		out = failover.Outcome{Error: err.Error()}
	}
	msg, encErr := failover.EncodeOutcome(out)
	if encErr != nil {
		msg, _ = failover.EncodeOutcome(failover.Outcome{Error: encErr.Error()})
		if err == nil {
			err = encErr
		}
	}
	return msg, err
}

func runFailoverRequest(nfsOps pool.NFSOperations, mountPath, action, rewindTo, vms string, force bool, expectJSON string, now time.Time) (*failover.Result, error) {
	req := failover.Request{MountPath: mountPath, Action: action, Force: force, Now: now}
	if rewindTo != "" {
		t, err := time.Parse(time.RFC3339Nano, rewindTo)
		if err != nil {
			return nil, fmt.Errorf("rewindTo: %w", err)
		}
		req.RewindTo = &t
	}
	names, err := failover.ParseVMs(vms)
	if err != nil {
		return nil, fmt.Errorf("vms: %w", err)
	}
	req.VMs = names
	if expectJSON != "" {
		var e failover.Expectation
		if err := json.Unmarshal([]byte(expectJSON), &e); err != nil {
			return nil, fmt.Errorf("the expected plan (--expect) is not valid: %w", err)
		}
		req.Expect = &e
	}
	return failover.RunJob(nfsOps, req)
}

// runFailover is the failover Job. The Failover controller starts it with the
// destination claim mounted at nfsMountPath, and reads the result from the
// termination log.
func runFailover(nfsMountPath, action, rewindTo, vms string, force bool, expectJSON string) {
	if nfsMountPath == "" || action == "" {
		klog.ErrorS(nil, "nfs-mount-path and failover-action are required for failover mode")
		os.Exit(1)
	}
	msg, err := failoverJobMessage(pool.NewRealNFSOperations(), nfsMountPath, action, rewindTo, vms, force, expectJSON, time.Now())
	if werr := os.WriteFile("/dev/termination-log", []byte(msg), 0o600); werr != nil {
		klog.ErrorS(werr, "Failed to write the termination log")
	}
	if err != nil {
		klog.ErrorS(err, "Failover failed", "action", action)
		os.Exit(1)
	}
	klog.InfoS("Failover finished", "action", action)
}

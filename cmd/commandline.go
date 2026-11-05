package main

import (
	"flag"
	"io"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// flagAliases are flags that were renamed. The old name still works for one
// release, is not in --help, and prints one line on stderr.
var flagAliases = []util.FlagAlias{{Old: "restore-moment", New: "restore-to-restore-point"}}

// parseCommandLine renames the deprecated flags, then parses.
func parseCommandLine(args []string, stderr io.Writer) error {
	args, err := util.RewriteAliasedArgs(args, flagAliases, stderr)
	if err != nil {
		return err
	}
	return flag.CommandLine.Parse(args)
}
